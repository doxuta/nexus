// assistant_handler.go — POST /assistant/ask, the ⌘K end-user data assistant endpoint.
//
// Unlike the E9 AI Builder (admin-gated /admin/ai/builder/*), this route is available to EVERY
// authenticated user of the tenant BY DESIGN: the assistant only READS data, and every read runs
// through the caller's own RecordScope + entity ACL inside the agent's tools — so a user can ask
// about exactly (and only) what their normal browsing already shows them.
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
	"nexus/platform/internal/services"
)

// AssistantAgent is the consumer-side view of the ⌘K agent (satisfied by *ai.AssistantAgent).
// Declared here so the handler test injects a fake that records the recordScope it receives —
// proving the handler threads scopeOf's output verbatim (trap #1) without a live gateway.
type AssistantAgent interface {
	Ask(ctx context.Context, recordScope services.RecordScope, tenantID, model string, messages []ai.ChatMessage) (ai.Result, error)
	// Enabled reports the honest AI on/off state (AI_ENABLED) for GET /ai/health.
	Enabled() bool
}

// UserTurnGuard is the assistant-side view of the M4 guard (satisfied by *ai.BudgetGuard):
// AcquireUser runs the SAME per-tenant gates as TurnGuard.Acquire PLUS the per-user daily-share
// gate (ASSISTANT_DAILY_TOKEN_SHARE_PCT) — the assistant is every-user, so one steady user must
// not burn the whole tenant budget. Same release/error contract as TurnGuard.
type UserTurnGuard interface {
	AcquireUser(ctx context.Context, tenantID, userID string) (release func(), err error)
}

// AssistantSessionStore is the consumer-side view of the session store's ⌘K-ASSISTANT surface
// (satisfied by *ai.SessionStore — mig 000059/F9-5: sys_ai_sessions.feature splits the two rails).
// Every method pins (tenant, user, feature='assistant'): a Builder thread, another user's thread
// or another tenant's thread all resolve to ai.ErrSessionNotFound — indistinguishable, no
// existence leak. Same per-method contracts as the builder's SessionStore otherwise (AppendTurn's
// atomic all-or-nothing turn write, newest-first list, soft delete).
type AssistantSessionStore interface {
	ListAssistantSessions(ctx context.Context, tenantID, userID string) ([]ai.SessionSummary, error)
	GetAssistantSession(ctx context.Context, tenantID, userID, id string) (ai.Session, []ai.Message, error)
	AppendAssistantTurn(ctx context.Context, tenantID, userID, sessionID, newTitle, userContent, assistantContent string, assistantDraft json.RawMessage, outcome string) (string, error)
	SoftDeleteAssistantSession(ctx context.Context, tenantID, userID, id string) error
}

// AssistantHandler serves POST /assistant/ask + the user-scoped /assistant/sessions CRUD. It
// shares the builder's *ai.SessionStore (same sys_ai_sessions tables, same tenant+user ownership
// guard) through the ASSISTANT-feature surface (F9-5) and a DEDICATED BudgetGuard instance
// (per-tenant rate/concurrency windows independent from the builder's; the daily token budget is
// naturally shared through the one sys_ai_runs ledger).
//
// Per-user fair-share is two-layered: middleware.AssistantRateLimit bounds the BURST per
// (tenant,user) window (2026-07-09, wired via router WithAssistantRateLimiter, default 5 < tenant
// 10), and the guard's AcquireUser bounds the DAILY token share per user
// (ASSISTANT_DAILY_TOKEN_SHARE_PCT of the tenant budget) — so neither a spike nor a steady drip
// lets one user drain the tenant's whole day.
type AssistantHandler struct {
	agent  AssistantAgent
	store  AssistantSessionStore
	guard  UserTurnGuard
	logger *zap.Logger
	// writeDeadline mirrors AIBuilderHandler's: a worst-case multi-step turn outlives the shared
	// server WriteTimeout, so the ask route extends its own response deadline. 0 = server default.
	writeDeadline time.Duration
}

// NewAssistantHandler wires the handler. guard is a REQUIRED param (may be explicitly nil for a
// test-only ungated shape — same post-review MED-2 contract as the builder handler); a nil guard
// logs once so an accidental ungated production wiring cannot be silent. A nil logger is replaced
// with a no-op.
func NewAssistantHandler(agent AssistantAgent, store AssistantSessionStore, guard UserTurnGuard, logger *zap.Logger) *AssistantHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	h := &AssistantHandler{agent: agent, store: store, guard: guard, logger: logger}
	if guard == nil {
		logger.Warn("handlers.Assistant: constructed WITHOUT a budget guard — ask is UNGATED (test-only shape; production must always pass one)")
	}
	return h
}

// WithWriteDeadline sets the per-request response-write deadline for the ask route (chainable).
// A non-positive value is ignored.
func (h *AssistantHandler) WithWriteDeadline(d time.Duration) *AssistantHandler {
	if d > 0 {
		h.writeDeadline = d
	}
	return h
}

// RegisterRoutes mounts the ⌘K routes on the PROTECTED group (Auth → Tenant) — deliberately
// NOT behind RequireAdmin: per-caller ACL is enforced inside the agent's read tools, not by a
// coarse admin gate. Optional per-route middleware (e.g. the per-user AssistantRateLimit) runs
// BEFORE the ask handler ONLY — the session routes have no LLM spend (same reasoning as the
// builder's ungated session CRUD), so they mount without it.
//
// GET /ai/health is the honest on/off probe (EASE Nước 7 enabler): the FE previously had NO way
// to know AI_ENABLED before the user typed a whole paragraph and got a 503. Authenticated (any
// user), leaks nothing beyond the boolean (no gateway URL, no model name for non-admins).
//
// The session routes (F9-5: ⌘K threads become listable/resumable) are USER-scoped, never admin:
// the store pins every read/write to (tenant, user, feature='assistant') — a foreign, builder or
// guessed id is a 404 SESSION_NOT_FOUND, no existence leak.
func (h *AssistantHandler) RegisterRoutes(rg *gin.RouterGroup, mw ...gin.HandlerFunc) {
	rg.POST("/assistant/ask", append(mw, h.Ask)...)
	rg.GET("/ai/health", h.Health)
	rg.GET("/assistant/sessions", h.ListSessions)
	rg.GET("/assistant/sessions/:id", h.GetSession)
	rg.DELETE("/assistant/sessions/:id", h.DeleteSession)
}

// Health handles GET /ai/health → {data:{enabled}} so every AI surface (⌘K "Hỏi AI" row, the
// /studio/ai composer) can render an honest on/off state BEFORE the user invests typing.
func (h *AssistantHandler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"enabled": h.agent.Enabled()}})
}

// Ask handles POST /assistant/ask {message, sessionId?} → {sessionId, reply, drafts, usage,
// outcome}. The flow mirrors the builder's Chat (server-held history, lazy session mint, atomic
// AppendAssistantTurn) with one structural difference: the handler builds the caller's row-level
// RecordScope via scopeOf — the SAME helper every dynamic-record route uses — and hands it to the
// agent VERBATIM (trap #1). On this route no RequireACL stage ran, so scopeOf fails closed to
// AccessLevel "none"; the agent's RecordReadAdapter then resolves the per-entity "view" level
// through ACLService per tool call, exactly like the middleware would per request.
//
// drafts[] (⌘K-can-act, EASE Nước 7) carries the turn's HITL record proposals — the SAME
// ai.DraftItem JSON the builder emits, restricted by construction to kinds
// "record_create"/"record_update". NOTHING was written: the FE renders confirm cards and applies
// via POST /:entity / PUT /:entity/:id under the user's own token.
func (h *AssistantHandler) Ask(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}

	if h.writeDeadline > 0 {
		if err := http.NewResponseController(c.Writer).SetWriteDeadline(time.Now().Add(h.writeDeadline)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			h.logger.Warn("handlers.Assistant.Ask: set write deadline", zap.String("tenant_id", tenantID), zap.Error(err))
		}
	}

	var req chatRequest // same {sessionId, message} body shape as the builder chat
	if !bindJSON(c, &req) {
		return
	}
	message := strings.TrimSpace(req.Message)
	if message == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "message không được rỗng", "code": "INVALID_BODY"})
		return
	}

	// The caller's verified row-level scope — built by the SHARED scopeOf helper, threaded down
	// UNCHANGED. Session ownership uses the same verified identity.
	recordScope := scopeOf(c)
	userID := recordScope.UserID
	ctx := c.Request.Context()

	// Prior history for an existing session — ownership-guarded (tenant+user) BEFORE any LLM
	// spend, no existence leak (identical contract to the builder chat).
	sessionID := strings.TrimSpace(req.SessionID)
	var prior []ai.Message
	if sessionID != "" {
		_, msgs, err := h.store.GetAssistantSession(ctx, tenantID, userID, sessionID)
		if err != nil {
			if errors.Is(err, ai.ErrSessionNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "Không tìm thấy phiên", "code": "SESSION_NOT_FOUND"})
				return
			}
			h.logger.Error("handlers.Assistant.Ask: load session", zap.String("tenant_id", tenantID), zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi hệ thống khi gọi trợ lý AI", "code": "AI_CHAT_FAILED"})
			return
		}
		prior = msgs
	}

	// Per-tenant budget/rate/concurrency gate + per-user daily share, immediately around the
	// agent turn (M4 contract; userID is the verified identity from scopeOf, never the body).
	if h.guard != nil {
		release, gerr := h.guard.AcquireUser(ctx, tenantID, userID)
		if release != nil {
			defer release()
		}
		if gerr != nil {
			h.respondError(c, tenantID, gerr)
			return
		}
	}

	// model "" → server-configured AI_MODEL (no client override, same as the builder).
	res, err := h.agent.Ask(ctx, recordScope, tenantID, "", buildHistory(prior, message))
	if err != nil {
		h.respondError(c, tenantID, err)
		return
	}

	// Persist the turn atomically (lazy session mint on success — a 502/504 retry mints nothing),
	// stamped feature='assistant' (F9-5) so the thread lives in the ⌘K rail, never the Builder's.
	// The draft round-trips through draft_json so a resumed thread re-renders its confirm cards.
	persistedID, perr := h.store.AppendAssistantTurn(ctx, tenantID, userID, sessionID, ai.AutoTitle(message), message, res.Reply, marshalDraft(res.Draft), res.Outcome)
	if perr != nil {
		if errors.Is(perr, ai.ErrSessionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Không tìm thấy phiên", "code": "SESSION_NOT_FOUND"})
			return
		}
		h.logger.Error("handlers.Assistant.Ask: persist turn", zap.String("tenant_id", tenantID), zap.Error(perr))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi hệ thống khi gọi trợ lý AI", "code": "AI_CHAT_FAILED"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"sessionId": persistedID,
		"reply":     res.Reply,
		"drafts":    res.Draft,
		"usage":     usageDTO(res.Usage),
		"outcome":   res.Outcome,
	}})
}

// ListSessions handles GET /assistant/sessions: the caller's own ⌘K threads, newest first, capped
// by the store (≈20 — the palette rail, not an archive).
func (h *AssistantHandler) ListSessions(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	userID := scopeOf(c).UserID
	rows, err := h.store.ListAssistantSessions(c.Request.Context(), tenantID, userID)
	if err != nil {
		h.logger.Error("handlers.Assistant.ListSessions", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi khi tải danh sách phiên", "code": "AI_SESSION_FAILED"})
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, gin.H{"id": r.ID, "title": r.Title, "updatedAt": r.UpdatedAt})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// GetSession handles GET /assistant/sessions/:id: the thread header + its messages, each
// assistant turn's drafts decoded for confirm-card re-render on resume. A session not owned by
// (tenant, user) — or living on the Builder surface — is a 404 with a body identical to a
// nonexistent id (no existence leak).
func (h *AssistantHandler) GetSession(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	userID := scopeOf(c).UserID
	sess, msgs, err := h.store.GetAssistantSession(c.Request.Context(), tenantID, userID, c.Param("id"))
	if err != nil {
		if errors.Is(err, ai.ErrSessionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Không tìm thấy phiên", "code": "SESSION_NOT_FOUND"})
			return
		}
		h.logger.Error("handlers.Assistant.GetSession", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi khi tải phiên", "code": "AI_SESSION_FAILED"})
		return
	}
	out := make([]gin.H, 0, len(msgs))
	for _, m := range msgs {
		row := gin.H{
			"seq":       m.Seq,
			"role":      m.Role,
			"content":   m.Content,
			"drafts":    h.decodeDrafts(m.DraftJSON),
			"createdAt": m.CreatedAt,
		}
		if m.Outcome != "" {
			row["outcome"] = m.Outcome // how the turn ended, as the builder's session read says it
		}
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"id":        sess.ID,
		"title":     sess.Title,
		"createdAt": sess.CreatedAt,
		"updatedAt": sess.UpdatedAt,
		"messages":  out,
	}})
}

// DeleteSession handles DELETE /assistant/sessions/:id: soft-delete, ownership-guarded (404 on a
// foreign/builder/guessed id, same no-leak contract). Returns 204 on success.
func (h *AssistantHandler) DeleteSession(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	userID := scopeOf(c).UserID
	if err := h.store.SoftDeleteAssistantSession(c.Request.Context(), tenantID, userID, c.Param("id")); err != nil {
		if errors.Is(err, ai.ErrSessionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Không tìm thấy phiên", "code": "SESSION_NOT_FOUND"})
			return
		}
		h.logger.Error("handlers.Assistant.DeleteSession", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi khi xoá phiên", "code": "AI_SESSION_FAILED"})
		return
	}
	c.Status(http.StatusNoContent)
}

// decodeDrafts parses a stored draft_json back into drafts[] (nil for a NULL/absent draft). A
// corrupt blob logs a warning and yields nil rather than failing the whole session read — the
// assistant-side twin of AIBuilderHandler.decodeDraft (kept separate so this handler touches zero
// builder code).
func (h *AssistantHandler) decodeDrafts(raw json.RawMessage) []ai.DraftItem {
	if len(raw) == 0 {
		return nil
	}
	var items []ai.DraftItem
	if err := json.Unmarshal(raw, &items); err != nil {
		// Log the fact only — never the unmarshal error, which could echo stored content into logs.
		h.logger.Warn("handlers.Assistant: corrupt draft_json on resume, skipping drafts")
		return nil
	}
	return items
}

// respondError maps the agent's typed sentinels to HTTP codes — the same code vocabulary as the
// builder (AI_DISABLED/AI_TIMEOUT/AI_GATEWAY_ERROR + the three M4 429s) with assistant-appropriate
// user-facing messages. Kept as the assistant's own copy (not shared with AIBuilderHandler) so
// this slice modifies zero builder code; the strings differ anyway.
func (h *AssistantHandler) respondError(c *gin.Context, tenantID string, err error) {
	switch {
	case errors.Is(err, ai.ErrDisabled):
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "Trợ lý AI đang tắt (AI_ENABLED=false)", "code": "AI_DISABLED",
		})
	case errors.Is(err, ai.ErrTimeout):
		h.logger.Warn("handlers.Assistant.Ask: gateway timeout", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusGatewayTimeout, gin.H{
			"error": "Trợ lý AI phản hồi quá chậm — thử lại", "code": "AI_TIMEOUT",
		})
	case errors.Is(err, ai.ErrGateway):
		h.logger.Error("handlers.Assistant.Ask: gateway error", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusBadGateway, gin.H{
			"error": "Trợ lý AI gặp lỗi — thử lại sau", "code": "AI_GATEWAY_ERROR",
		})
	case errors.Is(err, ai.ErrRateLimited):
		c.Header("Retry-After", strconv.Itoa(retryAfterSec(err, 60)))
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": "Quá nhiều câu hỏi trong thời gian ngắn — thử lại sau", "code": "AI_RATE_LIMITED",
		})
	case errors.Is(err, ai.ErrConcurrencyLimit):
		c.Header("Retry-After", strconv.Itoa(retryAfterSec(err, 5)))
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": "Đang có câu hỏi khác chạy — thử lại sau ít giây", "code": "AI_CONCURRENCY_LIMIT",
		})
	case errors.Is(err, ai.ErrBudgetExceeded):
		c.Header("Retry-After", strconv.Itoa(retryAfterSec(err, 3600)))
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": "Đã chạm ngân sách AI của hôm nay — thử lại sau nửa đêm UTC", "code": "AI_BUDGET_EXCEEDED",
		})
	default:
		h.logger.Error("handlers.Assistant.Ask", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Lỗi hệ thống khi gọi trợ lý AI", "code": "AI_CHAT_FAILED",
		})
	}
}
