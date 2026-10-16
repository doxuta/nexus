package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// projectRegistry is the consumer-side slice of *services.ProjectService the handler needs.
type projectRegistry interface {
	List(ctx context.Context, callerTenantID string) ([]services.ProjectView, error)
	Patch(ctx context.Context, actor services.ProjectActor, tenantID string, in services.ProjectPatch) (*services.ProjectView, error)
	HealthCheck(ctx context.Context, actor services.ProjectActor, tenantID string) (*services.ProjectHealth, error)
	Create(ctx context.Context, home auth.Subject, in services.ProjectCreateInput) (*services.ProjectCreated, error)
}

// AdminProjectHandler serves the project registry (specs/062-customer-workspace L5):
//
//	GET   /admin/projects                          every customer with stage, counts, stored health
//	PATCH /admin/projects/:tenantId                industry / "Chạy thử"
//	POST  /admin/projects/:tenantId/health-check   "Khám lại" - Doctor + app readiness, stored
//	POST  /admin/projects                          create a customer project in one call (L6)
//
// All four sit behind Auth → Tenant → RequireAdmin → RequirePlatformOperator: the workshop's
// platform operator and nobody else. A customer's admin, staff or owner and a builder workspace-
// session token get the byte-identical 403 of every other operator route.
type AdminProjectHandler struct {
	svc    projectRegistry
	gate   operatorGate
	logger *zap.Logger
}

// NewAdminProjectHandler wires the handler. logger may be nil (Nop).
func NewAdminProjectHandler(svc projectRegistry, gate operatorGate, logger *zap.Logger) *AdminProjectHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminProjectHandler{svc: svc, gate: gate, logger: logger}
}

// RegisterRoutes mounts the routes on their OWN operator-gated subgroup of the admin group (same
// shape as AdminTenantHandler.RegisterRoutes), so the live-row operator check applies here only.
func (h *AdminProjectHandler) RegisterRoutes(adminGated *gin.RouterGroup) {
	og := adminGated.Group("")
	og.Use(middleware.RequirePlatformOperator(h.gate, h.logger))
	og.GET("/admin/projects", h.List)
	og.POST("/admin/projects", h.Create)
	og.PATCH("/admin/projects/:tenantId", h.Patch)
	og.POST("/admin/projects/:tenantId/health-check", h.HealthCheck)
}

func (h *AdminProjectHandler) actor(c *gin.Context) (services.ProjectActor, bool) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return services.ProjectActor{}, false
	}
	sub, _ := middleware.SubjectFromContext(c)
	return services.ProjectActor{TenantID: tenantID, UserID: sub.UserID}, true
}

// fail writes a typed refusal with the status its code maps to, or a 500 that leaks nothing.
func (h *AdminProjectHandler) fail(c *gin.Context, op string, err error) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
		return
	}
	operatorTenant, _ := tenantOf(c)
	h.logger.Error("handlers.AdminProject."+op,
		zap.String("tenant_id", operatorTenant), zap.String("target_tenant_id", c.Param("tenantId")), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xử lý sổ dự án", "code": "PROJECT_FAILED"})
}

// List handles GET /admin/projects.
func (h *AdminProjectHandler) List(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	rows, err := h.svc.List(c.Request.Context(), actor.TenantID)
	if err != nil {
		h.fail(c, "List", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rows})
}

// optionalString records whether a JSON key was present at all and whether it was null, which a
// plain *string cannot: absent leaves the stored value alone, null / blank clears it.
type optionalString struct {
	set bool
	val *string
}

// UnmarshalJSON runs for a present key even when its value is null (value-receiver fields get the
// call; only pointer fields short-circuit null), which is what makes "set" observable.
func (o *optionalString) UnmarshalJSON(b []byte) error {
	o.set = true
	if string(b) == "null" {
		o.val = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	o.val = &s
	return nil
}

type patchProjectRequest struct {
	Industry optionalString `json:"industry"`
	Trial    *bool          `json:"trial"`
}

// Patch handles PATCH /admin/projects/:tenantId.
func (h *AdminProjectHandler) Patch(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	var req patchProjectRequest
	// strict: a key this route does not own ("stage", "deliveredAt") is a 400, never a silent 200
	if !bindJSONStrict(c, &req) {
		return
	}
	v, err := h.svc.Patch(c.Request.Context(), actor, c.Param("tenantId"),
		services.ProjectPatch{IndustrySet: req.Industry.set, Industry: req.Industry.val, Trial: req.Trial})
	if err != nil {
		h.fail(c, "Patch", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": v})
}

// createProjectRequest is the POST /admin/projects body (camelCase). Exactly one of adminPassword /
// generatePassword, as for POST /admin/tenants.
type createProjectRequest struct {
	Name             string `json:"name"`
	Slug             string `json:"slug"`
	Industry         string `json:"industry"`
	AdminEmail       string `json:"adminEmail"`
	AdminFullName    string `json:"adminFullName"`
	AdminPassword    string `json:"adminPassword"`
	GeneratePassword bool   `json:"generatePassword"`
	Start            struct {
		Mode string `json:"mode"`
		Pack string `json:"pack"`
	} `json:"start"`
}

// Create handles POST /admin/projects -> 201 {data:{project, admin, oneTimeCredentials?, start,
// workspace?}}. A starter-pack failure or a session that could not be opened is reported INSIDE the
// 201 (start.status=failed / no workspace): the workspace and its owner already exist and the
// one-time password has been minted, so a 5xx here would lose both.
func (h *AdminProjectHandler) Create(c *gin.Context) {
	claims, ok := middleware.ClaimsFromContext(c)
	if !ok {
		c.JSON(http.StatusForbidden, gin.H{"error": "Yêu cầu quyền quản trị viên", "code": "FORBIDDEN"})
		return
	}
	var req createProjectRequest
	if !bindJSONStrict(c, &req) {
		return
	}
	res, err := h.svc.Create(auditContext(c), claims.Subject, services.ProjectCreateInput{
		Name: req.Name, Slug: req.Slug, Industry: req.Industry,
		AdminEmail: req.AdminEmail, AdminFullName: req.AdminFullName,
		AdminPassword: req.AdminPassword, GeneratePassword: req.GeneratePassword,
		StartMode: req.Start.Mode, StartPack: req.Start.Pack,
	})
	if err != nil {
		h.fail(c, "Create", err)
		return
	}
	data := gin.H{"project": res.Project, "admin": res.Admin, "start": res.Start}
	if res.Workspace != nil {
		data["workspace"] = res.Workspace
	}
	if res.OneTimePassword != "" {
		// Show-once, the same key POST /admin/tenants uses: the plaintext exists ONLY in this response.
		data["oneTimeCredentials"] = gin.H{"password": res.OneTimePassword}
	}
	// Carries the show-once password and a builder token: keep proxies/browsers from storing it.
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, gin.H{"data": data})
}

// HealthCheck handles POST /admin/projects/:tenantId/health-check.
func (h *AdminProjectHandler) HealthCheck(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	health, err := h.svc.HealthCheck(c.Request.Context(), actor, c.Param("tenantId"))
	if err != nil {
		h.fail(c, "HealthCheck", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": health})
}
