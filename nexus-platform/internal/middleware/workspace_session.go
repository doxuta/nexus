package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// workspaceSessionNotAllowedMsg is the one sentence every refusal of RefuseActingSession carries
// (the FE maps the code to "Về Nexus để làm việc này").
const workspaceSessionNotAllowedMsg = "Không thực hiện được khi đang dựng trong không gian của khách — hãy về Nexus để làm việc này"

// RefuseActingSession refuses a builder workspace-session token (specs/062-customer-workspace;
// Subject.WorkspaceSessionID set by the Auth stage's resolver) with 403 WORKSPACE_SESSION_NOT_ALLOWED.
// It wraps the self-management routes of the builder's HOME account — password, logout-all, the
// session list/kill, MFA enroll/activate/disable — and POST /admin/workspace-sessions (no nested
// open): inside a customer the Subject's tenant is the customer, so acting on "my account" would
// either hit no row or, worse, mutate the wrong tenant's data. Home tokens pass untouched.
func RefuseActingSession() gin.HandlerFunc {
	return func(c *gin.Context) {
		if sub, ok := SubjectFromContext(c); ok && sub.WorkspaceSessionID != "" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": workspaceSessionNotAllowedMsg,
				"code":  "WORKSPACE_SESSION_NOT_ALLOWED",
			})
			return
		}
		c.Next()
	}
}
