package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"nexus/platform/internal/ai"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// ---- the tool set ------------------------------------------------------------------------------

// forbidden are tools an outside agent must never get, whatever the builder agent grows.
var forbidden = []string{"apply_draft", "publish", "publish_version", "rollback_version", "grant_access",
	"set_access", "write_records", "create_record", "update_record", "run_ddl", "delete_entity", "execute_sql"}

func fakeDef(name string) ai.Tool {
	return ai.Tool{Type: "function", Function: ai.ToolFunction{Name: name, Description: name, Parameters: map[string]any{"type": "object"}}}
}

// The pin: the exposed set is exactly the builder agent's read and propose_* tools, renamed
// nexus_<name> with the same description and schema, and a tool with any other verb never appears.
func TestTools_AreTheBuilderAgentsReadAndProposeToolsOnly(t *testing.T) {
	defs := ai.ToolDefinitions()
	for _, n := range forbidden {
		defs = append(defs, fakeDef(n))
	}
	got := map[string]*mcp.Tool{}
	for _, tl := range tools(defs) {
		got[tl.Name] = tl
	}
	for _, n := range forbidden {
		if _, ok := got[toolPrefix+n]; ok {
			t.Errorf("%s is exposed over MCP; only read and propose_* tools may be", toolPrefix+n)
		}
	}
	for name, tl := range got {
		base := strings.TrimPrefix(name, toolPrefix)
		if !strings.HasPrefix(name, toolPrefix) || !(isRead(base) || isPropose(base)) {
			t.Errorf("%s is neither a read tool nor a propose_* tool", name)
		}
		if tl.Annotations == nil || tl.Annotations.ReadOnlyHint != isRead(base) {
			t.Errorf("%s: readOnlyHint must say whether the tool only reads", name)
		}
	}
	want := 0
	for _, d := range ai.ToolDefinitions() {
		tl, ok := got[toolPrefix+d.Function.Name]
		if !ok {
			t.Errorf("builder tool %s is missing over MCP", d.Function.Name)
			continue
		}
		want++
		if !strings.HasPrefix(tl.Description, d.Function.Description) || !reflect.DeepEqual(tl.InputSchema, d.Function.Parameters) {
			t.Errorf("%s: description and schema must be the builder agent's own", tl.Name)
		}
	}
	if len(got) != want {
		t.Errorf("exposed %d tools, want the %d of the builder agent", len(got), want)
	}
	for _, n := range []string{"list_entities", "list_fields", "list_relationships", "snapshot_model", "explain_access",
		"propose_entity", "propose_field", "propose_relationship", "propose_layout"} {
		if _, ok := got[toolPrefix+n]; !ok {
			t.Errorf("core tool %s%s missing", toolPrefix, n)
		}
	}
}

// Every builder agent tool reads or proposes: a tool of another kind would be dropped from MCP
// silently, and would break "agents propose, humans dispose" in the Studio too.
func TestBuilderAgentTools_OnlyReadOrPropose(t *testing.T) {
	for _, d := range ai.ToolDefinitions() {
		if n := d.Function.Name; !isRead(n) && !isPropose(n) {
			t.Errorf("builder agent tool %q neither reads nor proposes", n)
		}
	}
}

// ---- fakes -------------------------------------------------------------------------------------

const (
	goodToken = "nxm_good"
	infraFail = "nxm_dbdown"
)

var principal = services.MCPPrincipal{TokenID: "tok-1", TenantID: "t1", UserID: "u-builder", SessionID: "sess-1"}

type fakeVerifier struct{ calls int }

func (f *fakeVerifier) Verify(_ context.Context, raw string) (*services.MCPPrincipal, error) {
	f.calls++
	switch raw {
	case goodToken:
		p := principal
		return &p, nil
	case infraFail:
		return nil, errors.New("db down")
	}
	return nil, auth.ErrMCPTokenInvalid
}

type runCall struct {
	tenantID, tag, name, args string
	prior                     []ai.DraftItem
}

type fakeRunner struct {
	calls []runCall
	res   string
	added []ai.DraftItem
}

func (f *fakeRunner) RunTool(_ context.Context, tenantID, tag string, prior []ai.DraftItem, name, args string) (string, []ai.DraftItem) {
	f.calls = append(f.calls, runCall{tenantID, tag, name, args, prior})
	return f.res, f.added
}

type appended struct {
	tenantID, userID, sessionID, user, assistant string
	draft                                        json.RawMessage
}

type fakeThreads struct {
	msgs      []ai.Message
	getErr    error
	appendErr error
	gets      int
	appends   []appended
}

func (f *fakeThreads) GetSession(_ context.Context, tenantID, userID, id string) (ai.Session, []ai.Message, error) {
	f.gets++
	if f.getErr != nil {
		return ai.Session{}, nil, f.getErr
	}
	if tenantID != principal.TenantID || userID != principal.UserID || id != principal.SessionID {
		return ai.Session{}, nil, ai.ErrSessionNotFound
	}
	return ai.Session{ID: id}, f.msgs, nil
}

func (f *fakeThreads) AppendTurn(_ context.Context, tenantID, userID, sessionID, _, user, assistant string, draft json.RawMessage) (string, error) {
	if f.appendErr != nil {
		return "", f.appendErr
	}
	f.appends = append(f.appends, appended{tenantID, userID, sessionID, user, assistant, draft})
	return sessionID, nil
}

// bearer adds a fixed Authorization header to every request of the MCP client.
type bearer struct{ value string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", b.value)
	return http.DefaultTransport.RoundTrip(r)
}

func serve(t *testing.T, v Verifier, run Runner, th Threads) *httptest.Server {
	t.Helper()
	return serveWith(t, v, run, th, nil)
}

func serveWith(t *testing.T, v Verifier, run Runner, th Threads, f Freezer) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(New(v, run, th, f, zap.NewNop()))
	t.Cleanup(ts.Close)
	return ts
}

func connect(t *testing.T, url, auth string) *mcp.ClientSession {
	t.Helper()
	tr := &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer{auth}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil).Connect(context.Background(), tr, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// ---- who may call ------------------------------------------------------------------------------

func TestAuth_Matrix(t *testing.T) {
	v := &fakeVerifier{}
	ts := serve(t, v, &fakeRunner{}, &fakeThreads{})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	for _, tc := range []struct {
		name, header string
		want         int
		code         string
		verified     bool
	}{
		{"no credential", "", http.StatusUnauthorized, "UNAUTHORIZED", false},
		{"not a bearer", "Basic YWRtaW46cHc=", http.StatusUnauthorized, "UNAUTHORIZED", false},
		{"a JWT access token", "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1In0.c2ln", http.StatusUnauthorized, "UNAUTHORIZED", false},
		{"an API key", "Bearer nxk_" + strings.Repeat("A", 43), http.StatusForbidden, "FORBIDDEN", false},
		{"an expired, revoked or unknown MCP token", "Bearer nxm_revoked", http.StatusUnauthorized, "UNAUTHORIZED", true},
		{"the token store is down", "Bearer " + infraFail, http.StatusInternalServerError, "INTERNAL", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := v.calls
			req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var env struct{ Code string }
			_ = json.NewDecoder(resp.Body).Decode(&env)
			if resp.StatusCode != tc.want || env.Code != tc.code {
				t.Fatalf("status %d code %q; want %d %q", resp.StatusCode, env.Code, tc.want, tc.code)
			}
			if verified := v.calls > before; verified != tc.verified {
				t.Fatalf("token store consulted = %v; want %v (only nxm_ strings are looked up)", verified, tc.verified)
			}
		})
	}

	t.Run("a live MCP token lists the tools", func(t *testing.T) {
		res, err := connect(t, ts.URL, "Bearer "+goodToken).ListTools(context.Background(), nil)
		if err != nil || len(res.Tools) == 0 {
			t.Fatalf("tools/list: %v (%d tools)", err, len(res.Tools))
		}
	})
}

// ---- what a call does --------------------------------------------------------------------------

func callTool(t *testing.T, ts *httptest.Server, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := connect(t, ts.URL, "Bearer "+goodToken).CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s: %v", name, err)
	}
	return res
}

func text(r *mcp.CallToolResult) string {
	var parts []string
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func TestCall_ReadToolRunsAsTheBuilderAndLeavesTheThreadAlone(t *testing.T) {
	run := &fakeRunner{res: `{"ok":true,"result":{"entities":[{"name":"crm_contacts","label":"Liên hệ"}]}}`}
	th := &fakeThreads{}
	res := callTool(t, serve(t, &fakeVerifier{}, run, th), "nexus_list_entities", nil)
	if res.IsError || !strings.Contains(text(res), "crm_contacts") {
		t.Fatalf("result = %s (error %v)", text(res), res.IsError)
	}
	if len(run.calls) != 1 || run.calls[0].tenantID != "t1" || run.calls[0].name != "list_entities" || run.calls[0].args != "{}" {
		t.Fatalf("runner calls = %+v; want list_entities in the token's tenant", run.calls)
	}
	if th.gets+len(th.appends) != 0 {
		t.Fatal("a read tool must not read or write the thread")
	}
}

func TestCall_ProposalLandsInTheTokensThread(t *testing.T) {
	earlier := []ai.DraftItem{{Kind: "entity", Entity: &ai.DraftEntity{Name: "suppliers"}}}
	raw, _ := json.Marshal(earlier)
	th := &fakeThreads{msgs: []ai.Message{
		{Seq: 0, Role: "user", Content: "[MCP] Kết nối agent bên ngoài"},
		{Seq: 1, Role: "assistant", Content: "…", DraftJSON: raw},
	}}
	added := []ai.DraftItem{{Kind: "records", Records: &ai.DraftRecords{Entity: "suppliers", Rows: []map[string]any{{"name": "Acme"}}}}}
	run := &fakeRunner{res: `{"ok":true,"result":{"buffered":"1 record mẫu cho \"suppliers\""}}`, added: added}

	res := callTool(t, serve(t, &fakeVerifier{}, run, th), "nexus_propose_records", map[string]any{"entity": "suppliers"})
	if res.IsError || !strings.Contains(text(res), "Studio") {
		t.Fatalf("result = %s; want success that says where the proposal went", text(res))
	}
	c := run.calls[0]
	if c.name != "propose_records" || c.tag != "sess-1" || !reflect.DeepEqual(c.prior, earlier) {
		t.Fatalf("runner call = %+v; want the thread's earlier proposals and the thread as @ref tag", c)
	}
	if len(th.appends) != 1 {
		t.Fatalf("thread writes = %d, want 1", len(th.appends))
	}
	a := th.appends[0]
	var stored []ai.DraftItem
	if err := json.Unmarshal(a.draft, &stored); err != nil || !reflect.DeepEqual(stored, added) {
		t.Fatalf("stored draft = %s; want exactly this call's proposals", a.draft)
	}
	if a.tenantID != "t1" || a.userID != "u-builder" || a.sessionID != "sess-1" || !strings.Contains(a.user, "nexus_propose_records") || !strings.Contains(a.assistant, "suppliers") {
		t.Fatalf("thread turn = %+v", a)
	}
}

// Issue #42: a card the builder applied, discarded or undid in the Studio is not replayed into the
// next proposal's draft; the rest of the thread is, in order.
func TestCall_SettledProposalsAreNotReplayed(t *testing.T) {
	raw := func(items ...ai.DraftItem) json.RawMessage { b, _ := json.Marshal(items); return b }
	field := func(n string) ai.DraftItem {
		return ai.DraftItem{Kind: "field", Field: &ai.DraftField{Entity: "patients", Name: n}}
	}
	th := &fakeThreads{msgs: []ai.Message{
		{Seq: 1, Role: "assistant", DraftJSON: raw(field("discarded"), field("pending")), Settled: []int{0}},
		{Seq: 3, Role: "assistant", DraftJSON: raw(field("applied")), Settled: []int{0}},
		{Seq: 5, Role: "assistant", DraftJSON: raw(field("also_pending"))},
	}}
	run := &fakeRunner{res: `{"ok":false,"error":"FIELD_ALREADY_EXISTS: x"}`}
	callTool(t, serve(t, &fakeVerifier{}, run, th), "nexus_propose_field", map[string]any{"entity": "patients"})
	if want := []ai.DraftItem{field("pending"), field("also_pending")}; len(run.calls) != 1 || !reflect.DeepEqual(run.calls[0].prior, want) {
		t.Fatalf("replayed %+v; want only the pending cards %+v", run.calls, want)
	}
}

func TestCall_ProposalRefusedByAValidatorIsAToolErrorAndWritesNothing(t *testing.T) {
	th := &fakeThreads{}
	run := &fakeRunner{res: `{"ok":false,"error":"ENTITY_NOT_FOUND: entity \"nope\" không tồn tại"}`}
	res := callTool(t, serve(t, &fakeVerifier{}, run, th), "nexus_propose_field", map[string]any{"entity": "nope"})
	if !res.IsError || !strings.Contains(text(res), "ENTITY_NOT_FOUND") || len(th.appends) != 0 {
		t.Fatalf("result = %s (error %v), writes %d; want the validator's error and no write", text(res), res.IsError, len(th.appends))
	}
}

func TestCall_ProposalWithoutItsThread(t *testing.T) {
	for _, tc := range []struct {
		name string
		th   *fakeThreads
		ran  bool
	}{
		{"the builder deleted the thread", &fakeThreads{getErr: ai.ErrSessionNotFound}, false},
		{"the thread cannot be written", &fakeThreads{appendErr: errors.New("db down")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := &fakeRunner{res: `{"ok":true,"result":{"buffered":"entity \"x\""}}`, added: []ai.DraftItem{{Kind: "entity", Entity: &ai.DraftEntity{Name: "x"}}}}
			res := callTool(t, serve(t, &fakeVerifier{}, run, tc.th), "nexus_propose_entity", map[string]any{"name": "x"})
			if !res.IsError || strings.Contains(text(res), `"ok":true`) || (len(run.calls) > 0) != tc.ran {
				t.Fatalf("result = %s (error %v), ran %d; the agent must not be told it proposed", text(res), res.IsError, len(run.calls))
			}
		})
	}
}

// panicRunner panics on its first call, then answers like a read tool.
type panicRunner struct{ calls int }

func (p *panicRunner) RunTool(context.Context, string, string, []ai.DraftItem, string, string) (string, []ai.DraftItem) {
	p.calls++
	if p.calls == 1 {
		panic("boom")
	}
	return `{"ok":true,"result":{"entities":[]}}`, nil
}

// go-sdk runs every tools/call in its own goroutine with no recover, out of gin's Recovery: a
// panicking tool must become a tool error, not a dead API.
func TestCall_APanicIsAToolErrorAndTheServerLives(t *testing.T) {
	run := &panicRunner{}
	ts := serve(t, &fakeVerifier{}, run, &fakeThreads{})
	res := callTool(t, ts, "nexus_list_entities", nil)
	if !res.IsError || !strings.HasPrefix(text(res), "INTERNAL:") {
		t.Fatalf("result = %s (error %v); want an INTERNAL tool error", text(res), res.IsError)
	}
	if res = callTool(t, ts, "nexus_list_entities", nil); res.IsError || run.calls != 2 {
		t.Fatalf("the next call: %s (error %v, calls %d); want the server still serving", text(res), res.IsError, run.calls)
	}
}

type fakeFreezer struct {
	until  time.Time
	frozen bool
}

func (f fakeFreezer) FrozenUntil(context.Context, string) (time.Time, bool, error) {
	return f.until, f.frozen, nil
}

// During a maintenance or restore freeze every /api/v1 write answers 503; over /mcp reads keep
// working and a proposal (a thread write) is refused with a tool error, before any work.
func TestCall_DuringAFreezeReadsWorkProposalsDoNot(t *testing.T) {
	until := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	run := &fakeRunner{res: `{"ok":true,"result":{"buffered":"entity \"x\""}}`, added: []ai.DraftItem{{Kind: "entity", Entity: &ai.DraftEntity{Name: "x"}}}}
	th := &fakeThreads{}
	ts := serveWith(t, &fakeVerifier{}, run, th, fakeFreezer{until: until, frozen: true})

	res := callTool(t, ts, "nexus_propose_entity", map[string]any{"name": "x"})
	if !res.IsError || !strings.HasPrefix(text(res), "TENANT_MAINTENANCE:") || !strings.Contains(text(res), "2026-10-17T12:00:00Z") {
		t.Fatalf("propose while frozen: %s (error %v); want a TENANT_MAINTENANCE tool error naming the end", text(res), res.IsError)
	}
	if len(run.calls)+th.gets+len(th.appends) != 0 {
		t.Fatalf("propose while frozen ran %d tool(s), read the thread %d time(s), wrote %d turn(s); want none", len(run.calls), th.gets, len(th.appends))
	}
	run.res, run.added = `{"ok":true,"result":{"entities":[]}}`, nil
	if res = callTool(t, ts, "nexus_list_entities", nil); res.IsError || len(run.calls) != 1 {
		t.Fatalf("a read while frozen: %s (error %v); want it served", text(res), res.IsError)
	}
}
