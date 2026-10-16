package handlers

// resolveCrossTenantTarget — the ONE body that decides whether a caller may aim an admin operation
// at another tenant, and which tenant that is.
//
// It was written for POST /admin/verticals/install?targetTenant= (MS-C) and lived as a method on
// that handler. Two more surfaces need exactly the same decision — the install DIFF, so an operator
// can ask "what does the customer actually have now?", and the ACL backfill, so a customer tenant
// whose staff can see nothing is repairable without a database session. Copying the check into each
// would put three copies of a security rule in the tree, and two copies of a rule are two things
// that drift; this project has spent the whole review fixing exactly that shape.
//
// ORDER IS LOAD-BEARING and preserved verbatim:
//  1. OPERATOR CHECK FIRST, against the caller's LIVE user row (not the token's claims) — so a
//     tenant admin cannot probe which slugs exist, and a since-deleted caller holding a live token
//     gets the uniform 403 rather than a 500.
//  2. SLUG RESOLUTION SECOND — unknown, soft-deleted and inactive tenants all collapse into ONE
//     404, deliberately indistinguishable.
//
// Unwired dependencies fail CLOSED at the 403 (honest-disabled). The response has already been
// written whenever ok is false.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
)

// tenantFreezeChecker is the consumer-side view of services.TenantFreeze (specs/068 slice B).
type tenantFreezeChecker interface {
	FrozenUntil(ctx context.Context, tenantID string) (time.Time, bool, error)
}

// ORDER, continued (specs/068): 3. FREEZE LAST — a write (method other than GET/HEAD) aimed at a tenant
// that is write-frozen answers 503 TENANT_MAINTENANCE. It comes after the operator check and the slug
// resolution so the freeze state never reaches a caller who may not even know the tenant exists. The
// HTTP middleware cannot do this: it reads the tenant from the TOKEN (the factory), not from
// ?targetTenant=. frozen == nil skips the check (unwired / older tests).
func resolveCrossTenantTarget(
	c *gin.Context,
	logger *zap.Logger,
	opGate operatorGate,
	tenants tenantSlugResolver,
	frozen tenantFreezeChecker,
	op, callerTenantID, targetSlug string,
) (string, bool) {
	// Byte-identical to RequireAdmin/RequirePlatformOperator (anti-enumeration: a tenant admin
	// cannot tell "route exists but I lack the operator flag" apart from any admin 403).
	const forbiddenMsg = "Yêu cầu quyền quản trị viên"
	forbid := func() { c.JSON(http.StatusForbidden, gin.H{"error": forbiddenMsg, "code": "FORBIDDEN"}) }

	if opGate == nil || tenants == nil {
		forbid() // feature not wired → nobody passes (fail-closed, honest-disabled)
		return "", false
	}
	sub, ok := middleware.SubjectFromContext(c)
	if !ok {
		forbid()
		return "", false
	}
	// A builder workspace-session token (specs/062-customer-workspace) works in ONE customer; its
	// tenant is that customer, so the operator read below would be pinned to the wrong tenant and a
	// session that could hop onwards would turn one opened customer into every customer. Refused
	// before ANY repo is read, with the same body (anti-enumeration).
	if sub.WorkspaceSessionID != "" {
		logger.Warn(op+": workspace session token refused a cross-tenant target",
			zap.String("tenant_id", callerTenantID), zap.String("user_id", sub.UserID))
		forbid()
		return "", false
	}
	u, err := opGate.GetUserByID(c.Request.Context(), callerTenantID, sub.UserID)
	if errors.Is(err, auth.ErrUserNotFound) {
		forbid() // since-deleted caller with a live token: uniform 403, not a 500
		return "", false
	}
	if err != nil {
		logger.Error(op+": load caller failed (fail-closed)",
			zap.String("tenant_id", callerTenantID), zap.String("user_id", sub.UserID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi hệ thống", "code": "INTERNAL"})
		return "", false
	}
	if !u.IsAdmin || !u.IsPlatformOperator || u.Status != "active" {
		logger.Warn(op+": rejected non-operator",
			zap.String("tenant_id", callerTenantID), zap.String("user_id", sub.UserID))
		forbid()
		return "", false
	}

	t, err := tenants.ResolveTenantBySlug(c.Request.Context(), targetSlug)
	if errors.Is(err, auth.ErrTenantNotFound) {
		// Unknown OR inactive — ONE code, deliberately indistinguishable (spec §3.1).
		c.JSON(http.StatusNotFound, gin.H{"error": "Không tìm thấy workspace", "code": "TENANT_NOT_FOUND"})
		return "", false
	}
	if err != nil {
		logger.Error(op+": resolve slug failed",
			zap.String("tenant_id", callerTenantID), zap.String("target_slug", targetSlug), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi hệ thống", "code": "INTERNAL"})
		return "", false
	}
	if frozen != nil && c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		// A read error is logged by the checker, which keeps a known live freeze; here only the verdict.
		if until, isFrozen, _ := frozen.FrozenUntil(c.Request.Context(), t.ID); isFrozen {
			middleware.WriteTenantMaintenance(c, until)
			return "", false
		}
	}
	return t.ID, true
}
