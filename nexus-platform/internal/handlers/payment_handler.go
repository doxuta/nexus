// PaymentHandler — DONG-TIEN-KHEP-KIN p1 endpoints (../specs/016-payment-flow/spec.md §3):
//
//	GET /:entity/:id/payment-qr?amountField=&memo=   user (recordGroup → RequireACL view + row-scope)
//	GET /tenant/payment-status                       user — {configured: bool}, NO account details
//	GET /admin/tenant/payment                        admin — caller tenant's bank config
//	PUT /admin/tenant/payment                        admin — full-replace ({} clears)
package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
)

// PaymentHandler serves the VietQR payment endpoints.
type PaymentHandler struct {
	svc    *services.TenantPaymentService
	logger *zap.Logger
}

func NewPaymentHandler(svc *services.TenantPaymentService, logger *zap.Logger) *PaymentHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &PaymentHandler{svc: svc, logger: logger}
}

// RegisterRecordRoute mounts the QR read on the ACL-gated record group (GET → "view", A5
// row-scope enforced inside RecordService.Get — only someone who can SEE the record can mint
// its QR).
func (h *PaymentHandler) RegisterRecordRoute(rg *gin.RouterGroup) {
	rg.GET("/:entity/:id/payment-qr", h.RecordQR)
}

// RegisterStatusRoute mounts the user-level "is payment set up?" probe on the protected
// (non-admin) group.
//
// It exists because the QR button was invisible to precisely the people it was built for.
// Minting a QR is user-level by design (RegisterRecordRoute above), but the FE decided
// whether to SHOW the button by reading GET /admin/tenant/payment — admin-only. For every
// non-admin that probe returned 403, so `bank` stayed undefined and the button never
// rendered: a sales rep, whose job is collecting money, could not reach it, and every
// record view paid for a doomed request (found 2026-07-29 walking the customer face).
//
// It returns a BOOLEAN and nothing else. The admin route keeps the account number, name and
// bank id; this one answers only "is it set up", which leaks nothing a payer would not read
// off the QR they are about to be shown anyway.
func (h *PaymentHandler) RegisterStatusRoute(rg *gin.RouterGroup) {
	rg.GET("/tenant/payment-status", h.Status)
}

// Status handles GET /tenant/payment-status — {configured: bool} for any authenticated
// user of the tenant. A read failure degrades to configured:false (the button hides)
// rather than 500-ing a record screen over an optional feature.
func (h *PaymentHandler) Status(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	p, err := h.svc.Get(c.Request.Context(), tenantID)
	if err != nil {
		h.logger.Debug("handlers.Payment.Status: read failed (reporting not-configured)",
			zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"configured": false}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"configured": p.BankBin != ""}})
}

// RegisterAdminRoutes mounts the bank-config management on the admin-gated group.
func (h *PaymentHandler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/tenant/payment", h.Get)
	rg.PUT("/admin/tenant/payment", h.Update)
}

// RecordQR handles GET /:entity/:id/payment-qr.
func (h *PaymentHandler) RecordQR(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	qr, err := h.svc.RecordQR(
		c.Request.Context(), scopeOf(c), tenantID,
		c.Param("entity"), c.Param("id"),
		c.Query("amountField"), c.Query("memo"),
	)
	if err != nil {
		h.writeErr(c, tenantID, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": qr})
}

// Get handles GET /admin/tenant/payment.
func (h *PaymentHandler) Get(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	p, err := h.svc.Get(c.Request.Context(), tenantID)
	if err != nil {
		h.logger.Error("handlers.Payment.Get", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không tải được cấu hình thanh toán", "code": "PAYMENT_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"payment": p}})
}

// Update handles PUT /admin/tenant/payment.
func (h *PaymentHandler) Update(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req services.PaymentConfig
	if !bindJSON(c, &req) {
		return
	}
	p, err := h.svc.Update(auditContext(c), tenantID, req)
	if err != nil {
		h.writeErr(c, tenantID, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"payment": p}})
}

// writeErr maps service errors (validation 422, record-engine 404 sentinels, else 500).
func (h *PaymentHandler) writeErr(c *gin.Context, tenantID string, err error) {
	switch {
	case errors.Is(err, services.ErrEntityNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "Entity không tồn tại", "code": "ENTITY_NOT_FOUND"})
		return
	case errors.Is(err, services.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "Không tìm thấy bản ghi", "code": "RECORD_NOT_FOUND"})
		return
	}
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
		return
	}
	h.logger.Error("handlers.Payment", zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi hệ thống", "code": "PAYMENT_FAILED"})
}
