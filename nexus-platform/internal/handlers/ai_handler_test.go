package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/ai"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
)

// fakeBuilderAgent is a scripted BuilderAgent — it NEVER touches a live LLM gateway. It records whether
// it was invoked (so the ACL/ownership gates can be proven to short-circuit before the agent runs)
// and the history it received (so the server-held-history contract can be asserted). steps, when
// set, are replayed to ChatWithProgress's callback before returning — the SSE tests' progress feed.
type fakeBuilderAgent struct {
	res      ai.Result
	err      error
	steps    []ai.StepEvent
	called   bool
	lastMsgs []ai.ChatMessage
	lastTurn ai.TurnContext
}

// Chat delegates to ChatWithProgress with a nil callback — the same shape as the real agent.
func (f *fakeBuilderAgent) Chat(ctx context.Context, scope ai.Scope, tenantID, model string, messages []ai.ChatMessage) (ai.Result, error) {
	return f.ChatWithProgress(ctx, scope, tenantID, model, ai.TurnContext{}, messages, nil)
}

func (f *fakeBuilderAgent) ChatWithProgress(_ context.Context, _ ai.Scope, _, _ string, turn ai.TurnContext, messages []ai.ChatMessage, onStep func(ai.StepEvent)) (ai.Result, error) {
	f.called = true
	f.lastMsgs = messages
	f.lastTurn = turn
	if onStep != nil {
		for _, ev := range f.steps {
			onStep(ev)
		}
	}
	return f.res, f.err
}

// fakeSessionStore is an in-memory SessionStore that enforces the SAME (tenant, user) ownership
// contract as the real sqlx store, so the handler's 404/no-leak paths are exercised against a real
// guard rather than a stub. A session owned by another (tenant, user) resolves to ErrSessionNotFound.
// AppendTurn mirrors the real store's ALL-OR-NOTHING contract: on appendTurnErr it returns the error
// WITHOUT mutating s.sessions at all — proving (structurally, at the fake level) that a persistence
// failure leaves no partial state for the handler to have created.
type fakeSessionStore struct {
	sessions      map[string]*fakeSess
	seq           int
	appendTurnErr error // when set, AppendTurn returns this error and touches nothing
}

type fakeSess struct {
	id, tenantID, userID, title string
	deleted                     bool
	msgs                        []ai.Message
}

func newFakeStore() *fakeSessionStore {
	return &fakeSessionStore{sessions: map[string]*fakeSess{}}
}

func (s *fakeSessionStore) ListSessions(_ context.Context, tenantID, userID string) ([]ai.SessionSummary, error) {
	var out []ai.SessionSummary
	for _, se := range s.sessions {
		if se.tenantID == tenantID && se.userID == userID && !se.deleted {
			out = append(out, ai.SessionSummary{ID: se.id, Title: se.title})
		}
	}
	return out, nil
}

// owned resolves a session ONLY when it matches (tenant, user) and is live — the ownership guard.
func (s *fakeSessionStore) owned(tenantID, userID, id string) (*fakeSess, bool) {
	se, ok := s.sessions[id]
	if !ok || se.deleted || se.tenantID != tenantID || se.userID != userID {
		return nil, false
	}
	return se, true
}

func (s *fakeSessionStore) GetSession(_ context.Context, tenantID, userID, id string) (ai.Session, []ai.Message, error) {
	se, ok := s.owned(tenantID, userID, id)
	if !ok {
		return ai.Session{}, nil, ai.ErrSessionNotFound
	}
	return ai.Session{ID: se.id, TenantID: se.tenantID, UserID: se.userID, Title: se.title}, se.msgs, nil
}

// AppendTurn mirrors ai.SessionStore.AppendTurn: sessionID=="" creates a new session owned by
// (tenantID, userID); sessionID!="" requires a USER-scoped ownership match (owned) else
// ErrSessionNotFound — matching the real store's post-review-hardened guard.
func (s *fakeSessionStore) AppendTurn(_ context.Context, tenantID, userID, sessionID, newTitle, userContent, assistantContent string, assistantDraft json.RawMessage) (string, error) {
	if s.appendTurnErr != nil {
		return "", s.appendTurnErr
	}

	var se *fakeSess
	if sessionID == "" {
		s.seq++
		id := fmt.Sprintf("sess-%d", s.seq)
		se = &fakeSess{id: id, tenantID: tenantID, userID: userID, title: newTitle}
		s.sessions[id] = se
		sessionID = id
	} else {
		var ok bool
		se, ok = s.owned(tenantID, userID, sessionID)
		if !ok {
			return "", ai.ErrSessionNotFound
		}
	}

	baseSeq := 0
	if len(se.msgs) > 0 {
		baseSeq = se.msgs[len(se.msgs)-1].Seq + 1
	}
	se.msgs = append(se.msgs,
		ai.Message{SessionID: sessionID, TenantID: tenantID, Seq: baseSeq, Role: "user", Content: userContent},
		ai.Message{SessionID: sessionID, TenantID: tenantID, Seq: baseSeq + 1, Role: "assistant", Content: assistantContent, DraftJSON: assistantDraft},
	)
	return sessionID, nil
}

func (s *fakeSessionStore) SoftDeleteSession(_ context.Context, tenantID, userID, id string) error {
	se, ok := s.owned(tenantID, userID, id)
	if !ok {
		return ai.ErrSessionNotFound
	}
	se.deleted = true
	return nil
}

// aiTestRouter mounts the AI handler behind the REAL RequireAdmin gate, with a shim that injects the
// verified claims (tenant t1 + user u1) the way AuthMiddleware would. isAdmin toggles the admin flag
// so the 403 path is exercised through the actual middleware, not a stub.
func aiTestRouter(agent BuilderAgent, store SessionStore, isAdmin bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", "t1")
		c.Set("authClaims", &auth.Claims{Subject: auth.Subject{
			UserID: "u1", TenantID: "t1", RoleID: "r1", IsAdmin: isAdmin,
		}})
		c.Next()
	})
	grp := r.Group("/api/v1")
	grp.Use(middleware.RequireAdmin(nil))
	// guard explicitly nil (test-only ungated shape, post-review MED-2: the param is now REQUIRED
	// so every call site states its intent instead of a chainable that could be forgotten).
	NewAIBuilderHandler(agent, store, nil, nil, nil).RegisterRoutes(grp)
	return r
}

func chatBody(sessionID, message string) map[string]any {
	b := map[string]any{"message": message}
	if sessionID != "" {
		b["sessionId"] = sessionID
	}
	return b
}

func okResult() ai.Result {
	return ai.Result{
		Reply:   "Tôi đề xuất entity patients.",
		Draft:   []ai.DraftItem{{Kind: "entity", Entity: &ai.DraftEntity{Name: "patients", Label: "Bệnh nhân"}}},
		Usage:   ai.Usage{PromptTokens: 30, CompletionTokens: 12, TotalTokens: 42, CacheReadTokens: 20, CacheWriteTokens: 5},
		RunID:   "run-1",
		Outcome: "ok",
	}
}

// TestAIHandler_Chat_NewSession_200 proves the M3 contract: no sessionId → the server mints a
// session (returned in the response) and persists the user message + the assistant reply (with its
// draft). usage serializes camelCase.
func TestAIHandler_Chat_NewSession_200(t *testing.T) {
	agent := &fakeBuilderAgent{res: okResult()}
	store := newFakeStore()
	r := aiTestRouter(agent, store, true)

	rec := doJSON(r, http.MethodPost, "/api/v1/admin/ai/builder/chat", chatBody("", "Tôi cần quản lý bệnh nhân"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "promptTokens") || strings.Contains(body, "prompt_tokens") {
		t.Fatalf("usage must serialize camelCase (promptTokens, not prompt_tokens); body=%s", body)
	}
	// The cached part of the prompt is reported too (issue #30 item 8).
	if body := rec.Body.String(); !strings.Contains(body, `"cacheReadTokens":20`) || !strings.Contains(body, `"cacheWriteTokens":5`) {
		t.Fatalf("usage must carry cacheReadTokens and cacheWriteTokens; body=%s", body)
	}
	var resp struct {
		Data struct {
			SessionID string         `json:"sessionId"`
			Reply     string         `json:"reply"`
			Draft     []ai.DraftItem `json:"draft"`
			RunID     string         `json:"runId"`
			Outcome   string         `json:"outcome"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.SessionID == "" || resp.Data.Reply == "" || resp.Data.RunID != "run-1" || len(resp.Data.Draft) != 1 {
		t.Fatalf("unexpected data: %+v", resp.Data)
	}
	// Persisted: exactly the user turn then the assistant turn, in seq order, the assistant carrying
	// the draft. This is what a resume replays.
	se := store.sessions[resp.Data.SessionID]
	if se == nil || len(se.msgs) != 2 {
		t.Fatalf("session must persist 2 messages; got %+v", se)
	}
	if se.msgs[0].Role != "user" || se.msgs[0].Seq != 0 || se.msgs[0].Content != "Tôi cần quản lý bệnh nhân" {
		t.Fatalf("message[0] = %+v; want the user turn at seq 0", se.msgs[0])
	}
	if se.msgs[1].Role != "assistant" || se.msgs[1].Seq != 1 || len(se.msgs[1].DraftJSON) == 0 {
		t.Fatalf("message[1] = %+v; want the assistant turn at seq 1 with a draft", se.msgs[1])
	}
	// New session: the agent history is exactly the one new user turn.
	if len(agent.lastMsgs) != 1 || agent.lastMsgs[0].Role != "user" {
		t.Fatalf("agent history = %+v; want just the new user turn", agent.lastMsgs)
	}
}

// TestAIHandler_Chat_ExistingSession_200 proves server-held history: the agent receives the PRIOR
// turns loaded from the session, then the new user message, and the new turn appends after them.
func TestAIHandler_Chat_ExistingSession_200(t *testing.T) {
	agent := &fakeBuilderAgent{res: okResult()}
	store := newFakeStore()
	// Seed a session for the requesting user (u1/t1) with one prior exchange.
	store.sessions["s-existing"] = &fakeSess{
		id: "s-existing", tenantID: "t1", userID: "u1", title: "prior",
		msgs: []ai.Message{
			{SessionID: "s-existing", TenantID: "t1", Seq: 0, Role: "user", Content: "câu hỏi cũ"},
			{SessionID: "s-existing", TenantID: "t1", Seq: 1, Role: "assistant", Content: "trả lời cũ"},
		},
	}
	r := aiTestRouter(agent, store, true)

	rec := doJSON(r, http.MethodPost, "/api/v1/admin/ai/builder/chat", chatBody("s-existing", "câu hỏi mới"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	// Agent got prior [user, assistant] + the new user message.
	if len(agent.lastMsgs) != 3 {
		t.Fatalf("agent history len = %d; want 3 (2 prior + 1 new)", len(agent.lastMsgs))
	}
	if agent.lastMsgs[2].Role != "user" || agent.lastMsgs[2].Content != "câu hỏi mới" {
		t.Fatalf("last history message = %+v; want the new user turn", agent.lastMsgs[2])
	}
	// The new turn appended at seq 2 (user) and 3 (assistant).
	se := store.sessions["s-existing"]
	if len(se.msgs) != 4 || se.msgs[2].Seq != 2 || se.msgs[3].Seq != 3 {
		t.Fatalf("session messages = %+v; want 4 with the new turn at seq 2,3", se.msgs)
	}
}

// TestAIHandler_Chat_ForeignSession_404 proves the ownership guard: a session owned by ANOTHER user
// (same tenant) is refused 404 — the agent never runs (no LLM spend) and nothing is persisted.
func TestAIHandler_Chat_ForeignSession_404(t *testing.T) {
	agent := &fakeBuilderAgent{res: okResult()}
	store := newFakeStore()
	store.sessions["s-foreign"] = &fakeSess{id: "s-foreign", tenantID: "t1", userID: "u2", title: "other user's"}
	r := aiTestRouter(agent, store, true)

	rec := doJSON(r, http.MethodPost, "/api/v1/admin/ai/builder/chat", chatBody("s-foreign", "cho tôi xem"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a foreign session (body=%s)", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec.Body.Bytes(), "SESSION_NOT_FOUND")
	if agent.called {
		t.Fatal("agent must NOT run for a foreign session id (ownership gates before any LLM spend)")
	}
	if len(store.sessions["s-foreign"].msgs) != 0 {
		t.Fatal("a foreign-session chat must persist nothing")
	}
}

// TestAIHandler_Chat_AppendTurnGenericError_500 proves a persistence failure (e.g. a DB error inside
// AppendTurn) surfaces as 500. The agent turn itself succeeded (LLM cost already spent — a documented,
// non-blocking wart), but AppendTurn's all-or-nothing contract (proven by the real-MySQL integration
// test) guarantees no partial write, so there is no orphan/ghost state to clean up here — the fake
// enforces the same all-or-nothing shape (see fakeSessionStore.AppendTurn: touches nothing on error).
func TestAIHandler_Chat_AppendTurnGenericError_500(t *testing.T) {
	agent := &fakeBuilderAgent{res: okResult()}
	store := newFakeStore()
	store.appendTurnErr = fmt.Errorf("db down")
	r := aiTestRouter(agent, store, true)

	rec := doJSON(r, http.MethodPost, "/api/v1/admin/ai/builder/chat", chatBody("", "Tôi cần quản lý bệnh nhân"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec.Body.Bytes(), "AI_CHAT_FAILED")
	if len(store.sessions) != 0 {
		t.Fatalf("a failed AppendTurn must leave no session (all-or-nothing); got %d", len(store.sessions))
	}
}

// TestAIHandler_Chat_AppendTurnSessionNotFound_404 simulates the mid-chat-delete race (reviewer
// Finding 2): a session existed at the earlier ownership read (GetSession, before the LLM call) but
// is gone by the time AppendTurn runs (e.g. another request soft-deleted it in between). AppendTurn's
// own USER-scoped guard re-detects this and returns ErrSessionNotFound — the handler MUST map it to
// 404 SESSION_NOT_FOUND, never a generic 500.
func TestAIHandler_Chat_AppendTurnSessionNotFound_404(t *testing.T) {
	agent := &fakeBuilderAgent{res: okResult()}
	store := newFakeStore()
	store.sessions["s-race"] = &fakeSess{id: "s-race", tenantID: "t1", userID: "u1", title: "will vanish"}
	// Force AppendTurn to behave as if its own guard re-checked and found the session gone — the
	// exact effect a mid-turn soft-delete would have, independent of what the earlier GetSession saw.
	store.appendTurnErr = ai.ErrSessionNotFound
	r := aiTestRouter(agent, store, true)

	rec := doJSON(r, http.MethodPost, "/api/v1/admin/ai/builder/chat", chatBody("s-race", "câu hỏi"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a session that vanished mid-turn (body=%s)", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec.Body.Bytes(), "SESSION_NOT_FOUND")
}

// TestAIHandler_Chat_EmptyMessage_400: message is required.
func TestAIHandler_Chat_EmptyMessage_400(t *testing.T) {
	agent := &fakeBuilderAgent{}
	store := newFakeStore()
	r := aiTestRouter(agent, store, true)

	rec := doJSON(r, http.MethodPost, "/api/v1/admin/ai/builder/chat", map[string]any{"message": "   "})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec.Body.Bytes(), "INVALID_BODY")
	if agent.called {
		t.Fatal("agent must not run when message is empty")
	}
}

func TestAIHandler_NonAdmin_403(t *testing.T) {
	agent := &fakeBuilderAgent{}
	r := aiTestRouter(agent, newFakeStore(), false)

	rec := doJSON(r, http.MethodPost, "/api/v1/admin/ai/builder/chat", chatBody("", "hi"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if agent.called {
		t.Fatal("agent must NOT run for a non-admin (ACL gates before business logic)")
	}
}

// TestAIHandler_Disabled_503_NoJunkSession proves lazy creation: an honest-disabled turn returns 503
// AND leaves NO session behind (eager creation would litter a junk session on every default-off call).
func TestAIHandler_Disabled_503_NoJunkSession(t *testing.T) {
	agent := &fakeBuilderAgent{err: ai.ErrDisabled}
	store := newFakeStore()
	r := aiTestRouter(agent, store, true)

	rec := doJSON(r, http.MethodPost, "/api/v1/admin/ai/builder/chat", chatBody("", "hi"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec.Body.Bytes(), "AI_DISABLED")
	if len(store.sessions) != 0 {
		t.Fatalf("a disabled turn must create no session; got %d", len(store.sessions))
	}
}

func TestAIHandler_GatewayError_502(t *testing.T) {
	agent := &fakeBuilderAgent{err: fmt.Errorf("ai.Chat: %w", ai.ErrGateway)}
	r := aiTestRouter(agent, newFakeStore(), true)

	rec := doJSON(r, http.MethodPost, "/api/v1/admin/ai/builder/chat", chatBody("", "hi"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec.Body.Bytes(), "AI_GATEWAY_ERROR")
}

func TestAIHandler_Timeout_504(t *testing.T) {
	agent := &fakeBuilderAgent{err: fmt.Errorf("ai.Chat: %w", ai.ErrTimeout)}
	r := aiTestRouter(agent, newFakeStore(), true)

	rec := doJSON(r, http.MethodPost, "/api/v1/admin/ai/builder/chat", chatBody("", "hi"))
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (body=%s)", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec.Body.Bytes(), "AI_TIMEOUT")
}

// TestAIHandler_ListSessions_200 proves the list is scoped to the caller: only u1/t1's sessions
// return, not another user's in the same tenant.
func TestAIHandler_ListSessions_200(t *testing.T) {
	store := newFakeStore()
	store.sessions["a"] = &fakeSess{id: "a", tenantID: "t1", userID: "u1", title: "mine 1"}
	store.sessions["b"] = &fakeSess{id: "b", tenantID: "t1", userID: "u1", title: "mine 2"}
	store.sessions["c"] = &fakeSess{id: "c", tenantID: "t1", userID: "u2", title: "not mine"}
	r := aiTestRouter(&fakeBuilderAgent{}, store, true)

	rec := doJSON(r, http.MethodGet, "/api/v1/admin/ai/builder/sessions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("list returned %d sessions; want 2 (only the caller's own)", len(resp.Data))
	}
	for _, s := range resp.Data {
		if s.ID == "c" {
			t.Fatal("another user's session leaked into the caller's list")
		}
	}
}

// TestAIHandler_GetSession_200_and_404 proves an owned session returns with messages + decoded
// draft, while a foreign session is 404 (no existence leak).
func TestAIHandler_GetSession_200_and_404(t *testing.T) {
	store := newFakeStore()
	store.sessions["mine"] = &fakeSess{
		id: "mine", tenantID: "t1", userID: "u1", title: "mine",
		msgs: []ai.Message{
			{Seq: 0, Role: "user", Content: "hỏi"},
			{Seq: 1, Role: "assistant", Content: "đáp", DraftJSON: json.RawMessage(`[{"kind":"entity","entity":{"name":"patients","label":"BN","fields":[]}}]`)},
		},
	}
	store.sessions["foreign"] = &fakeSess{id: "foreign", tenantID: "t1", userID: "u2", title: "not mine"}
	r := aiTestRouter(&fakeBuilderAgent{}, store, true)

	rec := doJSON(r, http.MethodGet, "/api/v1/admin/ai/builder/sessions/mine", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			ID       string `json:"id"`
			Messages []struct {
				Role  string         `json:"role"`
				Draft []ai.DraftItem `json:"draft"`
			} `json:"messages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.ID != "mine" || len(resp.Data.Messages) != 2 {
		t.Fatalf("unexpected session: %+v", resp.Data)
	}
	if len(resp.Data.Messages[1].Draft) != 1 || resp.Data.Messages[1].Draft[0].Entity == nil {
		t.Fatalf("assistant message draft did not decode for resume: %+v", resp.Data.Messages[1])
	}

	rec404 := doJSON(r, http.MethodGet, "/api/v1/admin/ai/builder/sessions/foreign", nil)
	if rec404.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a foreign session (body=%s)", rec404.Code, rec404.Body.String())
	}
	assertErrorCode(t, rec404.Body.Bytes(), "SESSION_NOT_FOUND")
}

// TestAIHandler_DeleteSession_204_and_404 proves soft-delete for an owned session (204) and 404 for
// a foreign one (which must remain untouched).
func TestAIHandler_DeleteSession_204_and_404(t *testing.T) {
	store := newFakeStore()
	store.sessions["mine"] = &fakeSess{id: "mine", tenantID: "t1", userID: "u1", title: "mine"}
	store.sessions["foreign"] = &fakeSess{id: "foreign", tenantID: "t1", userID: "u2", title: "not mine"}
	r := aiTestRouter(&fakeBuilderAgent{}, store, true)

	rec := doJSON(r, http.MethodDelete, "/api/v1/admin/ai/builder/sessions/mine", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}
	if !store.sessions["mine"].deleted {
		t.Fatal("owned session must be soft-deleted")
	}

	rec404 := doJSON(r, http.MethodDelete, "/api/v1/admin/ai/builder/sessions/foreign", nil)
	if rec404.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a foreign session (body=%s)", rec404.Code, rec404.Body.String())
	}
	if store.sessions["foreign"].deleted {
		t.Fatal("a foreign session must NOT be deleted by another user")
	}
}

// ---- M4 BudgetGuard gating (spec §12) ----------------------------------------------------------

// fakeTurnGuard is a scripted TurnGuard: err != nil rejects the turn; otherwise it admits and
// records whether the handler called the release (the concurrency-slot contract). gotTenant
// captures the tenantID argument the handler actually passed (post-review NIT-3) so a test can
// assert it is the JWT-derived tenant, not e.g. the userID — a swap of the two arguments at the
// call site would otherwise stay green.
type fakeTurnGuard struct {
	err       error
	acquired  bool
	released  bool
	gotTenant string
}

func (g *fakeTurnGuard) Acquire(_ context.Context, tenantID string) (func(), error) {
	g.acquired = true
	g.gotTenant = tenantID
	if g.err != nil {
		return func() {}, g.err
	}
	return func() { g.released = true }, nil
}

// aiTestRouterWithGuard is aiTestRouter plus the M4 guard wired through the (now required)
// constructor param, so the guard-mapping tests exercise the REAL handler + real respondError
// switch.
func aiTestRouterWithGuard(agent BuilderAgent, store SessionStore, guard TurnGuard, isAdmin bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", "t1")
		c.Set("authClaims", &auth.Claims{Subject: auth.Subject{
			UserID: "u1", TenantID: "t1", RoleID: "r1", IsAdmin: isAdmin,
		}})
		c.Next()
	})
	grp := r.Group("/api/v1")
	grp.Use(middleware.RequireAdmin(nil))
	NewAIBuilderHandler(agent, store, guard, nil, nil).RegisterRoutes(grp)
	return r
}

// TestAIHandler_Chat_GuardLimits_429 table-drives the three guard rejections: each maps to its own
// 429 code with the Retry-After carried on the LimitError, the agent never runs (no LLM spend),
// and nothing is persisted.
func TestAIHandler_Chat_GuardLimits_429(t *testing.T) {
	tests := []struct {
		name           string
		guardErr       error
		wantCode       string
		wantRetryAfter string
	}{
		{
			name:           "rate limited carries window as Retry-After",
			guardErr:       &ai.LimitError{Sentinel: ai.ErrRateLimited, RetryAfterSec: 60},
			wantCode:       "AI_RATE_LIMITED",
			wantRetryAfter: "60",
		},
		{
			name:           "concurrency limit carries short backoff",
			guardErr:       &ai.LimitError{Sentinel: ai.ErrConcurrencyLimit, RetryAfterSec: 5},
			wantCode:       "AI_CONCURRENCY_LIMIT",
			wantRetryAfter: "5",
		},
		{
			name:           "budget exceeded carries seconds to UTC midnight",
			guardErr:       &ai.LimitError{Sentinel: ai.ErrBudgetExceeded, RetryAfterSec: 12345},
			wantCode:       "AI_BUDGET_EXCEEDED",
			wantRetryAfter: "12345",
		},
		{
			name:           "bare wrapped sentinel still maps (fallback Retry-After)",
			guardErr:       fmt.Errorf("ai.BudgetGuard.Acquire: %w", ai.ErrRateLimited),
			wantCode:       "AI_RATE_LIMITED",
			wantRetryAfter: "60",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &fakeBuilderAgent{res: okResult()}
			store := newFakeStore()
			guard := &fakeTurnGuard{err: tt.guardErr}
			r := aiTestRouterWithGuard(agent, store, guard, true)

			rec := doJSON(r, http.MethodPost, "/api/v1/admin/ai/builder/chat", chatBody("", "hi"))
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429 (body=%s)", rec.Code, rec.Body.String())
			}
			assertErrorCode(t, rec.Body.Bytes(), tt.wantCode)
			if got := rec.Header().Get("Retry-After"); got != tt.wantRetryAfter {
				t.Fatalf("Retry-After = %q, want %q", got, tt.wantRetryAfter)
			}
			if agent.called {
				t.Fatal("agent must NOT run when the guard rejects (no LLM spend)")
			}
			if len(store.sessions) != 0 {
				t.Fatal("a guard-rejected turn must persist nothing")
			}
		})
	}
}

// TestAIHandler_Chat_GuardBudgetReadError_500 proves fail-closed: a budget-query DB error (a plain,
// non-sentinel error from the guard) is a 500 AI_CHAT_FAILED — never a silent pass-through, never
// a misleading 429.
func TestAIHandler_Chat_GuardBudgetReadError_500(t *testing.T) {
	agent := &fakeBuilderAgent{res: okResult()}
	guard := &fakeTurnGuard{err: fmt.Errorf("ai.BudgetGuard.Acquire: budget check: %w", fmt.Errorf("db down"))}
	r := aiTestRouterWithGuard(agent, newFakeStore(), guard, true)

	rec := doJSON(r, http.MethodPost, "/api/v1/admin/ai/builder/chat", chatBody("", "hi"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 fail-closed (body=%s)", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec.Body.Bytes(), "AI_CHAT_FAILED")
	if agent.called {
		t.Fatal("agent must NOT run when the budget check fails closed")
	}
}

// TestAIHandler_Chat_GuardAdmits_200_ReleaseCalled: an admitted turn completes normally, the
// handler releases the concurrency slot (deferred release runs), and the guard was called with the
// JWT-derived TENANT id — not, say, the user id (post-review NIT-3: a swapped argument at the call
// site must fail this test, not stay silently green).
func TestAIHandler_Chat_GuardAdmits_200_ReleaseCalled(t *testing.T) {
	agent := &fakeBuilderAgent{res: okResult()}
	guard := &fakeTurnGuard{}
	r := aiTestRouterWithGuard(agent, newFakeStore(), guard, true)

	rec := doJSON(r, http.MethodPost, "/api/v1/admin/ai/builder/chat", chatBody("", "Tôi cần quản lý bệnh nhân"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !guard.acquired || !guard.released {
		t.Fatalf("guard acquired=%v released=%v; want both true (defer release must run)", guard.acquired, guard.released)
	}
	if guard.gotTenant != "t1" {
		t.Fatalf("guard.Acquire tenant = %q; want the JWT tenant \"t1\" (not the user id or anything else)", guard.gotTenant)
	}
}

// TestAIHandler_Chat_GuardReleaseOnAgentError proves the slot is released even when the agent
// errors mid-turn — a failing gateway must not leak concurrency slots until restart.
func TestAIHandler_Chat_GuardReleaseOnAgentError(t *testing.T) {
	agent := &fakeBuilderAgent{err: fmt.Errorf("ai.Chat: %w", ai.ErrGateway)}
	guard := &fakeTurnGuard{}
	r := aiTestRouterWithGuard(agent, newFakeStore(), guard, true)

	rec := doJSON(r, http.MethodPost, "/api/v1/admin/ai/builder/chat", chatBody("", "hi"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	if !guard.released {
		t.Fatal("guard slot must be released when the agent errors (deferred release)")
	}
}

// TestAIHandler_Chat_ForeignSession404_BeforeGuard: the session-ownership 404 fires BEFORE the
// guard, so a foreign-id probe cannot consume the tenant's rate/budget.
func TestAIHandler_Chat_ForeignSession404_BeforeGuard(t *testing.T) {
	store := newFakeStore()
	store.sessions["s-foreign"] = &fakeSess{id: "s-foreign", tenantID: "t1", userID: "u2", title: "other user's"}
	guard := &fakeTurnGuard{}
	r := aiTestRouterWithGuard(&fakeBuilderAgent{res: okResult()}, store, guard, true)

	rec := doJSON(r, http.MethodPost, "/api/v1/admin/ai/builder/chat", chatBody("s-foreign", "hi"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	if guard.acquired {
		t.Fatal("guard must NOT be consulted for a foreign session id (ownership gates first)")
	}
}

// TestAIHandler_SessionRoutes_NotGated: session list/get/delete are plain DB reads — the guard
// must never be consulted there even when it would reject (spec §12: chat only).
func TestAIHandler_SessionRoutes_NotGated(t *testing.T) {
	store := newFakeStore()
	store.sessions["mine"] = &fakeSess{id: "mine", tenantID: "t1", userID: "u1", title: "mine"}
	guard := &fakeTurnGuard{err: &ai.LimitError{Sentinel: ai.ErrRateLimited, RetryAfterSec: 60}}
	r := aiTestRouterWithGuard(&fakeBuilderAgent{}, store, guard, true)

	if rec := doJSON(r, http.MethodGet, "/api/v1/admin/ai/builder/sessions", nil); rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec := doJSON(r, http.MethodGet, "/api/v1/admin/ai/builder/sessions/mine", nil); rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec := doJSON(r, http.MethodDelete, "/api/v1/admin/ai/builder/sessions/mine", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}
	if guard.acquired {
		t.Fatal("session routes must NOT consult the budget guard (no LLM spend to gate)")
	}
}

// ---- SSE stream endpoint (POST /admin/ai/builder/chat/stream) -----------------------------------

// sseEvent is one decoded (event, data) pair from a recorded text/event-stream body.
type sseEvent struct {
	name string
	data string
}

// parseSSE decodes the httptest-recorded SSE body: "event:<name>" + "data:<json>" blocks separated
// by blank lines (gin-contrib/sse encoding).
func parseSSE(body string) []sseEvent {
	var out []sseEvent
	var cur sseEvent
	flush := func() {
		if cur.name != "" || cur.data != "" {
			out = append(out, cur)
		}
		cur = sseEvent{}
	}
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "event:"):
			cur.name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			cur.data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case strings.TrimSpace(line) == "":
			flush()
		}
	}
	flush()
	return out
}

// TestAIHandler_ChatStream_EventSequence_And_ResultParity proves the stream contract end-to-end:
// SSE headers, ≥1 "step" event (forwarding the agent's StepEvent JSON verbatim), then EXACTLY one
// "result" event whose data equals the plain /chat endpoint's 200 body for the same turn, then
// "done" — and the turn persists exactly like the non-stream path (same AppendTurn).
func TestAIHandler_ChatStream_EventSequence_And_ResultParity(t *testing.T) {
	steps := []ai.StepEvent{
		{Step: 1, MaxSteps: 6, Phase: "thinking"},
		{Step: 1, MaxSteps: 6, Phase: "calling_tool", Tool: "propose_entity"},
	}
	streamAgent := &fakeBuilderAgent{res: okResult(), steps: steps}
	streamStore := newFakeStore()
	rs := aiTestRouter(streamAgent, streamStore, true)

	rec := doJSON(rs, http.MethodPost, "/api/v1/admin/ai/builder/chat/stream", chatBody("", "Tôi cần quản lý bệnh nhân"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", cc)
	}
	if xa := rec.Header().Get("X-Accel-Buffering"); xa != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no (proxies must not buffer the stream)", xa)
	}
	if !rec.Flushed {
		t.Fatal("stream must flush after events (no buffering until the turn ends)")
	}

	events := parseSSE(rec.Body.String())
	var names []string
	for _, ev := range events {
		names = append(names, ev.name)
	}
	if want := []string{"step", "step", "result", "done"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("event sequence = %v, want %v (body=%s)", names, want, rec.Body.String())
	}

	// The step event forwards the agent's StepEvent as camelCase JSON, tool name included.
	var step struct {
		Step     int    `json:"step"`
		MaxSteps int    `json:"maxSteps"`
		Phase    string `json:"phase"`
		Tool     string `json:"tool"`
	}
	if err := json.Unmarshal([]byte(events[1].data), &step); err != nil {
		t.Fatalf("decode step event %q: %v", events[1].data, err)
	}
	if step.Step != 1 || step.MaxSteps != 6 || step.Phase != "calling_tool" || step.Tool != "propose_entity" {
		t.Fatalf("step event = %+v; want the agent's event verbatim in camelCase", step)
	}

	// Result parity: the SAME turn through the plain endpoint (fresh agent+store so both mint the
	// same fake session id) must produce a byte-equivalent JSON body.
	plainAgent := &fakeBuilderAgent{res: okResult()}
	plainStore := newFakeStore()
	rp := aiTestRouter(plainAgent, plainStore, true)
	recPlain := doJSON(rp, http.MethodPost, "/api/v1/admin/ai/builder/chat", chatBody("", "Tôi cần quản lý bệnh nhân"))
	if recPlain.Code != http.StatusOK {
		t.Fatalf("plain status = %d, want 200 (body=%s)", recPlain.Code, recPlain.Body.String())
	}
	var fromStream, fromPlain any
	if err := json.Unmarshal([]byte(events[2].data), &fromStream); err != nil {
		t.Fatalf("decode result event: %v", err)
	}
	if err := json.Unmarshal(recPlain.Body.Bytes(), &fromPlain); err != nil {
		t.Fatalf("decode plain body: %v", err)
	}
	if !reflect.DeepEqual(fromStream, fromPlain) {
		t.Fatalf("result event data != plain endpoint body:\nstream: %s\nplain:  %s", events[2].data, recPlain.Body.String())
	}

	// Persistence parity: the stream path ran the same AppendTurn — one session, user+assistant rows.
	se := streamStore.sessions["sess-1"]
	if se == nil || len(se.msgs) != 2 || se.msgs[0].Role != "user" || se.msgs[1].Role != "assistant" {
		t.Fatalf("stream turn must persist exactly like the plain turn; got %+v", se)
	}
}

// TestAIHandler_ChatStream_AgentError_ErrorEvent: a mid-stream agent failure becomes ONE "error"
// event carrying the SAME {error,code} envelope the JSON endpoint returns (502 → AI_GATEWAY_ERROR),
// with no "result"/"done" after it, and nothing persisted.
func TestAIHandler_ChatStream_AgentError_ErrorEvent(t *testing.T) {
	agent := &fakeBuilderAgent{
		err:   fmt.Errorf("ai.Chat: %w", ai.ErrGateway),
		steps: []ai.StepEvent{{Step: 1, MaxSteps: 6, Phase: "thinking"}},
	}
	store := newFakeStore()
	r := aiTestRouter(agent, store, true)

	rec := doJSON(r, http.MethodPost, "/api/v1/admin/ai/builder/chat/stream", chatBody("", "hi"))
	// The stream was already admitted (200 + event-stream) — the failure rides the stream.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (error travels as an SSE event once streaming)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	events := parseSSE(rec.Body.String())
	if len(events) != 2 || events[0].name != "step" || events[1].name != "error" {
		t.Fatalf("events = %+v; want [step, error] and the stream closed", events)
	}
	var envelope struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal([]byte(events[1].data), &envelope); err != nil {
		t.Fatalf("decode error event %q: %v", events[1].data, err)
	}
	if envelope.Code != "AI_GATEWAY_ERROR" || envelope.Error == "" {
		t.Fatalf("error event = %+v; want the JSON endpoint's AI_GATEWAY_ERROR envelope", envelope)
	}
	if len(store.sessions) != 0 {
		t.Fatal("a failed stream turn must persist nothing (same lazy-persist rule as /chat)")
	}
}

// TestAIHandler_ChatStream_Guard429_PlainJSON_BeforeSSE: a budget-guard refusal happens BEFORE the
// response becomes a stream — the client gets the ordinary 429 JSON envelope WITH its Retry-After
// header (which an SSE event could not carry), never an event-stream. No agent run, no persistence.
func TestAIHandler_ChatStream_Guard429_PlainJSON_BeforeSSE(t *testing.T) {
	agent := &fakeBuilderAgent{res: okResult()}
	store := newFakeStore()
	guard := &fakeTurnGuard{err: &ai.LimitError{Sentinel: ai.ErrRateLimited, RetryAfterSec: 60}}
	r := aiTestRouterWithGuard(agent, store, guard, true)

	rec := doJSON(r, http.MethodPost, "/api/v1/admin/ai/builder/chat/stream", chatBody("", "hi"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 as plain JSON before any SSE body (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json (NOT an event-stream)", ct)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want 60", got)
	}
	assertErrorCode(t, rec.Body.Bytes(), "AI_RATE_LIMITED")
	if strings.Contains(rec.Body.String(), "event:") {
		t.Fatalf("refused turn must contain no SSE events; body=%s", rec.Body.String())
	}
	if agent.called {
		t.Fatal("agent must NOT run when the guard rejects the stream turn")
	}
	if len(store.sessions) != 0 {
		t.Fatal("a guard-rejected stream turn must persist nothing")
	}
}

// TestAIHandler_ChatStream_GuardWrapsTurn: an admitted stream turn acquires and releases the SAME
// per-tenant guard as /chat (Acquire with the JWT tenant, deferred release runs after the turn).
func TestAIHandler_ChatStream_GuardWrapsTurn(t *testing.T) {
	agent := &fakeBuilderAgent{res: okResult(), steps: []ai.StepEvent{{Step: 1, MaxSteps: 6, Phase: "thinking"}}}
	guard := &fakeTurnGuard{}
	r := aiTestRouterWithGuard(agent, newFakeStore(), guard, true)

	rec := doJSON(r, http.MethodPost, "/api/v1/admin/ai/builder/chat/stream", chatBody("", "hi"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !guard.acquired || !guard.released {
		t.Fatalf("guard acquired=%v released=%v; want both true (stream turn wraps the guard like /chat)", guard.acquired, guard.released)
	}
	if guard.gotTenant != "t1" {
		t.Fatalf("guard.Acquire tenant = %q; want the JWT tenant \"t1\"", guard.gotTenant)
	}
}

// TestAIHandler_ChatStream_NonAdmin_403: the stream route sits in the same RequireAdmin group as
// every other builder route — a non-admin is refused plain-JSON 403 before any streaming.
func TestAIHandler_ChatStream_NonAdmin_403(t *testing.T) {
	agent := &fakeBuilderAgent{res: okResult()}
	r := aiTestRouter(agent, newFakeStore(), false)

	rec := doJSON(r, http.MethodPost, "/api/v1/admin/ai/builder/chat/stream", chatBody("", "hi"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if agent.called {
		t.Fatal("agent must NOT run for a non-admin on the stream route")
	}
}

func assertErrorCode(t *testing.T, body []byte, want string) {
	t.Helper()
	var resp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if resp.Code != want {
		t.Fatalf("error code = %q, want %q", resp.Code, want)
	}
}
