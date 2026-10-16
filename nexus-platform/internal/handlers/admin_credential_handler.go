// AdminCredentialHandler — named-credential vault admin API (../specs/050-named-credentials S1b):
//
//	GET    /admin/credentials       list (never a secret, envelope or master-key id)
//	GET    /admin/credentials/:id   one key + the rule actions using it
//	POST   /admin/credentials       create — a Nexus-generated hmac secret is returned ONCE, here
//	PUT    /admin/credentials/:id   non-secret metadata; base URL only within its origin
//	DELETE /admin/credentials/:id   soft delete + crypto-shred; 409 while a rule uses it
//	POST   /admin/credentials/:id/rotate  new secret / headers / host, effective on the next send;
//	                                an hmac key keeps signing V2-Previous for graceHours (S4)
//
// Mounted on adminGated (Auth → Tenant → RequireAdmin), NOT builderGated: the customer's own admin
// enters and replaces partner keys without Nexus ever seeing them (plan D-4). Routes are registered
// with FULL paths so the orphan gate matches its ALLOWED_ORPHANS keys.
package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
)

// AdminCredentialHandler serves the vault endpoints.
type AdminCredentialHandler struct {
	svc    *services.CredentialService
	logger *zap.Logger
}

// NewAdminCredentialHandler wires the handler over the vault service.
func NewAdminCredentialHandler(svc *services.CredentialService, logger *zap.Logger) *AdminCredentialHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminCredentialHandler{svc: svc, logger: logger}
}

// RegisterAdminRoutes mounts the vault on the admin-gated group.
func (h *AdminCredentialHandler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/credentials", h.List)
	rg.GET("/admin/credentials/:id", h.Get)
	rg.POST("/admin/credentials", h.Create)
	rg.PUT("/admin/credentials/:id", h.Update)
	rg.DELETE("/admin/credentials/:id", h.Delete)
	rg.POST("/admin/credentials/:id/rotate", h.Rotate)
}

// rotateRequest is the POST /rotate body (camelCase). ExtraHeaders absent keeps the stored set; an
// empty object clears it. GraceHours is a pointer: 0 ("no grace") differs from absent (default 24).
type rotateRequest struct {
	Secret         *string           `json:"secret"`
	GenerateSecret bool              `json:"generateSecret"`
	ExtraHeaders   map[string]string `json:"extraHeaders"`
	GraceHours     *int              `json:"graceHours"`
	BaseURL        *string           `json:"baseUrl"`
	Username       *string           `json:"username"`
}

// credentialRequest is the POST/PUT body (camelCase). Secret, GenerateSecret and ExtraHeaders are
// create-only; the service refuses them on PUT instead of dropping them.
type credentialRequest struct {
	Name           *string           `json:"name"`
	Label          *string           `json:"label"`
	BaseURL        *string           `json:"baseUrl"`
	AuthType       *string           `json:"authType"`
	HeaderName     *string           `json:"headerName"`
	HeaderPrefix   *string           `json:"headerPrefix"`
	Username       *string           `json:"username"`
	Secret         *string           `json:"secret"`
	GenerateSecret bool              `json:"generateSecret"`
	ExtraHeaders   map[string]string `json:"extraHeaders"`
}

func credStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// List handles GET /admin/credentials.
func (h *AdminCredentialHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	rows, err := h.svc.List(c.Request.Context(), tenantID)
	if err != nil {
		h.writeErr(c, "List", tenantID, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"credentials": rows}})
}

// Get handles GET /admin/credentials/:id.
func (h *AdminCredentialHandler) Get(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	v, usages, err := h.svc.Get(c.Request.Context(), tenantID, c.Param("id"))
	if err != nil {
		h.writeErr(c, "Get", tenantID, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"credential": v, "usages": usages}})
}

// Create handles POST /admin/credentials. The response carries "secret" ONLY when Nexus generated
// it; a typed-in secret is the caller's own and is never echoed.
func (h *AdminCredentialHandler) Create(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req credentialRequest
	if !bindJSON(c, &req) {
		return
	}
	v, secret, err := h.svc.Create(auditContext(c), tenantID, services.CredentialInput{
		Name: credStr(req.Name), Label: credStr(req.Label), BaseURL: credStr(req.BaseURL), AuthType: credStr(req.AuthType),
		HeaderName: req.HeaderName, HeaderPrefix: req.HeaderPrefix, Username: req.Username,
		Secret: req.Secret, GenerateSecret: req.GenerateSecret, ExtraHeaders: req.ExtraHeaders,
		ActorID: actorOf(c),
	})
	if err != nil {
		h.writeErr(c, "Create", tenantID, err)
		return
	}
	data := gin.H{"credential": v}
	if secret != "" {
		data["secret"] = secret
		c.Header("Cache-Control", "no-store")
	}
	c.JSON(http.StatusCreated, gin.H{"data": data})
}

// Update handles PUT /admin/credentials/:id.
func (h *AdminCredentialHandler) Update(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req credentialRequest
	if !bindJSON(c, &req) {
		return
	}
	v, err := h.svc.Update(auditContext(c), tenantID, c.Param("id"), services.CredentialUpdate{
		Label: req.Label, BaseURL: req.BaseURL, HeaderName: req.HeaderName, HeaderPrefix: req.HeaderPrefix,
		Username: req.Username, Name: req.Name, AuthType: req.AuthType,
		Secret: req.Secret, GenerateSecret: req.GenerateSecret, ExtraHeaders: req.ExtraHeaders,
		ActorID: actorOf(c),
	})
	if err != nil {
		h.writeErr(c, "Update", tenantID, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"credential": v}})
}

// Rotate handles POST /admin/credentials/:id/rotate. Like Create, the response carries "secret"
// ONLY when Nexus generated it, and then it must not be cached.
func (h *AdminCredentialHandler) Rotate(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req rotateRequest
	if !bindJSONStrict(c, &req) {
		return
	}
	v, secret, err := h.svc.Rotate(auditContext(c), tenantID, c.Param("id"), services.CredentialRotation{
		Secret: req.Secret, GenerateSecret: req.GenerateSecret, ExtraHeaders: req.ExtraHeaders,
		GraceHours: req.GraceHours, BaseURL: req.BaseURL, Username: req.Username, ActorID: actorOf(c),
	})
	if err != nil {
		h.writeErr(c, "Rotate", tenantID, err)
		return
	}
	data := gin.H{"credential": v}
	if secret != "" {
		data["secret"] = secret
		c.Header("Cache-Control", "no-store")
	}
	c.JSON(http.StatusOK, gin.H{"data": data})
}

// Delete handles DELETE /admin/credentials/:id.
func (h *AdminCredentialHandler) Delete(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	if err := h.svc.Delete(auditContext(c), tenantID, c.Param("id"), actorOf(c)); err != nil {
		h.writeErr(c, "Delete", tenantID, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// writeErr maps a ValidationError through statusForCode (CREDENTIALS_NOT_CONFIGURED 503,
// CREDENTIAL_NOT_FOUND 404, CREDENTIAL_NAME_TAKEN / CREDENTIAL_IN_USE 409, the rest 422) with its
// details (field, usages); anything else is a logged 500 that never echoes the cause.
func (h *AdminCredentialHandler) writeErr(c *gin.Context, op, tenantID string, err error) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
		return
	}
	h.logger.Error("handlers.AdminCredential."+op, zap.String("tenant_id", tenantID), zap.String("user_id", actorOf(c)), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không xử lý được yêu cầu kho khoá kết nối", "code": "CREDENTIAL_OP_FAILED"})
}
