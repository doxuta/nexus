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
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"nexus/platform/internal/config"
)

// ask is a request shaped "system user"; q changes the contents, never the shape.
func ask(q string) ChatRequest {
	return ChatRequest{Model: "m", Messages: []ChatMessage{{Role: "system", Content: "persona"}, {Role: "user", Content: q}}}
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
	if err := os.WriteFile(path, []byte(`{"fingerprint":"a line from an older recording"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("cassette has %d lines; want %d (older recording dropped, failed call not written)", len(got), len(want))
	}
	for i, w := range want {
		e := got[i]
		if lastUserMessage(e.Request.Messages) != w.q || replyOf(e.Response) != w.reply || e.Fingerprint != "system user" {
			t.Errorf("line %d = %q → %q [%s]; want %q → %q [system user]",
				i+1, lastUserMessage(e.Request.Messages), replyOf(e.Response), e.Fingerprint, w.q, w.reply)
		}
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
	// Same shapes as the recording, different contents (a live run carries fresh ids).
	for i, req := range []ChatRequest{ask("other words"), toolTurn("list_entities"), ask("a fresh id 7f3c")} {
		resp, err := c.Complete(context.Background(), req)
		if want := fmt.Sprintf("a%d", i+1); err != nil || replyOf(resp) != want {
			t.Fatalf("call %d = %q, %v; want %q", i+1, replyOf(resp), err, want)
		}
	}
}

// A cassette that no longer matches the calls fails as a gateway error that says it is stale and how
// to re-record — never by quietly answering with the wrong exchange.
func TestReplay_StaleCassetteFailsWithReRecordHint(t *testing.T) {
	tests := []struct {
		name  string
		calls []ChatRequest // the last call must fail
	}{
		{name: "roles differ", calls: []ChatRequest{{Messages: []ChatMessage{{Role: "user"}}}}},
		{name: "tool call differs", calls: []ChatRequest{ask("q1"), toolTurn("get_layout")}},
		{name: "cassette exhausted", calls: []ChatRequest{ask("q1"), toolTurn("list_entities"), ask("q3")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := recordCassette(t, []ChatRequest{ask("q1"), toolTurn("list_entities")},
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
			for _, want := range []string{"stale", path, "AI_GATEWAY_MODE=record"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestFingerprint_IsTheShapeNotTheContents(t *testing.T) {
	twoCalls := ChatRequest{Messages: []ChatMessage{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Function: ToolCallFunc{Name: "list_entities"}}, {ID: "c2", Function: ToolCallFunc{Name: "get_layout"}}}},
		{Role: "tool", ToolCallID: "c1"},
		{Role: "tool", ToolCallID: "c2"},
	}}
	tests := []struct {
		name string
		req  ChatRequest
		want string
	}{
		{name: "roles in order", req: ask("q"), want: "system user"},
		{name: "tool calls named", req: twoCalls, want: "user assistant(list_entities,get_layout) tool tool"},
		{name: "contents, ids and model ignored", req: ChatRequest{Model: "other", Messages: []ChatMessage{
			{Role: "system", Content: "grounding with a fresh uuid 7f3c"}, {Role: "user", Content: "other words"}}}, want: "system user"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fingerprint(tt.req); got != tt.want {
				t.Fatalf("fingerprint = %q; want %q", got, tt.want)
			}
		})
	}
}

// The builder and the assistant can call at once: record writes whole lines, each request beside its
// own response, and replay hands out every exchange exactly once. Meaningful under -race.
func TestRecordReplay_ConcurrentCalls(t *testing.T) {
	const n = 32
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "concurrent.jsonl")
	rec, err := newRecorder(echoCompleter{}, path)
	if err != nil {
		t.Fatalf("newRecorder: %v", err)
	}
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			if _, err := rec.Complete(ctx, ask(fmt.Sprintf("q%02d", i))); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	lines := readCassette(t, path)
	if len(lines) != n {
		t.Fatalf("cassette has %d lines; want %d", len(lines), n)
	}
	for _, e := range lines {
		if want := "echo " + lastUserMessage(e.Request.Messages); replyOf(e.Response) != want {
			t.Errorf("a line pairs %q with %q", lastUserMessage(e.Request.Messages), replyOf(e.Response))
		}
	}

	rep, err := NewCompleter(config.AIConfig{GatewayMode: config.AIGatewayReplay, Cassette: path})
	if err != nil {
		t.Fatalf("NewCompleter(replay): %v", err)
	}
	served := make(chan string, n)
	for range n {
		wg.Go(func() {
			resp, err := rep.Complete(ctx, ask("any"))
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
	if _, err := rep.Complete(ctx, ask("one more")); !errors.Is(err, ErrGateway) {
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
	got, err := NewAIBuilderAgent(cfg, replay, freshDB, nil, nil, nil).Chat(ctx, Scope{UserID: "u1"}, "tenant-A", "", msgs)
	if err != nil {
		t.Fatalf("replayed Chat: %v", err)
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
	if err := os.WriteFile(notJSON, []byte(`{"fingerprint":"user"}`+"\nnot json\n"), 0o644); err != nil {
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
