package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
)

// AdminUsageHandler serves "Mức dùng" (specs/066 L4): the tenant's per-day count of rule runs, mail
// and webhooks. Like "Việc nền" it is mounted on adminGated, never builderGated: the reader is the
// customer's own tenant-admin. The tenant always comes from the verified token.
type AdminUsageHandler struct {
	svc    *services.UsageService
	logger *zap.Logger
}

// NewAdminUsageHandler wires the handler over the usage service.
func NewAdminUsageHandler(svc *services.UsageService, logger *zap.Logger) *AdminUsageHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminUsageHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts the route under the given (admin-gated) group.
func (h *AdminUsageHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/usage", h.Daily)
}

// Daily handles GET /admin/usage?from=YYYY-MM-DD&to=YYYY-MM-DD.
//
//	200 {data:{timezone, from, to, rollupEnabled, lastRollupAt, days:[…], totals:{…}}}
//	400 USAGE_RANGE_INVALID · 500 USAGE_QUERY_FAILED
func (h *AdminUsageHandler) Daily(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	rep, err := h.svc.Daily(c.Request.Context(), tenantID,
		strings.TrimSpace(c.Query("from")), strings.TrimSpace(c.Query("to")))
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
			return
		}
		h.logger.Error("handlers.AdminUsageHandler.Daily", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không đọc được mức dùng", "code": "USAGE_QUERY_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rep})
}
