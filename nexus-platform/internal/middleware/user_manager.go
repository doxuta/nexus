package middleware

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
)

// RequireUserManager gates /org-admin/* (specs/065-customer-owner-tier): the customer owner manages the
// accounts of their OWN tenant without being an admin. Chain: Auth → RequireTenant → … → this gate.
//
// The authority is the users.can_manage_users column read FRESH on every request through the same live
// reader RequirePlatformOperator uses (never a JWT claim, never the claim cache), so switching it off
// bites on the very next request. Every refusal is the byte-identical RequireAdmin 403 body.
//
// Refused BEFORE any repo read (explicitly, not "closed by construction"):
//   - no claims (misordered chain);
//   - an API key (AuthMiddlewareLive never re-reads a key's user row);
//   - a preview-as-role token (synthetic principal, specs/064);
//   - a builder workspace-session token (acts inside a customer the bearer does not own, specs/062).
//
// A user that is gone → 403; any other repo error → 500 (fail closed, never Next).
func RequireUserManager(repo platformOperatorRepo, logger *zap.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = zap.NewNop()
	}
	refuse := func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": forbiddenMsg, "code": forbiddenErr})
	}
	return func(c *gin.Context) {
		claims, ok := ClaimsFromContext(c)
		if !ok {
			refuse(c)
			return
		}
		sub := claims.Subject
		if claims.TokenType == auth.TokenTypeAPIKey || claims.TokenType == auth.TokenTypePreview || sub.WorkspaceSessionID != "" {
			logger.Warn("middleware.RequireUserManager: non-human or foreign token refused",
				zap.String("tenant_id", sub.TenantID), zap.String("user_id", sub.UserID), zap.String("token_type", string(claims.TokenType)))
			refuse(c)
			return
		}
		u, err := repo.GetUserByID(c.Request.Context(), sub.TenantID, sub.UserID)
		if errors.Is(err, auth.ErrUserNotFound) {
			refuse(c)
			return
		}
		if err != nil {
			logger.Error("middleware.RequireUserManager: load caller failed (fail-closed)",
				zap.String("tenant_id", sub.TenantID), zap.String("user_id", sub.UserID), zap.Error(err))
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Lỗi hệ thống", "code": "INTERNAL"})
			return
		}
		if u.Status != "active" || u.IsAdmin || !u.CanManageUsers {
			logger.Warn("middleware.RequireUserManager: rejected non-owner",
				zap.String("tenant_id", sub.TenantID), zap.String("user_id", sub.UserID))
			refuse(c)
			return
		}
		c.Next()
	}
}
