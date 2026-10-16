package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
)

// AdminHealthHandler serves "Sức khoẻ" (specs/066 L6): GET /admin/health. Mounted on adminGated,
// like "Việc nền": the reader is the customer's own tenant-admin. The tenant comes from the token.
type AdminHealthHandler struct {
	svc    *services.HealthService
	logger *zap.Logger
}

// NewAdminHealthHandler wires the handler over the health service.
func NewAdminHealthHandler(svc *services.HealthService, logger *zap.Logger) *AdminHealthHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminHealthHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts the route under the given (admin-gated) group.
func (h *AdminHealthHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/health", h.Get)
}

// Get handles GET /admin/health → 200 {data:{status, generatedAt, …}} · 500 HEALTH_QUERY_FAILED.
func (h *AdminHealthHandler) Get(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	sum, err := h.svc.Summary(c.Request.Context(), tenantID)
	if err != nil {
		h.logger.Error("handlers.AdminHealthHandler.Get", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không đọc được sức khoẻ vận hành", "code": "HEALTH_QUERY_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": sum})
}
