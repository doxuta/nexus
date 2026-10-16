package middleware

// tenant_freeze.go — specs/068-tenant-backup-restore slice B. RequireTenantWritable refuses the
// WRITE methods of a tenant that is under a maintenance freeze (503 TENANT_MAINTENANCE + Retry-After)
// while reads keep flowing. The decision is on the MATCHED route (c.FullPath()), against a CLOSED
// list of exact (method, route) pairs — never a prefix (plan D-12): a prefix would let any future
// route under /auth/ or /admin/ slip through the gate (fail-open).

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// TenantFreezeChecker is the consumer-side view of services.TenantFreeze.
type TenantFreezeChecker interface {
	FrozenUntil(ctx context.Context, tenantID string) (time.Time, bool, error)
}

type freezeKey struct{ method, path string }

// freezeExempt: routes that only write the caller's own session/credential state or revoke access.
// A restore is usually the reaction to an incident, so blocking revocation mid-freeze would be wrong.
// Paths are relative to the API base. A route that does not exist any more makes
// TestRouterFreeze_ExemptRoutesAreRegistered red (renames cannot silently empty the list).
var freezeExempt = map[freezeKey]bool{
	{http.MethodPut, "/auth/me/password"}:                           true,
	{http.MethodPost, "/auth/logout-all"}:                           true,
	{http.MethodDelete, "/auth/sessions/:id"}:                       true,
	{http.MethodPost, "/auth/mfa/enroll"}:                           true,
	{http.MethodPost, "/auth/mfa/activate"}:                         true,
	{http.MethodPost, "/auth/mfa/disable"}:                          true,
	{http.MethodPost, "/assistant/ask"}:                             true,
	{http.MethodDelete, "/admin/api-keys/:id"}:                      true,
	{http.MethodDelete, "/admin/users/:id/sessions/:sessionId"}:     true,
	{http.MethodPut, "/admin/users/:id"}:                            true,
	{http.MethodPut, "/org-admin/users/:id"}:                        true, // the owner's lock-a-leaver (specs/065), same as the admin one above
	{http.MethodDelete, "/org-admin/users/:id/sessions/:sessionId"}: true, // owner ends a leaver's session (specs/065 lát C); create + invite stay frozen
	{http.MethodPost, "/admin/inbound-hooks/:id/rotate"}:            true,
	{http.MethodDelete, "/admin/credentials/:id"}:                   true,
	{http.MethodPatch, "/admin/sso/providers/:id"}:                  true,
	{http.MethodDelete, "/admin/workspace-sessions/current"}:        true,
	{http.MethodDelete, "/preview-sessions/current"}:                true,
}

// FreezeExemptRoutes returns the exempt (method, relative route) pairs, for the router test that
// pins them against engine.Routes().
func FreezeExemptRoutes() [][2]string {
	out := make([][2]string, 0, len(freezeExempt))
	for k := range freezeExempt {
		out = append(out, [2]string{k.method, k.path})
	}
	return out
}

// WriteTenantMaintenance writes the ONE 503 body every freeze gate uses.
func WriteTenantMaintenance(c *gin.Context, until time.Time) {
	secs := int(time.Until(until).Seconds()) + 1
	if secs < 1 {
		secs = 1
	}
	c.Header("Retry-After", strconv.Itoa(secs))
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
		"error":   "Dữ liệu đang được bảo trì/khôi phục — hiện chỉ xem được, thử lại sau ít phút",
		"code":    "TENANT_MAINTENANCE",
		"details": gin.H{"until": until.UTC().Format(time.RFC3339)},
	})
}

// RequireTenantWritable must be mounted on the protected group directly after PreviewReadOnly and
// BEFORE every route and sub-group (gin snapshots a group's handlers at registration time — a later
// Use() silently leaves the sub-groups ungated). apiBase is the group's base path ("/api/v1").
func RequireTenantWritable(chk TenantFreezeChecker, apiBase string, logger *zap.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = zap.NewNop()
	}
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			c.Next()
			return
		}
		if chk == nil || freezeExempt[freezeKey{c.Request.Method, strings.TrimPrefix(c.FullPath(), apiBase)}] {
			c.Next()
			return
		}
		tenantID, ok := TenantFromContext(c)
		if !ok {
			c.Next() // RequireTenant (earlier in the chain) already refuses tenant-less requests
			return
		}
		until, frozen, err := chk.FrozenUntil(c.Request.Context(), tenantID)
		if err != nil {
			// The checker already decided (a known live freeze stays frozen); here we only log.
			logger.Warn("middleware.RequireTenantWritable: freeze read failed",
				zap.String("tenant_id", tenantID), zap.Bool("frozen", frozen), zap.Error(err))
		}
		if frozen {
			WriteTenantMaintenance(c, until)
			return
		}
		c.Next()
	}
}
