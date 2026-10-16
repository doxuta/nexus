package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
)

// AdminPublishHandler exposes the draft -> publish lifecycle of a delivered customer workspace
// under /admin/publish (specs/063-draft-publish). Every route sits on builderGated (Auth → Tenant →
// RequireAdmin → operator tier) and the service re-checks the builder; a non-builder gets the SAME
// 403 body as every other /admin route.
type AdminPublishHandler struct {
	svc    *services.PublishService
	logger *zap.Logger
}

// NewAdminPublishHandler wires the handler over the publish service.
func NewAdminPublishHandler(svc *services.PublishService, logger *zap.Logger) *AdminPublishHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminPublishHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts the publish routes on the (builder-gated) group.
func (h *AdminPublishHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/publish/state", h.State)
	rg.GET("/admin/publish/changes", h.Changes)
	rg.GET("/admin/publish/versions", h.ListVersions)
	rg.GET("/admin/publish/versions/:id", h.GetVersion)
	rg.POST("/admin/publish/baseline", h.Baseline)
	rg.POST("/admin/publish", h.Publish)
	rg.DELETE("/admin/publish/changes/:id", h.DiscardOne)
	rg.DELETE("/admin/publish/changes", h.DiscardAll)
	rg.POST("/admin/publish/versions/:id/retry-schema", h.RetrySchema)
	rg.GET("/admin/publish/versions/:id/rollback-plan", h.RollbackPlan)
	rg.POST("/admin/publish/versions/:id/rollback", h.Rollback)
	rg.POST("/admin/publish/versions/:id/revert", h.Revert)
}

// Revert handles POST /admin/publish/versions/:id/revert: undo one applied-at-once entry (specs/063 US9).
func (h *AdminPublishHandler) Revert(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	res, err := h.svc.Revert(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")))
	if err != nil {
		h.fail(c, err, "Revert", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// RollbackPlan handles GET /admin/publish/versions/:id/rollback-plan.
func (h *AdminPublishHandler) RollbackPlan(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	plan, err := h.svc.RollbackPlan(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")))
	if err != nil {
		h.fail(c, err, "RollbackPlan", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": plan})
}

type rollbackRequest struct {
	ExpectedHeadVersion uint   `json:"expectedHeadVersion"`
	Note                string `json:"note"`
	AcceptUntouched     bool   `json:"acceptUntouched"`
	ExpectedPlanToken   string `json:"expectedPlanToken"`
}

// Rollback handles POST /admin/publish/versions/:id/rollback.
func (h *AdminPublishHandler) Rollback(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req rollbackRequest
	if !bindJSON(c, &req) {
		return
	}
	res, err := h.svc.Rollback(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")),
		services.RollbackRequest{ExpectedHeadVersion: req.ExpectedHeadVersion, Note: req.Note, AcceptUntouched: req.AcceptUntouched, ExpectedPlanToken: req.ExpectedPlanToken})
	if err != nil {
		h.fail(c, err, "Rollback", tenantID)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": res})
}

// RetrySchema handles POST /admin/publish/versions/:id/retry-schema: re-run the deferred field / table
// deletes of a version whose schema_status is "failed". A malformed id is the same 404 as an unknown
// one, and another tenant's version is indistinguishable from either (FR-081).
func (h *AdminPublishHandler) RetrySchema(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Không tìm thấy phiên bản này", "code": "VERSION_NOT_FOUND", "details": gin.H{}})
		return
	}
	res, err := h.svc.RetrySchema(c.Request.Context(), tenantID, id)
	if err != nil {
		h.fail(c, err, "RetrySchema", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

func (h *AdminPublishHandler) fail(c *gin.Context, err error, op, tenantID string) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		if ve.Code == "FORBIDDEN" { // byte-identical with middleware.RequireAdmin
			c.JSON(http.StatusForbidden, gin.H{"error": ve.Msg, "code": ve.Code})
			return
		}
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
		return
	}
	h.logger.Error("handlers.AdminPublish."+op, zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xử lý yêu cầu xuất bản", "code": "PUBLISH_OP_FAILED"})
}

// State handles GET /admin/publish/state.
func (h *AdminPublishHandler) State(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	st, err := h.svc.State(c.Request.Context(), tenantID)
	if err != nil {
		h.fail(c, err, "State", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": st})
}

// Changes handles GET /admin/publish/changes.
func (h *AdminPublishHandler) Changes(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	items, hash, err := h.svc.Changes(c.Request.Context(), tenantID)
	if err != nil {
		h.fail(c, err, "Changes", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "meta": gin.H{"changesetHash": hash}})
}

type publishBaselineRequest struct {
	Note string `json:"note"`
}

// Baseline handles POST /admin/publish/baseline ("Xuất bản lần đầu").
func (h *AdminPublishHandler) Baseline(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req publishBaselineRequest
	if c.Request.ContentLength != 0 && !bindJSON(c, &req) { // the body is optional
		return
	}
	res, err := h.svc.Baseline(c.Request.Context(), tenantID, req.Note)
	if err != nil {
		h.fail(c, err, "Baseline", tenantID)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": res})
}

type publishRequest struct {
	ChangesetHash string `json:"changesetHash"`
	Note          string `json:"note"`
	Force         bool   `json:"force"`
}

// Publish handles POST /admin/publish.
func (h *AdminPublishHandler) Publish(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req publishRequest
	if !bindJSON(c, &req) {
		return
	}
	res, err := h.svc.Publish(c.Request.Context(), tenantID, services.PublishRequest{ChangesetHash: req.ChangesetHash, Note: req.Note, Force: req.Force})
	if err != nil {
		h.fail(c, err, "Publish", tenantID)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": res})
}

// DiscardOne handles DELETE /admin/publish/changes/:id. An id that is not a UUID is the same 404 as
// an unknown one, and another tenant's id is indistinguishable from either.
func (h *AdminPublishHandler) DiscardOne(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Không tìm thấy thay đổi này trong bản nháp", "code": "DRAFT_NOT_FOUND", "details": gin.H{}})
		return
	}
	if err := h.svc.Discard(c.Request.Context(), tenantID, id); err != nil {
		h.fail(c, err, "DiscardOne", tenantID)
		return
	}
	c.Status(http.StatusNoContent)
}

// DiscardAll handles DELETE /admin/publish/changes?changesetHash=.
func (h *AdminPublishHandler) DiscardAll(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	if err := h.svc.DiscardAll(c.Request.Context(), tenantID, c.Query("changesetHash")); err != nil {
		h.fail(c, err, "DiscardAll", tenantID)
		return
	}
	c.Status(http.StatusNoContent)
}

// ListVersions handles GET /admin/publish/versions?limit&offset (newest first).
func (h *AdminPublishHandler) ListVersions(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit")) // junk -> 0 -> the service default
	offset, _ := strconv.Atoi(c.Query("offset"))
	items, total, err := h.svc.ListVersions(c.Request.Context(), tenantID, limit, offset)
	if err != nil {
		h.fail(c, err, "ListVersions", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "pagination": gin.H{"total": total, "limit": len(items), "offset": offset}})
}

// GetVersion handles GET /admin/publish/versions/:id.
func (h *AdminPublishHandler) GetVersion(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	v, err := h.svc.GetVersion(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")))
	if err != nil {
		h.fail(c, err, "GetVersion", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": v})
}
