package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/services"
)

// AdminAppHandler exposes the App-container endpoints (../specs/010-app-container/spec.md §3). The CRUD
// surface (/admin/apps, incl. PUT /admin/apps/:id/nav) sits behind Auth → Tenant → RequireAdmin;
// the READ surface (GET /apps, GET /apps/:name/nav) sits on the protected (non-admin) group —
// the App Switcher is for every authenticated user, and the nav read filters entity items
// through the caller's own ACL server-side (INV-3). Mirrors the AdminDashboardHandler wiring.
type AdminAppHandler struct {
	svc    *services.AppService
	logger *zap.Logger
	// Cross-tenant repair deps (WithCrossTenantRepair). Optional: nil leaves ACL backfill
	// tenant-local, and a ?targetTenant= on it fails CLOSED at the uniform 403.
	opGate  operatorGate
	tenants tenantSlugResolver
	frozen  tenantFreezeChecker // specs/068: write-freeze of the TARGET tenant (nil = not enforced)
}

// WithTenantFreeze arms the target-tenant write-freeze check of the cross-tenant path. Chainable.
func (h *AdminAppHandler) WithTenantFreeze(f tenantFreezeChecker) *AdminAppHandler {
	h.frozen = f
	return h
}

// WithCrossTenantRepair lets an OPERATOR aim POST /admin/apps/:id/acl-backfill at a customer
// workspace (?targetTenant=<slug>). Same gate, same body, same refusals as the install path —
// resolveCrossTenantTarget is shared, not copied.
//
// Why a repair tool needs to reach across at all: when a customer's staff can see nothing, the
// fix is a grant seed in THEIR tenant. Until now that could only be done with a database session,
// which is precisely the kind of "reach into MySQL" this product keeps eliminating. The root cause
// is separately fixed (re-handover now reconciles role grants), so this is the recovery path for
// tenants delivered before that landed.
func (h *AdminAppHandler) WithCrossTenantRepair(gate operatorGate, tenants tenantSlugResolver) *AdminAppHandler {
	h.opGate, h.tenants = gate, tenants
	return h
}

// NewAdminAppHandler wires the handler over the app service.
func NewAdminAppHandler(svc *services.AppService, logger *zap.Logger) *AdminAppHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminAppHandler{svc: svc, logger: logger}
}

// RegisterAdminRoutes mounts the app CRUD + nav-write routes under the given (admin-gated) group.
func (h *AdminAppHandler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/apps", h.AdminList)
	rg.GET("/admin/apps/:id", h.AdminGet)
	rg.POST("/admin/apps", h.Create)
	rg.PUT("/admin/apps/:id", h.Update)
	rg.DELETE("/admin/apps/:id", h.Delete)
	rg.PUT("/admin/apps/:id/nav", h.ReplaceNav)
	rg.POST("/admin/apps/:id/acl-backfill", h.ACLBackfill)
}

// RegisterReadRoutes mounts the non-admin app reads under the given protected group. Must be
// registered BEFORE the dynamic /:entity dispatcher so the static "apps" segment resolves ahead
// of the catch-all param route (same precedent as /dashboards, /reports).
func (h *AdminAppHandler) RegisterReadRoutes(rg *gin.RouterGroup) {
	rg.GET("/apps", h.Catalog)
	rg.GET("/apps/:name/nav", h.Nav)
}

// appRequest is the POST/PUT body for an app. Name (the slug) is only read on create.
type appRequest struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty"`
	Color       string `json:"color,omitempty"`
	IsDefault   bool   `json:"isDefault,omitempty"`
	SortOrder   int    `json:"sortOrder,omitempty"`
	// HomeDashboardID is a pointer so an absent key (old client) is distinguishable from "" (clear).
	HomeDashboardID *string `json:"homeDashboardId,omitempty"`
	// HomeQueueIDs / DefaultRoleIDs: same pointer contract (absent = keep, [] = clear).
	HomeQueueIDs   *[]string `json:"homeQueueIds,omitempty"`
	DefaultRoleIDs *[]string `json:"defaultRoleIds,omitempty"`
}

// navItemRequest is one nav reference in the PUT /admin/apps/:id/nav body (a bare JSON array —
// replace-all: send the full nav in its new order; reorder = resend).
type navItemRequest struct {
	ItemType string `json:"itemType"`
	RefID    string `json:"refId"`
	Label    string `json:"label,omitempty"`
}

// appSummary projects an app to the spec'd catalog shape (§3 GET /apps) — the read surface
// deliberately omits audit/description fields the Switcher does not need.
func appSummary(a *appmeta.App) gin.H {
	return gin.H{
		"id": a.ID, "name": a.Name, "label": a.Label, "icon": a.Icon, "color": a.Color,
		"isDefault": a.IsDefault, "sortOrder": a.SortOrder, "homeDashboardId": a.HomeDashboardID,
		// The home queues are what the app's front page shows (anyone may see them); the roles an app is
		// default for are not - only whether it is the caller's own default.
		"homeQueueIds": a.HomeQueueIDs, "defaultForCaller": a.DefaultForCaller,
	}
}

// Catalog handles GET /apps: the apps the caller can open something in, in App Switcher order
// (admins: every app). Apps are not an access boundary (INV-3) — entity ACL still decides what
// opens; the catalog just omits lenses that would be empty for this caller.
func (h *AdminAppHandler) Catalog(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	apps, err := h.svc.ListAppsForCaller(c.Request.Context(), scopeOf(c), tenantID)
	if err != nil {
		h.logger.Error("handlers.AdminApp.Catalog", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể liệt kê app", "code": "APP_LIST_FAILED"})
		return
	}
	items := make([]gin.H, 0, len(apps))
	for i := range apps {
		items = append(items, appSummary(&apps[i]))
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"apps": items}})
}

// Nav handles GET /apps/:name/nav: the app's nav items in order, entity items filtered
// through the caller's OWN entity ACL server-side (INV-3 — a denied entity is dropped, no
// existence leak).
func (h *AdminAppHandler) Nav(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	name := strings.TrimSpace(c.Param("name"))
	app, items, err := h.svc.GetNav(c.Request.Context(), scopeOf(c), tenantID, name)
	if err != nil {
		h.respondServiceError(c, err, "Nav", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"app": appSummary(app), "items": items}})
}

// AdminList handles GET /admin/apps: full rows (incl. navCount + audit fields).
func (h *AdminAppHandler) AdminList(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	apps, err := h.svc.ListApps(c.Request.Context(), tenantID)
	if err != nil {
		h.logger.Error("handlers.AdminApp.AdminList", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể liệt kê app", "code": "APP_LIST_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"apps": apps}})
}

// AdminGet handles GET /admin/apps/:id: one app plus its raw nav rows (unfiltered — the admin
// builder needs to see every reference).
func (h *AdminAppHandler) AdminGet(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	app, items, err := h.svc.GetApp(c.Request.Context(), tenantID, c.Param("id"))
	if err != nil {
		h.respondServiceError(c, err, "AdminGet", tenantID)
		return
	}
	resp := gin.H{"app": app, "items": items}
	// TRỤ 2 mục 4 item 5: the "App sẵn sàng thao tác" honesty gate. This is a pure read layered
	// on top of an already-succeeded app-detail fetch, so a computation failure degrades
	// gracefully (omit the key, log a warning) instead of turning a working app-detail page into
	// a 500. readiness is nil (no key added) when WithReadiness was never wired.
	readiness, rerr := h.svc.Readiness(c.Request.Context(), tenantID, items)
	if rerr != nil {
		h.logger.Warn("handlers.AdminGet.readiness", zap.String("tenant_id", tenantID), zap.String("app_id", app.ID), zap.Error(rerr))
	} else if readiness != nil {
		resp["readiness"] = readiness
	}
	c.JSON(http.StatusOK, gin.H{"data": resp})
}

// Create handles POST /admin/apps: validates and inserts an app → 201.
func (h *AdminAppHandler) Create(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req appRequest
	if !bindJSON(c, &req) {
		return
	}
	// A new app has no dashboards/queues/role yet: refuse the keys instead of silently dropping them.
	if req.HomeDashboardID != nil || req.HomeQueueIDs != nil || req.DefaultRoleIDs != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Trang chủ và app mặc định đặt sau khi tạo app", "code": "APP_HOME_ON_CREATE"})
		return
	}
	app, err := h.svc.CreateApp(auditContext(c), tenantID, services.CreateAppInput{
		Name:        req.Name,
		Label:       req.Label,
		Description: req.Description,
		Icon:        req.Icon,
		Color:       req.Color,
		IsDefault:   req.IsDefault,
		SortOrder:   req.SortOrder,
		ActorID:     actorOf(c),
	})
	if err != nil {
		h.respondServiceError(c, err, "Create", tenantID)
		return
	}
	jsonWV(c, http.StatusCreated, gin.H{"data": gin.H{"app": app}})
}

// Update handles PUT /admin/apps/:id: full-replace of the mutable fields. Unknown id → 404.
func (h *AdminAppHandler) Update(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req appRequest
	if !bindJSON(c, &req) {
		return
	}
	app, err := h.svc.UpdateApp(auditContext(c), tenantID, c.Param("id"), services.UpdateAppInput{
		Label:       req.Label,
		Description: req.Description,
		Icon:        req.Icon,
		Color:       req.Color,
		IsDefault:   req.IsDefault,
		SortOrder:   req.SortOrder,
		ActorID:     actorOf(c),

		HomeDashboardID: req.HomeDashboardID,
		HomeQueueIDs:    req.HomeQueueIDs,
		DefaultRoleIDs:  req.DefaultRoleIDs,
	})
	if err != nil {
		h.respondServiceError(c, err, "Update", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"app": app}})
}

// Delete handles DELETE /admin/apps/:id: soft-deletes the app (+ its nav rows, same tx).
func (h *AdminAppHandler) Delete(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	if err := h.svc.DeleteApp(auditContext(c), tenantID, c.Param("id")); err != nil {
		h.respondServiceError(c, err, "Delete", tenantID)
		return
	}
	noContentWV(c)
}

// ReplaceNav handles PUT /admin/apps/:id/nav: replace-all nav write. Body is a bare JSON array
// [{itemType,refId,label?}] in the desired order; every refId is validated against the tenant
// (422 APP_NAV_REF_INVALID otherwise). An empty array clears the nav.
func (h *AdminAppHandler) ReplaceNav(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req []navItemRequest
	if !bindJSON(c, &req) {
		return
	}
	items := make([]services.AppNavItemInput, 0, len(req))
	for _, it := range req {
		items = append(items, services.AppNavItemInput{ItemType: it.ItemType, RefID: it.RefID, Label: it.Label})
	}
	saved, err := h.svc.ReplaceNav(auditContext(c), tenantID, c.Param("id"), items)
	if err != nil {
		h.respondServiceError(c, err, "ReplaceNav", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"items": saved}})
}

// ACLBackfill handles POST /admin/apps/:id/acl-backfill?dryRun=true (TRỤ 2 mục 4 gap-closer):
// walks every entity in the app's nav and re-applies the SAME idempotent default-role seed
// CreateEntity runs at entity-creation time (AppACLSeedService.SeedForEntity) — closing the gap
// for entities that predate that wiring or were created without an appId. Always 200 with the
// visible {seeded, skipped, failed} counts (never a silent 204) — per spec, this is a
// deliberate, hard-to-reverse grant-expansion action, so the caller must see exactly what
// happened. ?dryRun=true (any other value, including absent, is treated as false) computes the
// SAME classification without writing anything.
func (h *AdminAppHandler) ACLBackfill(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	dryRun := strings.EqualFold(strings.TrimSpace(c.Query("dryRun")), "true")
	// ?targetTenant= aims the repair at a customer workspace. Operator-gated through the SAME body
	// the install uses; the app id is then resolved inside that tenant, so an id from the caller's
	// own tenant simply answers APP_NOT_FOUND there rather than writing anything.
	if targetSlug := strings.TrimSpace(c.Query("targetTenant")); targetSlug != "" {
		targetID, ok := resolveCrossTenantTarget(c, h.logger, h.opGate, h.tenants, h.frozen,
			"handlers.AdminApp.ACLBackfill", tenantID, targetSlug)
		if !ok {
			return
		}
		tenantID = targetID
	}
	result, err := h.svc.ACLBackfill(c.Request.Context(), tenantID, c.Param("id"), actorOf(c), dryRun)
	if err != nil {
		h.respondServiceError(c, err, "ACLBackfill", tenantID)
		return
	}
	jsonWV(c, http.StatusOK, gin.H{"data": result})
}

// respondServiceError maps an app-service error to the right HTTP status: a ValidationError
// goes through the shared statusForCode (APP_NOT_FOUND → 404, APP_ALREADY_EXISTS → 409,
// APP_NAME_INVALID / APP_NAV_REF_INVALID / APP_LABEL_REQUIRED → 422); anything else is a
// logged 500.
func (h *AdminAppHandler) respondServiceError(c *gin.Context, err error, op, tenantID string) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
		return
	}
	h.logger.Error("handlers.AdminApp."+op, zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xử lý yêu cầu app", "code": "APP_OP_FAILED"})
}
