package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/ai"
	"nexus/platform/internal/middleware"
)

// BuilderAgent is the consumer-side view of the E9 AI Builder the handler drives (satisfied by
// *ai.AIBuilderAgent). Declaring it here lets the handler test inject a fake agent — the handler is
// never allowed to reach a live LLM gateway in tests. ChatWithProgress is the same turn with an
// optional step-event callback (nil = no progress) — the SSE endpoint's view; requiring it on the
// interface (rather than a type-assert with a silent no-progress fallback) makes "the wired agent
// can stream" a compile-time guarantee.
type BuilderAgent interface {
	Chat(ctx context.Context, scope ai.Scope, tenantID, model string, messages []ai.ChatMessage) (ai.Result, error)
	ChatWithProgress(ctx context.Context, scope ai.Scope, tenantID, model string, turn ai.TurnContext, messages []ai.ChatMessage, onStep func(ai.StepEvent)) (ai.Result, error)
}

// ContextResolver is the consumer-side view of ai.ContextResolver (specs/067): it re-resolves the
// client's context lookup keys inside the tenant of the JWT. Returns *ai.ContextError for a
// client-correctable miss, any other error for an infrastructure failure.
type ContextResolver interface {
	Resolve(ctx context.Context, tenantID string, in ai.TurnContextInput) (ai.TurnContext, error)
}

// SessionStore is the consumer-side view of the M3 session store (satisfied by *ai.SessionStore).
// EVERY method is scoped to the tenant + user the handler takes from the verified JWT — never from
// the body — so the handler cannot address another owner's sessions. Declared here so the handler
// test injects an in-memory fake that enforces the same ownership contract.
//
// AppendTurn is the ONLY write path into a session's messages (post-review FIX 1): it persists the
// whole turn (new user message + assistant reply + draft), creating the session first when
// sessionID is "", ATOMICALLY — so a mid-turn failure can never leave an orphan user-turn or an
// empty ghost session (see ai.SessionStore.AppendTurn's doc for the full contract, including the
// USER-scoped ownership guard on an existing sessionID).
type SessionStore interface {
	ListSessions(ctx context.Context, tenantID, userID string) ([]ai.SessionSummary, error)
	GetSession(ctx context.Context, tenantID, userID, id string) (ai.Session, []ai.Message, error)
	AppendTurn(ctx context.Context, tenantID, userID, sessionID, newTitle, userContent, assistantContent string, assistantDraft json.RawMessage) (string, error)
	SoftDeleteSession(ctx context.Context, tenantID, userID, id string) error
}

// TurnGuard is the consumer-side view of the M4 ai.BudgetGuard (spec §12, MED-1): the per-tenant
// rate/concurrency/daily-budget gate around one chat turn. Acquire either admits the turn
// (returning a release the handler MUST defer — it is idempotent) or rejects it with a
// *ai.LimitError (→ 429) or a plain error (budget-read failure → fail-closed 500). Declared here
// so handler tests inject a stub and never depend on the guard's internals.
type TurnGuard interface {
	Acquire(ctx context.Context, tenantID string) (release func(), err error)
}

// AIBuilderHandler serves the admin-only conversational AI-Builder surface (spec §6 + §11): the
// chat endpoint plus the session CRUD (list/get/delete). The tenant + verified user come from the
// JWT claim AuthMiddleware placed in context — NEVER from the request body — and the whole group
// sits behind RequireAdmin (a non-admin is refused 403 before any handler runs).
type AIBuilderHandler struct {
	agent  BuilderAgent
	store  SessionStore
	logger *zap.Logger
	// guard, when non-nil, gates every CHAT turn per tenant (M4/MED-1). nil = no gating —
	// kept for existing tests that construct the handler bare; production main.go ALWAYS
	// wires one via WithBudgetGuard. Session list/get/delete are never gated (no LLM spend).
	// guard gates every CHAT turn per tenant (M4/MED-1) — a REQUIRED constructor param (post-review
	// MED-2: WithBudgetGuard made the gate forgettable — a handler built without remembering to
	// chain it would silently run ungated). nil is still accepted (test-only shape: fakes that
	// don't care about gating), but every construction site must now say so EXPLICITLY, and a nil
	// guard logs once so a future accidental nil-wiring in production can't be silent. Session
	// list/get/delete are never gated (no LLM spend).
	guard TurnGuard
	// resolver re-resolves a chat turn's optional context block. nil is test-only: a turn that DOES
	// carry a context then fails 500 rather than running with the context silently dropped.
	resolver ContextResolver
	// writeDeadline, when > 0, extends the response-write deadline for the CHAT route only (via
	// http.NewResponseController) to cover a worst-case multi-step agent turn. The shared server's
	// WriteTimeout (default 30s) is far shorter than the AI budget (MaxSteps × per-call timeout), so
	// without this a long-but-successful turn would be cut off mid-write. 0 leaves the server default
	// (used by tests, which have no real write timeout anyway).
	writeDeadline time.Duration
}

// NewAIBuilderHandler wires the handler over the agent, the session store, and the M4 budget guard.
// guard is REQUIRED to pass (may be explicitly nil for a test-only ungated shape — see the field
// doc); this makes the gating decision visible at every call site instead of an easily-forgotten
// chained WithBudgetGuard. A nil logger is replaced with a no-op.
func NewAIBuilderHandler(agent BuilderAgent, store SessionStore, guard TurnGuard, resolver ContextResolver, logger *zap.Logger) *AIBuilderHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	h := &AIBuilderHandler{agent: agent, store: store, guard: guard, resolver: resolver, logger: logger}
	if guard == nil {
		// One warning per handler construction (not per-request) — loud enough to catch a
		// misconfigured production wiring in startup logs, quiet enough not to spam per-turn.
		logger.Warn("handlers.AIBuilder: constructed WITHOUT a budget guard — chat is UNGATED (test-only shape; production must always pass one)")
	}
	if resolver == nil {
		logger.Warn("handlers.AIBuilder: constructed WITHOUT a context resolver — a chat turn carrying `context` answers 500 (test-only shape; production must always pass one)")
	}
	return h
}

// WithWriteDeadline sets the per-request response-write deadline for the chat route (chainable).
// Production sizes it to the worst-case agent turn (MaxSteps × AI_TIMEOUT_SEC + slack) so the shared
// 30s server WriteTimeout never truncates a valid slow turn. A non-positive value is ignored.
func (h *AIBuilderHandler) WithWriteDeadline(d time.Duration) *AIBuilderHandler {
	if d > 0 {
		h.writeDeadline = d
	}
	return h
}

// RegisterRoutes mounts the chat + session routes on the admin-gated group. The caller (router.go)
// mounts this group behind Auth → Tenant → RequireAdmin, so every request here is a verified admin
// of a resolved tenant.
func (h *AIBuilderHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/admin/ai/builder/chat", h.Chat)
	rg.POST("/admin/ai/builder/chat/stream", h.ChatStream)
	rg.GET("/admin/ai/builder/sessions", h.ListSessions)
	rg.GET("/admin/ai/builder/sessions/:id", h.GetSession)
	rg.DELETE("/admin/ai/builder/sessions/:id", h.DeleteSession)
}

// chatRequest is the M3 POST body: a SINGLE new user message plus an optional session id. This
// REPLACES the M2 client-held messages[] — the server now owns the history (loaded from the
// session) so a caller can no longer replay arbitrary turns.
//
// SECURITY NOTE — forged-role protection is now structural: the client sends only opaque text, and
// the server always constructs the new turn as role:"user" and rebuilds prior turns from
// server-written rows (user/assistant only). There is therefore NO way for a caller to inject a
// competing "system"/"tool" message (the M1 forged-role 400 is moot because no role crosses the
// wire at all). buildHistory re-asserts the user/assistant-only invariant as defense-in-depth.
type chatRequest struct {
	SessionID string `json:"sessionId"`
	Message   string `json:"message"`
}

// builderChatRequest is the builder-only body: chatRequest plus the optional screen context. The
// ⌘K assistant keeps the plain chatRequest on purpose — it can never receive a builder context
// (specs/067 D1). Context carries lookup KEYS only; there is no tenant key and unknown keys drop.
type builderChatRequest struct {
	SessionID string               `json:"sessionId"`
	Message   string               `json:"message"`
	Context   *ai.TurnContextInput `json:"context"`
}

// Chat handles POST /admin/ai/builder/chat: it resolves the session (loading prior history), runs
// one agentic turn, persists the user message + assistant reply (with its draft), and returns the
// reply plus the session id. Errors map to the spec §6 codes:
//
//	AI_DISABLED      → 503   AI_GATEWAY_ERROR → 502
//	AI_TIMEOUT       → 504   AI_INVALID_DRAFT → 422
//
// A max_steps turn is a 200 (partial draft + outcome="max_steps"); its assistant message is still
// persisted (empty content + the partial draft) so a resume re-renders the draft. A turn the model declined
// (finish_reason "refusal") is a 200 with outcome="refused" and no reply, persisted the same way. Apply is NOT
// handled here — the FE applies a draft through the existing /admin/entities + /admin/fields +
// /admin/relationships + /admin/layouts + /admin/workflows + /admin/reports + /admin/apps APIs
// (the last five for draft items of kind "relationship" — M5's N-N proposal — "layout" — M6's
// layout proposal, an upsert — "workflow" — M7's automation-rule proposal — "report" — M8's
// saved-report proposal — and "app" — Apps Phase 3's sidebar-container proposal, applied as
// POST /admin/apps + PUT /admin/apps/:id/nav with nav names resolved to entity ids by the FE).
//
// Session creation for a NEW turn is deliberately LAZY — the session is minted only AFTER the agent
// succeeds. Lazy creation MATCHES spec §11's atomic AppendTurn create-if-new
// (the old doc's §3/§11 gave two orderings; the spec now pins this one): eager
// creation would mint a junk empty session on every honest-disabled 503 (AI_ENABLED=false is the
// default) and on every 502/504 retry. Consequence: on a gateway error/timeout the turn — including
// the user message — is NOT persisted; the builder simply retries. An EXISTING session id is still
// ownership-verified BEFORE the agent runs, so a forged/foreign id is refused 404 with no LLM spend.
//
// PERSISTENCE (post-review FIX 1): the whole turn — session creation (when new) + both messages +
// the updated_at bump — is written by ONE call to store.AppendTurn, which does it all inside a
// single DB transaction. There is no separate CreateSession/AppendMessage sequence, so there is no
// window in which a partial turn (e.g. an orphan user-message with no assistant reply, or an empty
// ghost session) can be durably observed. If a session was soft-deleted in the race window between
// the ownership read above and this call, AppendTurn's own (now USER-scoped) guard re-detects it and
// returns ai.ErrSessionNotFound, which this handler maps to 404 SESSION_NOT_FOUND — not a 500.
//
// The body is prepareTurn (validation + session load) + runTurn (guard → agent → persist), the SAME
// two helpers ChatStream runs — the JSON and SSE endpoints cannot drift because they share the one
// sequence; only the response medium differs.
func (h *AIBuilderHandler) Chat(c *gin.Context) {
	t, ok := h.prepareTurn(c)
	if !ok {
		return
	}
	body, ok := h.runTurn(c, t, nil, nil, func(e apiError) { h.writeAPIError(c, e) })
	if !ok {
		return
	}
	c.JSON(http.StatusOK, body)
}

// ChatStream handles POST /admin/ai/builder/chat/stream: the SAME turn as Chat, delivered as
// Server-Sent Events so the FE can show live step progress instead of a blind multi-minute wait.
// The FE feature-detects this endpoint; the plain /chat endpoint is unchanged for older clients.
//
// Contract:
//   - Every PRE-ADMISSION refusal (400 body, 404 foreign session, 429 guard, 500 fail-closed) is a
//     PLAIN JSON response with the exact envelope the non-stream endpoint returns — the 429 keeps
//     its Retry-After header (an SSE event could not carry one). The response only becomes an SSE
//     stream once the guard admits the turn.
//   - Admitted → Content-Type: text/event-stream and this event sequence:
//     "step" ×N (ai.StepEvent JSON: {step,maxSteps,phase,tool?}), then EITHER one "result" (data =
//     EXACTLY the JSON body POST /admin/ai/builder/chat returns) followed by one "done", OR one
//     "error" (data = the SAME {error,code} envelope the JSON endpoint would return) and the stream
//     closes. Each event is flushed immediately (X-Accel-Buffering: no keeps nginx-class proxies
//     from re-buffering).
//   - Client disconnect cancels c.Request.Context(), which the agent's next gateway call observes —
//     the turn stops burning steps/spend. A disconnected turn is NOT persisted (same lazy-persist
//     rule as a gateway error); persisting partial turns is OUT of scope for this slice.
//
// This streams OUR loop's step events; the gateway call itself stays non-streaming (token-level
// streaming of the model's text is a possible follow-up, not this slice).
func (h *AIBuilderHandler) ChatStream(c *gin.Context) {
	t, ok := h.prepareTurn(c)
	if !ok {
		return
	}

	// The response medium switches at admission: refusals before startStream are plain JSON
	// (writeAPIError), failures after it ride the stream as an "error" event.
	streaming := false
	startStream := func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
		streaming = true
	}
	fail := func(e apiError) {
		if !streaming {
			h.writeAPIError(c, e)
			return
		}
		c.SSEvent("error", e.body)
		c.Writer.Flush()
	}
	onStep := func(ev ai.StepEvent) {
		c.SSEvent("step", ev)
		c.Writer.Flush()
	}

	body, ok := h.runTurn(c, t, onStep, startStream, fail)
	if !ok {
		return
	}
	c.SSEvent("result", body)
	c.Writer.Flush()
	c.SSEvent("done", gin.H{})
	c.Writer.Flush()
}

// preparedTurn is everything both chat endpoints establish BEFORE the turn is admitted: the
// verified principal (JWT-derived, never from the body), the validated new message, and the prior
// history of an ownership-checked session ("" sessionID = new thread).
type preparedTurn struct {
	tenantID  string
	userID    string
	scope     ai.Scope
	sessionID string
	message   string
	prior     []ai.Message
	turn      ai.TurnContext // resolved screen context (zero = none)
}

// prepareTurn runs the shared pre-agent sequence of Chat and ChatStream — write-deadline extension,
// body validation, principal extraction, session-ownership load — writing the plain-JSON error
// response itself (and returning ok=false) on any refusal. Both endpoints answer these refusals as
// plain JSON by design: the stream endpoint switches to SSE only after the turn is admitted.
func (h *AIBuilderHandler) prepareTurn(c *gin.Context) (preparedTurn, bool) {
	var t preparedTurn

	tenantID, ok := tenantOf(c)
	if !ok {
		return t, false
	}

	// A full agent turn can take up to MaxSteps × per-call timeout — well beyond the shared server's
	// 30s WriteTimeout. Extend the write deadline for THIS response so a slow-but-valid turn is not
	// truncated mid-write. Best-effort: a writer that does not support deadlines (e.g. httptest's
	// recorder) returns ErrNotSupported, which is expected and ignored; any other error is logged.
	if h.writeDeadline > 0 {
		if err := http.NewResponseController(c.Writer).SetWriteDeadline(time.Now().Add(h.writeDeadline)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			h.logger.Warn("handlers.AIBuilder.Chat: set write deadline", zap.String("tenant_id", tenantID), zap.Error(err))
		}
	}

	var req builderChatRequest
	if !bindJSON(c, &req) {
		return t, false
	}
	message := strings.TrimSpace(req.Message)
	if message == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "message không được rỗng", "code": "INVALID_BODY"})
		return t, false
	}

	// Context SHAPE check — pure, before any I/O (specs/067 FR-014 order: shape → session → resolve).
	var ctxIn ai.TurnContextInput
	if req.Context != nil {
		var cerr error
		if ctxIn, cerr = ai.NormalizeTurnContext(*req.Context); cerr != nil {
			h.writeContextError(c, tenantID, cerr)
			return t, false
		}
	}

	sub, _ := middleware.SubjectFromContext(c)

	// Load prior history for an EXISTING session — ownership-guarded (tenant+user) so a foreign/
	// guessed id is refused 404 BEFORE any LLM spend, with no existence leak.
	sessionID := strings.TrimSpace(req.SessionID)
	var prior []ai.Message
	if sessionID != "" {
		_, msgs, err := h.store.GetSession(c.Request.Context(), tenantID, sub.UserID, sessionID)
		if err != nil {
			if errors.Is(err, ai.ErrSessionNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "Không tìm thấy phiên", "code": "SESSION_NOT_FOUND"})
				return t, false
			}
			h.logger.Error("handlers.AIBuilder.Chat: load session", zap.String("tenant_id", tenantID), zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi hệ thống khi gọi AI Builder", "code": "AI_CHAT_FAILED"})
			return t, false
		}
		prior = msgs
	}

	// Resolve the context inside the JWT's tenant — BEFORE the budget gate, so a wrong context costs
	// no token, no ledger row and no session.
	var turn ai.TurnContext
	if !ctxIn.Empty() {
		if h.resolver == nil {
			h.logger.Error("handlers.AIBuilder.Chat: context sent but no resolver wired", zap.String("tenant_id", tenantID))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi hệ thống khi gọi AI Builder", "code": "AI_CHAT_FAILED"})
			return t, false
		}
		var rerr error
		if turn, rerr = h.resolver.Resolve(c.Request.Context(), tenantID, ctxIn); rerr != nil {
			h.writeContextError(c, tenantID, rerr)
			return t, false
		}
	}

	return preparedTurn{
		turn:      turn,
		tenantID:  tenantID,
		userID:    sub.UserID,
		scope:     ai.Scope{UserID: sub.UserID, RoleID: sub.RoleID, IsAdmin: sub.IsAdmin},
		sessionID: sessionID,
		message:   message,
		prior:     prior,
	}, true
}

// runTurn executes the admitted part of one chat turn — budget-guard acquire/release, the agent
// call, and the atomic persist — identically for both endpoints. onAdmitted (optional) fires once
// after the guard admits, BEFORE any LLM spend — the stream endpoint flips its response to SSE
// there. fail delivers every refusal envelope in the endpoint's medium (plain JSON / SSE "error"
// event). Returns the success response body (chatResponseBody) or ok=false after fail has spoken.
func (h *AIBuilderHandler) runTurn(c *gin.Context, t preparedTurn, onStep func(ai.StepEvent), onAdmitted func(), fail func(apiError)) (gin.H, bool) {
	ctx := c.Request.Context()

	// M4 (MED-1): the per-tenant budget/rate/concurrency gate sits IMMEDIATELY around the agent
	// turn — after body validation and the session-ownership 404 (a foreign-id probe must not
	// consume the tenant's budget), before any LLM spend.
	if h.guard != nil {
		release, gerr := h.guard.Acquire(ctx, t.tenantID)
		// Defer BEFORE checking gerr (post-review LOW-2): release is idempotent (safe to call on a
		// rejection too, where the guard's own contract already returns a no-op), so deferring it
		// unconditionally makes "no leaked slot" a STRUCTURAL property of this function rather than
		// something that depends on every future edit remembering to release on the error path too.
		if release != nil {
			defer release()
		}
		if gerr != nil {
			fail(h.classifyAgentError(t.tenantID, gerr))
			return nil, false
		}
	}
	if onAdmitted != nil {
		onAdmitted()
	}

	// model "" → the agent uses the server-configured AI_MODEL (no client override, MED-1).
	res, err := h.agent.ChatWithProgress(ctx, t.scope, t.tenantID, "", t.turn, buildHistory(t.prior, t.message), onStep)
	if err != nil {
		fail(h.classifyAgentError(t.tenantID, err))
		return nil, false
	}

	// Persist the whole turn ATOMICALLY (session creation for a new thread + both messages + the
	// updated_at bump, all in one DB transaction — see the method doc for why this replaced 3
	// separate calls). newTitle is only used when sessionID is "" (ignored by the store otherwise).
	persistedID, perr := h.store.AppendTurn(ctx, t.tenantID, t.userID, t.sessionID, ai.AutoTitle(t.message), t.message, res.Reply, marshalDraft(res.Draft))
	if perr != nil {
		fail(h.classifyPersistError(t.tenantID, perr))
		return nil, false
	}

	return chatResponseBody(persistedID, res, t.turn), true
}

// chatResponseBody is THE success envelope of a chat turn — returned verbatim by both POST /chat
// (as the 200 JSON body) and POST /chat/stream (as the "result" event payload), so the two
// endpoints answer the same turn with the same bytes by construction.
func chatResponseBody(sessionID string, res ai.Result, turn ai.TurnContext) gin.H {
	return gin.H{"data": gin.H{
		"sessionId": sessionID,
		"reply":     res.Reply,
		"draft":     res.Draft,
		"runId":     res.RunID,
		"usage":     usageDTO(res.Usage),
		"outcome":   res.Outcome,
		"context":   contextDTO(turn),
	}}
}

// contextDTO is the camelCase echo of the RESOLVED context (null when the turn had none): project
// name, app {name,label}, panel token. The nav list stays server-side — the FE only needs enough
// to draw its "Đang dựng" chip from what the server actually used.
func contextDTO(turn ai.TurnContext) gin.H {
	if !turn.Present {
		return nil
	}
	out := gin.H{"project": gin.H{"name": turn.Project}, "app": nil, "panel": nil, "entity": nil, "recordId": nil}
	if turn.App != nil {
		out["app"] = gin.H{"name": turn.App.Name, "label": turn.App.Label}
	}
	if turn.Panel != "" {
		out["panel"] = turn.Panel
	}
	if turn.Entity != nil {
		out["entity"] = gin.H{"name": turn.Entity.Name, "label": turn.Entity.Label, "inApp": turn.Entity.InApp}
	}
	if turn.RecordID != "" {
		out["recordId"] = turn.RecordID
	}
	return out
}

// writeContextError answers a context refusal: 422 {error, code, details:{field}} for a
// *ai.ContextError (ONE sentence for NOT_FOUND, so a foreign tenant's app and a missing app are
// byte-identical), 500 AI_CHAT_FAILED for anything else. Always plain JSON — on the stream route
// too, since no turn has been admitted yet.
func (h *AIBuilderHandler) writeContextError(c *gin.Context, tenantID string, err error) {
	var ce *ai.ContextError
	if errors.As(err, &ce) {
		msg := "Ngữ cảnh gửi lên không hợp lệ"
		if ce.Code == ai.CodeContextNotFound {
			msg = "Không tìm thấy app hoặc bảng đang dựng trong dự án này"
		}
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": msg, "code": ce.Code, "details": gin.H{"field": ce.Field}})
		return
	}
	h.logger.Error("handlers.AIBuilder.Chat: resolve context", zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi hệ thống khi gọi AI Builder", "code": "AI_CHAT_FAILED"})
}

// ListSessions handles GET /admin/ai/builder/sessions: the caller's own sessions, newest first.
func (h *AIBuilderHandler) ListSessions(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	sub, _ := middleware.SubjectFromContext(c)
	rows, err := h.store.ListSessions(c.Request.Context(), tenantID, sub.UserID)
	if err != nil {
		h.logger.Error("handlers.AIBuilder.ListSessions", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi khi tải danh sách phiên", "code": "AI_SESSION_FAILED"})
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, gin.H{"id": r.ID, "title": r.Title, "updatedAt": r.UpdatedAt})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// GetSession handles GET /admin/ai/builder/sessions/:id: the session header + its messages (with
// each assistant turn's draft decoded for re-render). A session not owned by (tenant, user) → 404
// with an identical body to a nonexistent id — no existence disclosure (spec §7).
func (h *AIBuilderHandler) GetSession(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	sub, _ := middleware.SubjectFromContext(c)
	sess, msgs, err := h.store.GetSession(c.Request.Context(), tenantID, sub.UserID, c.Param("id"))
	if err != nil {
		if errors.Is(err, ai.ErrSessionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Không tìm thấy phiên", "code": "SESSION_NOT_FOUND"})
			return
		}
		h.logger.Error("handlers.AIBuilder.GetSession", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi khi tải phiên", "code": "AI_SESSION_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": h.sessionDTO(sess, msgs)})
}

// DeleteSession handles DELETE /admin/ai/builder/sessions/:id: soft-delete, ownership-guarded (404
// on a foreign/guessed id, same no-leak contract). Returns 204 on success.
func (h *AIBuilderHandler) DeleteSession(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	sub, _ := middleware.SubjectFromContext(c)
	if err := h.store.SoftDeleteSession(c.Request.Context(), tenantID, sub.UserID, c.Param("id")); err != nil {
		if errors.Is(err, ai.ErrSessionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Không tìm thấy phiên", "code": "SESSION_NOT_FOUND"})
			return
		}
		h.logger.Error("handlers.AIBuilder.DeleteSession", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi khi xoá phiên", "code": "AI_SESSION_FAILED"})
		return
	}
	c.Status(http.StatusNoContent)
}

// buildHistory turns the persisted prior turns plus the new user message into the []ai.ChatMessage
// the agent replays (the agent prepends its OWN persona + grounding). Defense-in-depth: only
// user/assistant roles feed the gateway (persisted rows are always one of those, server-written),
// and an empty-content assistant turn (a max_steps turn that produced only a draft) is skipped so
// the gateway never receives an empty assistant message.
func buildHistory(prior []ai.Message, newUserMsg string) []ai.ChatMessage {
	hist := make([]ai.ChatMessage, 0, len(prior)+1)
	for _, m := range prior {
		switch m.Role {
		case "user":
			hist = append(hist, ai.ChatMessage{Role: "user", Content: m.Content})
		case "assistant":
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			hist = append(hist, ai.ChatMessage{Role: "assistant", Content: m.Content})
		}
	}
	hist = append(hist, ai.ChatMessage{Role: "user", Content: newUserMsg})
	return hist
}

// marshalDraft serializes the turn's draft for draft_json (nil when empty → stored as SQL NULL). A
// marshal failure degrades to nil (draft resume is best-effort; the reply + ledger are unaffected).
func marshalDraft(draft []ai.DraftItem) json.RawMessage {
	if len(draft) == 0 {
		return nil
	}
	b, err := json.Marshal(draft)
	if err != nil {
		return nil
	}
	return b
}

// sessionDTO maps a session + its messages to the camelCase API shape (api-conventions §Response
// Format). Each message's stored draft_json is decoded back to draft[] so the FE re-renders an
// applicable draft-card on resume.
func (h *AIBuilderHandler) sessionDTO(sess ai.Session, msgs []ai.Message) gin.H {
	out := make([]gin.H, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, gin.H{
			"seq":       m.Seq,
			"role":      m.Role,
			"content":   m.Content,
			"draft":     h.decodeDraft(m.DraftJSON),
			"createdAt": m.CreatedAt,
		})
	}
	return gin.H{
		"id":        sess.ID,
		"title":     sess.Title,
		"createdAt": sess.CreatedAt,
		"updatedAt": sess.UpdatedAt,
		"messages":  out,
	}
}

// decodeDraft parses a stored draft_json back into draft[] (nil for a NULL/absent draft). A corrupt
// blob logs a warning and yields nil rather than failing the whole session read.
func (h *AIBuilderHandler) decodeDraft(raw json.RawMessage) []ai.DraftItem {
	if len(raw) == 0 {
		return nil
	}
	var items []ai.DraftItem
	if err := json.Unmarshal(raw, &items); err != nil {
		// Log the fact only — never the unmarshal error, which could echo a snippet of the stored
		// blob into logs (belt-and-suspenders on the "no raw content in logs" rule).
		h.logger.Warn("handlers.AIBuilder: corrupt draft_json on resume, skipping draft")
		return nil
	}
	return items
}

// usageDTO maps ai.Usage (whose json tags are the OpenAI snake_case parse shape) to the camelCase
// the Nexus API contract mandates (api-conventions §Response Format). outputTokens mirrors the
// sys_ai_runs.output_tokens column (the completion side of the turn). promptTokens includes the cached
// input; cacheReadTokens/cacheWriteTokens say how much of it the provider's prompt cache served or stored.
func usageDTO(u ai.Usage) gin.H {
	return gin.H{
		"promptTokens":     u.PromptTokens,
		"outputTokens":     u.CompletionTokens,
		"totalTokens":      u.TotalTokens,
		"cacheReadTokens":  u.CacheReadTokens,
		"cacheWriteTokens": u.CacheWriteTokens,
	}
}

// apiError is one classified refusal: the status + envelope the JSON endpoint writes, and that the
// SSE endpoint's "error" event carries as its data (status/Retry-After cannot ride an SSE event —
// mid-stream the {error,code} envelope alone is the FE's signal; the guard 429s never reach the
// stream because admission happens pre-SSE).
type apiError struct {
	status     int
	retryAfter int // seconds; 0 = no Retry-After header
	body       gin.H
}

// writeAPIError renders one classified refusal as the plain JSON response the non-stream contract
// mandates (Retry-After header included when carried).
func (h *AIBuilderHandler) writeAPIError(c *gin.Context, e apiError) {
	if e.retryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(e.retryAfter))
	}
	c.JSON(e.status, e.body)
}

// classifyAgentError maps the agent's typed sentinels to the spec §6 status + envelope — the ONE
// mapping both the JSON endpoint (respondError) and the SSE endpoint ("error" event) render, so
// the two can never drift. An unclassified error is a 500 (logged with tenant context). Logging
// happens here (classification runs exactly once per failure on either endpoint).
func (h *AIBuilderHandler) classifyAgentError(tenantID string, err error) apiError {
	switch {
	case errors.Is(err, ai.ErrDisabled):
		return apiError{status: http.StatusServiceUnavailable, body: gin.H{
			"error": "AI Builder đang tắt (AI_ENABLED=false) — dùng builder thủ công", "code": "AI_DISABLED",
		}}
	case errors.Is(err, ai.ErrTimeout):
		h.logger.Warn("handlers.AIBuilder.Chat: gateway timeout", zap.String("tenant_id", tenantID), zap.Error(err))
		return apiError{status: http.StatusGatewayTimeout, body: gin.H{
			"error": "LLM gateway phản hồi quá chậm — thử lại", "code": "AI_TIMEOUT",
		}}
	case errors.Is(err, ai.ErrGateway):
		h.logger.Error("handlers.AIBuilder.Chat: gateway error", zap.String("tenant_id", tenantID), zap.Error(err))
		return apiError{status: http.StatusBadGateway, body: gin.H{
			"error": "LLM gateway lỗi — thử lại sau", "code": "AI_GATEWAY_ERROR",
		}}
	case errors.Is(err, ai.ErrInvalidDraft):
		return apiError{status: http.StatusUnprocessableEntity, body: gin.H{
			"error": "Đề xuất không hợp lệ theo field-spec", "code": "AI_INVALID_DRAFT",
		}}
	// M4 (spec §12) — the three per-tenant 429s from the BudgetGuard. The guard already logged
	// each rejection at Warn with tenant context; here we only shape the response. Retry-After
	// rides on the *ai.LimitError (errors.As); the fallbacks match the documented defaults and
	// only apply if a guard ever returns a bare sentinel. Messages reveal no other tenant's
	// thresholds — only that THIS tenant's own cap was hit.
	case errors.Is(err, ai.ErrRateLimited):
		return apiError{status: http.StatusTooManyRequests, retryAfter: retryAfterSec(err, 60), body: gin.H{
			"error": "Quá nhiều lượt AI Builder trong thời gian ngắn — thử lại sau", "code": "AI_RATE_LIMITED",
		}}
	case errors.Is(err, ai.ErrConcurrencyLimit):
		return apiError{status: http.StatusTooManyRequests, retryAfter: retryAfterSec(err, 5), body: gin.H{
			"error": "Đang có lượt AI Builder khác chạy — thử lại sau ít giây", "code": "AI_CONCURRENCY_LIMIT",
		}}
	case errors.Is(err, ai.ErrBudgetExceeded):
		return apiError{status: http.StatusTooManyRequests, retryAfter: retryAfterSec(err, 3600), body: gin.H{
			"error": "Đã chạm ngân sách token AI của hôm nay — thử lại sau nửa đêm UTC", "code": "AI_BUDGET_EXCEEDED",
		}}
	default:
		h.logger.Error("handlers.AIBuilder.Chat", zap.String("tenant_id", tenantID), zap.Error(err))
		return apiError{status: http.StatusInternalServerError, body: gin.H{
			"error": "Lỗi hệ thống khi gọi AI Builder", "code": "AI_CHAT_FAILED",
		}}
	}
}

// classifyPersistError maps an AppendTurn failure to its envelope — 404 for the vanished-session
// race (the session existed at the ownership read but was deleted mid-turn; not a server fault,
// reported exactly like a never-existed id), 500 otherwise — shared by both endpoints like
// classifyAgentError.
func (h *AIBuilderHandler) classifyPersistError(tenantID string, err error) apiError {
	if errors.Is(err, ai.ErrSessionNotFound) {
		return apiError{status: http.StatusNotFound, body: gin.H{
			"error": "Không tìm thấy phiên", "code": "SESSION_NOT_FOUND",
		}}
	}
	h.logger.Error("handlers.AIBuilder.Chat: persist turn", zap.String("tenant_id", tenantID), zap.Error(err))
	return apiError{status: http.StatusInternalServerError, body: gin.H{
		"error": "Lỗi hệ thống khi gọi AI Builder", "code": "AI_CHAT_FAILED",
	}}
}

// retryAfterSec extracts the Retry-After hint carried on a *ai.LimitError, falling back to the
// given default for a bare sentinel (defensive — the BudgetGuard always wraps).
func retryAfterSec(err error, fallback int) int {
	var le *ai.LimitError
	if errors.As(err, &le) && le.RetryAfterSec > 0 {
		return le.RetryAfterSec
	}
	return fallback
}
