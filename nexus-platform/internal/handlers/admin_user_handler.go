package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// AdminUserHandler exposes the A3 user-management endpoints under /admin/users (CRUD + role
// assignment via role_id). It sits behind the AuthMiddleware → RequireTenant → RequireAdmin
// chain (mounted in router.go), so the tenant is always the verified claim and every caller is
// already an admin. The handler ADDITIONALLY passes the verified is_admin flag into the service
// for create/update/delete (defense-in-depth privilege-escalation guard): if those routes were
// ever mis-mounted off the admin gate, the service still refuses a non-admin with 403.
type AdminUserHandler struct {
	svc         *services.UserService
	resetIssuer resetLinkIssuer
	sessions    sessionManager
	logger      *zap.Logger
}

// sessionManager is the consumer-side slice of AuthService the admin "phiên đăng nhập" routes need
// (../specs/055-session-list-kill/spec.md US3): list one tenant user's live login sessions, and end
// one of them.
type sessionManager interface {
	ListSessions(ctx context.Context, tenantID, userID, currentSessionID string) ([]services.SessionView, error)
	EndSession(ctx context.Context, tenantID, userID, sessionID string) error
}

// WithSessionManager injects the login-session service (the production AuthService). Without it
// the /admin/users/:id/sessions routes are not mounted. Returns the handler for fluent wiring.
func (h *AdminUserHandler) WithSessionManager(m sessionManager) *AdminUserHandler {
	if m != nil {
		h.sessions = m
	}
	return h
}

// ListSessions handles GET /admin/users/:id/sessions: the live login sessions of one user of the
// admin's tenant (tenant from the verified claim, never the path). Unknown / other-tenant user →
// 404 USER_NOT_FOUND.
func (h *AdminUserHandler) ListSessions(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	userID := strings.TrimSpace(c.Param("id"))
	if !h.mayActOn(c, tenantID, userID, "ListSessions") {
		return
	}
	// "current" means the CALLER's session, so it can only mark a row when the admin is looking at
	// their own account.
	current := ""
	if sub, _ := middleware.SubjectFromContext(c); sub.UserID == userID {
		current = sub.SessionID
	}
	views, err := h.sessions.ListSessions(c.Request.Context(), tenantID, userID, current)
	if err != nil {
		writeSessionError(c, h.logger, "AdminUser.ListSessions", tenantID, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": views})
}

// EndSession handles DELETE /admin/users/:id/sessions/:sessionId (specs/055 US3): end one login
// session of a user of the admin's tenant — the device loses access on its next request. The
// session must belong to that user in that tenant, else 404 SESSION_NOT_FOUND.
func (h *AdminUserHandler) EndSession(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	userID := strings.TrimSpace(c.Param("id"))
	sessionID := strings.TrimSpace(c.Param("sessionId"))
	if !h.mayActOn(c, tenantID, userID, "EndSession") {
		return
	}
	if err := h.sessions.EndSession(auditContext(c), tenantID, userID, sessionID); err != nil {
		writeSessionError(c, h.logger, "AdminUser.EndSession", tenantID, err)
		return
	}
	h.logger.Info("handlers.AdminUser.EndSession: session ended by admin",
		zap.String("tenant_id", tenantID), zap.String("actor_id", actorOf(c)),
		zap.String("user_id", userID), zap.String("session_id", sessionID))
	c.Status(http.StatusNoContent)
}

// resetLinkIssuer is the consumer-side slice of AuthService the W7 invite flow needs: mint a
// password-reset token for one (verified-tenant, user) and return the link/token to the admin.
type resetLinkIssuer interface {
	IssueResetLinkForUser(ctx context.Context, tenantID, userID string) (*services.ResetLinkIssue, error)
}

// NewAdminUserHandler wires the handler over the user service.
func NewAdminUserHandler(svc *services.UserService, logger *zap.Logger) *AdminUserHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminUserHandler{svc: svc, logger: logger}
}

// WithResetLinkIssuer injects the reset-link minting service (the production AuthService —
// the SAME machinery as forgot-password, ENG-AUTH2 #1). Without it the reset-password-link
// route is not mounted. Returns the handler for fluent wiring.
func (h *AdminUserHandler) WithResetLinkIssuer(i resetLinkIssuer) *AdminUserHandler {
	if i != nil {
		h.resetIssuer = i
	}
	return h
}

// RegisterRoutes mounts the user CRUD routes under the given (admin-gated) group.
func (h *AdminUserHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/users", h.List)
	rg.GET("/admin/users/:id", h.Get)
	rg.POST("/admin/users", h.Create)
	rg.PUT("/admin/users/:id", h.Update)
	rg.DELETE("/admin/users/:id", h.Delete)
	// W7 invite flow: mounted only when the issuer is wired (main.go always wires it; a test
	// harness without reset support simply lacks the route).
	if h.resetIssuer != nil {
		rg.POST("/admin/users/:id/reset-password-link", h.IssueResetLink)
	}
	// specs/055 US3: mounted only when the session service is wired (main.go always wires it).
	if h.sessions != nil {
		rg.GET("/admin/users/:id/sessions", h.ListSessions)
		rg.DELETE("/admin/users/:id/sessions/:sessionId", h.EndSession)
	}
}

// IssueResetLink handles POST /admin/users/:id/reset-password-link (W7 §3.13): mints a
// single-use, 30-minute password-reset token for the target user — the SAME token the public
// forgot-password flow issues and the SAME POST /auth/reset-password consumes — and returns
// the link + raw token to the ADMIN caller, who hands it to the user out-of-band. This
// doubles as the minimal honest INVITE: create the user with a random password, then issue a
// link so they set their own. When SMTP is wired a courtesy copy is also queued to the user
// (emailQueued=true). Unknown/cross-tenant id → 404; non-active target → 422 USER_NOT_ACTIVE.
//
//	201 {"data":{"resetLink","token","expiresAt","ttlMinutes","emailQueued"}}
func (h *AdminUserHandler) IssueResetLink(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	targetID := strings.TrimSpace(c.Param("id"))
	if !h.mayActOn(c, tenantID, targetID, "IssueResetLink") {
		return
	}
	issue, err := h.resetIssuer.IssueResetLinkForUser(auditContext(c), tenantID, targetID)
	if err != nil {
		h.respondServiceError(c, err, "IssueResetLink", tenantID)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": issue})
}

// List handles GET /admin/users: returns every user in the tenant (safe view, no password hash).
func (h *AdminUserHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	users, err := h.svc.ListUsers(c.Request.Context(), tenantID)
	if err != nil {
		h.logger.Error("handlers.AdminUser.List", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể liệt kê user", "code": "USER_LIST_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"users": users}})
}

// Get handles GET /admin/users/:id: returns one tenant-scoped user, or 404 for an unknown id.
func (h *AdminUserHandler) Get(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	user, err := h.svc.GetUser(c.Request.Context(), tenantID, id)
	if err != nil {
		h.respondServiceError(c, err, "Get", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"user": user}})
}

// createUserRequest is the POST /admin/users body.
type createUserRequest struct {
	Email    string  `json:"email"`
	Password string  `json:"password"`
	FullName string  `json:"fullName"`
	Status   string  `json:"status"`
	IsAdmin  bool    `json:"isAdmin"`
	RoleID   *string `json:"roleId"`
}

// Create handles POST /admin/users. 🔒 GATE is_admin: the verified is_admin flag from the token
// is passed to the service, which refuses a non-admin with 403 (defense-in-depth beyond the
// RequireAdmin middleware). A duplicate email is 409; missing fields 422. The created user's
// safe view (no password hash) is returned with 201.
func (h *AdminUserHandler) Create(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req createUserRequest
	if !bindJSON(c, &req) {
		return
	}

	user, err := h.svc.CreateUser(auditContext(c), tenantID, middleware.IsAdminFromContext(c), services.CreateUserInput{
		Email:    req.Email,
		Password: req.Password,
		FullName: req.FullName,
		Status:   req.Status,
		IsAdmin:  req.IsAdmin,
		RoleID:   req.RoleID,
	})
	if err != nil {
		h.respondServiceError(c, err, "Create", tenantID)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"user": user}})
}

// updateUserRequest is the PUT /admin/users/:id body. Pointer fields use apply-when-non-nil
// semantics so an omitted key leaves the stored value untouched (an explicit value, even
// false/"", is applied). Email is not updatable (login identity).
type updateUserRequest struct {
	FullName *string `json:"fullName"`
	Status   *string `json:"status"`
	IsAdmin  *bool   `json:"isAdmin"`
	RoleID   *string `json:"roleId"`
	Password *string `json:"password"`
}

// Update handles PUT /admin/users/:id: applies the supplied fields to the tenant-scoped user
// and returns the refreshed safe view. Unknown id is 404. 🔒 GATE is_admin: the verified
// is_admin flag is passed to the service, which refuses a non-admin with 403 (defense-in-depth
// beyond the RequireAdmin middleware) — setting is_admin/role_id is a privilege-escalation op,
// so update carries the same service-layer gate as create/delete.
func (h *AdminUserHandler) Update(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	var req updateUserRequest
	if !bindJSON(c, &req) {
		return
	}

	user, err := h.svc.UpdateUser(auditContext(c), tenantID, actorOf(c), middleware.IsAdminFromContext(c), id, services.UpdateUserInput{
		FullName: req.FullName,
		Status:   req.Status,
		IsAdmin:  req.IsAdmin,
		RoleID:   req.RoleID,
		Password: req.Password,
	})
	if err != nil {
		h.respondServiceError(c, err, "Update", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"user": user}})
}

// Delete handles DELETE /admin/users/:id. 🔒 GATE is_admin: the verified is_admin flag is passed
// to the service, which refuses a non-admin with 403 (defense-in-depth). The verified actor
// user id is also passed so the service can block self-deletion (422 SELF_DELETE_FORBIDDEN) and
// removal of the last active admin (422 LAST_ADMIN). The user is soft-deleted (recoverable).
// Unknown id is 404.
func (h *AdminUserHandler) Delete(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	// Actor identity comes from the verified token subject, never a client field, so the
	// self-delete guard cannot be spoofed.
	sub, _ := middleware.SubjectFromContext(c)
	if err := h.svc.DeleteUser(auditContext(c), tenantID, sub.UserID, middleware.IsAdminFromContext(c), id); err != nil {
		h.respondServiceError(c, err, "Delete", tenantID)
		return
	}
	// 204 No Content on success (DELETE returns no body per api-conventions). The user is
	// soft-deleted (recoverable); refresh tokens are revoked by the service.
	c.Status(http.StatusNoContent)
}

// respondServiceError maps a user-service error to the right HTTP status: ErrUserForbidden →
// 403 (admin gate); a ValidationError → its code's status (404/409/422 via statusForCode);
// anything else → a logged 500. Centralizing this keeps the five handlers thin and consistent.
func (h *AdminUserHandler) respondServiceError(c *gin.Context, err error, op, tenantID string) {
	if writeUserAuthzError(c, err) {
		return
	}
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
		return
	}
	h.logger.Error("handlers.AdminUser."+op, zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xử lý yêu cầu user", "code": "USER_OP_FAILED"})
}

// mayActOn applies rule B2 (specs/065 FR-018) before a route that reaches into another account: an admin
// without the operator flag cannot touch an operator account (403 USER_NOT_MANAGEABLE). It writes the
// refusal itself and returns false when the request must stop.
func (h *AdminUserHandler) mayActOn(c *gin.Context, tenantID, targetID, op string) bool {
	if err := h.svc.AssertAdminMayAct(c.Request.Context(), tenantID, actorOf(c), targetID); err != nil {
		h.respondServiceError(c, err, op, tenantID)
		return false
	}
	return true
}

// callerIsOperator is THE single answer to "is the caller a platform operator" for the user-manager
// route (specs/065 D-14, one nook for every caller shape). A builder workspace session is confirmed in
// this very request by the auth stage's resolver (sub.IsPlatformOperator) and has no user row in the
// customer tenant, so it is answered without a DB read; a preview token or an API key is never an
// operator; a home token is read FRESH from the caller's own row.
func (h *AdminUserHandler) callerIsOperator(c *gin.Context) (bool, error) {
	claims, ok := middleware.ClaimsFromContext(c)
	if !ok {
		return false, nil
	}
	sub := claims.Subject
	if sub.WorkspaceSessionID != "" {
		return sub.IsPlatformOperator, nil
	}
	if claims.TokenType == auth.TokenTypePreview || claims.TokenType == auth.TokenTypeAPIKey {
		return false, nil
	}
	return h.svc.IsPlatformOperator(c.Request.Context(), sub.TenantID, sub.UserID)
}

// RegisterBuilderRoutes mounts the operator-only user routes on builderGated.
func (h *AdminUserHandler) RegisterBuilderRoutes(rg *gin.RouterGroup) {
	rg.PUT("/admin/users/:id/user-manager", h.SetUserManager)
}

// userManagerRequest is the PUT /admin/users/:id/user-manager body. A pointer so a missing key is a
// 400, never a silent "false".
type userManagerRequest struct {
	Enabled *bool `json:"enabled"`
}

// SetUserManager handles PUT /admin/users/:id/user-manager: the builder switches the customer-owner
// capability on or off for one account (specs/065). The route sits on builderGated; the service checks
// operator again from callerIsOperator (defense in depth). Turning it off never touches the user's role.
func (h *AdminUserHandler) SetUserManager(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req userManagerRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Thiếu trường enabled", "code": "INVALID_BODY"})
		return
	}
	isOp, err := h.callerIsOperator(c)
	if err != nil {
		h.respondServiceError(c, err, "SetUserManager", tenantID)
		return
	}
	user, err := h.svc.SetUserManager(auditContext(c), tenantID, isOp, strings.TrimSpace(c.Param("id")), *req.Enabled)
	if err != nil {
		h.respondServiceError(c, err, "SetUserManager", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"user": user}})
}
