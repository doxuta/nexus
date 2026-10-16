package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// PreviewHandler serves "preview as role" (specs/064-preview-as-role-sample-data S9-1):
//
//   - POST /admin/preview-sessions — builderGated (Auth → Tenant → PreviewReadOnly → RequireAdmin →
//     operator tier): a builder opens a read-only, short-lived session as one role of the tenant
//     and receives its token. The service re-checks the builder live (ActorLive).
//   - DELETE /preview-sessions/current — protected: the preview token ends its own session. It is
//     the ONLY write PreviewReadOnly lets a preview token make.
type PreviewHandler struct {
	svc    *services.PreviewService
	logger *zap.Logger
}

// NewPreviewHandler wires the handler over the preview service.
func NewPreviewHandler(svc *services.PreviewService, logger *zap.Logger) *PreviewHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &PreviewHandler{svc: svc, logger: logger}
}

// RegisterAdminRoutes mounts the start route on the builder-gated group.
func (h *PreviewHandler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.POST("/admin/preview-sessions", h.Start)
}

// RegisterRoutes mounts the exit route on the protected group (before the /:entity dispatcher).
func (h *PreviewHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.DELETE("/preview-sessions/current", h.End)
}

// startPreviewRequest is the POST /admin/preview-sessions body.
type startPreviewRequest struct {
	RoleID string `json:"roleId" binding:"required"`
	AppID  string `json:"appId"`
	Draft  *bool  `json:"draft"` // absent = default (on when the workspace has a draft)
}

// Start handles POST /admin/preview-sessions → 201 {data: {sessionId, token, expiresAt, role, appId,
// draft}}. 400 INVALID_BODY · 403 FORBIDDEN (not a builder's login token, or the builder's login
// session is no longer live) · 422 PREVIEW_ROLE_NOT_FOUND · 422 PREVIEW_DRAFT_UNAVAILABLE.
func (h *PreviewHandler) Start(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	claims, ok := middleware.ClaimsFromContext(c)
	if !ok || claims.TokenType != auth.TokenTypeAccess {
		// Only a person's login token may open a preview: an API key is a machine identity, and a
		// preview token never reaches here (PreviewReadOnly + RequireAdmin) — refuse regardless.
		c.JSON(http.StatusForbidden, gin.H{"error": "Không có quyền mở phiên xem thử", "code": "FORBIDDEN"})
		return
	}
	var req startPreviewRequest
	if !bindJSON(c, &req) {
		return
	}
	res, err := h.svc.Start(c.Request.Context(), claims.Subject, services.PreviewStartInput{
		RoleID: req.RoleID, AppID: req.AppID, Draft: req.Draft,
	})
	if err != nil {
		var ve *services.ValidationError
		switch {
		case errors.Is(err, services.ErrPreviewForbidden):
			c.JSON(http.StatusForbidden, gin.H{"error": "Không có quyền mở phiên xem thử", "code": "FORBIDDEN"})
		case errors.As(err, &ve) && ve.Code == "INVALID_BODY":
			c.JSON(http.StatusBadRequest, gin.H{"error": ve.Msg, "code": ve.Code})
		case errors.As(err, &ve):
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
		default:
			h.logger.Error("handlers.PreviewHandler.Start", zap.String("tenant_id", tenantID),
				zap.String("user_id", claims.UserID), zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể mở phiên xem thử", "code": "PREVIEW_START_FAILED"})
		}
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": res})
}

// End handles DELETE /preview-sessions/current?reason=exit|left → 204 (idempotent). 409
// PREVIEW_NOT_ACTIVE when the caller is not in a preview session; 400 INVALID_QUERY for another
// reason. An absent reason means "exit".
func (h *PreviewHandler) End(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	claims, ok := middleware.ClaimsFromContext(c)
	if !ok || claims.TokenType != auth.TokenTypePreview {
		c.JSON(http.StatusConflict, gin.H{"error": "Bạn không ở chế độ xem thử", "code": "PREVIEW_NOT_ACTIVE"})
		return
	}
	reason := c.DefaultQuery("reason", "exit")
	if !services.PreviewEndReasons[reason] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reason chỉ nhận exit hoặc left", "code": "INVALID_QUERY"})
		return
	}
	if err := h.svc.End(c.Request.Context(), claims.Subject, reason); err != nil {
		h.logger.Error("handlers.PreviewHandler.End", zap.String("tenant_id", tenantID),
			zap.String("user_id", claims.ActorUserID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể kết thúc phiên xem thử", "code": "PREVIEW_END_FAILED"})
		return
	}
	c.Status(http.StatusNoContent)
}
