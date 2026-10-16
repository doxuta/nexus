package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/ai"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/services"
)

// fakeAssistantAgent records the recordScope + history it receives so the handler tests can prove
// the trap-#1 threading: what the agent gets is EXACTLY scopeOf(c)'s output.
type fakeAssistantAgent struct {
	res      ai.Result
	err      error
	called   bool
	gotScope services.RecordScope
	lastMsgs []ai.ChatMessage
	enabled  bool
}

func (f *fakeAssistantAgent) Enabled() bool { return f.enabled }

func (f *fakeAssistantAgent) Ask(_ context.Context, recordScope services.RecordScope, _, _ string, messages []ai.ChatMessage) (ai.Result, error) {
	f.called = true
	f.gotScope = recordScope
	f.lastMsgs = messages
	return f.res, f.err
}

// fakeAssistantStore adapts the shared in-memory fakeSessionStore to the ASSISTANT-feature
// surface (AssistantSessionStore). The real feature-column split (builder vs assistant rails) is
// SQL and proven by the store's integration tests; here the fake enforces the same
// (tenant, user) ownership contract so the handler's 404/no-leak paths run against a real guard.
type fakeAssistantStore struct{ *fakeSessionStore }

func newFakeAssistantStore() *fakeAssistantStore { return &fakeAssistantStore{newFakeStore()} }

func (s *fakeAssistantStore) ListAssistantSessions(ctx context.Context, tenantID, userID string) ([]ai.SessionSummary, error) {
	return s.ListSessions(ctx, tenantID, userID)
}

func (s *fakeAssistantStore) GetAssistantSession(ctx context.Context, tenantID, userID, id string) (ai.Session, []ai.Message, error) {
	return s.GetSession(ctx, tenantID, userID, id)
}

func (s *fakeAssistantStore) AppendAssistantTurn(ctx context.Context, tenantID, userID, sessionID, newTitle, userContent, assistantContent string, assistantDraft json.RawMessage) (string, error) {
	return s.AppendTurn(ctx, tenantID, userID, sessionID, newTitle, userContent, assistantContent, assistantDraft)
}

func (s *fakeAssistantStore) SoftDeleteAssistantSession(ctx context.Context, tenantID, userID, id string) error {
	return s.SoftDeleteSession(ctx, tenantID, userID, id)
}

// assistantTestRouter mounts the assistant handler on a PLAIN authenticated group — deliberately
// NO RequireAdmin and NO RequireACL stage, mirroring router.go's protected-group mount. The shim
// injects the verified claims the way AuthMiddleware would.
func assistantTestRouter(agent AssistantAgent, store AssistantSessionStore, isAdmin bool) *gin.Engine {
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
	// guard explicitly nil (test-only ungated shape, same contract as the builder handler).
	NewAssistantHandler(agent, store, nil, nil).RegisterRoutes(grp)
	return r
}

func assistantOKResult() ai.Result {
	return ai.Result{
		Reply:   "Bạn có 12 cơ hội đang mở.",
		Usage:   ai.Usage{PromptTokens: 30, CompletionTokens: 12, TotalTokens: 42},
		RunID:   "run-1",
		Outcome: "ok",
	}
}

// TestAssistantHandler_NonAdmin_200 is the endpoint's reason to exist: a NON-admin user asks and
// gets a 200 with {sessionId, reply, usage, outcome} — no RequireAdmin gate anywhere.
func TestAssistantHandler_NonAdmin_200(t *testing.T) {
	agent := &fakeAssistantAgent{res: assistantOKResult()}
	store := newFakeAssistantStore()
	r := assistantTestRouter(agent, store, false /* NON-admin */)

	rec := doJSON(r, http.MethodPost, "/api/v1/assistant/ask", chatBody("", "có bao nhiêu cơ hội đang mở?"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a non-admin caller (body=%s)", rec.Code, rec.Body.String())
	}
	if !agent.called {
		t.Fatal("agent was never invoked")
	}
	var resp struct {
		Data struct {
			SessionID string `json:"sessionId"`
			Reply     string `json:"reply"`
			Outcome   string `json:"outcome"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.SessionID == "" || resp.Data.Reply == "" || resp.Data.Outcome != "ok" {
		t.Fatalf("unexpected data: %+v", resp.Data)
	}
	if body := rec.Body.String(); !strings.Contains(body, "promptTokens") {
		t.Fatalf("usage must serialize camelCase; body=%s", body)
	}
	// The turn persisted through the shared session store (user + assistant messages).
	se := store.sessions[resp.Data.SessionID]
	if se == nil || len(se.msgs) != 2 || se.userID != "u1" || se.tenantID != "t1" {
		t.Fatalf("session must persist the turn under (t1,u1); got %+v", se)
	}
}

// TestAssistantHandler_ScopeThreading is the trap-#1 pin at the HTTP layer: the recordScope the
// agent receives is EXACTLY scopeOf(c)'s output for this request — verified identity from the
// JWT claims, and AccessLevel failed CLOSED to "none" because no RequireACL stage runs on this
// route (the adapter later resolves the per-entity level). It must never arrive rebuilt (e.g.
// with a missing RoleID or a defaulted "all" level).
func TestAssistantHandler_ScopeThreading(t *testing.T) {
	agent := &fakeAssistantAgent{res: assistantOKResult()}
	r := assistantTestRouter(agent, newFakeAssistantStore(), false)

	rec := doJSON(r, http.MethodPost, "/api/v1/assistant/ask", chatBody("", "hỏi dữ liệu"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	want := services.RecordScope{
		UserID:      "u1",
		AccessLevel: string(dynrepo.AccessNone), // fail-closed: no ACL stage ran on this route
		IsAdmin:     false,
		RoleID:      "r1",
	}
	if agent.gotScope != want {
		t.Fatalf("agent recordScope = %+v; want scopeOf output %+v (threaded VERBATIM)", agent.gotScope, want)
	}
	// History: exactly the one new user turn for a fresh session.
	if len(agent.lastMsgs) != 1 || agent.lastMsgs[0].Role != "user" || agent.lastMsgs[0].Content != "hỏi dữ liệu" {
		t.Fatalf("agent history = %+v; want just the new user turn", agent.lastMsgs)
	}
}

// TestAssistantHandler_AdminScopeThreading: an admin's IsAdmin flag rides the same verbatim path.
func TestAssistantHandler_AdminScopeThreading(t *testing.T) {
	agent := &fakeAssistantAgent{res: assistantOKResult()}
	r := assistantTestRouter(agent, newFakeAssistantStore(), true)

	if rec := doJSON(r, http.MethodPost, "/api/v1/assistant/ask", chatBody("", "hỏi")); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !agent.gotScope.IsAdmin || agent.gotScope.UserID != "u1" {
		t.Fatalf("admin scope = %+v; want IsAdmin=true UserID=u1", agent.gotScope)
	}
}

// TestAssistantHandler_EmptyMessage_400 + foreign session 404 (no LLM spend on either).
func TestAssistantHandler_BadRequests(t *testing.T) {
	agent := &fakeAssistantAgent{res: assistantOKResult()}
	store := newFakeAssistantStore()
	// A session owned by ANOTHER user — must resolve 404 with no agent call (no existence leak).
	store.sessions["s-foreign"] = &fakeSess{id: "s-foreign", tenantID: "t1", userID: "someone-else"}
	r := assistantTestRouter(agent, store, false)

	if rec := doJSON(r, http.MethodPost, "/api/v1/assistant/ask", chatBody("", "   ")); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty message: status = %d, want 400", rec.Code)
	}
	rec := doJSON(r, http.MethodPost, "/api/v1/assistant/ask", chatBody("s-foreign", "câu hỏi"))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "SESSION_NOT_FOUND") {
		t.Fatalf("foreign session: status = %d body=%s; want 404 SESSION_NOT_FOUND", rec.Code, rec.Body.String())
	}
	if agent.called {
		t.Fatal("agent must not run for a rejected request (no LLM spend)")
	}
}

// TestAssistantHandler_Disabled_503: honest-disabled maps to 503 AI_DISABLED, nothing persisted.
func TestAssistantHandler_Disabled_503(t *testing.T) {
	agent := &fakeAssistantAgent{err: ai.ErrDisabled}
	store := newFakeAssistantStore()
	r := assistantTestRouter(agent, store, false)

	rec := doJSON(r, http.MethodPost, "/api/v1/assistant/ask", chatBody("", "hỏi"))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "AI_DISABLED") {
		t.Fatalf("status = %d body=%s; want 503 AI_DISABLED", rec.Code, rec.Body.String())
	}
	if len(store.sessions) != 0 {
		t.Fatal("a failed turn must not mint a session")
	}
}

// TestAssistantHandler_DraftsInResponse (⌘K-can-act): the agent's HITL record drafts ride the
// response as drafts[] — the SAME ai.DraftItem JSON the builder emits (camelCase recordCreate
// payload) — AND round-trip into the persisted assistant message's draft_json so a resumed thread
// re-renders its confirm cards. Nothing here writes a record.
func TestAssistantHandler_DraftsInResponse(t *testing.T) {
	res := assistantOKResult()
	res.Reply = "Đã soạn bản nháp — bấm Xác nhận để lưu."
	res.Draft = []ai.DraftItem{{
		Kind:         "record_create",
		RecordCreate: &ai.DraftRecordCreate{Entity: "tickets", Data: map[string]any{"title": "Khách A lỗi đăng nhập"}},
	}}
	agent := &fakeAssistantAgent{res: res}
	store := newFakeAssistantStore()
	r := assistantTestRouter(agent, store, false)

	rec := doJSON(r, http.MethodPost, "/api/v1/assistant/ask", chatBody("", "tạo ticket cho khách A"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			SessionID string         `json:"sessionId"`
			Drafts    []ai.DraftItem `json:"drafts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data.Drafts) != 1 || resp.Data.Drafts[0].Kind != "record_create" ||
		resp.Data.Drafts[0].RecordCreate == nil || resp.Data.Drafts[0].RecordCreate.Entity != "tickets" {
		t.Fatalf("drafts = %+v; want the one record_create confirm card", resp.Data.Drafts)
	}
	// Persisted for resume: the assistant message carries the draft JSON (not NULL).
	se := store.sessions[resp.Data.SessionID]
	if se == nil || len(se.msgs) != 2 || len(se.msgs[1].DraftJSON) == 0 {
		t.Fatalf("assistant turn must persist draft_json for resume; got %+v", se)
	}
}

// TestAssistantHandler_SessionRoutes (F9-5): the user-scoped ⌘K thread CRUD — list own newest
// first, get own (messages + drafts decoded for confirm-card re-render), delete own; a foreign
// user's id is 404 SESSION_NOT_FOUND on get AND delete (no existence leak). Mirrors the builder's
// admin session tests but on the PROTECTED (non-admin) group.
func TestAssistantHandler_SessionRoutes(t *testing.T) {
	agent := &fakeAssistantAgent{res: assistantOKResult()}
	store := newFakeAssistantStore()
	// Own thread WITH a persisted draft turn, seeded through the store's own write path.
	draft := json.RawMessage(`[{"kind":"record_update","recordUpdate":{"entity":"orders","recordId":"r-9","data":{"status":"delivered"},"before":{"status":"open"}}}]`)
	ownID, err := store.AppendAssistantTurn(context.Background(), "t1", "u1", "", "đổi trạng thái đơn", "đổi trạng thái đơn X", "Đã soạn bản nháp.", draft)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	store.sessions["s-foreign"] = &fakeSess{id: "s-foreign", tenantID: "t1", userID: "someone-else", title: "not yours"}
	r := assistantTestRouter(agent, store, false)

	// List: own thread only — the foreign one is invisible.
	rec := doJSON(r, http.MethodGet, "/api/v1/assistant/sessions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, ownID) || strings.Contains(body, "s-foreign") {
		t.Fatalf("list must contain own thread and never a foreign one; body=%s", body)
	}

	// Get own: header + messages, drafts decoded back to typed items.
	rec = doJSON(r, http.MethodGet, "/api/v1/assistant/sessions/"+ownID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Data struct {
			ID       string `json:"id"`
			Messages []struct {
				Role   string         `json:"role"`
				Drafts []ai.DraftItem `json:"drafts"`
			} `json:"messages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if got.Data.ID != ownID || len(got.Data.Messages) != 2 {
		t.Fatalf("get = %+v; want the seeded 2-message thread", got.Data)
	}
	ad := got.Data.Messages[1].Drafts
	if len(ad) != 1 || ad[0].Kind != "record_update" || ad[0].RecordUpdate == nil || ad[0].RecordUpdate.RecordID != "r-9" {
		t.Fatalf("resumed drafts = %+v; want the stored record_update card", ad)
	}

	// Foreign get/delete: 404 SESSION_NOT_FOUND, identical no-leak body.
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		rec = doJSON(r, method, "/api/v1/assistant/sessions/s-foreign", nil)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "SESSION_NOT_FOUND") {
			t.Fatalf("%s foreign: status = %d body=%s; want 404 SESSION_NOT_FOUND", method, rec.Code, rec.Body.String())
		}
	}

	// Delete own: 204, then the thread is gone (404 on get).
	if rec = doJSON(r, http.MethodDelete, "/api/v1/assistant/sessions/"+ownID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d; want 204", rec.Code)
	}
	if rec = doJSON(r, http.MethodGet, "/api/v1/assistant/sessions/"+ownID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d; want 404", rec.Code)
	}
}
