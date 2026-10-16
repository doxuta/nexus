// BrandingHandler — AO-THUONG-HIEU p1 endpoints (../specs/013-tenant-branding/spec.md §3):
//
//	GET /auth/branding/:tenant     public — branding for a login slug (fail-soft {})
//	GET /admin/tenant/branding     admin  — caller tenant's branding
//	PUT /admin/tenant/branding     admin  — full-replace ({} = back to default Nexus)
package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
)

// BrandingHandler serves the white-label branding endpoints.
type BrandingHandler struct {
	svc    *services.TenantBrandingService
	logger *zap.Logger
}

func NewBrandingHandler(svc *services.TenantBrandingService, logger *zap.Logger) *BrandingHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &BrandingHandler{svc: svc, logger: logger}
}

// RegisterPublicRoute mounts the pre-auth read on the UNLIMITED public group — deliberately
// NOT the /auth rate-limit bucket (mirror AUTH-ME-RATELIMIT B11: every login-page load and app
// boot fetches this; an office behind NAT would burn its own login budget). The read is a
// single indexed SELECT with a fail-soft empty answer, so there is nothing to brute-force.
func (h *BrandingHandler) RegisterPublicRoute(rg *gin.RouterGroup) {
	rg.GET("/auth/branding/:tenant", h.GetPublic)
}

// RegisterAdminRoutes mounts management on the admin-gated group. No tenant parameter —
// the target is ALWAYS the caller's JWT-claim tenant (an admin cannot brand another tenant).
func (h *BrandingHandler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/tenant/branding", h.Get)
	rg.PUT("/admin/tenant/branding", h.Update)
}

// GetPublic handles GET /auth/branding/:tenant. Always 200 with one shape — unknown slug,
// suspended tenant and unbranded tenant are indistinguishable (spec §3 posture).
func (h *BrandingHandler) GetPublic(c *gin.Context) {
	b := h.svc.GetPublicBySlug(c.Request.Context(), c.Param("tenant"))
	// Public + fail-soft → safe to cache: login-page loads, app boots and the login debounce
	// all hit this; a short TTL lets browser/CDN absorb the unthrottled traffic (security audit).
	c.Header("Cache-Control", "public, max-age=60")
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"branding": b}})
}

// Get handles GET /admin/tenant/branding.
func (h *BrandingHandler) Get(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	b, state, err := h.svc.GetState(c.Request.Context(), tenantID)
	if err != nil {
		h.logger.Error("handlers.Branding.Get", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không tải được thương hiệu", "code": "BRANDING_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": brandingData(b, state)})
}

// brandingData is the response body of the admin routes: the document, plus draftState when the
// caller reads the builder's overlay (specs/063 S7-3).
func brandingData(b services.Branding, state string) gin.H {
	d := gin.H{"branding": b}
	if state != "" {
		d["draftState"] = state
	}
	return d
}

// Update handles PUT /admin/tenant/branding.
func (h *BrandingHandler) Update(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req services.Branding
	if !bindJSON(c, &req) {
		return
	}
	b, err := h.svc.Update(auditContext(c), tenantID, req)
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			status := http.StatusUnprocessableEntity
			if ve.Code == "TENANT_NOT_FOUND" {
				status = http.StatusNotFound
			}
			c.JSON(status, gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.Branding.Update", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không lưu được thương hiệu", "code": "BRANDING_FAILED"})
		return
	}
	// Delivered workspace + builder: the save went to the draft layer; say so, so the screen can
	// label it. Everyone else gets the plain document.
	_, state, serr := h.svc.GetState(c.Request.Context(), tenantID)
	if serr != nil {
		h.logger.Warn("handlers.Branding.Update: cannot read draft state after save", zap.String("tenant_id", tenantID), zap.Error(serr))
		state = ""
	}
	jsonWV(c, http.StatusOK, gin.H{"data": brandingData(b, state)})
}
