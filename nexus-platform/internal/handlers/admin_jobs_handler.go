package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/services"
)

// AdminJobsHandler serves the "Việc nền" surface (DAILY-QUEUE #31/#32, specs/021 §3): the tenant's
// background-job log. It MUST be mounted on adminGated, never builderGated: the persona is the
// customer's own tenant-admin, who sits outside the builder/operator subset (specs/034) and would
// get 403 there. The tenant always comes from the verified token.
type AdminJobsHandler struct {
	svc    *services.BackgroundJobService
	logger *zap.Logger
}

// NewAdminJobsHandler wires the handler over the background-job service.
func NewAdminJobsHandler(svc *services.BackgroundJobService, logger *zap.Logger) *AdminJobsHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminJobsHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts the routes under the given (admin-gated) group.
func (h *AdminJobsHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/jobs", h.List)
	rg.POST("/admin/jobs/:id/retry", h.Retry)
}

// jobStatuses is the set a ?status= filter may name (the sys_jobs lifecycle).
var jobStatuses = map[string]bool{
	jobs.StatusQueued: true, jobs.StatusRunning: true, jobs.StatusDone: true, jobs.StatusFailed: true,
}

// List handles GET /admin/jobs?type=&status=&limit=&after= — the tenant's jobs newest first,
// keyset-paged. Returns {data:[…], pagination:{limit, nextCursor}} (the audit-log envelope; limit
// echoes the clamped page size).
func (h *AdminJobsHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	f := jobs.TenantJobFilter{
		Type:   strings.TrimSpace(c.Query("type")),
		Status: strings.TrimSpace(c.Query("status")),
		Limit:  atoiDefault(c.Query("limit"), 50),
		After:  c.Query("after"),
	}
	// A filter the store would silently match nothing with is a client bug: say so.
	if (f.Status != "" && !jobStatuses[f.Status]) || len(f.Type) > 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bộ lọc không hợp lệ: status ∈ queued|running|done|failed, type tối đa 64 ký tự", "code": "JOB_FILTER_INVALID"})
		return
	}
	rows, next, err := h.svc.List(c.Request.Context(), tenantID, f)
	if err != nil {
		if errors.Is(err, dynrepo.ErrInvalidCursor) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cursor không hợp lệ", "code": "INVALID_CURSOR"})
			return
		}
		h.logger.Error("handlers.AdminJobsHandler.List", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không đọc được danh sách việc nền", "code": "JOBS_QUERY_FAILED"})
		return
	}
	if rows == nil {
		rows = []jobs.TenantJob{} // never null: the FE maps over it
	}
	var nextCursor *string
	if next != "" {
		nextCursor = &next
	}
	c.JSON(http.StatusOK, gin.H{
		"data":       rows,
		"pagination": gin.H{"limit": jobs.ClampListLimit(f.Limit), "nextCursor": nextCursor},
	})
}

// Retry handles POST /admin/jobs/:id/retry — "Gửi lại" one failed job of the caller's tenant: the
// SAME row goes back to the queue (a webhook keeps its X-Nexus-Delivery-Id). 200 {data:{job}} with
// the requeued row; 404 JOB_NOT_FOUND (not this tenant's, unknown, or a system job — one answer),
// 409 JOB_NOT_RETRYABLE (not failed, or already requeued), 503 JOBS_DISABLED (JOBS_ENABLED=false).
// The service checks the tenant itself: the list's filter cannot protect a path that takes any id.
func (h *AdminJobsHandler) Retry(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	actorID := actorOf(c)
	job, err := h.svc.Retry(c.Request.Context(), tenantID, actorID, c.Param("id"))
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			status := statusForCode(ve.Code)
			if ve.Code == "JOBS_DISABLED" {
				status = http.StatusServiceUnavailable
			}
			c.JSON(status, gin.H{"error": ve.Msg, "code": ve.Code})
			return
		}
		h.logger.Error("handlers.AdminJobsHandler.Retry", zap.String("tenant_id", tenantID),
			zap.String("user_id", actorID), zap.String("job_id", c.Param("id")), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không gửi lại được việc nền", "code": "JOBS_RETRY_FAILED"})
		return
	}
	h.logger.Info("background job resent by tenant admin", zap.String("tenant_id", tenantID),
		zap.String("user_id", actorID), zap.String("job_id", job.ID), zap.String("job_type", job.Type))
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"job": job}})
}
