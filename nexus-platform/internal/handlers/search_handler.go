// SearchHandler — SEARCH-UNIFIED (Wave 3.5): GET /search?q= on the protected group. Runs under
// the caller's own scope; each entity is ACL-gated + row-scoped + field-ACL-stripped inside the
// service, so the endpoint needs no route-level RequireACL (it is not an /:entity route).
package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
)

// SearchHandler serves the unified cross-entity record search.
type SearchHandler struct {
	svc    *services.SearchService
	logger *zap.Logger
}

func NewSearchHandler(svc *services.SearchService, logger *zap.Logger) *SearchHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &SearchHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts GET /search on the protected (authenticated) group.
func (h *SearchHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/search", h.Search)
}

// Search handles GET /search?q=<term>.
func (h *SearchHandler) Search(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	groups, err := h.svc.Search(c.Request.Context(), scopeOf(c), tenantID, c.Query("q"))
	if err != nil {
		h.logger.Error("handlers.Search", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không tìm kiếm được", "code": "SEARCH_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"groups": groups}})
}
