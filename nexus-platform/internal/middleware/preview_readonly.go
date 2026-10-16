package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metaversion"
)

func isReadMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// previewExitPath is the ONE non-read route a preview token may call (relative to the API base):
// leaving the preview session.
const previewExitPath = "/preview-sessions/current"

// PreviewReadOnly is the single middleware that answers 403 PREVIEW_READ_ONLY
// (specs/064-preview-as-role-sample-data D5 / plan A5). It is mounted on the protected group right
// after RequireTenant — before RequireTenantWritable, DraftCaller, the data rate limiter and every
// route or sub-group (gin snapshots a group's chain at registration time) — and decides on the
// MATCHED route (c.FullPath()), relative to apiBase (the protected group's base path, "/api/v1"):
//
//   - not a preview token ⇒ the draft branch (specs/063-draft-publish S7-3, below), else Next;
//   - /auth/* or /assistant/* ⇒ 403 PREVIEW_FORBIDDEN, reads included (the account surface and
//     the LLM assistant are not part of "seeing the app as a role");
//   - GET / HEAD / OPTIONS ⇒ Next;
//   - DELETE /preview-sessions/current ⇒ Next (the only exception);
//   - anything else ⇒ 403 PREVIEW_READ_ONLY {method, path}, one zap.Warn naming the real builder.
//
// The draft branch (S7-3, FR-014): the BUILDER (metaversion.IsBuilder on the Subject the Auth stage
// rebuilt from the live user row — no DB read here, DraftCaller has not run yet) whose request carries
// X-Nexus-Draft: 1 is looking at the draft preview tab, which never writes for real: any
// non-GET/HEAD/OPTIONS call outside /auth/* is refused with the SAME 403 PREVIEW_READ_ONLY — /admin/*
// included (users, roles, key store…). Studio is unaffected: it only attaches the header to GETs.
// Everyone else (legacy customer admin, staff, API key) sending the header gets it ignored silently.
//
// Closed by default: a POST that is "really a read" (mark-notification-read, ⌘K) is refused too,
// and so is every write route added later until it is deliberately allowed here.
func PreviewReadOnly(apiBase string, logger *zap.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = zap.NewNop()
	}
	return func(c *gin.Context) {
		claims, ok := ClaimsFromContext(c)
		if !ok {
			c.Next()
			return
		}
		full := c.FullPath()
		rel := strings.TrimPrefix(full, apiBase)
		if claims.TokenType != auth.TokenTypePreview {
			if c.GetHeader(DraftHeader) == "1" && claims.TokenType != auth.TokenTypeAPIKey && metaversion.IsBuilder(claims.Subject) &&
				!isReadMethod(c.Request.Method) && !strings.HasPrefix(rel, "/auth/") {
				logger.Warn("middleware.PreviewReadOnly: write refused in draft preview",
					zap.String("tenant_id", claims.TenantID), zap.String("user_id", claims.UserID),
					zap.String("method", c.Request.Method), zap.String("path", full))
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
					"error":   "Đang xem bản nháp — chỉ đọc",
					"code":    "PREVIEW_READ_ONLY",
					"details": gin.H{"method": c.Request.Method, "path": full},
				})
				return
			}
			c.Next()
			return
		}
		if strings.HasPrefix(rel, "/auth/") || strings.HasPrefix(rel, "/assistant/") {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "Chức năng này không dùng được khi đang xem thử",
				"code":  "PREVIEW_FORBIDDEN",
			})
			return
		}
		if isReadMethod(c.Request.Method) {
			c.Next()
			return
		}
		switch c.Request.Method {
		case http.MethodDelete:
			if rel == previewExitPath {
				c.Next()
				return
			}
		}
		logger.Warn("middleware.PreviewReadOnly: write refused in preview session",
			zap.String("tenant_id", claims.TenantID), zap.String("user_id", claims.ActorUserID),
			zap.String("role_id", claims.RoleID), zap.String("method", c.Request.Method), zap.String("path", full))
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error":   "Đang xem thử — không ghi được",
			"code":    "PREVIEW_READ_ONLY",
			"details": gin.H{"method": c.Request.Method, "path": full},
		})
	}
}
