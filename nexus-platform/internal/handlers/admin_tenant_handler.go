package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// tenantProvisioner is the consumer-side slice of *services.TenantProvisionService the handler
// needs. Declared here so the handler unit-tests against a fake.
type tenantProvisioner interface {
	Provision(ctx context.Context, in services.ProvisionTenantInput) (*services.ProvisionedTenant, error)
	ListTenants(ctx context.Context) ([]services.TenantListItem, error)
	// RetireTenant soft-deletes one workspace. callerTenantID is passed so the service can refuse
	// the self-retire foot-gun rather than trusting the handler to remember.
	RetireTenant(ctx context.Context, callerTenantID, tenantID string) error
	// PurgeTenant permanently removes a RETIRED workspace's data across every tenant-scoped table.
	// All the guards (self, not-retired, slug confirmation, schema drift) live in the service, so
	// no caller can reach the destructive path by forgetting one.
	PurgeTenant(ctx context.Context, callerTenantID, actorUserID, tenantID, confirmSlug string, dryRun bool) (*services.PurgeReport, error)
	// SetOwnerTier converts / reverts a workspace's accounts between admin and customer owner
	// (specs/065 lát D). The self-target refusal lives in the service.
	SetOwnerTier(ctx context.Context, callerTenantID, targetTenantID string, enabled bool) (*services.OwnerTierResult, error)
}

// operatorGate reads a caller's live authority row for the RequirePlatformOperator middleware.
// Satisfied by *auth.UserRepository; assignable to middleware.RequirePlatformOperator's parameter
// (identical method set) so the gate is applied without exporting middleware internals.
type operatorGate interface {
	GetUserByID(ctx context.Context, tenantID, id string) (*auth.User, error)
}

// AdminTenantHandler serves the operator-only tenant-provisioning routes (TENANT-PROVISION):
// POST /admin/tenants (create a customer workspace + first admin) and GET /admin/tenants (registry
// for the deploy wizard). Both sit behind Auth → Tenant → RequireAdmin → RequirePlatformOperator.
type AdminTenantHandler struct {
	svc    tenantProvisioner
	gate   operatorGate
	logger *zap.Logger
}

// NewAdminTenantHandler wires the handler. logger may be nil (Nop).
func NewAdminTenantHandler(svc tenantProvisioner, gate operatorGate, logger *zap.Logger) *AdminTenantHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminTenantHandler{svc: svc, gate: gate, logger: logger}
}

// RegisterRoutes mounts the routes on their OWN operator-gated subgroup of the admin group, so the
// RequirePlatformOperator gate (fresh DB read of the caller's flag) applies only here and the rest
// of /admin/* is unaffected. Mounted by RegisterAPIRoutes exactly like the other admin handlers.
func (h *AdminTenantHandler) RegisterRoutes(adminGated *gin.RouterGroup) {
	og := adminGated.Group("")
	og.Use(middleware.RequirePlatformOperator(h.gate, h.logger))
	og.POST("/admin/tenants", h.Create)
	og.GET("/admin/tenants", h.List)
	og.DELETE("/admin/tenants/:id", h.Retire)
	og.POST("/admin/tenants/:id/purge", h.Purge)
	og.POST("/admin/tenants/:id/owner-tier", h.EnableOwnerTier)
	og.DELETE("/admin/tenants/:id/owner-tier", h.DisableOwnerTier)
}

// purgeTenantRequest is the POST body. dryRun is a POINTER so an absent key means "dry run",
// not Go's zero value false: the safe reading of silence on an irreversible endpoint is
// "count, don't delete". Sending {"dryRun": false} is therefore a deliberate act.
type purgeTenantRequest struct {
	ConfirmSlug string `json:"confirmSlug"`
	DryRun      *bool  `json:"dryRun"`
}

// Purge handles POST /admin/tenants/:id/purge — permanently delete a RETIRED workspace's data.
//
// Retire stops logins but keeps every row; this is the other half. It is POST rather than a
// second DELETE because it carries a body (the typed slug confirmation) and because it must not
// be reachable by anything that reflexively re-sends a DELETE.
//
// 200 with the per-table report in both modes: a dry run is a real answer the operator reads
// before deciding, not an error.
func (h *AdminTenantHandler) Purge(c *gin.Context) {
	operatorTenant, _ := tenantOf(c)
	sub, _ := middleware.SubjectFromContext(c)
	target := c.Param("id")

	var req purgeTenantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Body không hợp lệ", "code": "INVALID_BODY"})
		return
	}
	dryRun := true
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}

	report, err := h.svc.PurgeTenant(auditContext(c), operatorTenant, sub.UserID, target, req.ConfirmSlug, dryRun)
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.AdminTenant.Purge",
			zap.String("operator_tenant", operatorTenant), zap.String("target", target), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xoá vĩnh viễn workspace", "code": "TENANT_PURGE_FAILED"})
		return
	}
	if !dryRun {
		h.logger.Warn("handlers.AdminTenant.Purge: workspace data purged",
			zap.String("operator_tenant", operatorTenant), zap.String("operator_user", sub.UserID),
			zap.String("target_tenant", target), zap.Int64("rows", report.TotalRows))
	}
	c.JSON(http.StatusOK, gin.H{"data": report})
}

// Retire handles DELETE /admin/tenants/:id — soft-delete a customer workspace. Until this route
// existed the only way to remove one was reaching into MySQL, which is not a thing an operator
// should have to do and not a thing that gets audited.
//
// Soft, not hard: the data stays, but login and inbound webhooks both stop working (measured on
// the live engine — 401 in each case, the webhook even with a valid signature). 204 on success,
// mirroring the other destructive endpoints.
func (h *AdminTenantHandler) Retire(c *gin.Context) {
	operatorTenant, _ := tenantOf(c)
	sub, _ := middleware.SubjectFromContext(c)
	target := c.Param("id")

	if err := h.svc.RetireTenant(auditContext(c), operatorTenant, target); err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.AdminTenant.Retire",
			zap.String("operator_tenant", operatorTenant), zap.String("target", target), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xoá workspace", "code": "TENANT_RETIRE_FAILED"})
		return
	}
	// Audited at Warn with the operator's identity: removing a customer workspace is the kind of
	// action someone asks about months later.
	h.logger.Warn("handlers.AdminTenant.Retire: workspace retired",
		zap.String("operator_tenant", operatorTenant), zap.String("operator_user", sub.UserID),
		zap.String("target_tenant", target))
	c.Status(http.StatusNoContent)
}

// EnableOwnerTier handles POST /admin/tenants/:id/owner-tier — convert the workspace's role-less
// admins into customer owners (specs/065 lát D). Operator-only (the subgroup gate); idempotent.
// The response carries counts and the owner role id only — never an e-mail or user id.
func (h *AdminTenantHandler) EnableOwnerTier(c *gin.Context) { h.setOwnerTier(c, true) }

// DisableOwnerTier handles DELETE /admin/tenants/:id/owner-tier — return the customer owners to admins.
func (h *AdminTenantHandler) DisableOwnerTier(c *gin.Context) { h.setOwnerTier(c, false) }

func (h *AdminTenantHandler) setOwnerTier(c *gin.Context, enabled bool) {
	operatorTenant, _ := tenantOf(c)
	sub, _ := middleware.SubjectFromContext(c)
	target := c.Param("id")

	res, err := h.svc.SetOwnerTier(auditContext(c), operatorTenant, target, enabled)
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.AdminTenant.setOwnerTier",
			zap.String("operator_tenant", operatorTenant), zap.String("target", target), zap.Bool("enabled", enabled), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể đổi tầng tài khoản", "code": "TENANT_OWNER_TIER_FAILED"})
		return
	}
	h.logger.Warn("handlers.AdminTenant.setOwnerTier: owner tier switched",
		zap.String("operator_tenant", operatorTenant), zap.String("operator_user", sub.UserID),
		zap.String("target_tenant", target), zap.Bool("enabled", enabled),
		zap.Int("converted", res.Converted), zap.Int("reverted", res.Reverted), zap.Int("revoke_failed", res.RevokeFailed))
	if enabled {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"converted": res.Converted, "skippedWithRole": res.SkippedWithRole,
			"ownerRoleId": res.OwnerRoleID, "revokeFailed": res.RevokeFailed,
		}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"reverted": res.Reverted, "revokeFailed": res.RevokeFailed}})
}

// createTenantRequest is the POST body (camelCase). Exactly one of adminPassword / generatePassword.
type createTenantRequest struct {
	Slug             string `json:"slug"`
	Name             string `json:"name"`
	AdminEmail       string `json:"adminEmail"`
	AdminFullName    string `json:"adminFullName"`
	AdminPassword    string `json:"adminPassword"`
	GeneratePassword bool   `json:"generatePassword"`
}

// Create handles POST /admin/tenants — provision a new customer tenant + first admin atomically.
// On success it returns 201 with the tenant + admin views; when the server minted the password it
// ALSO returns oneTimeCredentials ONCE (never persisted, never re-fetchable). Every call is audited
// (zap) with the operator's identity + target slug; the password never appears in any log.
func (h *AdminTenantHandler) Create(c *gin.Context) {
	operatorTenant, _ := tenantOf(c)
	sub, _ := middleware.SubjectFromContext(c)

	var req createTenantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Body không hợp lệ", "code": "INVALID_BODY"})
		return
	}

	// auditContext attributes the RM-19 provision audit row to the verified operator; ActorTenantID
	// is the operator's tenant (where that row is written).
	res, err := h.svc.Provision(auditContext(c), services.ProvisionTenantInput{
		Slug:             req.Slug,
		Name:             req.Name,
		AdminEmail:       req.AdminEmail,
		AdminFullName:    req.AdminFullName,
		AdminPassword:    req.AdminPassword,
		GeneratePassword: req.GeneratePassword,
		ActorTenantID:    operatorTenant,
	})
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			// A refused create is still audited (attempted provision), never with any secret.
			h.logger.Warn("handlers.AdminTenant.Create: refused",
				zap.String("operator_tenant_id", operatorTenant), zap.String("operator_user_id", sub.UserID),
				zap.String("target_slug", req.Slug), zap.String("code", ve.Code))
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.AdminTenant.Create: failed",
			zap.String("operator_tenant_id", operatorTenant), zap.String("operator_user_id", sub.UserID),
			zap.String("target_slug", req.Slug), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể tạo workspace", "code": "TENANT_PROVISION_FAILED"})
		return
	}

	h.logger.Info("handlers.AdminTenant.Create: provisioned",
		zap.String("operator_tenant_id", operatorTenant), zap.String("operator_user_id", sub.UserID),
		zap.String("target_tenant_id", res.Tenant.ID), zap.String("target_slug", res.Tenant.Slug))

	data := gin.H{"tenant": res.Tenant, "admin": res.Admin}
	if res.OneTimePassword != "" {
		// Show-once: the plaintext exists ONLY in this response (mirrors POST /admin/api-keys).
		data["oneTimeCredentials"] = gin.H{"password": res.OneTimePassword}
	}
	c.JSON(http.StatusCreated, gin.H{"data": data})
}

// List handles GET /admin/tenants — the tenant registry (newest first) for the deploy wizard's
// target selector. Returns each tenant + its usable-admin count; no email/user of any tenant is
// exposed.
func (h *AdminTenantHandler) List(c *gin.Context) {
	items, err := h.svc.ListTenants(c.Request.Context())
	if err != nil {
		h.logger.Error("handlers.AdminTenant.List: failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể tải danh sách workspace", "code": "TENANT_LIST_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": items})
}
