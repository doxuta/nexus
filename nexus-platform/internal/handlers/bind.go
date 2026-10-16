package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/middleware"
)

// bindJSON is a drop-in replacement for c.ShouldBindJSON that additionally detects when
// the bind failed because the request body exceeded the MaxBodySize limit (RM-16). When
// that is the case it writes 413 REQUEST_TOO_LARGE instead of the generic 400 INVALID_BODY,
// so clients can distinguish "body too big" from "body malformed".
//
// Usage — replace every pattern:
//
//	if err := c.ShouldBindJSON(&req); err != nil {
//	    c.JSON(http.StatusBadRequest, gin.H{"error": "...", "code": "INVALID_BODY"})
//	    return
//	}
//
// with:
//
//	if !bindJSON(c, &req) {
//	    return
//	}
//
// The response has already been written when false is returned; the caller must only return.
func bindJSON(c *gin.Context, v any) bool {
	if err := c.ShouldBindJSON(v); err != nil {
		if middleware.IsMaxBytesError(err) {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
				"error":   "Request body exceeds the maximum allowed size",
				"code":    "REQUEST_TOO_LARGE",
				"details": gin.H{},
			})
			return false
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "Body JSON không hợp lệ", "code": "INVALID_BODY"})
		return false
	}
	return true
}

// auditContext returns the request context with the verified actor user id attached
// (audit.WithActor), so admin service mutations (entity/field/role) can attribute the RM-19
// audit row to the caller without threading a userID parameter through every method. The
// actor is the verified JWT subject placed in the gin context by AuthMiddleware; when no
// subject is present (a mis-wired route — every admin route sits behind the auth chain) the
// actor is "" and the audit records an unknown actor rather than panicking.
func auditContext(c *gin.Context) context.Context {
	sub, _ := middleware.SubjectFromContext(c)
	return audit.WithActor(c.Request.Context(), sub.UserID)
}

// actorOf returns the verified actor user id (the JWT subject placed in the gin context by
// AuthMiddleware), or "" when none is present (a mis-wired route). Used by admin handlers that
// pass the actor id explicitly into a service input (e.g. workflow created_by/updated_by) rather
// than threading it through the request context.
func actorOf(c *gin.Context) string {
	sub, _ := middleware.SubjectFromContext(c)
	return sub.UserID
}

// bindJSONStrict is bindJSON that refuses a body naming a field the request struct does not have
// (400 INVALID_BODY naming it), for routes whose contract is "reject, never silently drop".
func bindJSONStrict(c *gin.Context, v any) bool {
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if middleware.IsMaxBytesError(err) {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
				"error":   "Request body exceeds the maximum allowed size",
				"code":    "REQUEST_TOO_LARGE",
				"details": gin.H{},
			})
			return false
		}
		msg := "Body JSON không hợp lệ"
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			msg += " (" + err.Error() + ")"
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": msg, "code": "INVALID_BODY"})
		return false
	}
	return true
}

// versionWarningOf returns the notice of a direct version entry that failed to write on this request
// (specs/063 FR-033), "" when there is none.
func versionWarningOf(c *gin.Context) string {
	if w := metaversion.Warnings(c.Request.Context()); len(w) > 0 {
		return w[0]
	}
	return ""
}

// jsonWV is c.JSON for a successful config write: the change IS applied, and when its direct version
// entry could not be recorded the body also carries versionWarning (the status stays 2xx).
func jsonWV(c *gin.Context, status int, body gin.H) {
	if w := versionWarningOf(c); w != "" {
		body["versionWarning"] = w
	}
	c.JSON(status, body)
}

// noContentWV answers a successful DELETE: 204, or 200 {versionWarning} when the version entry failed.
func noContentWV(c *gin.Context) {
	if w := versionWarningOf(c); w != "" {
		c.JSON(http.StatusOK, gin.H{"versionWarning": w})
		return
	}
	c.Status(http.StatusNoContent)
}
