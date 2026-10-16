package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// workspaceSessionService is the slice of services.WorkspaceSessionService the handler drives.
type workspaceSessionService interface {
	Open(ctx context.Context, home auth.Subject, in services.WorkspaceOpenInput) (*services.WorkspaceOpenResult, error)
	Close(ctx context.Context, sub auth.Subject) error
}

// AdminWorkspaceSessionHandler serves the builder workspace session of
// specs/062-customer-workspace (L1b):
//
//   - POST /admin/workspace-sessions — builderGated + RefuseActingSession (HOME token only): open,
//     reuse or resume the builder's session in a customer tenant and receive its short-lived token.
//   - DELETE /admin/workspace-sessions/current — builderGated (SESSION token): "Về Nexus". Mounted on
//     builderGated rather than adminGated so a customer-side admin / owner holding a plain token gets the
//     same 403 as every other builder route (no 404 that would reveal the route exists).
type AdminWorkspaceSessionHandler struct {
	svc    workspaceSessionService
	logger *zap.Logger
}

// NewAdminWorkspaceSessionHandler wires the handler over the workspace-session service.
func NewAdminWorkspaceSessionHandler(svc workspaceSessionService, logger *zap.Logger) *AdminWorkspaceSessionHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminWorkspaceSessionHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts both routes on the builder-gated group, BEFORE the dynamic /:entity
// dispatcher. POST additionally refuses a workspace-session token (no nested open).
func (h *AdminWorkspaceSessionHandler) RegisterRoutes(builder *gin.RouterGroup) {
	builder.POST("/admin/workspace-sessions", middleware.RefuseActingSession(), h.Open)
	builder.DELETE("/admin/workspace-sessions/current", h.Close)
}

// openWorkspaceRequest is the POST /admin/workspace-sessions body.
type openWorkspaceRequest struct {
	TenantSlug      string `json:"tenantSlug"`
	ResumeSessionID string `json:"resumeSessionId"`
}

// Open handles POST /admin/workspace-sessions → 201 (new session) / 200 (reused or resumed)
// {data: {accessToken, expiresAt, session}}. 401 UNAUTHORIZED (no sid / home login dead) · 403
// WORKSPACE_SESSION_NOT_ALLOWED · 404 TENANT_NOT_FOUND · 404 WORKSPACE_SESSION_ENDED (resume only) ·
// 422 WORKSPACE_IS_HOME · 422 WORKSPACE_SLUG_REQUIRED.
func (h *AdminWorkspaceSessionHandler) Open(c *gin.Context) {
	claims, ok := middleware.ClaimsFromContext(c)
	if !ok || claims.TokenType != auth.TokenTypeAccess {
		// Only a person's login token opens a session: an API key is a machine identity.
		c.JSON(http.StatusForbidden, gin.H{"error": "Yêu cầu quyền quản trị viên", "code": "FORBIDDEN"})
		return
	}
	var req openWorkspaceRequest
	if !bindJSON(c, &req) {
		return
	}
	res, err := h.svc.Open(c.Request.Context(), claims.Subject, services.WorkspaceOpenInput{
		TenantSlug: req.TenantSlug, ResumeSessionID: req.ResumeSessionID,
	})
	if err != nil {
		switch {
		case errors.Is(err, services.ErrWorkspaceNotAllowed):
			c.JSON(http.StatusForbidden, gin.H{"error": "Không thực hiện được khi đang dựng trong không gian của khách — hãy về Nexus để làm việc này", "code": "WORKSPACE_SESSION_NOT_ALLOWED"})
		case errors.Is(err, services.ErrWorkspaceSlugRequired):
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Thiếu mã không gian của khách", "code": "WORKSPACE_SLUG_REQUIRED"})
		case errors.Is(err, services.ErrWorkspaceUnauthorized):
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Yêu cầu xác thực không hợp lệ", "code": "UNAUTHORIZED"})
		case errors.Is(err, services.ErrWorkspaceTenantNotFound):
			// Same wording as the cross-tenant routes: unknown, suspended and inactive are ONE answer.
			c.JSON(http.StatusNotFound, gin.H{"error": "Không tìm thấy workspace", "code": "TENANT_NOT_FOUND"})
		case errors.Is(err, services.ErrWorkspaceSessionEnded):
			c.JSON(http.StatusNotFound, gin.H{"error": "Phiên dựng đã kết thúc", "code": "WORKSPACE_SESSION_ENDED"})
		case errors.Is(err, services.ErrWorkspaceIsHome):
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Đây là không gian của chính bạn — không cần mở phiên dựng", "code": "WORKSPACE_IS_HOME"})
		default:
			h.logger.Error("handlers.AdminWorkspaceSessionHandler.Open",
				zap.String("tenant_id", claims.TenantID), zap.String("user_id", claims.UserID), zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể mở phiên dựng", "code": "WORKSPACE_SESSION_OPEN_FAILED"})
		}
		return
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	c.JSON(status, gin.H{"data": res})
}

// Close handles DELETE /admin/workspace-sessions/current → 204. A HOME token holds no session ⇒ 404
// WORKSPACE_SESSION_NOT_FOUND. A second close of the same session by a request that already passed the
// Auth stage (two tabs) is 204 too; one that arrives AFTER the close never gets here — the Auth stage
// answers 401 (the route is not exempt from the resolver: a closed session's token must die everywhere).
func (h *AdminWorkspaceSessionHandler) Close(c *gin.Context) {
	claims, ok := middleware.ClaimsFromContext(c)
	if !ok {
		c.JSON(http.StatusForbidden, gin.H{"error": "Yêu cầu quyền quản trị viên", "code": "FORBIDDEN"})
		return
	}
	if err := h.svc.Close(c.Request.Context(), claims.Subject); err != nil {
		if errors.Is(err, services.ErrWorkspaceSessionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Không có phiên dựng đang mở", "code": "WORKSPACE_SESSION_NOT_FOUND"})
			return
		}
		h.logger.Error("handlers.AdminWorkspaceSessionHandler.Close",
			zap.String("tenant_id", claims.TenantID), zap.String("user_id", claims.ActorUserID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể kết thúc phiên dựng", "code": "WORKSPACE_SESSION_CLOSE_FAILED"})
		return
	}
	c.Status(http.StatusNoContent)
}
