package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
)

// AdminRoleHandler exposes the A4 role-management endpoints under /admin/roles (CRUD + grant
// assignment, ../specs/001-auth-security/spec.md §3 bảng endpoint). It sits behind the AuthMiddleware →
// RequireTenant → RequireAdmin chain (mounted in router.go on the admin-gated group), so the
// tenant is always the verified claim and every caller is already an admin. The grants it
// manages are what the ACL stage later checks for non-admin principals on the dynamic /:entity
// surface.
type AdminRoleHandler struct {
	svc    *services.RoleService
	logger *zap.Logger
}

// NewAdminRoleHandler wires the handler over the role service.
func NewAdminRoleHandler(svc *services.RoleService, logger *zap.Logger) *AdminRoleHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminRoleHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts the role CRUD routes under the given (admin-gated) group.
func (h *AdminRoleHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/roles", h.List)
	rg.GET("/admin/roles/:id", h.Get)
	rg.POST("/admin/roles", h.Create)
	rg.PUT("/admin/roles/:id", h.Update)
	rg.DELETE("/admin/roles/:id", h.Delete)
}

// roleActionRequest is the API shape of one grant in a role create/update body.
type roleActionRequest struct {
	EntityName  string `json:"entityName"`
	Action      string `json:"action"`
	AccessLevel string `json:"accessLevel"`
}

// toServiceActions maps request grants to the service input type.
func toServiceActions(in []roleActionRequest) []services.RoleActionInput {
	out := make([]services.RoleActionInput, len(in))
	for i, g := range in {
		out[i] = services.RoleActionInput{
			EntityName:  g.EntityName,
			Action:      g.Action,
			AccessLevel: g.AccessLevel,
		}
	}
	return out
}

// List handles GET /admin/roles: returns every role in the tenant (without grants).
func (h *AdminRoleHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	roles, err := h.svc.ListRoles(c.Request.Context(), tenantID)
	if err != nil {
		h.logger.Error("handlers.AdminRole.List", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể liệt kê role", "code": "ROLE_LIST_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"roles": roles}})
}

// Get handles GET /admin/roles/:id: returns one tenant-scoped role WITH its grants, 404 unknown.
func (h *AdminRoleHandler) Get(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	role, err := h.svc.GetRole(c.Request.Context(), tenantID, id)
	if err != nil {
		h.respondServiceError(c, err, "Get", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"role": role}})
}

// createRoleRequest is the POST /admin/roles body.
type createRoleRequest struct {
	Name    string              `json:"name"`
	Label   string              `json:"label"`
	Actions []roleActionRequest `json:"actions"`
}

// Create handles POST /admin/roles: registers a role and assigns its initial grants. A
// duplicate name is 409; an invalid grant (bad action/access_level/blank entity) or missing
// name is 422. The created role's view (with grants) is returned with 201.
func (h *AdminRoleHandler) Create(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req createRoleRequest
	if !bindJSON(c, &req) {
		return
	}
	role, err := h.svc.CreateRole(auditContext(c), tenantID, services.CreateRoleInput{
		Name:    req.Name,
		Label:   req.Label,
		Actions: toServiceActions(req.Actions),
	})
	if err != nil {
		h.respondServiceError(c, err, "Create", tenantID)
		return
	}
	jsonWV(c, http.StatusCreated, gin.H{"data": gin.H{"role": role}})
}

// updateRoleRequest is the PUT /admin/roles/:id body. Name/Label use apply-when-non-nil
// semantics. Actions, when present (non-nil pointer), REPLACES the full grant set (an empty
// array clears all grants); omitting the key leaves the existing grants untouched.
type updateRoleRequest struct {
	Name    *string              `json:"name"`
	Label   *string              `json:"label"`
	Actions *[]roleActionRequest `json:"actions"`
}

// Update handles PUT /admin/roles/:id: applies the supplied fields and, when actions is
// provided, replaces the role's grant set. Unknown id is 404; a name clash 409; invalid grant 422.
func (h *AdminRoleHandler) Update(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	var req updateRoleRequest
	if !bindJSON(c, &req) {
		return
	}

	in := services.UpdateRoleInput{Name: req.Name, Label: req.Label}
	if req.Actions != nil {
		// A present (even empty) actions key means "replace grants"; map it to a non-nil slice
		// so the service distinguishes replace-with-empty from leave-untouched.
		in.Actions = toServiceActions(*req.Actions)
		if in.Actions == nil {
			in.Actions = []services.RoleActionInput{}
		}
	}

	role, err := h.svc.UpdateRole(auditContext(c), tenantID, id, in)
	if err != nil {
		h.respondServiceError(c, err, "Update", tenantID)
		return
	}
	jsonWV(c, http.StatusOK, gin.H{"data": gin.H{"role": role}})
}

// Delete handles DELETE /admin/roles/:id: removes the role (cascading its grants, detaching
// users via users.role_id ON DELETE SET NULL). Unknown id is 404.
func (h *AdminRoleHandler) Delete(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if err := h.svc.DeleteRole(auditContext(c), tenantID, id); err != nil {
		h.respondServiceError(c, err, "Delete", tenantID)
		return
	}
	// 204 No Content on success (DELETE returns no body per api-conventions).
	noContentWV(c)
}

// respondServiceError maps a role-service error to the right HTTP status: a ValidationError →
// its code's status (404/409/422 via statusForCode); anything else → a logged 500.
func (h *AdminRoleHandler) respondServiceError(c *gin.Context, err error, op, tenantID string) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
		return
	}
	h.logger.Error("handlers.AdminRole."+op, zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xử lý yêu cầu role", "code": "ROLE_OP_FAILED"})
}
