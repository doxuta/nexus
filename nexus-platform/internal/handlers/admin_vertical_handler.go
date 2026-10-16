package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
	"nexus/platform/internal/services/vertical_pack"
)

// verticalPackService is the consumer-side slice of *vertical_pack.Service the handler needs.
// Declared here so the MS-C gate unit-tests run against a fake (mirrors tenantProvisioner).
type verticalPackService interface {
	Export(ctx context.Context, tenantID string, entityNames []string, includeData bool, manifest *vertical_pack.ExportManifest) (*vertical_pack.Pack, []string, error)
	EntityDependencies(ctx context.Context, tenantID string, names []string) ([]vertical_pack.EntityDependency, error)
	Install(ctx context.Context, tenantID, userID string, pack *vertical_pack.Pack) (*vertical_pack.InstallResult, error)
	DryRunInstall(ctx context.Context, tenantID string, pack *vertical_pack.Pack) (*vertical_pack.InstallPlan, error)
	ListInstalls(ctx context.Context, tenantID string) ([]vertical_pack.InstallRecord, error)
	DiffInstalled(ctx context.Context, tenantID, slug string) (*vertical_pack.DiffReport, error)
	PlanUpgrade(ctx context.Context, tenantID, slug string, pack *vertical_pack.Pack, source string) (*vertical_pack.UpgradePlan, error)
	ApplyUpgrade(ctx context.Context, tenantID, userID, slug string, pack *vertical_pack.Pack, source, planHash string, decisions []vertical_pack.UpgradeDecision) (*vertical_pack.UpgradeRun, error)
	ListUpgrades(ctx context.Context, tenantID, slug string, limit int) ([]vertical_pack.UpgradeRun, error)
}

// tenantSlugResolver resolves an ACTIVE tenant by its login slug for the MS-C cross-tenant
// install target. Satisfied by *auth.UserRepository (ResolveTenantBySlug folds unknown /
// soft-deleted / non-active into ONE ErrTenantNotFound — exactly the anti-enumeration contract
// spec §3.1 requires for the 404).
type tenantSlugResolver interface {
	ResolveTenantBySlug(ctx context.Context, slug string) (*auth.Tenant, error)
}

// AdminVerticalHandler exposes the vertical seed-pack endpoints: EXPORT the current tenant's
// metadata (entities/fields/relationships/layouts/roles) as a reusable JSON pack, and INSTALL a
// pack (uploaded, or a named built-in) into the current tenant. The tenant is always taken from
// the verified JWT claim (never a header/body) via tenantOf, and the routes sit behind the
// admin gate (RequireAdmin) — packaging schema is an admin operation.
//
// MS-C (../specs/027-handover-wizard/spec.md §3.1): POST /admin/verticals/install additionally accepts
// ?targetTenant=<slug> — install into ANOTHER tenant, gated on the caller being a platform
// operator (the SAME live-DB predicate middleware.RequirePlatformOperator enforces on
// /admin/tenants; re-implemented in-handler because the gate must fire per-QUERY-PARAM, which a
// route-mounted middleware cannot express). Absent param ⇒ behavior byte-identical to before.
type AdminVerticalHandler struct {
	svc    verticalPackService
	logger *zap.Logger
	// MS-C optional deps (WithCrossTenantInstall). All-or-nothing: while unwired, any request
	// carrying ?targetTenant fails CLOSED with the uniform operator 403.
	opGate  operatorGate        // live is_platform_operator read (same repo the middleware uses)
	frozen  tenantFreezeChecker // specs/068: write-freeze of the TARGET tenant (nil = not enforced)
	tenants tenantSlugResolver  // slug → ACTIVE tenant (anti-enumeration single sentinel)
	auditor audit.Auditor       // source-tenant __handover__ event; never nil (noop default)
}

// NewAdminVerticalHandler wires the handler over the vertical-pack service.
func NewAdminVerticalHandler(svc verticalPackService, logger *zap.Logger) *AdminVerticalHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminVerticalHandler{svc: svc, logger: logger, auditor: audit.NewNoopAuditor()}
}

// WithTenantFreeze arms the target-tenant write-freeze check of the cross-tenant path. Chainable.
func (h *AdminVerticalHandler) WithTenantFreeze(f tenantFreezeChecker) *AdminVerticalHandler {
	h.frozen = f
	return h
}

// WithCrossTenantInstall arms the MS-C ?targetTenant path: gate reads the caller's LIVE
// is_platform_operator row (satisfied by *auth.UserRepository — the same repo /admin/tenants'
// RequirePlatformOperator uses, so the two gates can never drift), tenants resolves the target
// slug, and auditor receives the source-tenant __handover__ event (nil keeps the noop). Chainable.
func (h *AdminVerticalHandler) WithCrossTenantInstall(gate operatorGate, tenants tenantSlugResolver, auditor audit.Auditor) *AdminVerticalHandler {
	h.opGate = gate
	h.tenants = tenants
	if auditor != nil {
		h.auditor = auditor
	}
	return h
}

// RegisterRoutes mounts the vertical routes under the given (admin-gated) group. The static
// /admin/verticals segments resolve ahead of the catch-all /:entity dispatcher.
func (h *AdminVerticalHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/verticals/export", h.Export)
	rg.GET("/admin/verticals/dependencies", h.Dependencies)
	rg.POST("/admin/verticals/install", h.Install)
	rg.GET("/admin/verticals/builtin", h.ListBuiltin)
	rg.GET("/admin/verticals/installs", h.ListInstalls)
	rg.GET("/admin/verticals/installs/:slug/diff", h.DiffInstall)
	rg.POST("/admin/verticals/installs/:slug/upgrade-plan", h.UpgradePlan)
	rg.POST("/admin/verticals/installs/:slug/upgrade", h.ApplyUpgrade)
	rg.GET("/admin/verticals/installs/:slug/upgrades", h.ListUpgrades)
}

// Export handles GET /admin/verticals/export?entities=a,b&includeData=true — it packages the named
// entities (or ALL of the tenant's entities when ?entities is omitted) plus their fields/
// relationships (incl. N-N)/layouts and the tenant's roles into a single JSON pack that
// re-installs cleanly into another tenant. ?includeData=true (default false) additionally inlines
// the entities' records as Samples (computed keys stripped, relation FKs rewritten to @refs) so a
// seeded pack can be handed over; the default is a metadata-only pack. An unknown entity name is 404.
func (h *AdminVerticalHandler) Export(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}

	var entities []string
	if raw := strings.TrimSpace(c.Query("entities")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			if p := strings.TrimSpace(part); p != "" {
				entities = append(entities, p)
			}
		}
	}

	includeData := parseBoolQuery(c.Query("includeData"))

	// PACK-EXPORT-APPSLUG: optional source-app identity so the pack's manifest carries a real
	// slug/name/icon and re-install mints a correctly-named sidebar app. Absent appSlug → generic
	// manifest (no app minted), the pre-slice behavior.
	var manifest *vertical_pack.ExportManifest
	if slug := strings.TrimSpace(c.Query("appSlug")); slug != "" {
		manifest = &vertical_pack.ExportManifest{
			Slug:        slug,
			Name:        strings.TrimSpace(c.Query("appName")),
			Icon:        strings.TrimSpace(c.Query("appIcon")),
			Description: strings.TrimSpace(c.Query("appDescription")),
		}
	}

	// warnings names everything the pack CANNOT faithfully carry (a workflow on an excluded
	// entity, an unresolvable assign team, a report with joins, a truncated sample dump…) so the
	// export dialog can show an honest included/excluded manifest instead of silently dropping.
	pack, warnings, err := h.svc.Export(c.Request.Context(), tenantID, entities, includeData, manifest)
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.AdminVertical.Export", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xuất vertical pack", "code": "VERTICAL_EXPORT_FAILED"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"pack": pack, "warnings": warnings}})
}

// Dependencies handles GET /admin/verticals/dependencies?entities=a,b — the tables the named
// ones need but did not name (rollup children, relation targets), so the handover wizard can
// pre-tick them (queue #296). Tenant from the JWT; read-only.
func (h *AdminVerticalHandler) Dependencies(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var names []string
	for _, part := range strings.Split(c.Query("entities"), ",") {
		if p := strings.TrimSpace(part); p != "" {
			names = append(names, p)
		}
	}
	deps, err := h.svc.EntityDependencies(c.Request.Context(), tenantID, names)
	if err != nil {
		h.logger.Error("handlers.AdminVertical.Dependencies", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể tính các bảng phụ thuộc", "code": "VERTICAL_DEPENDENCIES_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": deps})
}

// installRequest is the POST /admin/verticals/install body. Provide EITHER an inline `pack`
// (a full vertical_pack.Pack JSON) OR a `builtin` slug naming one of the shipped packs
// ("crm", "clinic"). If both are present, the inline pack wins. `dryRun` (body flag, or the
// `?dryRun=true` query param) runs the full validate/diff WITHOUT writing anything and returns a
// plan the deploy wizard renders — either source turns it on.
type installRequest struct {
	Builtin string              `json:"builtin,omitempty"`
	Pack    *vertical_pack.Pack `json:"pack,omitempty"`
	DryRun  bool                `json:"dryRun,omitempty"`
}

// Install handles POST /admin/verticals/install — it creates every entity/field/relationship/
// layout/role in the supplied (or named built-in) pack into the current tenant. Install is
// idempotent (an entity/field/role already present is skipped) and best-effort reversible (on a
// hard failure the entities THIS install created are soft-deleted). A validation failure in any
// resource (bad field type, unknown relation target, invalid grant) surfaces with the underlying
// service's code/status. The response reports what was created vs. skipped.
//
// When dryRun is set (via `?dryRun=true` OR body `{"dryRun":true}`) it instead runs the full
// validate/diff WITHOUT writing anything and returns a plan (what would be created vs. skipped,
// per pass, plus pre-flight conflicts) the deploy wizard renders — no metadata, obj_* rows, or
// install-ledger entries are touched.
//
// MS-C (?targetTenant=<slug>, ../specs/027-handover-wizard/spec.md §3.1): with the param present the caller must
// be a platform operator (fresh DB read — 403 byte-identical to RequireAdmin otherwise) and the
// slug must name an ACTIVE tenant (404 TENANT_NOT_FOUND for unknown AND inactive alike —
// anti-enumeration, one code). Every install/dry-run variant then runs with tenantID = TARGET
// (all installer passes are tenant-scoped by parameter — nothing reads the context tenant below
// this handler), and a successful real install into a DIFFERENT tenant emits a __handover__
// audit event in the SOURCE tenant (actor = operator, detail = target slug + pack name).
// targetTenant resolving to the caller's own tenant behaves exactly like the absent param.
func (h *AdminVerticalHandler) Install(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}

	// MS-C: authorize + resolve the cross-tenant target BEFORE touching the body, so a
	// non-operator learns nothing (not even whether the payload parses) beyond the uniform 403.
	sourceTenantID := tenantID
	targetSlug := strings.TrimSpace(c.Query("targetTenant"))
	if targetSlug != "" {
		targetID, ok := h.resolveInstallTarget(c, sourceTenantID, targetSlug)
		if !ok {
			return
		}
		tenantID = targetID
	}

	var req installRequest
	if !bindJSON(c, &req) {
		return
	}

	pack := req.Pack
	if pack == nil {
		slug := strings.ToLower(strings.TrimSpace(req.Builtin))
		if slug == "" {
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error": "Cần cung cấp `pack` (nội dung) hoặc `builtin` (tên pack có sẵn)",
				"code":  "VERTICAL_PACK_REQUIRED",
			})
			return
		}
		bp, found := vertical_pack.BuiltinPack(slug)
		if !found {
			c.JSON(http.StatusNotFound, gin.H{
				"error": "Không tìm thấy vertical pack có sẵn: " + slug,
				"code":  "VERTICAL_BUILTIN_NOT_FOUND",
			})
			return
		}
		pack = bp
	}

	// Dry-run: either the ?dryRun query flag OR the body flag turns it on. With ?targetTenant it
	// plans against the TARGET (wizard step 4: "thấy trước … TRÊN TENANT KHÁCH"); zero writes, so
	// no audit event either.
	if req.DryRun || parseBoolQuery(c.Query("dryRun")) {
		plan, err := h.svc.DryRunInstall(auditContext(c), tenantID, pack)
		if err != nil {
			h.writeInstallError(c, tenantID, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"plan": plan}})
		return
	}

	sub, _ := middleware.SubjectFromContext(c)
	res, err := h.svc.Install(auditContext(c), tenantID, sub.UserID, pack)
	if err != nil {
		h.writeInstallError(c, tenantID, err)
		return
	}

	// MS-C dual audit: the TARGET side already has its sys_pack_installs ledger row (written by
	// Install, actor in result_json.installedBy); the SOURCE tenant gets the __handover__ event.
	// Emitted only for a genuinely cross-tenant apply — targetTenant=self ≡ no param, no event.
	if targetSlug != "" && tenantID != sourceTenantID {
		h.auditor.Record(c.Request.Context(), audit.Event{
			TenantID:    sourceTenantID,
			ActorUserID: sub.UserID,
			EntityName:  "__handover__",
			Action:      "install",
			Diff: audit.ComputeDiff(nil, map[string]any{
				"targetTenant": targetSlug,
				"pack":         packDisplayName(pack, req.Builtin),
			}),
			At: time.Now().UTC(),
		})
		h.logger.Info("handlers.AdminVertical.Install: cross-tenant install",
			zap.String("source_tenant_id", sourceTenantID),
			zap.String("target_tenant_id", tenantID),
			zap.String("target_slug", targetSlug),
			zap.String("operator_user_id", sub.UserID))
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"result": res}})
}

// resolveInstallTarget enforces the MS-C gate and resolves ?targetTenant. Order is load-bearing:
// (1) OPERATOR CHECK first — the caller's LIVE user row must be an active admin with
// is_platform_operator (the exact middleware.RequirePlatformOperator predicate; a DB error fails
// CLOSED with 500, a missing/deleted caller with the uniform 403) — so a non-operator can never
// probe which slugs exist; (2) SLUG RESOLUTION second — unknown, soft-deleted and inactive
// tenants all collapse into ONE 404 TENANT_NOT_FOUND (auth.ErrTenantNotFound). Unwired deps
// (WithCrossTenantInstall never called) fail closed at the 403. The response has been written
// whenever ok=false.
func (h *AdminVerticalHandler) resolveInstallTarget(c *gin.Context, callerTenantID, targetSlug string) (string, bool) {
	// Body moved to resolveCrossTenantTarget so the install, the diff and the ACL backfill share ONE
	// implementation of this decision. Kept as a method because the doc comment above documents the
	// MS-C contract and the call sites read better for it.
	return resolveCrossTenantTarget(c, h.logger, h.opGate, h.tenants, h.frozen,
		"handlers.AdminVertical.resolveInstallTarget", callerTenantID, targetSlug)
}

// packDisplayName names the installed pack for the __handover__ audit detail: the manifest
// name/slug when present (built-ins always carry one), else the legacy pack name, else the
// requested builtin slug, else a stable placeholder for a nameless uploaded pack.
func packDisplayName(pack *vertical_pack.Pack, builtin string) string {
	if pack != nil && pack.Manifest != nil {
		if n := strings.TrimSpace(pack.Manifest.Name); n != "" {
			return n
		}
		if s := strings.TrimSpace(pack.Manifest.Slug); s != "" {
			return s
		}
	}
	if pack != nil && strings.TrimSpace(pack.Name) != "" {
		return strings.TrimSpace(pack.Name)
	}
	if b := strings.TrimSpace(builtin); b != "" {
		return "builtin:" + strings.ToLower(b)
	}
	return "uploaded-pack"
}

// writeInstallError maps an install/dry-run error to the right HTTP status + code. Shared by the
// real-install and dry-run paths so both reject identically (empty pack → 422, ValidationError →
// its mapped status, anything else → 500).
func (h *AdminVerticalHandler) writeInstallError(c *gin.Context, tenantID string, err error) {
	if errors.Is(err, vertical_pack.ErrPackEmpty) {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Vertical pack không có entity nào", "code": "VERTICAL_PACK_EMPTY"})
		return
	}
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
		return
	}
	h.logger.Error("handlers.AdminVertical.Install", zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể cài đặt vertical pack", "code": "VERTICAL_INSTALL_FAILED"})
}

// ListBuiltin handles GET /admin/verticals/builtin — returns the shipped built-in packs as
// {slug, manifest, entityCount} entries (FE-CATCHUP-2) so a client can show each pack's REAL
// name/version/description pre-install instead of a static mirror. Read-only: nothing is
// installed and no tenant state is touched; the manifests come straight from the in-code
// registry (vertical_pack.BuiltinInfos). Envelope stays {data:{builtins:[...]}} — the items
// changed from bare slug strings to objects (the FE list page is the only caller).
func (h *AdminVerticalHandler) ListBuiltin(c *gin.Context) {
	if _, ok := tenantOf(c); !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"builtins": vertical_pack.BuiltinInfos()}})
}

// ListInstalls handles GET /admin/verticals/installs — returns the tenant's pack-install ledger
// (newest first) so the Studio can show which packs/versions are installed and when. Tenant-scoped.
func (h *AdminVerticalHandler) ListInstalls(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	installs, err := h.svc.ListInstalls(c.Request.Context(), tenantID)
	if err != nil {
		h.logger.Error("handlers.AdminVertical.ListInstalls", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể đọc lịch sử cài đặt", "code": "VERTICAL_INSTALLS_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"installs": installs}})
}

// DiffInstall handles GET /admin/verticals/installs/:slug/diff — the KHUON-SONG-P1 read-only
// provenance diff report (../specs/011-pack-provenance/spec.md §3): per pack item, unchanged | modified |
// missing | undiffable against the content recorded at install. Tenant-scoped, admin-gated by
// the surrounding group; a slug never installed in this tenant is 404.
func (h *AdminVerticalHandler) DiffInstall(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	// MS-C parity for the READ side. Install could target a customer workspace but the DIFF could
	// not, so after handover nobody could answer "what does the customer actually have now?" without
	// a database session — the one question every follow-up delivery starts from. Same gate, same
	// order, same uniform refusals as the install path (operator check first, so a non-operator
	// cannot probe which slugs exist; then slug resolution, where unknown/retired/inactive all
	// collapse into one 404).
	slug := strings.TrimSpace(c.Param("slug"))
	if targetSlug := strings.TrimSpace(c.Query("targetTenant")); targetSlug != "" {
		targetID, ok := h.resolveInstallTarget(c, tenantID, targetSlug)
		if !ok {
			return
		}
		tenantID = targetID
	}
	report, err := h.svc.DiffInstalled(c.Request.Context(), tenantID, slug)
	if err != nil {
		if errors.Is(err, vertical_pack.ErrInstallNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Chưa có bản cài nào của khuôn này", "code": "VERTICAL_INSTALL_NOT_FOUND"})
			return
		}
		h.logger.Error("handlers.AdminVertical.DiffInstall", zap.String("tenant_id", tenantID), zap.String("slug", slug), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể đối chiếu khuôn", "code": "VERTICAL_DIFF_FAILED"})
		return
	}
	report.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	c.JSON(http.StatusOK, gin.H{"data": report})
}

// parseBoolQuery interprets a query-string flag as a boolean. "true"/"1"/"yes"/"on"
// (case-insensitive) are true; everything else (incl. empty) is false — so an absent
// ?includeData reads as the metadata-only default.
func parseBoolQuery(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// upgradePlanRequest is the POST …/upgrade-plan body: EXACTLY one of `builtin` (a shipped pack by
// name) or `pack` (an uploaded pack file, kept raw so it can be decoded strictly).
type upgradePlanRequest struct {
	Builtin string          `json:"builtin,omitempty"`
	Pack    json.RawMessage `json:"pack,omitempty"`
}

// unknownFieldRe pulls the field name out of encoding/json's DisallowUnknownFields error.
var unknownFieldRe = regexp.MustCompile(`unknown field "([^"]*)"`)

// UpgradePlan handles POST /admin/verticals/installs/:slug/upgrade-plan[?targetTenant=<slug>] —
// the READ-ONLY 3-way upgrade plan (specs/069 L1): per item, what the new pack changed, what the
// builder changed, and whether both did. Writes nothing. Same gate order as Install: tenant/target
// authorization BEFORE the body is read, so a non-operator learns nothing from a malformed body.
func (h *AdminVerticalHandler) UpgradePlan(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	slug := strings.TrimSpace(c.Param("slug"))
	if targetSlug := strings.TrimSpace(c.Query("targetTenant")); targetSlug != "" {
		targetID, ok := h.resolveInstallTarget(c, tenantID, targetSlug)
		if !ok {
			return
		}
		tenantID = targetID
	}

	var req upgradePlanRequest
	if !bindJSON(c, &req) {
		return
	}
	pack, source, ok := h.upgradeSource(c, req)
	if !ok {
		return
	}

	plan, err := h.svc.PlanUpgrade(c.Request.Context(), tenantID, slug, pack, source)
	if err != nil {
		if errors.Is(err, vertical_pack.ErrInstallNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Chưa có bản cài nào của khuôn này", "code": "VERTICAL_INSTALL_NOT_FOUND"})
			return
		}
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.AdminVertical.UpgradePlan",
			zap.String("tenant_id", tenantID), zap.String("slug", slug), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể lập kế hoạch nâng cấp", "code": "VERTICAL_UPGRADE_PLAN_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"plan": plan}})
}

// upgradeSource resolves the new pack of an upgrade request: a shipped builtin, or an uploaded
// file decoded STRICTLY (DisallowUnknownFields) — a field this server does not know would
// otherwise be silently dropped and the plan would lie about what the file contains (FR-004).
func (h *AdminVerticalHandler) upgradeSource(c *gin.Context, req upgradePlanRequest) (*vertical_pack.Pack, string, bool) {
	hasPack := len(req.Pack) > 0 && !bytes.Equal(bytes.TrimSpace(req.Pack), []byte("null"))
	builtin := strings.ToLower(strings.TrimSpace(req.Builtin))
	if hasPack == (builtin != "") { // neither, or both
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "Cần đúng một trong `pack` (nội dung) hoặc `builtin` (tên pack có sẵn)",
			"code":  "VERTICAL_PACK_REQUIRED",
		})
		return nil, "", false
	}
	if hasPack {
		dec := json.NewDecoder(bytes.NewReader(req.Pack))
		dec.DisallowUnknownFields()
		var pack vertical_pack.Pack
		if err := dec.Decode(&pack); err != nil {
			if m := unknownFieldRe.FindStringSubmatch(err.Error()); m != nil {
				c.JSON(http.StatusUnprocessableEntity, gin.H{
					"error":   "Tệp gói có trường mà máy chủ này không biết: " + m[1],
					"code":    "PACK_UPGRADE_UNKNOWN_FIELD",
					"details": gin.H{"field": m[1]},
				})
				return nil, "", false
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": "Tệp gói không hợp lệ", "code": "INVALID_BODY"})
			return nil, "", false
		}
		return &pack, "upload", true
	}
	bp, found := vertical_pack.BuiltinPack(builtin)
	if !found {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "Không tìm thấy vertical pack có sẵn: " + builtin,
			"code":  "VERTICAL_BUILTIN_NOT_FOUND",
		})
		return nil, "", false
	}
	return bp, "builtin", true
}

// upgradeApplyRequest is the POST …/upgrade body: the same source as upgrade-plan, the plan hash the
// caller reviewed and its per-item choices (items without one use the plan's default).
type upgradeApplyRequest struct {
	Builtin   string                          `json:"builtin,omitempty"`
	Pack      json.RawMessage                 `json:"pack,omitempty"`
	PlanHash  string                          `json:"planHash"`
	Decisions []vertical_pack.UpgradeDecision `json:"decisions"`
	// ConfirmImmediate: the builder ticked "Áp ngay cho khách" (L7) for changes that reach a delivered customer at once, with no trace.
	ConfirmImmediate bool `json:"confirmImmediate,omitempty"`
}

// upgradeApplyDeadline outlasts the service's 2-minute run budget plus bookkeeping, so the
// response is not cut by the server-wide WriteTimeout while the run is still finishing.
const upgradeApplyDeadline = 2*time.Minute + 15*time.Second

// ApplyUpgrade handles POST /admin/verticals/installs/:slug/upgrade[?targetTenant=<slug>] — applies
// the reviewed plan item by item (specs/069 L2a). Gate order matches Install/UpgradePlan: operator
// and target tenant BEFORE the body. The run continues if the client disconnects (service-side).
func (h *AdminVerticalHandler) ApplyUpgrade(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	sourceTenantID := tenantID
	slug := strings.TrimSpace(c.Param("slug"))
	targetSlug := strings.TrimSpace(c.Query("targetTenant"))
	if targetSlug != "" {
		targetID, ok := h.resolveInstallTarget(c, tenantID, targetSlug)
		if !ok {
			return
		}
		tenantID = targetID
	}
	var req upgradeApplyRequest
	if !bindJSON(c, &req) {
		return
	}
	if strings.TrimSpace(req.PlanHash) == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Thiếu planHash của kế hoạch đã xem", "code": "PACK_UPGRADE_PLAN_HASH_REQUIRED"})
		return
	}
	pack, source, ok := h.upgradeSource(c, upgradePlanRequest{Builtin: req.Builtin, Pack: req.Pack})
	if !ok {
		return
	}
	if err := http.NewResponseController(c.Writer).SetWriteDeadline(time.Now().Add(upgradeApplyDeadline)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		h.logger.Warn("handlers.AdminVertical.ApplyUpgrade: set write deadline", zap.String("tenant_id", tenantID), zap.Error(err))
	}

	sub, _ := middleware.SubjectFromContext(c)
	actx := auditContext(c)
	if req.ConfirmImmediate {
		actx = vertical_pack.WithConfirmImmediate(actx)
	}
	run, err := h.svc.ApplyUpgrade(actx, tenantID, sub.UserID, slug, pack, source, strings.TrimSpace(req.PlanHash), req.Decisions)
	if err != nil {
		h.writeUpgradeError(c, "ApplyUpgrade", tenantID, slug, err)
		return
	}
	h.auditor.Record(c.Request.Context(), audit.Event{
		TenantID: tenantID, ActorUserID: sub.UserID, EntityName: "__pack_upgrade__", Action: "apply",
		Diff: audit.ComputeDiff(nil, map[string]any{"slug": slug, "from": run.FromVersions, "to": run.ToVersion, "counts": run.Counts}),
		At:   time.Now().UTC(),
	})
	if tenantID != sourceTenantID {
		h.auditor.Record(c.Request.Context(), audit.Event{
			TenantID: sourceTenantID, ActorUserID: sub.UserID, EntityName: "__pack_upgrade__", Action: "apply",
			Diff: audit.ComputeDiff(nil, map[string]any{"targetTenant": targetSlug, "slug": slug, "to": run.ToVersion}),
			At:   time.Now().UTC(),
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"run": run}})
}

// ListUpgrades handles GET /admin/verticals/installs/:slug/upgrades[?limit=][&targetTenant=] — the
// upgrade history of one pack, newest first (default 10, max 50).
func (h *AdminVerticalHandler) ListUpgrades(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	slug := strings.TrimSpace(c.Param("slug"))
	if targetSlug := strings.TrimSpace(c.Query("targetTenant")); targetSlug != "" {
		targetID, ok := h.resolveInstallTarget(c, tenantID, targetSlug)
		if !ok {
			return
		}
		tenantID = targetID
	}
	limit, _ := strconv.Atoi(c.Query("limit")) // junk ⇒ 0 ⇒ the service default
	runs, err := h.svc.ListUpgrades(c.Request.Context(), tenantID, slug, limit)
	if err != nil {
		h.writeUpgradeError(c, "ListUpgrades", tenantID, slug, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"upgrades": runs}})
}

// writeUpgradeError maps an upgrade error: service ValidationErrors keep their code/status/details,
// an unknown install is 404, anything else is a logged 500.
func (h *AdminVerticalHandler) writeUpgradeError(c *gin.Context, op, tenantID, slug string, err error) {
	if errors.Is(err, vertical_pack.ErrInstallNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Chưa có bản cài nào của khuôn này", "code": "VERTICAL_INSTALL_NOT_FOUND"})
		return
	}
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
		return
	}
	h.logger.Error("handlers.AdminVertical."+op, zap.String("tenant_id", tenantID), zap.String("slug", slug), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể nâng cấp gói", "code": "VERTICAL_UPGRADE_FAILED"})
}
