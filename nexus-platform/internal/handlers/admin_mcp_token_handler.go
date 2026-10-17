package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// AdminMCPTokenHandler is the Studio's "Connect an agent (MCP)" surface (issue #3) under
// /admin/mcp-tokens, mounted on builderGated: the same gate as the AI builder agent, so a non-admin
// gets 403 and an admin without the builder flag too. A builder sees and revokes only the tokens
// they issued. The raw token is in the create response only, once.
type AdminMCPTokenHandler struct {
	svc    *services.MCPTokenService
	logger *zap.Logger
}

// NewAdminMCPTokenHandler wires the handler over the token service.
func NewAdminMCPTokenHandler(svc *services.MCPTokenService, logger *zap.Logger) *AdminMCPTokenHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminMCPTokenHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts the routes on the builder-gated group.
func (h *AdminMCPTokenHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/mcp-tokens", h.List)
	rg.POST("/admin/mcp-tokens", h.Create)
	rg.DELETE("/admin/mcp-tokens/:id", h.Revoke)
}

// Create handles POST /admin/mcp-tokens {expiresAt?}: 201 {data: {token, secret}}. expiresAt is
// optional RFC 3339, at most 8 h away (422 MCP_TOKEN_EXPIRY_INVALID); default 8 h.
func (h *AdminMCPTokenHandler) Create(c *gin.Context) {
	if _, ok := tenantOf(c); !ok {
		return
	}
	var req struct {
		ExpiresAt *time.Time `json:"expiresAt"`
	}
	if !bindJSON(c, &req) {
		return
	}
	sub, _ := middleware.SubjectFromContext(c)
	tok, secret, err := h.svc.Issue(auditContext(c), sub, req.ExpiresAt)
	if err != nil {
		h.respondError(c, err, "Create", sub.TenantID)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"token": tok, "secret": secret}})
}

// List handles GET /admin/mcp-tokens: the caller's tokens that still work, newest first.
func (h *AdminMCPTokenHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	rows, err := h.svc.List(c.Request.Context(), tenantID, actorOf(c))
	if err != nil {
		h.respondError(c, err, "List", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"tokens": rows}})
}

// Revoke handles DELETE /admin/mcp-tokens/:id: 204; 404 MCP_TOKEN_NOT_FOUND for an id that is not
// one of the caller's tokens.
func (h *AdminMCPTokenHandler) Revoke(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	if err := h.svc.Revoke(auditContext(c), tenantID, actorOf(c), c.Param("id")); err != nil {
		h.respondError(c, err, "Revoke", tenantID)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *AdminMCPTokenHandler) respondError(c *gin.Context, err error, op, tenantID string) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
		return
	}
	h.logger.Error("handlers.AdminMCPToken."+op, zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không xử lý được yêu cầu token MCP", "code": "MCP_TOKEN_OP_FAILED"})
}
