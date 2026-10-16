package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/doctor"
	"nexus/platform/internal/services"
)

// DoctorRunner is the consumer-side slice of *services.DoctorService the handler needs —
// declared here (like TokenVerifier) so the handler stays wiring-thin and testable with a
// three-method fake.
type DoctorRunner interface {
	Scan(ctx context.Context, tenantID string) ([]doctor.CheckResult, error)
	Fix(ctx context.Context, tenantID string, checkIDs []string, confirm bool) ([]doctor.FixResult, error)
	// FlushCaches clears the wired in-proc caches and returns the names actually flushed.
	FlushCaches(ctx context.Context, tenantID string) []string
}

// AdminDoctorHandler exposes the Nexus Doctor endpoints (../specs/008-nexus-doctor/spec.md §2.3):
// POST /admin/doctor/scan (read-only, absolutely), POST /admin/doctor/fix and
// POST /admin/doctor/flush-caches. All three sit behind Auth → Tenant → RequireAdmin →
// RequirePlatformOperator (builderGated — operator-only since 2026-07-25); still ACL-based,
// never a special-cased identity (the Nexus `$current_user->id != '1'` anti-lesson).
type AdminDoctorHandler struct {
	svc    DoctorRunner
	logger *zap.Logger
	// now is the handler-owned clock stamping scannedAt (injectable in tests).
	now func() time.Time
}

// NewAdminDoctorHandler wires the handler over the doctor service.
func NewAdminDoctorHandler(svc DoctorRunner, logger *zap.Logger) *AdminDoctorHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminDoctorHandler{svc: svc, logger: logger, now: time.Now}
}

// RegisterRoutes mounts the doctor endpoints under the given (admin-gated) group. All are
// POST: scan is read-only but stays POST (it is an on-demand action with server load, not a
// cacheable resource read — mirroring the workflow dry-run precedent).
func (h *AdminDoctorHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/admin/doctor/scan", h.Scan)
	rg.POST("/admin/doctor/fix", h.Fix)
	rg.POST("/admin/doctor/flush-caches", h.FlushCaches)
}

// doctorFlushNote is the honest replica caveat shipped with every flush response: the three
// caches are in-process, so on a multi-replica deployment only the instance that served this
// request flushes — the others converge within their TTL backstops. We have no replica
// detection, so the note is STATIC (always shown) rather than pretending to know.
const doctorFlushNote = "in-proc cache — chỉ instance này; replica khác chờ TTL"

// doctorFixRequest is the POST /admin/doctor/fix body.
type doctorFixRequest struct {
	CheckIDs []string `json:"checkIds"`
	Confirm  bool     `json:"confirm"`
}

// Scan handles POST /admin/doctor/scan → 200 {data:{findings:[...], scannedAt}}.
func (h *AdminDoctorHandler) Scan(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	findings, err := h.svc.Scan(c.Request.Context(), tenantID)
	if err != nil {
		h.logger.Error("handlers.AdminDoctor.Scan", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể quét hệ thống", "code": "DOCTOR_SCAN_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"findings":  findings,
		"scannedAt": h.now().UTC().Format(time.RFC3339),
	}})
}

// Fix handles POST /admin/doctor/fix → 200 {data:{results:[...]}}. Unknown checkId → 422
// DOCTOR_CHECK_UNKNOWN; needs-confirm without confirm=true → 422 DOCTOR_CONFIRM_REQUIRED
// (both via the shared statusForCode default). The request context carries the verified
// actor (auditContext) so every repair's audit row is attributable.
func (h *AdminDoctorHandler) Fix(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req doctorFixRequest
	if !bindJSON(c, &req) {
		return
	}
	results, err := h.svc.Fix(auditContext(c), tenantID, req.CheckIDs, req.Confirm)
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
			return
		}
		h.logger.Error("handlers.AdminDoctor.Fix", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể chạy sửa lỗi", "code": "DOCTOR_FIX_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"results": results}})
}

// FlushCaches handles POST /admin/doctor/flush-caches → 200 {data:{flushed:[...], note}}.
// It clears the in-proc metadata/ACL/workflow caches and the #14 live user-row cache
// (DOCTOR-OPS) — the escape hatch for cache-vs-DB drift the DB checks cannot see. The audit
// context carries the verified actor.
func (h *AdminDoctorHandler) FlushCaches(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	flushed := h.svc.FlushCaches(auditContext(c), tenantID)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"flushed": flushed,
		"note":    doctorFlushNote,
	}})
}
