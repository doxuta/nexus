package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
)

// AdminTeamHandler exposes the A5-extra team-management endpoints under /admin/teams (CRUD) and
// /admin/teams/:id/members (add/remove). It sits behind the AuthMiddleware → RequireTenant →
// RequireAdmin chain (mounted in router.go on the admin-gated group), so the tenant is always the
// verified claim and every caller is already an admin. Teams + memberships back the A5 row-level
// filter: a record's team_set_id resolves to the creator's teams, and access_level=team viewers see
// records owned by anyone on a team they share.
type AdminTeamHandler struct {
	svc    *services.TeamService
	logger *zap.Logger
}

// NewAdminTeamHandler wires the handler over the team service.
func NewAdminTeamHandler(svc *services.TeamService, logger *zap.Logger) *AdminTeamHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminTeamHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts the team CRUD + member routes under the given (admin-gated) group.
func (h *AdminTeamHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/teams", h.List)
	rg.GET("/admin/teams/:id", h.Get)
	rg.POST("/admin/teams", h.Create)
	rg.PUT("/admin/teams/:id", h.Update)
	rg.DELETE("/admin/teams/:id", h.Delete)

	rg.GET("/admin/teams/:id/members", h.ListMembers)
	rg.POST("/admin/teams/:id/members", h.AddMember)
	rg.DELETE("/admin/teams/:id/members/:userId", h.RemoveMember)

	// W7 sharing screens: READ-ONLY team-set inspection (sets are engine-derived on record
	// writes from the creator's memberships — dynrepo A5 — so there is no write surface here).
	rg.GET("/admin/team-sets", h.ListTeamSets)
	rg.GET("/admin/team-sets/:id", h.GetTeamSet)
}

// ListTeamSets handles GET /admin/team-sets: every team set of the tenant with its member
// teams — the data behind "who sees this record and why" (a record's team_set_id resolves to
// one of these sets). Envelope: {"data":{"teamSets":[{id,primaryTeamId,teams:[{id,name}],createdAt}]}}.
func (h *AdminTeamHandler) ListTeamSets(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	sets, err := h.svc.ListTeamSets(c.Request.Context(), tenantID)
	if err != nil {
		h.respondServiceError(c, err, "ListTeamSets", tenantID)
		return
	}
	if sets == nil {
		sets = []services.TeamSetView{}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"teamSets": sets}})
}

// GetTeamSet handles GET /admin/team-sets/:id: one tenant-scoped team set with its member
// teams. Unknown or cross-tenant id → 404 TEAM_SET_NOT_FOUND (indistinguishable by design).
func (h *AdminTeamHandler) GetTeamSet(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	set, err := h.svc.GetTeamSet(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")))
	if err != nil {
		h.respondServiceError(c, err, "GetTeamSet", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"teamSet": set}})
}

// List handles GET /admin/teams: returns every team in the tenant.
func (h *AdminTeamHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	teams, err := h.svc.ListTeams(c.Request.Context(), tenantID)
	if err != nil {
		h.logger.Error("handlers.AdminTeam.List", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể liệt kê team", "code": "TEAM_LIST_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"teams": teams}})
}

// Get handles GET /admin/teams/:id: returns one tenant-scoped team, 404 unknown/cross-tenant.
func (h *AdminTeamHandler) Get(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	team, err := h.svc.GetTeam(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")))
	if err != nil {
		h.respondServiceError(c, err, "Get", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"team": team}})
}

// createTeamRequest is the POST /admin/teams body. parentId is optional.
type createTeamRequest struct {
	Name     string  `json:"name"`
	ParentID *string `json:"parentId"`
}

// Create handles POST /admin/teams: registers a team. A duplicate name in the tenant is 409; a
// missing name is 422; an unknown parent is 404. The created team's view is returned with 201.
func (h *AdminTeamHandler) Create(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req createTeamRequest
	if !bindJSON(c, &req) {
		return
	}
	team, err := h.svc.CreateTeam(auditContext(c), tenantID, services.CreateTeamInput{
		Name:     req.Name,
		ParentID: req.ParentID,
	})
	if err != nil {
		h.respondServiceError(c, err, "Create", tenantID)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"team": team}})
}

// updateTeamRequest is the PUT /admin/teams/:id body. Both fields use apply-when-non-nil semantics:
// an omitted key leaves the stored value untouched; parentId="" explicitly detaches from the
// hierarchy.
type updateTeamRequest struct {
	Name     *string `json:"name"`
	ParentID *string `json:"parentId"`
}

// Update handles PUT /admin/teams/:id: applies the supplied fields. Unknown id 404; name clash 409.
func (h *AdminTeamHandler) Update(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req updateTeamRequest
	if !bindJSON(c, &req) {
		return
	}
	team, err := h.svc.UpdateTeam(auditContext(c), tenantID, strings.TrimSpace(c.Param("id")), services.UpdateTeamInput{
		Name:     req.Name,
		ParentID: req.ParentID,
	})
	if err != nil {
		h.respondServiceError(c, err, "Update", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"team": team}})
}

// Delete handles DELETE /admin/teams/:id: removes the team (cascading memberships + set
// compositions via DB FKs; child teams re-homed to the deleted team's parent). Unknown id is 404.
func (h *AdminTeamHandler) Delete(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if err := h.svc.DeleteTeam(auditContext(c), tenantID, id); err != nil {
		h.respondServiceError(c, err, "Delete", tenantID)
		return
	}
	// 204 No Content on success (DELETE returns no body per api-conventions). Cascades remove
	// the team's memberships + set links.
	c.Status(http.StatusNoContent)
}

// ListMembers handles GET /admin/teams/:id/members: returns the users on a tenant-scoped team.
func (h *AdminTeamHandler) ListMembers(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	members, err := h.svc.ListMembers(c.Request.Context(), tenantID, strings.TrimSpace(c.Param("id")))
	if err != nil {
		h.respondServiceError(c, err, "ListMembers", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"members": members}})
}

// addMemberRequest is the POST /admin/teams/:id/members body.
type addMemberRequest struct {
	UserID string `json:"userId"`
}

// AddMember handles POST /admin/teams/:id/members: adds a user (same tenant) to the team. Unknown
// team is 404; unknown/cross-tenant user is 404 (MEMBER_USER_NOT_FOUND); missing userId is 422. A
// re-add is idempotent and returns 200.
func (h *AdminTeamHandler) AddMember(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req addMemberRequest
	if !bindJSON(c, &req) {
		return
	}
	teamID := strings.TrimSpace(c.Param("id"))
	if err := h.svc.AddMember(auditContext(c), tenantID, teamID, req.UserID); err != nil {
		h.respondServiceError(c, err, "AddMember", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"teamId": teamID, "userId": strings.TrimSpace(req.UserID), "member": true}})
}

// RemoveMember handles DELETE /admin/teams/:id/members/:userId: removes a user from the team.
// Unknown team is 404; a non-member is 404 (MEMBERSHIP_NOT_FOUND).
func (h *AdminTeamHandler) RemoveMember(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	teamID := strings.TrimSpace(c.Param("id"))
	userID := strings.TrimSpace(c.Param("userId"))
	if err := h.svc.RemoveMember(auditContext(c), tenantID, teamID, userID); err != nil {
		h.respondServiceError(c, err, "RemoveMember", tenantID)
		return
	}
	// 204 No Content on success (DELETE returns no body per api-conventions).
	c.Status(http.StatusNoContent)
}

// respondServiceError maps a team-service error to the right HTTP status: a ValidationError → its
// code's status (404/409/422 via statusForCode); anything else → a logged 500.
func (h *AdminTeamHandler) respondServiceError(c *gin.Context, err error, op, tenantID string) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
		return
	}
	h.logger.Error("handlers.AdminTeam."+op, zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xử lý yêu cầu team", "code": "TEAM_OP_FAILED"})
}
