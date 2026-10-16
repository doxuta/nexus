// InboundHookHandler — CONG-TIN-HIEU endpoints (../specs/015-inbound-hooks/spec.md §3):
//
//	POST   /hooks/:tenant/:hook                  public, HMAC-signed (no JWT)
//	GET    /admin/inbound-hooks                  admin — list (secret included, spec §6)
//	POST   /admin/inbound-hooks                  admin — create (server mints the secret)
//	PUT    /admin/inbound-hooks/:id              admin — update config (secret untouched)
//	POST   /admin/inbound-hooks/:id/rotate       admin — rotate secret
//	DELETE /admin/inbound-hooks/:id              admin — soft delete
//	GET    /admin/inbound-hooks/:id/deliveries   admin — recent deliveries
package handlers

import (
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/inboundhook"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// InboundHookHandler serves the inbound-webhook endpoints.
type InboundHookHandler struct {
	svc    *services.InboundHookService
	logger *zap.Logger
}

func NewInboundHookHandler(svc *services.InboundHookService, logger *zap.Logger) *InboundHookHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &InboundHookHandler{svc: svc, logger: logger}
}

// RegisterPublicRoute mounts the receive endpoint on the public (pre-auth) group. Auth is the
// HMAC signature — the global body-size cap and HTTP timeouts still apply.
func (h *InboundHookHandler) RegisterPublicRoute(rg *gin.RouterGroup) {
	rg.POST("/hooks/:tenant/:hook", h.Receive)
}

// RegisterAdminRoutes mounts management on the admin-gated group.
func (h *InboundHookHandler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/inbound-hooks", h.List)
	rg.POST("/admin/inbound-hooks", h.Create)
	rg.PUT("/admin/inbound-hooks/:id", h.Update)
	rg.POST("/admin/inbound-hooks/:id/rotate", h.Rotate)
	rg.DELETE("/admin/inbound-hooks/:id", h.Delete)
	rg.GET("/admin/inbound-hooks/:id/deliveries", h.Deliveries)
}

// Receive handles POST /hooks/:tenant/:hook. The body is read RAW (the HMAC covers the exact
// bytes); every pre-write rejection is the same 401 (spec §5 anti-probing).
func (h *InboundHookHandler) Receive(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		// An over-cap body gets the standard 413 (review S3 — the cap is public knowledge, a
		// legitimate partner should learn it, not debug a misleading 401); anything else
		// (broken transfer) stays on the uniform 401.
		if middleware.IsMaxBytesError(err) {
			middleware.RespondTooLarge(c)
			return
		}
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Tín hiệu không hợp lệ", "code": "HOOK_UNAUTHORIZED"})
		return
	}
	res, err := h.svc.Receive(
		c.Request.Context(),
		c.Param("tenant"),
		c.Param("hook"),
		c.GetHeader("X-Nexus-Signature"),
		body,
	)
	if err != nil {
		if errors.Is(err, services.ErrHookUnauthorized) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Tín hiệu không hợp lệ", "code": "HOOK_UNAUTHORIZED"})
			return
		}
		var me *services.MaintenanceError
		if errors.As(err, &me) {
			// specs/068: signature verified, tenant write-frozen — the partner retries after Retry-After.
			middleware.WriteTenantMaintenance(c, me.Until)
			return
		}
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			// Signature already verified — the sender is the configured partner; the real
			// validation code helps them fix their payload (and it landed in the delivery log).
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		if errors.Is(err, services.ErrRecordHooksUnavailable) {
			// specs/048 FR-021, same envelope as the record routes: the entity's before rules could
			// not be read, so the write was refused unjudged: nothing was written, a retry is safe.
			h.logger.Warn("handlers.InboundHook.Receive: before rules unavailable", zap.Error(err))
			c.Header("Retry-After", "1")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Hệ thống bận, thử lại sau ít giây", "code": "RECORD_HOOKS_UNAVAILABLE"})
			return
		}
		h.logger.Error("handlers.InboundHook.Receive", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi hệ thống", "code": "HOOK_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// inboundHookRequest is the create/update body (camelCase per API conventions).
type inboundHookRequest struct {
	Name         string                    `json:"name"`
	EntityName   string                    `json:"entityName"`
	Mapping      []inboundhook.Mapping     `json:"mapping"`
	MatchField   string                    `json:"matchField"`
	PaymentMatch *inboundhook.PaymentMatch `json:"paymentMatch"`
	IsActive     *bool                     `json:"isActive"`
}

func (r inboundHookRequest) toInput() services.InboundHookInput {
	// An all-empty paymentMatch object counts as "no matching step" (FE sends {} when the
	// section is collapsed) — normalize to nil so validate doesn't reject phantom configs.
	pm := r.PaymentMatch
	if pm != nil && *pm == (inboundhook.PaymentMatch{}) {
		pm = nil
	}
	return services.InboundHookInput{
		Name: r.Name, EntityName: r.EntityName, Mapping: r.Mapping,
		MatchField: r.MatchField, PaymentMatch: pm, IsActive: r.IsActive,
	}
}

// List handles GET /admin/inbound-hooks.
func (h *InboundHookHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	rows, err := h.svc.ListAdmin(c.Request.Context(), tenantID)
	if err != nil {
		h.logger.Error("handlers.InboundHook.List", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không tải được danh sách hook", "code": "HOOK_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"hooks": rows}})
}

// Create handles POST /admin/inbound-hooks.
func (h *InboundHookHandler) Create(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req inboundHookRequest
	if !bindJSON(c, &req) {
		return
	}
	hook, err := h.svc.Create(auditContext(c), tenantID, actorOf(c), req.toInput())
	if err != nil {
		h.writeErr(c, tenantID, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"hook": hook}})
}

// Update handles PUT /admin/inbound-hooks/:id.
func (h *InboundHookHandler) Update(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req inboundHookRequest
	if !bindJSON(c, &req) {
		return
	}
	hook, err := h.svc.Update(auditContext(c), tenantID, c.Param("id"), req.toInput())
	if err != nil {
		h.writeErr(c, tenantID, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"hook": hook}})
}

// Rotate handles POST /admin/inbound-hooks/:id/rotate.
func (h *InboundHookHandler) Rotate(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	hook, err := h.svc.RotateSecret(auditContext(c), tenantID, c.Param("id"))
	if err != nil {
		h.writeErr(c, tenantID, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"hook": hook}})
}

// Delete handles DELETE /admin/inbound-hooks/:id.
func (h *InboundHookHandler) Delete(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	if err := h.svc.Delete(auditContext(c), tenantID, c.Param("id")); err != nil {
		h.writeErr(c, tenantID, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// Deliveries handles GET /admin/inbound-hooks/:id/deliveries.
func (h *InboundHookHandler) Deliveries(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	rows, err := h.svc.ListDeliveries(c.Request.Context(), tenantID, c.Param("id"))
	if err != nil {
		h.writeErr(c, tenantID, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deliveries": rows}})
}

// writeErr maps service errors to the API envelope.
func (h *InboundHookHandler) writeErr(c *gin.Context, tenantID string, err error) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		status := http.StatusUnprocessableEntity
		switch ve.Code {
		case "HOOK_NOT_FOUND":
			status = http.StatusNotFound
		case "HOOK_ALREADY_EXISTS":
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
		return
	}
	h.logger.Error("handlers.InboundHook", zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi hệ thống", "code": "HOOK_FAILED"})
}
