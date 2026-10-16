package middleware

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
)

// platformOperatorRepo reads a caller's LIVE authority row for RequirePlatformOperator. Satisfied
// by *auth.UserRepository (GetUserByID is tenant-pinned). Consumer-side (mirrors tokenVerifier) so
// the gate unit-tests against a fake with no DB.
type platformOperatorRepo interface {
	GetUserByID(ctx context.Context, tenantID, id string) (*auth.User, error)
}

// The refusal body shared by RequireAdmin-tier gates (RequirePlatformOperator, RequireUserManager):
// byte-identical so a caller cannot tell which gate (or which missing flag) refused them (anti-enumeration).
const (
	forbiddenMsg = "Yêu cầu quyền quản trị viên"
	forbiddenErr = "FORBIDDEN"
)

// RequirePlatformOperator gates the operator-only tenant-provisioning routes (POST/GET
// /admin/tenants). It stands AFTER RequireAdmin in the chain
// (Auth → RequireTenant → RequireAdmin → RequirePlatformOperator) and reads the caller's user row
// FRESH each request — the authority is a DB column, never a JWT claim, so revoking it takes effect
// on the very next request with no stale-token window (../specs/024-tenant-provisioning/spec.md §2.3).
//
// The caller must be an ACTIVE admin whose is_platform_operator is set. Anything else refuses with
// a 403 body BYTE-IDENTICAL to RequireAdmin's, so a tenant-admin cannot distinguish "this route
// exists but I lack the flag" from any other admin route they cannot reach (anti-enumeration,
// spec §2.4). A repo error fails CLOSED (500, never Next) — an operator gate must never fail open.
//
// API keys can never pass: their Claims floor IsAdmin:false, so they are refused at RequireAdmin
// upstream — provisioning is human-only by construction.
func RequirePlatformOperator(repo platformOperatorRepo, logger *zap.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = zap.NewNop()
	}
	return func(c *gin.Context) {
		sub, ok := SubjectFromContext(c)
		if !ok {
			// Unreachable after Auth+RequireTenant, but fail closed if the chain is ever misordered.
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": forbiddenMsg, "code": forbiddenErr})
			return
		}
		if sub.WorkspaceSessionID != "" {
			// A builder workspace-session token (specs/062-customer-workspace) acts INSIDE one
			// customer: sub.TenantID is that customer, so the live-row read below would ask about
			// the wrong tenant, and these routes (/admin/tenants*, /admin/projects*) are exactly the
			// ones that must never be reachable from within a customer (self-suspend, provisioning).
			// Refused before any repo read, byte-identical to every other 403 of this gate.
			logger.Warn("middleware.RequirePlatformOperator: workspace session token refused",
				zap.String("tenant_id", sub.TenantID), zap.String("user_id", sub.UserID))
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": forbiddenMsg, "code": forbiddenErr})
			return
		}
		u, err := repo.GetUserByID(c.Request.Context(), sub.TenantID, sub.UserID)
		if errors.Is(err, auth.ErrUserNotFound) {
			// A since-deleted operator whose access token is still live: refuse like any other
			// non-operator (byte-identical 403), NOT a 500 — this is an expected sentinel, not a
			// repo failure, and keeps anti-enumeration uniform.
			logger.Warn("middleware.RequirePlatformOperator: caller no longer exists",
				zap.String("tenant_id", sub.TenantID), zap.String("user_id", sub.UserID))
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": forbiddenMsg, "code": forbiddenErr})
			return
		}
		if err != nil {
			logger.Error("middleware.RequirePlatformOperator: load caller failed (fail-closed)",
				zap.String("tenant_id", sub.TenantID), zap.String("user_id", sub.UserID), zap.Error(err))
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Lỗi hệ thống", "code": "INTERNAL"})
			return
		}
		if !u.IsAdmin || !u.IsPlatformOperator || u.Status != "active" {
			logger.Warn("middleware.RequirePlatformOperator: rejected non-operator",
				zap.String("tenant_id", sub.TenantID), zap.String("user_id", sub.UserID))
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": forbiddenMsg, "code": forbiddenErr})
			return
		}
		c.Next()
	}
}

// RequireBuilderTier gates the builder surfaces of /admin/* (builderGated, plus the purge group): the
// factory tier of EASE Nước 6. A home token behaves EXACTLY like RequirePlatformOperator (live DB
// read of the caller's flag). A builder workspace-session token (specs/062-customer-workspace) is
// let through when the Auth stage's resolver — in THIS very request, uncached — confirmed the
// builder and set Subject.IsPlatformOperator; RequirePlatformOperator itself refuses that token,
// because it reads the user row by sub.TenantID, which in a session is the customer. A token with
// a wsid but no confirmed flag is refused byte-identical (fail closed).
func RequireBuilderTier(repo platformOperatorRepo, logger *zap.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = zap.NewNop()
	}
	operator := RequirePlatformOperator(repo, logger)
	return func(c *gin.Context) {
		sub, ok := SubjectFromContext(c)
		if ok && sub.WorkspaceSessionID != "" {
			if sub.IsPlatformOperator {
				c.Next()
				return
			}
			logger.Warn("middleware.RequireBuilderTier: workspace session without a confirmed builder flag",
				zap.String("tenant_id", sub.TenantID), zap.String("user_id", sub.UserID))
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Yêu cầu quyền quản trị viên", "code": "FORBIDDEN"})
			return
		}
		operator(c)
	}
}
