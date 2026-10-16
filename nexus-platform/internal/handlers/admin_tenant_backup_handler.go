package handlers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/middleware"
	"nexus/platform/internal/tenantbackup"
)

// tenantBackupService is the consumer-side slice of *tenantbackup.Service the handler needs.
type tenantBackupService interface {
	Create(ctx context.Context, in tenantbackup.CreateInput) (*tenantbackup.BackupView, error)
	List(ctx context.Context, in tenantbackup.ListInput) ([]tenantbackup.BackupView, tenantbackup.ListMeta, error)
	Get(ctx context.Context, tenantID, callerTenantID, id string) (*tenantbackup.BackupView, error)
	OpenFile(ctx context.Context, tenantID string, actor tenantbackup.Actor, id string) (*tenantbackup.FileDownload, error)
	SetMaintenance(ctx context.Context, in tenantbackup.MaintenanceInput) (*tenantbackup.MaintenanceView, error)
	ClearMaintenance(ctx context.Context, in tenantbackup.MaintenanceInput) error
	Preview(ctx context.Context, in tenantbackup.PreviewInput) (*tenantbackup.RestorePlan, error)
	StartRestore(ctx context.Context, in tenantbackup.RestoreInput) (*tenantbackup.RestoreStarted, error)
	Delete(ctx context.Context, tenantID string, actor tenantbackup.Actor, id string) error
	GetSchedule(ctx context.Context, tenantID string) (*tenantbackup.ScheduleView, error)
	PutSchedule(ctx context.Context, tenantID string, actor tenantbackup.Actor, enabled *bool, keep *int) (*tenantbackup.ScheduleView, error)
}

// AdminTenantBackupHandler serves the per-customer backup routes (specs/068-tenant-backup-restore):
//
//	GET  /admin/tenants/:id/backups                    list (newest first, keyset page)
//	POST /admin/tenants/:id/backups                    "Sao lưu ngay" — 202, runs in the background
//	GET  /admin/tenants/:id/backups/:backupId          one backup (the poll target)
//	GET  /admin/tenants/:id/backups/:backupId/file     download the zip
//	POST   /admin/tenants/:id/maintenance              {minutes 1..120} — freeze the customer's writes
//	DELETE /admin/tenants/:id/maintenance              lift the freeze
//	POST /admin/tenants/:id/restores                   "Kiểm tra khôi phục" — read-only plan (dryRun omitted = true)
//	DELETE /admin/tenants/:id/backups/:backupId        delete one backup by hand (204; in use = 409 BACKUP_DELETE_IN_USE)
//	GET  /admin/tenants/:id/backup-schedule            the customer's automatic-backup setting
//	PUT  /admin/tenants/:id/backup-schedule            {enabled, keepScheduled 1..30} (else 422 BACKUP_SCHEDULE_INVALID)
//
// Every route sits behind Auth → Tenant → RequireAdmin → RequirePlatformOperator: the platform
// operator of the workshop and nobody else. A customer's own admin, a customer's staff and a
// builder workspace-session token all get the byte-identical 403 of the other /admin/tenants routes.
type AdminTenantBackupHandler struct {
	svc              tenantBackupService
	gate             operatorGate
	downloadDeadline time.Duration
	logger           *zap.Logger
}

// NewAdminTenantBackupHandler wires the handler. downloadDeadline lifts the server-wide write
// timeout for the file route only (a multi-GB zip outlives 30 s). logger may be nil.
func NewAdminTenantBackupHandler(svc tenantBackupService, gate operatorGate, downloadDeadline time.Duration, logger *zap.Logger) *AdminTenantBackupHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminTenantBackupHandler{svc: svc, gate: gate, downloadDeadline: downloadDeadline, logger: logger}
}

// RegisterRoutes mounts the routes on their OWN operator-gated subgroup of the admin group (same
// shape as AdminTenantHandler.RegisterRoutes), so the live-row operator check applies here only.
func (h *AdminTenantBackupHandler) RegisterRoutes(adminGated *gin.RouterGroup) {
	og := adminGated.Group("")
	og.Use(middleware.RequirePlatformOperator(h.gate, h.logger))
	og.GET("/admin/tenants/:id/backups", h.List)
	og.POST("/admin/tenants/:id/backups", h.Create)
	og.GET("/admin/tenants/:id/backups/:backupId", h.Get)
	og.GET("/admin/tenants/:id/backups/:backupId/file", h.Download)
	og.POST("/admin/tenants/:id/maintenance", h.SetMaintenance)
	og.DELETE("/admin/tenants/:id/maintenance", h.ClearMaintenance)
	// "restores" (plural, under /tenants/:id) is not the recycle bin's /:entity/:id/restore.
	og.POST("/admin/tenants/:id/restores", h.Restore)
	og.DELETE("/admin/tenants/:id/backups/:backupId", h.Delete)
	og.GET("/admin/tenants/:id/backup-schedule", h.GetSchedule)
	og.PUT("/admin/tenants/:id/backup-schedule", h.PutSchedule)
}

// Delete handles DELETE /admin/tenants/:id/backups/:backupId — 204.
func (h *AdminTenantBackupHandler) Delete(c *gin.Context) {
	actor, _ := h.actor(c)
	if err := h.svc.Delete(c.Request.Context(), c.Param("id"), actor, c.Param("backupId")); err != nil {
		h.fail(c, "Delete", err)
		return
	}
	c.Status(http.StatusNoContent)
}

// GetSchedule handles GET /admin/tenants/:id/backup-schedule.
func (h *AdminTenantBackupHandler) GetSchedule(c *gin.Context) {
	v, err := h.svc.GetSchedule(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.fail(c, "GetSchedule", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": v})
}

type putScheduleRequest struct {
	Enabled       *bool `json:"enabled"`
	KeepScheduled *int  `json:"keepScheduled"`
}

// PutSchedule handles PUT /admin/tenants/:id/backup-schedule — a missing or out-of-range field is
// the service's 422 BACKUP_SCHEDULE_INVALID; only a body that is not JSON of this shape is a 400.
func (h *AdminTenantBackupHandler) PutSchedule(c *gin.Context) {
	actor, _ := h.actor(c)
	var req putScheduleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Body không hợp lệ", "code": "INVALID_BODY"})
		return
	}
	v, err := h.svc.PutSchedule(c.Request.Context(), c.Param("id"), actor, req.Enabled, req.KeepScheduled)
	if err != nil {
		h.fail(c, "PutSchedule", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": v})
}

func (h *AdminTenantBackupHandler) actor(c *gin.Context) (tenantbackup.Actor, string) {
	operatorTenant, _ := tenantOf(c)
	sub, _ := middleware.SubjectFromContext(c)
	return tenantbackup.Actor{TenantID: operatorTenant, UserID: sub.UserID}, operatorTenant
}

// fail writes a typed refusal with the status its code maps to, or a 500 for anything else.
func (h *AdminTenantBackupHandler) fail(c *gin.Context, op string, err error) {
	var be *tenantbackup.Error
	if errors.As(err, &be) {
		c.JSON(statusForCode(be.Code), gin.H{"error": be.Msg, "code": be.Code, "details": be.Details})
		return
	}
	operatorTenant, _ := tenantOf(c)
	h.logger.Error("handlers.AdminTenantBackup."+op,
		zap.String("operator_tenant_id", operatorTenant), zap.String("target_tenant_id", c.Param("id")), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xử lý yêu cầu sao lưu", "code": "BACKUP_FAILED"})
}

// List handles GET /admin/tenants/:id/backups?limit=&after=.
func (h *AdminTenantBackupHandler) List(c *gin.Context) {
	_, operatorTenant := h.actor(c)
	limit := 0
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit không hợp lệ", "code": "INVALID_QUERY"})
			return
		}
		limit = n
	}
	data, meta, err := h.svc.List(c.Request.Context(), tenantbackup.ListInput{
		TenantID: c.Param("id"), CallerTenantID: operatorTenant, Limit: limit, After: c.Query("after"),
	})
	if err != nil {
		h.fail(c, "List", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": meta})
}

type createBackupRequest struct {
	Note string `json:"note"`
}

// Create handles POST /admin/tenants/:id/backups — 202: the zip is written in the background; the
// body is the new row in status "running". The body is optional ({} or empty).
func (h *AdminTenantBackupHandler) Create(c *gin.Context) {
	actor, _ := h.actor(c)
	var req createBackupRequest
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Body không hợp lệ", "code": "INVALID_BODY"})
		return
	}
	v, err := h.svc.Create(c.Request.Context(), tenantbackup.CreateInput{TenantID: c.Param("id"), Note: req.Note, Actor: actor})
	if err != nil {
		h.fail(c, "Create", err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"data": v})
}

type setMaintenanceRequest struct {
	Minutes *int `json:"minutes"`
}

// SetMaintenance handles POST /admin/tenants/:id/maintenance — 200 {data:{maintenanceUntil}}. A
// missing or non-integer minutes is a 400 here; the 1..120 range and the other refusals are the
// service's.
func (h *AdminTenantBackupHandler) SetMaintenance(c *gin.Context) {
	actor, _ := h.actor(c)
	var req setMaintenanceRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Minutes == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cần số phút khoá ghi (số nguyên)", "code": "INVALID_BODY"})
		return
	}
	v, err := h.svc.SetMaintenance(c.Request.Context(), tenantbackup.MaintenanceInput{TenantID: c.Param("id"), Minutes: *req.Minutes, Actor: actor})
	if err != nil {
		h.fail(c, "SetMaintenance", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": v})
}

// ClearMaintenance handles DELETE /admin/tenants/:id/maintenance — 204, idempotent.
func (h *AdminTenantBackupHandler) ClearMaintenance(c *gin.Context) {
	actor, _ := h.actor(c)
	if err := h.svc.ClearMaintenance(c.Request.Context(), tenantbackup.MaintenanceInput{TenantID: c.Param("id"), Actor: actor}); err != nil {
		h.fail(c, "ClearMaintenance", err)
		return
	}
	c.Status(http.StatusNoContent)
}

// Get handles GET /admin/tenants/:id/backups/:backupId.
func (h *AdminTenantBackupHandler) Get(c *gin.Context) {
	_, operatorTenant := h.actor(c)
	v, err := h.svc.Get(c.Request.Context(), c.Param("id"), operatorTenant, c.Param("backupId"))
	if err != nil {
		h.fail(c, "Get", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": v})
}

// Download handles GET /admin/tenants/:id/backups/:backupId/file. The service audits the download
// before this streams a byte. Headers keep a customer's data out of every cache and stop the
// browser guessing a type; the write deadline is lifted for this response only.
func (h *AdminTenantBackupHandler) Download(c *gin.Context) {
	actor, _ := h.actor(c)
	dl, err := h.svc.OpenFile(c.Request.Context(), c.Param("id"), actor, c.Param("backupId"))
	if err != nil {
		h.fail(c, "Download", err)
		return
	}
	defer dl.Body.Close()

	if h.downloadDeadline > 0 {
		// A writer that cannot take a deadline (httptest's recorder) answers ErrNotSupported: expected.
		if derr := http.NewResponseController(c.Writer).SetWriteDeadline(time.Now().Add(h.downloadDeadline)); derr != nil && !errors.Is(derr, http.ErrNotSupported) {
			h.logger.Warn("handlers.AdminTenantBackup.Download: set write deadline", zap.Error(derr))
		}
	}
	hdr := c.Writer.Header()
	hdr.Set("Content-Type", "application/zip")
	hdr.Set("Content-Disposition", `attachment; filename="`+dl.Name+`"`)
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	if dl.Size >= 0 {
		hdr.Set("Content-Length", strconv.FormatInt(dl.Size, 10))
	}
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, dl.Body); err != nil {
		// Headers are gone; the only honest thing left is to log. The client sees a short body.
		h.logger.Warn("handlers.AdminTenantBackup.Download: stream interrupted",
			zap.String("target_tenant_id", c.Param("id")), zap.String("backup_id", c.Param("backupId")), zap.Error(err))
	}
}
