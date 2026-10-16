// WorkQueueHandler — VIEC-HOM-NAY endpoints (../specs/012-work-queues/spec.md §3):
//
//	GET    /work-queues              any authenticated user — visible queues + live counts
//	GET    /admin/work-queues        admin — all queues
//	POST   /admin/work-queues        admin — create (full config-time validation)
//	PUT    /admin/work-queues/:id    admin — full-replace
//	DELETE /admin/work-queues/:id    admin — soft delete
package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
	"nexus/platform/internal/workqueue"
)

// WorkQueueHandler serves the queue endpoints.
type WorkQueueHandler struct {
	svc    *services.WorkQueueService
	logger *zap.Logger
}

func NewWorkQueueHandler(svc *services.WorkQueueService, logger *zap.Logger) *WorkQueueHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &WorkQueueHandler{svc: svc, logger: logger}
}

// RegisterUserRoutes mounts the end-user listing on the authed (non-admin) group.
func (h *WorkQueueHandler) RegisterUserRoutes(rg *gin.RouterGroup) {
	rg.GET("/work-queues", h.ListForUser)
}

// RegisterAdminRoutes mounts CRUD on the admin-gated group.
func (h *WorkQueueHandler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/work-queues", h.ListAdmin)
	rg.POST("/admin/work-queues", h.Create)
	rg.PUT("/admin/work-queues/:id", h.Update)
	rg.DELETE("/admin/work-queues/:id", h.Delete)
}

// workQueueRequest is the create/update body (camelCase per API conventions).
type workQueueRequest struct {
	Name       string                  `json:"name"`
	Label      string                  `json:"label"`
	EntityName string                  `json:"entityName"`
	RoleID     string                  `json:"roleId"`
	Filters    []workqueue.QueueFilter `json:"filters"`
	SortField  string                  `json:"sortField"`
	SortOrder  string                  `json:"sortOrder"`
	ActionVerb string                  `json:"actionVerb"`
	Position   int                     `json:"position"`
	IsActive   *bool                   `json:"isActive"`
}

func (r workQueueRequest) toInput() services.WorkQueueInput {
	return services.WorkQueueInput{
		Name: r.Name, Label: r.Label, EntityName: r.EntityName, RoleID: r.RoleID,
		Filters: r.Filters, SortField: r.SortField, SortOrder: r.SortOrder,
		ActionVerb: r.ActionVerb, Position: r.Position, IsActive: r.IsActive,
	}
}

// ListForUser handles GET /work-queues — the home-screen cards.
func (h *WorkQueueHandler) ListForUser(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	scope := scopeOf(c)
	views, err := h.svc.ListForUser(c.Request.Context(), scope, tenantID)
	if err != nil {
		h.logger.Error("handlers.WorkQueue.ListForUser", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không tải được hàng đợi công việc", "code": "WORKQUEUE_LIST_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"queues": views}})
}

// ListAdmin handles GET /admin/work-queues.
func (h *WorkQueueHandler) ListAdmin(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	rows, err := h.svc.ListAdmin(c.Request.Context(), tenantID)
	if err != nil {
		h.logger.Error("handlers.WorkQueue.ListAdmin", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không tải được danh sách hàng đợi", "code": "WORKQUEUE_LIST_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"queues": rows}})
}

// Create handles POST /admin/work-queues.
func (h *WorkQueueHandler) Create(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req workQueueRequest
	if !bindJSON(c, &req) {
		return
	}
	q, err := h.svc.Create(c.Request.Context(), tenantID, req.toInput())
	if err != nil {
		h.writeErr(c, tenantID, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"queue": q}})
}

// Update handles PUT /admin/work-queues/:id.
func (h *WorkQueueHandler) Update(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req workQueueRequest
	if !bindJSON(c, &req) {
		return
	}
	q, err := h.svc.Update(c.Request.Context(), tenantID, c.Param("id"), req.toInput())
	if err != nil {
		h.writeErr(c, tenantID, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"queue": q}})
}

// Delete handles DELETE /admin/work-queues/:id.
func (h *WorkQueueHandler) Delete(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	if err := h.svc.Delete(c.Request.Context(), tenantID, c.Param("id")); err != nil {
		h.writeErr(c, tenantID, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// writeErr maps service errors to the API envelope (422 validation / 404 / 409 / 500).
func (h *WorkQueueHandler) writeErr(c *gin.Context, tenantID string, err error) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		status := http.StatusUnprocessableEntity
		switch ve.Code {
		case "WORKQUEUE_NOT_FOUND":
			status = http.StatusNotFound
		case "WORKQUEUE_ALREADY_EXISTS":
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
		return
	}
	h.logger.Error("handlers.WorkQueue", zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi hệ thống", "code": "WORKQUEUE_FAILED"})
}
