package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
)

// AdminAPIKeyHandler exposes the machine-identity admin surface (APIKEY-1,
// docs/api-conventions.md §3e) under /admin/api-keys, behind Auth → Tenant → RequireAdmin like
// every other /admin resource. Contract highlights:
//
//   - POST returns the raw secret ONCE, in the create response only — it is never stored and
//     never retrievable again (the service persists a SHA-256 only).
//   - GET lists metadata (name, keyPrefix, role, lastUsedAt, expiresAt, revokedAt) — the entity
//     carries no secret and its hash field is json:"-", so the list is leak-proof by shape.
//   - DELETE soft-revokes (row kept for audit), idempotent.
type AdminAPIKeyHandler struct {
	svc    *services.APIKeyService
	logger *zap.Logger
}

// NewAdminAPIKeyHandler wires the handler over the API-key service.
func NewAdminAPIKeyHandler(svc *services.APIKeyService, logger *zap.Logger) *AdminAPIKeyHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminAPIKeyHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts the API-key routes under the given (admin-gated) group.
func (h *AdminAPIKeyHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/api-keys", h.List)
	rg.POST("/admin/api-keys", h.Create)
	rg.DELETE("/admin/api-keys/:id", h.Revoke)
}

// createAPIKeyRequest is the POST /admin/api-keys body. ExpiresAt is optional RFC3339.
type createAPIKeyRequest struct {
	Name      string     `json:"name"`
	RoleID    string     `json:"roleId"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// Create handles POST /admin/api-keys: validates (name, in-tenant role, future expiry), mints
// the key and returns 201 with the metadata row PLUS the raw secret — the only response that
// will ever contain it ("copy now" contract).
func (h *AdminAPIKeyHandler) Create(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req createAPIKeyRequest
	if !bindJSON(c, &req) {
		return
	}
	key, secret, err := h.svc.IssueAPIKey(auditContext(c), tenantID, services.IssueAPIKeyInput{
		Name:      req.Name,
		RoleID:    req.RoleID,
		ExpiresAt: req.ExpiresAt,
		ActorID:   actorOf(c),
	})
	if err != nil {
		h.respondServiceError(c, err, "Create", tenantID)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"apiKey": key, "secret": secret}})
}

// List handles GET /admin/api-keys: every key of the tenant (revoked included, for audit) —
// never the secret or its hash.
func (h *AdminAPIKeyHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	keys, err := h.svc.ListAPIKeys(c.Request.Context(), tenantID)
	if err != nil {
		h.logger.Error("handlers.AdminAPIKey.List", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể liệt kê khoá API", "code": "APIKEY_LIST_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"apiKeys": keys}})
}

// Revoke handles DELETE /admin/api-keys/:id: soft-revoke (revoked_at set, row kept). Unknown /
// cross-tenant id → 404 APIKEY_NOT_FOUND; already revoked → idempotent 204.
func (h *AdminAPIKeyHandler) Revoke(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	if err := h.svc.RevokeAPIKey(auditContext(c), tenantID, c.Param("id")); err != nil {
		h.respondServiceError(c, err, "Revoke", tenantID)
		return
	}
	c.Status(http.StatusNoContent)
}

// respondServiceError maps a service error to HTTP: ValidationError → statusForCode
// (APIKEY_NOT_FOUND → 404; APIKEY_NAME_REQUIRED / APIKEY_NAME_TOO_LONG / APIKEY_ROLE_INVALID /
// APIKEY_EXPIRY_INVALID → 422 default); anything else is a logged 500.
func (h *AdminAPIKeyHandler) respondServiceError(c *gin.Context, err error, op, tenantID string) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
		return
	}
	h.logger.Error("handlers.AdminAPIKey."+op, zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xử lý yêu cầu khoá API", "code": "APIKEY_OP_FAILED"})
}
