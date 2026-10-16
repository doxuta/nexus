package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// OrgAdminHandler serves /org-admin/* (specs/065-customer-owner-tier lát B): the customer owner's
// "Cài đặt › Người dùng". It is mounted on the protected group behind middleware.RequireUserManager —
// NOT under /admin, whose invariant ("every /admin/* needs is_admin") stays intact — and BEFORE the
// dynamic /:entity dispatcher (a hyphenated prefix can never be an entity name anyway).
//
// The tenant and the actor always come from the verified token. The PUT body struct has no isAdmin,
// password or canManageUsers field, so those keys are dropped by construction.
type OrgAdminHandler struct {
	svc         *services.UserService
	resetIssuer resetLinkIssuer // lát C: nil ⇒ the invite route is not mounted
	sessions    sessionManager  // lát C: nil ⇒ the session routes are not mounted
	logger      *zap.Logger
}

// WithResetLinkIssuer injects the invite-link minting service (the production AuthService).
func (h *OrgAdminHandler) WithResetLinkIssuer(i resetLinkIssuer) *OrgAdminHandler {
	if i != nil {
		h.resetIssuer = i
	}
	return h
}

// WithSessionManager injects the login-session service (the production AuthService).
func (h *OrgAdminHandler) WithSessionManager(m sessionManager) *OrgAdminHandler {
	if m != nil {
		h.sessions = m
	}
	return h
}

// NewOrgAdminHandler wires the handler over the user service.
func NewOrgAdminHandler(svc *services.UserService, logger *zap.Logger) *OrgAdminHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &OrgAdminHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts the owner routes on a group that already carries the owner gate.
func (h *OrgAdminHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/org-admin/users", h.List)
	rg.GET("/org-admin/users/:id", h.Get)
	rg.PUT("/org-admin/users/:id", h.Update)
	rg.GET("/org-admin/roles", h.Roles)
	rg.POST("/org-admin/users", h.Create)
	if h.resetIssuer != nil {
		rg.POST("/org-admin/users/:id/reset-password-link", h.IssueResetLink)
	}
	if h.sessions != nil {
		rg.GET("/org-admin/users/:id/sessions", h.ListSessions)
		rg.DELETE("/org-admin/users/:id/sessions/:sessionId", h.EndSession)
	}
}

// orgCreateRequest is the POST /org-admin/users body: exactly three keys. password / isAdmin /
// canManageUsers are dropped by construction (the password is generated server-side).
type orgCreateRequest struct {
	Email    string  `json:"email"`
	FullName string  `json:"fullName"`
	RoleID   *string `json:"roleId"`
}

// Create handles POST /org-admin/users: a plain active staff account; the audit row names the owner.
func (h *OrgAdminHandler) Create(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req orgCreateRequest
	if !bindJSON(c, &req) {
		return
	}
	user, err := h.svc.CreateStaff(auditContext(c), tenantID, actorOf(c),
		services.OrgCreateInput{Email: req.Email, FullName: req.FullName, RoleID: req.RoleID})
	if err != nil {
		h.respondError(c, err, "Create", tenantID)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"user": user}})
}

// mayManage runs AssertManageable and writes the refusal itself; false means stop.
func (h *OrgAdminHandler) mayManage(c *gin.Context, tenantID, targetID, op string) bool {
	if err := h.svc.AssertManageable(c.Request.Context(), tenantID, actorOf(c), targetID); err != nil {
		h.respondError(c, err, op, tenantID)
		return false
	}
	return true
}

// IssueResetLink handles POST /org-admin/users/:id/reset-password-link: the invite link for a manageable account.
func (h *OrgAdminHandler) IssueResetLink(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	targetID := strings.TrimSpace(c.Param("id"))
	if !h.mayManage(c, tenantID, targetID, "IssueResetLink") {
		return
	}
	issue, err := h.resetIssuer.IssueResetLinkForUser(auditContext(c), tenantID, targetID)
	if err != nil {
		h.respondError(c, err, "IssueResetLink", tenantID)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": issue})
}

// ListSessions handles GET /org-admin/users/:id/sessions. The target is never the caller (self is not
// manageable), so no row is ever "phiên này".
func (h *OrgAdminHandler) ListSessions(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	userID := strings.TrimSpace(c.Param("id"))
	if !h.mayManage(c, tenantID, userID, "ListSessions") {
		return
	}
	views, err := h.sessions.ListSessions(c.Request.Context(), tenantID, userID, "")
	if err != nil {
		writeSessionError(c, h.logger, "OrgAdmin.ListSessions", tenantID, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": views})
}

// EndSession handles DELETE /org-admin/users/:id/sessions/:sessionId; the audit row names the owner.
func (h *OrgAdminHandler) EndSession(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	userID := strings.TrimSpace(c.Param("id"))
	if !h.mayManage(c, tenantID, userID, "EndSession") {
		return
	}
	if err := h.sessions.EndSession(auditContext(c), tenantID, userID, strings.TrimSpace(c.Param("sessionId"))); err != nil {
		writeSessionError(c, h.logger, "OrgAdmin.EndSession", tenantID, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// List handles GET /org-admin/users.
func (h *OrgAdminHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	users, err := h.svc.ListOrgUsers(c.Request.Context(), tenantID, actorOf(c))
	if err != nil {
		h.respondError(c, err, "List", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"users": users}})
}

// Get handles GET /org-admin/users/:id.
func (h *OrgAdminHandler) Get(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	user, err := h.svc.GetOrgUser(c.Request.Context(), tenantID, actorOf(c), strings.TrimSpace(c.Param("id")))
	if err != nil {
		h.respondError(c, err, "Get", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"user": user}})
}

// orgUpdateRequest is the PUT /org-admin/users/:id body: exactly the three things an owner may change.
type orgUpdateRequest struct {
	FullName *string `json:"fullName"`
	Status   *string `json:"status"`
	RoleID   *string `json:"roleId"`
}

// Update handles PUT /org-admin/users/:id. The audit row is attributed to the owner (auditContext).
func (h *OrgAdminHandler) Update(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req orgUpdateRequest
	if !bindJSON(c, &req) {
		return
	}
	user, err := h.svc.UpdateOrgUser(auditContext(c), tenantID, actorOf(c), strings.TrimSpace(c.Param("id")),
		services.OrgUpdateInput{FullName: req.FullName, Status: req.Status, RoleID: req.RoleID})
	if err != nil {
		h.respondError(c, err, "Update", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"user": user}})
}

// Roles handles GET /org-admin/roles: the role picker (label + assignable, no grants).
func (h *OrgAdminHandler) Roles(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	roles, err := h.svc.ListAssignableRoles(c.Request.Context(), tenantID, actorOf(c))
	if err != nil {
		h.respondError(c, err, "Roles", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"roles": roles}})
}

// respondError maps service errors: ErrUserForbidden → the byte-identical FORBIDDEN body;
// ErrUserNotManageable → 403 USER_NOT_MANAGEABLE; ValidationError → its code's status; else a logged 500.
func (h *OrgAdminHandler) respondError(c *gin.Context, err error, op, tenantID string) {
	if writeUserAuthzError(c, err) {
		return
	}
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
		return
	}
	h.logger.Error("handlers.OrgAdmin."+op, zap.String("tenant_id", tenantID), zap.String("user_id", actorOf(c)), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xử lý yêu cầu người dùng", "code": "USER_OP_FAILED"})
}

// writeUserAuthzError writes the two authorization refusals shared by /admin/users and /org-admin and
// reports whether it did. One place for the bodies, so the two surfaces cannot drift.
func writeUserAuthzError(c *gin.Context, err error) bool {
	switch {
	case errors.Is(err, services.ErrUserForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": "Yêu cầu quyền quản trị viên", "code": "FORBIDDEN"})
	case errors.Is(err, auth.ErrUserNotManageable):
		c.JSON(http.StatusForbidden, gin.H{"error": "Bạn không quản lý được tài khoản này", "code": "USER_NOT_MANAGEABLE"})
	default:
		return false
	}
	return true
}
