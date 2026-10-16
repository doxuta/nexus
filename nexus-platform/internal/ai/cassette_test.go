package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"nexus/platform/internal/config"
	"nexus/platform/internal/metadata"
)

// ask is a request shaped "system user" whose prompt is q.
func ask(q string) ChatRequest {
	return ChatRequest{Model: "m", Messages: []ChatMessage{{Role: "system", Content: "persona"}, {Role: "user", Content: q}}}
}

// freshIDs is req as a later run sends it: the same prompts, but grounding and tool results that
// carry other ids, as they do against another database.
func freshIDs(req ChatRequest) ChatRequest {
	req.Messages = slices.Clone(req.Messages)
	for i := range req.Messages {
		if req.Messages[i].Role != "user" {
			req.Messages[i].Content += " (fresh id 7f3c)"
		}
	}
	return req
}

// toolTurn is a request whose assistant turn called the tool name and got its result back.
func toolTurn(name string) ChatRequest {
	return ChatRequest{Model: "m", Messages: []ChatMessage{
		{Role: "system", Content: "persona"},
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_" + name, Type: "function", Function: ToolCallFunc{Name: name, Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "call_" + name, Name: name, Content: `{"ok":true}`},
	}}
}

// echoCompleter answers every call with its user message. Stateless, so safe for concurrent use.
type echoCompleter struct{}

func (echoCompleter) Complete(_ context.Context, req ChatRequest) (ChatResponse, error) {
	return finalTextResp("echo " + lastUserMessage(req.Messages)), nil
}

// countingCompleter answers the nth call with "reply n". Safe for concurrent use.
type countingCompleter struct{ n atomic.Int64 }

func (c *countingCompleter) Complete(context.Context, ChatRequest) (ChatResponse, error) {
	return finalTextResp(fmt.Sprintf("reply %d", c.n.Add(1))), nil
}

// recordCassette records reqs[i] → resps[i] through the recorder and returns the cassette path.
func recordCassette(t *testing.T, reqs []ChatRequest, resps []ChatResponse) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cassette.jsonl")
	rec, err := newRecorder(&scriptedGateway{t: t, responses: resps}, path)
	if err != nil {
		t.Fatalf("newRecorder: %v", err)
	}
	for _, req := range reqs {
		if _, err := rec.Complete(context.Background(), req); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	return path
}

// readCassette parses the cassette with nothing but encoding/json: one JSON object per line.
func readCassette(t *testing.T, path string) []cassetteEntry {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cassette: %v", err)
	}
	var out []cassetteEntry
	for _, line := range bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n")) {
		var e cassetteEntry
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("cassette line %q is not one JSON object: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// olderRecording writes five long, valid cassette lines to path — longer than anything a test records
// afterwards, so leftovers show — and returns them.
func olderRecording(t *testing.T, path string) string {
	t.Helper()
	line := `{"fingerprint":{"shape":"older recording"},"request":{"model":"old","messages":[{"role":"user","content":"` +
		strings.Repeat("x", 400) + `"}]},"response":{"choices":[{"message":{"role":"assistant","content":"old"}}]}}` + "\n"
	old := strings.Repeat(line, 5)
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	return old
}

func replyOf(resp ChatResponse) string {
	if len(resp.Choices) == 0 {
		return ""
	}
	return resp.Choices[0].Message.Content
}

// record goes through the live gateway, key and all, and appends one JSON line per exchange in call
// order, starting the cassette afresh. The key only ever rides on the HTTP header, so nothing needs
// redacting: it never reaches the file. A failed call is passed through and not written.
func TestRecord_AppendsEveryExchangeInOrder_NeverTheKey(t *testing.T) {
	const key = "sk-cassette-secret-42"
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+key {
			t.Errorf("Authorization = %q; want the key on the wire", got)
		}
		n := calls.Add(1)
		if n == 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"reply %d"},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d}}`, n, n)
	}))
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "demo.jsonl")
	olderRecording(t, path)
	c, err := NewCompleter(config.AIConfig{GatewayMode: config.AIGatewayRecord, Cassette: path, BaseURL: srv.URL, APIKey: key, TimeoutSec: 5})
	if err != nil {
		t.Fatalf("NewCompleter(record): %v", err)
	}
	for i, q := range []string{"q1", "q2", "q3", "q4"} {
		resp, err := c.Complete(context.Background(), ask(q))
		if i == 2 {
			if !errors.Is(err, ErrGateway) {
				t.Fatalf("call 3 error = %v; want the live ErrGateway passed through", err)
			}
			continue
		}
		if err != nil || replyOf(resp) != fmt.Sprintf("reply %d", i+1) {
			t.Fatalf("call %d = %q, %v; want the live reply", i+1, replyOf(resp), err)
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), key) {
		t.Fatal("the cassette contains the API key")
	}
	got := readCassette(t, path)
	want := []struct{ q, reply string }{{"q1", "reply 1"}, {"q2", "reply 2"}, {"q4", "reply 4"}}
	if len(got) != len(want) {
		t.Fatalf("cassette has %d lines; want only the %d new ones (older recording dropped, failed call not written)", len(got), len(want))
	}
	for i, w := range want {
		e := got[i]
		if lastUserMessage(e.Request.Messages) != w.q || replyOf(e.Response) != w.reply {
			t.Errorf("line %d = %q → %q; want %q → %q", i+1, lastUserMessage(e.Request.Messages), replyOf(e.Response), w.q, w.reply)
		}
		if why := fingerprintOf(ask(w.q)).mismatch(e.Fingerprint); why != "" {
			t.Errorf("line %d fingerprint: the call %s", i+1, why)
		}
	}
}

// Record wipes nothing until it has something to write: a boot that records nothing (AI off, the
// gateway down, a restart before the first turn) leaves the previous cassette as it was, and the
// first exchange that succeeds replaces it.
func TestRecord_KeepsTheOldCassetteUntilAnExchangeSucceeds(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "demo.jsonl")
	old := olderRecording(t, path)
	rec, err := newRecorder(&scriptedGateway{t: t,
		errs:      []error{fmt.Errorf("%w: connection refused", ErrGateway)},
		responses: []ChatResponse{{}, finalTextResp("new")}}, path)
	if err != nil {
		t.Fatalf("newRecorder: %v", err)
	}
	unchanged := func(when string) {
		t.Helper()
		if raw, err := os.ReadFile(path); err != nil || string(raw) != old {
			t.Fatalf("%s: the older recording was touched (%d bytes, %v)", when, len(raw), err)
		}
	}
	unchanged("after boot")
	if _, err := rec.Complete(ctx, ask("q1")); !errors.Is(err, ErrGateway) {
		t.Fatalf("call 1 error = %v; want the gateway's ErrGateway", err)
	}
	unchanged("after a failed call")
	if _, err := rec.Complete(ctx, ask("q2")); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if lines := readCassette(t, path); len(lines) != 1 || replyOf(lines[0].Response) != "new" {
		t.Fatalf("cassette = %d lines; want only the new exchange", len(lines))
	}
}

// replay answers each call with the next recorded response and never touches the network: the base
// URL points at a server that fails the test if it is called.
func TestReplay_AnswersInOrderWithoutNetwork(t *testing.T) {
	path := recordCassette(t, []ChatRequest{ask("q1"), toolTurn("list_entities"), ask("q3")},
		[]ChatResponse{finalTextResp("a1"), finalTextResp("a2"), finalTextResp("a3")})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("replay called the live gateway")
	}))
	t.Cleanup(srv.Close)

	c, err := NewCompleter(config.AIConfig{GatewayMode: config.AIGatewayReplay, Cassette: path, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewCompleter(replay): %v", err)
	}
	// The recorded prompts, tools and shapes; grounding and tool results carry fresh ids.
	for i, req := range []ChatRequest{freshIDs(ask("q1")), freshIDs(toolTurn("list_entities")), freshIDs(ask("q3"))} {
		resp, err := c.Complete(context.Background(), req)
		if want := fmt.Sprintf("a%d", i+1); err != nil || replyOf(resp) != want {
			t.Fatalf("call %d = %q, %v; want %q", i+1, replyOf(resp), err, want)
		}
		// No model ran, so nothing is spent: recorded usage would eat the tenant's daily budget.
		if resp.Usage != (Usage{}) {
			t.Fatalf("call %d usage = %+v; want zero on a replayed call", i+1, resp.Usage)
		}
	}
}

// A cassette that no longer matches the calls fails as a gateway error that says it is stale, why,
// and how to re-record — never by quietly answering with the wrong exchange.
func TestReplay_StaleCassetteFailsWithReRecordHint(t *testing.T) {
	builderTools := ask("q1")
	builderTools.Tools = ToolDefinitions()
	assistantTools := ask("q1")
	assistantTools.Tools = AssistantToolDefinitions()
	tests := []struct {
		name  string
		calls []ChatRequest // the last call must fail
		why   string
	}{
		{name: "roles differ", calls: []ChatRequest{{Tools: ToolDefinitions(), Messages: []ChatMessage{{Role: "user", Content: "q1"}}}}, why: "sends [user] where the cassette recorded [system user]"},
		{name: "tool call differs", calls: []ChatRequest{builderTools, toolTurn("get_layout")}, why: "assistant(list_entities)"},
		{name: "another prompt", calls: []ChatRequest{func() ChatRequest { r := builderTools; r.Messages = ask("q2").Messages; return r }()}, why: "(another prompt)"},
		{name: "another agent", calls: []ChatRequest{assistantTools}, why: "(another agent, or tools added or renamed)"},
		{name: "cassette exhausted", calls: []ChatRequest{builderTools, toolTurn("list_entities"), ask("q3")}, why: "no exchange left for call 3 (the cassette holds 2)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := recordCassette(t, []ChatRequest{builderTools, toolTurn("list_entities")},
				[]ChatResponse{finalTextResp("a1"), finalTextResp("a2")})
			c, err := NewCompleter(config.AIConfig{GatewayMode: config.AIGatewayReplay, Cassette: path})
			if err != nil {
				t.Fatalf("NewCompleter(replay): %v", err)
			}
			last := len(tt.calls) - 1
			for i, req := range tt.calls[:last] {
				if _, err := c.Complete(context.Background(), req); err != nil {
					t.Fatalf("call %d: %v; want it answered", i+1, err)
				}
			}
			_, err = c.Complete(context.Background(), tt.calls[last])
			if !errors.Is(err, ErrGateway) {
				t.Fatalf("error = %v; want ErrGateway", err)
			}
			for _, want := range []string{"is stale: ", tt.why, "AI_GATEWAY_MODE=record AI_GATEWAY_CASSETTE=" + path} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// The fingerprint holds what tells one turn from another — the tools offered (names, not schemas),
// the user messages (hashed) and the conversation's shape — and leaves out what a fresh database
// changes: grounding, tool results and their ids.
func TestFingerprint_ToolsPromptsAndShapeNotFreshIDs(t *testing.T) {
	base := ChatRequest{Model: "m", Tools: ToolDefinitions(), Messages: []ChatMessage{
		{Role: "system", Content: "persona"},
		{Role: "system", Content: "grounding: entity patients, id 7f3c"},
		{Role: "user", Content: "Tạo bảng ticket"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Function: ToolCallFunc{Name: "list_entities"}}, {ID: "c2", Function: ToolCallFunc{Name: "get_layout"}}}},
		{Role: "tool", ToolCallID: "c1", Content: `{"entities":[{"id":"7f3c"}]}`},
		{Role: "tool", ToolCallID: "c2", Content: `{"layout":"9e1d"}`},
	}}
	edit := func(f func(r *ChatRequest)) ChatRequest {
		r := base
		r.Messages = slices.Clone(base.Messages)
		f(&r)
		return r
	}
	got := fingerprintOf(base)
	if got.Shape != "system system user assistant(list_entities,get_layout) tool tool" || got.User != sha256Hex("Tạo bảng ticket")[:8] ||
		len(got.Tools) != len(ToolDefinitions()) || got.Tools[0] != ToolDefinitions()[0].Function.Name {
		t.Fatalf("fingerprintOf = %+v; want the tool names, the hashed prompt and the shape", got)
	}
	tests := []struct {
		name  string
		req   ChatRequest
		match bool
	}{
		{name: "fresh ids, other schemas and the model ignored", match: true, req: edit(func(r *ChatRequest) {
			r.Model = "other"
			r.Tools = slices.Clone(r.Tools)
			r.Tools[0].Function.Description = "a reworded description"
			r.Messages[1].Content = "grounding: entity patients, id 0b2a"
			r.Messages[4].Content = `{"entities":[{"id":"0b2a"}]}`
		})},
		{name: "another agent's tools", req: edit(func(r *ChatRequest) { r.Tools = AssistantToolDefinitions() })},
		{name: "another prompt", req: edit(func(r *ChatRequest) { r.Messages[2].Content = "Tạo bảng khách hàng" })},
		{name: "another tool called", req: edit(func(r *ChatRequest) {
			r.Messages[3].ToolCalls = []ToolCall{{ID: "c1", Function: ToolCallFunc{Name: "list_entities"}}, {ID: "c2", Function: ToolCallFunc{Name: "list_fields"}}}
		})},
		{name: "another shape", req: edit(func(r *ChatRequest) { r.Messages = r.Messages[:3] })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if why := fingerprintOf(tt.req).mismatch(got); (why == "") != tt.match {
				t.Fatalf("mismatch = %q; want a match: %v", why, tt.match)
			}
		})
	}
}

// The builder and the assistant can call at once: record writes whole lines, each request beside its
// own response, and replay hands out every exchange exactly once. Meaningful under -race.
func TestRecordReplay_ConcurrentCalls(t *testing.T) {
	const n = 32
	ctx := context.Background()
	record := func(live Completer, prompt func(i int) string) string {
		path := filepath.Join(t.TempDir(), "concurrent.jsonl")
		rec, err := newRecorder(live, path)
		if err != nil {
			t.Fatalf("newRecorder: %v", err)
		}
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				if _, err := rec.Complete(ctx, ask(prompt(i))); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		return path
	}

	lines := readCassette(t, record(echoCompleter{}, func(i int) string { return fmt.Sprintf("q%02d", i) }))
	if len(lines) != n {
		t.Fatalf("cassette has %d lines; want %d", len(lines), n)
	}
	for _, e := range lines {
		if want := "echo " + lastUserMessage(e.Request.Messages); replyOf(e.Response) != want {
			t.Errorf("a line pairs %q with %q", lastUserMessage(e.Request.Messages), replyOf(e.Response))
		}
	}

	// n recordings of one prompt, so any arrival order matches; each must be served once.
	rep, err := NewCompleter(config.AIConfig{GatewayMode: config.AIGatewayReplay,
		Cassette: record(&countingCompleter{}, func(int) string { return "same prompt" })})
	if err != nil {
		t.Fatalf("NewCompleter(replay): %v", err)
	}
	served := make(chan string, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			resp, err := rep.Complete(ctx, ask("same prompt"))
			if err != nil {
				t.Error(err)
				return
			}
			served <- replyOf(resp)
		})
	}
	wg.Wait()
	close(served)
	seen := map[string]bool{}
	for r := range served {
		if seen[r] {
			t.Errorf("%q served twice", r)
		}
		seen[r] = true
	}
	if len(seen) != n {
		t.Fatalf("replay served %d distinct exchanges; want all %d", len(seen), n)
	}
	if _, err := rep.Complete(ctx, ask("same prompt")); !errors.Is(err, ErrGateway) {
		t.Fatalf("call %d error = %v; want the exhausted-cassette ErrGateway", n+1, err)
	}
}

// The AI surfaces run on a cassette alone: a builder turn recorded once, tool loop and all, replays
// with AI_ENABLED=true and neither base URL nor key — against a database whose contents differ from
// the one it was recorded on, as a fresh CI database does, so its grounding and tool results differ.
func TestReplay_DrivesTheBuilderWithoutBaseURLOrKey(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "builder.jsonl")
	rec, err := newRecorder(&scriptedGateway{t: t, responses: []ChatResponse{
		toolCallResp("call_1", "list_entities", "{}"),
		toolCallResp("call_2", "propose_entity",
			`{"name":"appointments","label":"Lịch hẹn","fields":[{"name":"start_at","label":"Bắt đầu","fieldType":"datetime","required":true}]}`),
		finalTextResp("Đã đề xuất appointments."),
	}}, path)
	if err != nil {
		t.Fatalf("newRecorder: %v", err)
	}
	msgs := []ChatMessage{{Role: "user", Content: "cần quản lý lịch hẹn"}}
	recorded, err := NewAIBuilderAgent(enabledCfg(6), rec, twoTenantReader(), nil, nil, nil).Chat(ctx, Scope{UserID: "u1"}, "tenant-A", "", msgs)
	if err != nil {
		t.Fatalf("recorded Chat: %v", err)
	}

	cfg := enabledCfg(6)
	cfg.GatewayMode, cfg.Cassette = config.AIGatewayReplay, path // BaseURL and APIKey stay empty
	replay, err := NewCompleter(cfg)
	if err != nil {
		t.Fatalf("NewCompleter(replay): %v", err)
	}
	freshDB := twoTenantReader()
	freshDB.entities["tenant-A"][0].Label = "Bệnh nhân (fresh database)"
	ledger := &fakeRecorder{}
	got, err := NewAIBuilderAgent(cfg, replay, freshDB, nil, ledger, nil).Chat(ctx, Scope{UserID: "u1"}, "tenant-A", "", msgs)
	if err != nil {
		t.Fatalf("replayed Chat: %v", err)
	}
	// The ledger row is what the daily token budget sums: a rehearsal must add nothing to it, while
	// the cassette keeps what the recorded turn really cost.
	if len(ledger.runs) != 1 || ledger.runs[0].PromptTokens != 0 || ledger.runs[0].OutputTokens != 0 || got.Usage != (Usage{}) {
		t.Fatalf("replayed turn spent %+v (ledger %+v); want zero tokens", got.Usage, ledger.runs)
	}
	if lines := readCassette(t, path); lines[0].Response.Usage.PromptTokens != 10 {
		t.Fatalf("cassette usage = %+v; want the recorded 10 prompt tokens kept", lines[0].Response.Usage)
	}
	if got.Outcome != outcomeOK || got.Reply != recorded.Reply || len(got.Draft) != 1 || got.Draft[0].Entity == nil || got.Draft[0].Entity.Name != "appointments" {
		t.Fatalf("replayed turn = %q %q %+v; want the recorded reply and the appointments draft", got.Outcome, got.Reply, got.Draft)
	}
}

// live (and the zero value) is today's gateway; anything that cannot work fails at startup, not on
// the first chat turn.
func TestNewCompleter_Modes(t *testing.T) {
	for _, mode := range []string{"", config.AIGatewayLive} {
		c, err := NewCompleter(config.AIConfig{GatewayMode: mode, BaseURL: "http://127.0.0.1:4000/v1"})
		if _, ok := c.(*Gateway); err != nil || !ok {
			t.Fatalf("mode %q = %T, %v; want the live *Gateway", mode, c, err)
		}
	}

	dir := t.TempDir()
	notJSON := filepath.Join(dir, "not-json.jsonl")
	if err := os.WriteFile(notJSON, []byte(`{"fingerprint":{"shape":"user"}}`+"\nnot json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		cfg     config.AIConfig
		wantErr string
	}{
		{name: "replay, no such cassette", cfg: config.AIConfig{GatewayMode: config.AIGatewayReplay, Cassette: filepath.Join(dir, "missing.jsonl")}, wantErr: "missing.jsonl"},
		{name: "replay, a line that is not JSON", cfg: config.AIConfig{GatewayMode: config.AIGatewayReplay, Cassette: notJSON}, wantErr: "line 2"},
		{name: "record into a missing directory", cfg: config.AIConfig{GatewayMode: config.AIGatewayRecord, Cassette: filepath.Join(dir, "no", "such.jsonl")}, wantErr: "such.jsonl"},
		{name: "unknown mode", cfg: config.AIConfig{GatewayMode: "replya"}, wantErr: "replya"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewCompleter(tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("NewCompleter error = %v; want one naming %q", err, tt.wantErr)
			}
			if c != nil {
				t.Fatalf("NewCompleter returned %T beside its error; want nil", c)
			}
		})
	}
}

// A replay answers each turn with that turn's own recording or refuses. Recorded as builder "A",
// builder "B", then the ⌘K assistant "C": replaying with the assistant first, or with the two builder
// prompts swapped, fails with the stale error at call 1, never hands one turn another's answer; the
// recorded order replays against a database whose contents differ.
func TestReplay_RefusesTurnsInAnotherOrder(t *testing.T) {
	ctx := context.Background()
	type turn struct{ agent, prompt, reply string }
	a := turn{"builder", "Tạo bảng ticket", "reply A"}
	b := turn{"builder", "Thêm trường ưu tiên cho ticket", "reply B"}
	c := turn{"assistant", "Có bao nhiêu ticket đang mở?", "reply C"}
	run := func(gw Completer, label string, tr turn) (string, error) {
		msgs := []ChatMessage{{Role: "user", Content: tr.prompt}}
		if tr.agent == "builder" {
			db := twoTenantReader()
			db.entities["tenant-A"][0].Label = label
			res, err := NewAIBuilderAgent(enabledCfg(3), gw, db, nil, nil, nil).Chat(ctx, Scope{UserID: "admin"}, "tenant-A", "", msgs)
			return res.Reply, err
		}
		rd := &fakeRecordReader{ents: []metadata.EntityDefinition{{Name: "tickets", Label: label}}}
		res, err := NewAssistantAgent(enabledCfg(3), gw, rd, nil, nil).Ask(ctx, callerScope(), "tenant-A", "", msgs)
		return res.Reply, err
	}

	path := filepath.Join(t.TempDir(), "turns.jsonl")
	rec, err := newRecorder(&scriptedGateway{t: t, responses: []ChatResponse{
		finalTextResp(a.reply), finalTextResp(b.reply), finalTextResp(c.reply)}}, path)
	if err != nil {
		t.Fatalf("newRecorder: %v", err)
	}
	for _, tr := range []turn{a, b, c} {
		if _, err := run(rec, "recorded database", tr); err != nil {
			t.Fatalf("record %q: %v", tr.prompt, err)
		}
	}

	cfg := enabledCfg(3)
	cfg.GatewayMode, cfg.Cassette = config.AIGatewayReplay, path
	tests := []struct {
		name        string
		turns       []turn
		wantStaleAt int // 0 = every turn gets its own recorded reply
	}{
		{name: "recorded order, another database", turns: []turn{a, b, c}},
		{name: "assistant first", turns: []turn{c, a, b}, wantStaleAt: 1},
		{name: "builder prompts swapped", turns: []turn{b, a, c}, wantStaleAt: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			replay, err := NewCompleter(cfg)
			if err != nil {
				t.Fatalf("NewCompleter(replay): %v", err)
			}
			for i, tr := range tt.turns {
				reply, err := run(replay, "fresh database", tr)
				if i+1 == tt.wantStaleAt {
					if !errors.Is(err, ErrGateway) || !strings.Contains(err.Error(), "is stale: call 1 ") {
						t.Fatalf("turn %d (%s %q) = %q, %v; want the stale-cassette ErrGateway at call 1", i+1, tr.agent, tr.prompt, reply, err)
					}
					return
				}
				if err != nil || reply != tr.reply {
					t.Fatalf("turn %d (%s %q) = %q, %v; want its own recorded %q", i+1, tr.agent, tr.prompt, reply, err, tr.reply)
				}
			}
		})
	}
}
