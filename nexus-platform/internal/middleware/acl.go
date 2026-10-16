package middleware

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// methodAction maps an HTTP method on the dynamic /:entity surface to the RBAC action the ACL
// stage authorizes (spec ../specs/001-auth-security/spec.md §A4: GET→view, POST→create, PUT→edit, DELETE→
// delete). A method with no entry has no authorizable action and is FAILED CLOSED (403) rather
// than silently allowed.
var methodAction = map[string]string{
	http.MethodGet:    "view",
	http.MethodPost:   "create",
	http.MethodPut:    "edit",
	http.MethodDelete: "delete",
}

// ACLChecker is the subset of the ACL service the middleware needs: deciding whether a
// (tenant, role) principal may perform an action on an entity AND, when allowed, the row-level
// access_level (none/owner/team/all) of the matching grant — which the A5 row filter consumes.
// Declared consumer-side (and exported so the route builder can reference it as
// middleware.ACLChecker) so the middleware is unit-testable with a fake and does not import the
// concrete services.ACLService (which would also create an import cycle). The real
// implementation (services.ACLService) default-denies (spec §138): an unknown role, no matching
// grant, or a cross-tenant role all yield (false, "", nil), and any repository error fails
// closed. A nil checker disables the ACL stage at the call site.
type ACLChecker interface {
	Authorize(ctx context.Context, tenantID, roleID, entity, action string) (allowed bool, accessLevel string, err error)
}

// ctxKeyAccessLevel / ctxKeyACLUserID are where RequireACL stashes the resolved row-level
// access_level and the verified user id for the dynamic record handler to read (so the A5 row
// filter can be built without a second ACL/role query). Read them via AccessLevelFromContext /
// ACLUserIDFromContext.
const (
	ctxKeyAccessLevel = "aclAccessLevel"
	ctxKeyACLUserID   = "aclUserID"
	// ctxKeyACLAction is the action the stashed access_level was resolved for (specs/051: the
	// manual-share clause is read-only, so the handler arms it only for "view").
	ctxKeyACLAction = "aclAction"
)

// RequireACL returns a gin handler enforcing the per-action RBAC stage (spec §4 "ACL trước
// logic", §A4 line 126) on the dynamic /:entity surface. It runs AFTER AuthMiddleware +
// RequireTenant (so the verified tenant/role/is_admin claims are already in context) and BEFORE
// any record handler.
//
// Decision order:
//   - D3 ADMIN BYPASS: a principal whose verified claim carries is_admin=true passes every
//     action on every entity WITHOUT consulting the checker (super-user).
//   - Otherwise map the request method → action (GET/POST/PUT/DELETE). An unmapped method has no
//     action to authorize → fail closed (403).
//   - Ask the checker whether (tenant_id, role_id) may perform (action) on :entity. The tenant
//     comes from the verified claim (D4: a forged tenant cannot ride another tenant's role —
//     enforced in the checker's tenant-pinned query). DEFAULT DENY: not-allowed or any checker
//     error → 403; the handler never runs.
//
// checker must be non-nil; logger may be nil (no-op). It is mounted ONLY on the non-admin /:entity
// group — never on /admin/* (which keeps its coarse RequireAdmin gate).
func RequireACL(checker ACLChecker, logger *zap.Logger) gin.HandlerFunc {
	// Delegate to the fixed-action core with a per-request method→action resolver so the two
	// entry points can never drift on the admin-bypass / authorize / accessLevel-stash logic.
	return requireACL(checker, logger, func(c *gin.Context) (string, bool) {
		action, mapped := methodAction[c.Request.Method]
		return action, mapped
	})
}

// RequireACLAction is RequireACL for an endpoint whose semantic permission does NOT match the
// default method→action map — it authorizes a FIXED action regardless of HTTP method. The only
// current use is POST /:entity/:id/restore (W9b), which must require the "delete" grant (restore
// is the inverse of delete), not "create" (the default POST mapping). Admin bypass, tenant-pinned
// authorization, and accessLevel/user-id stashing are IDENTICAL to RequireACL, so the record
// handler builds the A5 row scope for the restore exactly as it does for a delete.
//
// action must be a non-empty RBAC action (view/create/edit/delete); an empty action fails closed
// (403) for every request, so a mis-wired mount can never open access.
func RequireACLAction(checker ACLChecker, logger *zap.Logger, action string) gin.HandlerFunc {
	return requireACL(checker, logger, func(*gin.Context) (string, bool) {
		return action, action != ""
	})
}

// requireACL is the shared core behind RequireACL and RequireACLAction. actionFor resolves the
// RBAC action to authorize for the request (method-mapped for RequireACL, constant for
// RequireACLAction); a false second return means "no authorizable action" → fail closed (403).
func requireACL(checker ACLChecker, logger *zap.Logger, actionFor func(*gin.Context) (string, bool)) gin.HandlerFunc {
	if logger == nil {
		logger = zap.NewNop()
	}
	return func(c *gin.Context) {
		sub, ok := SubjectFromContext(c)
		if !ok {
			// No verified claims → the request bypassed AuthMiddleware (mounting bug) or carried
			// an unusable token. Fail closed with the same 403 shape used for an explicit deny.
			logger.Warn("middleware.RequireACL: no claims in context; refusing")
			abortForbidden(c)
			return
		}

		// D3: is_admin is the super-gate — skip ACL entirely for admins. Admins read every row,
		// so stash the "all" access level + the verified user id for the record handler's A5 scope.
		if sub.IsAdmin {
			c.Set(ctxKeyAccessLevel, "all")
			c.Set(ctxKeyACLUserID, sub.UserID)
			c.Next()
			return
		}

		action, mapped := actionFor(c)
		if !mapped {
			// No action to authorize (unmapped method, or empty fixed action) → deny.
			logger.Warn("middleware.RequireACL: no authorizable action; refusing",
				zap.String("method", c.Request.Method))
			abortForbidden(c)
			return
		}

		entity := c.Param("entity")
		allowed, accessLevel, err := checker.Authorize(c.Request.Context(), sub.TenantID, sub.RoleID, entity, action)
		if err != nil {
			// Fail closed: an infrastructure error must never open access.
			logger.Warn("middleware.RequireACL: decision error; refusing",
				zap.String("entity", entity), zap.String("action", action), zap.Error(err))
			abortForbidden(c)
			return
		}
		if !allowed {
			logger.Warn("middleware.RequireACL: access denied",
				zap.String("tenant_id", sub.TenantID), zap.String("role_id", sub.RoleID),
				zap.String("entity", entity), zap.String("action", action))
			abortForbidden(c)
			return
		}
		// Stash the resolved row-level access_level + the verified user id so the dynamic record
		// handler can build the A5 row scope without re-querying the role's grants.
		c.Set(ctxKeyAccessLevel, accessLevel)
		c.Set(ctxKeyACLUserID, sub.UserID)
		c.Set(ctxKeyACLAction, action)
		c.Next()
	}
}

// AccessLevelFromContext returns the row-level access_level RequireACL resolved for this request
// (none/owner/team/all). The second return is false when no ACL stage ran (e.g. a route mounted
// without an aclChecker); the caller must then fail closed rather than assume "all".
func AccessLevelFromContext(c *gin.Context) (string, bool) {
	v, exists := c.Get(ctxKeyAccessLevel)
	if !exists {
		return "", false
	}
	lvl, ok := v.(string)
	if !ok {
		return "", false
	}
	return lvl, true
}

// ACLActionFromContext returns the RBAC action the stashed access_level was resolved FOR
// (view/create/edit/delete — the method map, or the fixed action of RequireACLAction). The second
// return is false when no ACL stage ran or the admin bypass skipped it.
func ACLActionFromContext(c *gin.Context) (string, bool) {
	action, ok := c.Get(ctxKeyACLAction)
	if !ok {
		return "", false
	}
	s, ok := action.(string)
	return s, ok && s != ""
}

// ACLUserIDFromContext returns the verified user id RequireACL stashed for the A5 row scope.
// The second return is false when no ACL stage ran; callers fall back to SubjectFromContext.
func ACLUserIDFromContext(c *gin.Context) (string, bool) {
	v, exists := c.Get(ctxKeyACLUserID)
	if !exists {
		return "", false
	}
	uid, ok := v.(string)
	if !ok || uid == "" {
		return "", false
	}
	return uid, true
}

// abortForbidden writes the single structured 403 the ACL stage uses for EVERY refusal (no rule,
// explicit deny, decider error, or missing claims) so the response cannot be used to distinguish
// "you lack this grant" from "the check failed" (anti-probing).
func abortForbidden(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
		"error": "Bạn không có quyền thực hiện thao tác này",
		"code":  "FORBIDDEN",
	})
}
