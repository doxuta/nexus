package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/workqueue"
)

// appStore is the consumer-side slice of the app repository (appmeta.SQLRepository in
// production). Every method is tenant-scoped in the real implementation (INV-5).
type appStore interface {
	Create(ctx context.Context, in appmeta.CreateInput) (string, error)
	GetByID(ctx context.Context, tenantID, id string) (*appmeta.App, error)
	GetByName(ctx context.Context, tenantID, name string) (*appmeta.App, error)
	List(ctx context.Context, tenantID string) ([]appmeta.App, error)
	Update(ctx context.Context, tenantID, id string, in appmeta.UpdateInput) error
	SoftDelete(ctx context.Context, tenantID, id string) error
	ReplaceNav(ctx context.Context, tenantID, appID string, items []appmeta.NavItemInput) error
	ListNav(ctx context.Context, tenantID, appID string) ([]appmeta.NavItem, error)
}

// appEntityLister resolves the tenant's entity registry — used to validate entity nav refs
// and to project name/label/icon into the nav read. *metadata.Repository in production.
type appEntityLister interface {
	ListEntities(tenantID string) ([]metadata.EntityDefinition, error)
}

// appDashboardLister resolves the tenant's dashboards — used to validate dashboard nav refs
// and to project names into the nav read. *dashboard.SQLRepository in production.
type appDashboardLister interface {
	List(ctx context.Context, tenantID string) ([]dashboard.Record, error)
}

// appNavACL is the per-entity "view" decision the nav read filters through (INV-3) — the
// SAME decider the ⌘K assistant adapter and middleware.RequireACL use (*services.ACLService).
type appNavACL interface {
	CanViewEntity(ctx context.Context, tenantID, roleID, entityName string) (bool, error)
}

// appReadinessRoles resolves which roles (if any) currently hold a "view" grant on any of a set
// of entity names — TRỤ 2 mục 4 readiness check (b): "≥1 vai không-admin có grant view trên
// entity của app". Implemented by *auth.RoleRepository.
type appReadinessRoles interface {
	RolesGrantingView(ctx context.Context, tenantID string, entityNames []string) ([]string, error)
}

// appReadinessUsers resolves the tenant's users — TRỤ 2 mục 4 readiness check (c): "≥1 user
// không-admin mang vai đó". Implemented by *auth.UserRepository (ListUsers already exists; a
// small tenant's user list is cheap to scan in-process rather than adding a specialized query).
type appReadinessUsers interface {
	ListUsers(ctx context.Context, tenantID string) ([]auth.User, error)
}

// appReadinessFields resolves an entity's active fields — TRỤ 2 mục 4 readiness check (a):
// "mọi entity trong nav có ≥1 field". Implemented by *metadata.Repository (ListFields already
// exists — the same method entity_orchestration.go depends on via MetadataWriter).
type appReadinessFields interface {
	ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error)
}

// appACLSeeder is the single-entity seed primitive ACLBackfill composes over — satisfied by
// *AppACLSeedService (app_acl_seed.go), the SAME service CreateEntity calls at entity-create
// time. Declared consumer-side (mirrors every other appReadiness*/appEntityLister dependency
// above) so AppService stays unit-testable with a fake.
type appACLSeeder interface {
	SeedForEntity(ctx context.Context, tenantID, appID, entityName string) (seeded bool, warning string, err error)
}

// appHomeQueues / appHomeRoles validate the ids the builder picks for an app's home queues and
// per-role default (*workqueue.Repository and *auth.RoleRepository in production). Tenant-scoped.
type appHomeQueues interface {
	ListAll(ctx context.Context, tenantID string) ([]workqueue.Queue, error)
}
type appHomeRoles interface {
	RoleExists(ctx context.Context, tenantID, id string) (bool, error)
}

// appNameRe is the app slug shape: lowercase snake starting with a letter, ≤60 chars — the
// entity-name convention widened to the VARCHAR(100) column, capped so the soft-delete
// tombstone suffix ("__deleted_<id8>", 18 chars) always fits.
var appNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,59}$`)

// IsAppSlug reports whether s (trimmed, lowercased) has the app slug shape. Shared with the AI
// builder's context validator so the two can never disagree about what an app name looks like.
func IsAppSlug(s string) bool { return appNameRe.MatchString(strings.ToLower(strings.TrimSpace(s))) }

// Column-width caps (security LOW-1): reject over-long values with a 422 instead of letting
// MySQL strict mode turn them into a 500 (or non-strict silently truncate). Rune-counted.
const (
	maxAppLabelRunes = 100 // sys_apps.label VARCHAR(100), also sys_app_nav_items.label
	maxAppIconRunes  = 50  // sys_apps.icon  VARCHAR(50)
	maxAppColorRunes = 20  // sys_apps.color VARCHAR(20)
)

// capRunes rejects a value longer than max runes with APP_FIELD_TOO_LONG (422).
func capRunes(field, val string, max int) error {
	if len([]rune(val)) > max {
		return invalid("APP_FIELD_TOO_LONG", "%s vượt quá %d ký tự", field, max)
	}
	return nil
}

// ValidateAppConfig is the config-time validator for an app's identity fields — the ONE body
// shared by the apply path (CreateApp) and the AI Builder's propose_app tool (validator parity,
// ../specs/010-app-container/spec.md §7 Phase 3): slug shape (APP_NAME_INVALID), label required
// (APP_LABEL_REQUIRED), and the column-width caps (APP_FIELD_TOO_LONG). Inputs may be raw
// (untrimmed, mixed-case name) — normalization happens here, mirroring what CreateApp persists.
// sortOrder is deliberately NOT a parameter: the tool never proposes it, and the apply paths
// guard it separately (validAppSortOrder).
func ValidateAppConfig(name, label, icon, color string) error {
	if !IsAppSlug(name) {
		return invalid("APP_NAME_INVALID", "name app phải là slug [a-z][a-z0-9_]* tối đa 60 ký tự, nhận %q", name)
	}
	return validAppMutable(label, icon, color)
}

// validAppMutable guards the mutable identity fields (label required + column-width caps) —
// shared by ValidateAppConfig (create/propose, which also checks the slug) and UpdateApp
// (slug immutable, so no name check).
func validAppMutable(label, icon, color string) error {
	label = strings.TrimSpace(label)
	if label == "" {
		return invalid("APP_LABEL_REQUIRED", "label app là bắt buộc")
	}
	if err := capRunes("label", label, maxAppLabelRunes); err != nil {
		return err
	}
	if err := capRunes("icon", strings.TrimSpace(icon), maxAppIconRunes); err != nil {
		return err
	}
	return capRunes("color", strings.TrimSpace(color), maxAppColorRunes)
}

// validAppSortOrder guards the INT32 column (security LOW-2: out-of-range would 500 at MySQL).
func validAppSortOrder(sortOrder int) error {
	if sortOrder < math.MinInt32 || sortOrder > math.MaxInt32 {
		return invalid("APP_SORT_ORDER_INVALID", "sortOrder ngoài khoảng INT32")
	}
	return nil
}

// AppService implements the App-container business logic (../specs/010-app-container/spec.md §4):
// admin CRUD over /admin/apps, replace-all nav writes with tenant-pinned ref validation, and
// the ACL-filtered nav read behind GET /apps/:name/nav. App membership is UX visibility only —
// entity ACL remains the sole access boundary (INV-3), enforced here on every nav read.
type AppService struct {
	repo       appStore
	entities   appEntityLister
	dashboards appDashboardLister
	acl        appNavACL
	logger     *zap.Logger
	// readinessRoles / readinessUsers / readinessFields back Readiness (TRỤ 2 mục 4 item 5, the
	// "App sẵn sàng thao tác" honesty gate). All three nil (the default — no existing caller of
	// NewAppService sets them) makes Readiness a no-op (nil, nil): callers that never opt in via
	// WithReadiness keep GetApp's response exactly as it was before this field existed.
	readinessRoles  appReadinessRoles
	readinessUsers  appReadinessUsers
	readinessFields appReadinessFields
	// aclSeeder backs ACLBackfill (TRỤ 2 mục 4). nil (the default) makes ACLBackfill fail
	// closed with APP_ACL_BACKFILL_UNAVAILABLE rather than silently no-op.
	aclSeeder appACLSeeder
	// auditor records ACLBackfill's committed (non-dry-run) runs (RM-19). Defaults to a noop
	// so existing/legacy callers of NewAppService are unaffected; production wires the real
	// AsyncDispatcher via WithAuditor.
	auditor audit.Auditor
	// draftMode skips the document-audit row for a write that only reached the builder's draft
	// (specs/062 L4 / FR-022); nil = audit every write. "app" is not a draft type yet (ClassOf), so
	// main.go passes nil — see TestDocAudit_WiringMatchesDraftClasses.
	draftMode docWriteModer
	// roleCleanup lets DeleteApp remove the app's own auto-seeded roles (APP-DELETE-ORPHAN-ROLES,
	// WAVE 6). nil (the default) skips cleanup — the pre-slice behaviour.
	roleCleanup appRoleCleanup
	// homeQueues / homeRoles back the explicit home-queue and per-role-default pick. nil = those
	// keys are refused (fail closed) rather than stored unchecked.
	homeQueues appHomeQueues
	homeRoles  appHomeRoles
	// rec writes the direct version entry of an app create / delete on a delivered workspace
	// (specs/063 S7-5: those two always apply at once). nil = none.
	rec *VersionRecorder
}

// WithHomeRefs attaches the readers UpdateApp validates homeQueueIds / defaultRoleIds against.
func (s *AppService) WithHomeRefs(q appHomeQueues, r appHomeRoles) *AppService {
	s.homeQueues, s.homeRoles = q, r
	return s
}

// WithRecorder arms the direct version entries for app create / delete. Chainable.
func (s *AppService) WithRecorder(r *VersionRecorder) *AppService {
	s.rec = r
	return s
}

// appRoleCleanup is the slice of *auth.RoleRepository DeleteApp's role cleanup needs.
type appRoleCleanup interface {
	GetRoleByName(ctx context.Context, tenantID, name string) (*auth.Role, error)
	DeleteRole(ctx context.Context, tenantID, id string) error
}

// WithRoleCleanup attaches the role reader/deleter DeleteApp uses to take its auto-seeded roles
// with it. Chainable; nil is ignored (cleanup silently skipped, as before the slice).
func (s *AppService) WithRoleCleanup(rc appRoleCleanup) *AppService {
	if rc != nil {
		s.roleCleanup = rc
	}
	return s
}

// NewAppService wires the app service. acl may not be nil in production — the nav read
// fails closed for non-admins without it.
func NewAppService(repo appStore, entities appEntityLister, dashboards appDashboardLister, acl appNavACL, logger *zap.Logger) *AppService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AppService{repo: repo, entities: entities, dashboards: dashboards, acl: acl, logger: logger, auditor: audit.NewNoopAuditor()}
}

// WithACLSeeder attaches the TRỤ 2 mục 4 single-entity ACL seeder that ACLBackfill composes
// over. Chainable; a nil seeder is ignored (keeps ACLBackfill fail-closed-unavailable).
func (s *AppService) WithACLSeeder(seeder appACLSeeder) *AppService {
	if seeder != nil {
		s.aclSeeder = seeder
	}
	return s
}

// WithAuditor attaches the RM-19 auditor that ACLBackfill runs, app create / update / delete and
// nav writes (__app__, specs/062 L4) are recorded through, plus the optional draft mode that
// suppresses a document row for a draft-only write (nil = audit every write). Chainable; a nil
// auditor is ignored (keeps the default noop).
func (s *AppService) WithAuditor(a audit.Auditor, mode docWriteModer) *AppService {
	if a != nil {
		s.auditor = a
	}
	s.draftMode = mode
	return s
}

// WithReadiness attaches the role/user/field readers Readiness needs (TRỤ 2 mục 4 item 5).
// Chainable; production wiring lives in cmd/server/main.go (roleRepo, userRepo, metaRepo — all
// already constructed before appSvc). Any nil argument leaves readiness unavailable — Readiness
// degrades to (nil, nil) rather than a partial/misleading computation.
func (s *AppService) WithReadiness(roles appReadinessRoles, users appReadinessUsers, fields appReadinessFields) *AppService {
	s.readinessRoles = roles
	s.readinessUsers = users
	s.readinessFields = fields
	return s
}

// CreateAppInput is the payload to create an app.
type CreateAppInput struct {
	Name        string
	Label       string
	Description string
	Icon        string
	Color       string
	IsDefault   bool
	SortOrder   int
	ActorID     string
}

// UpdateAppInput is the full-replace payload for an app's mutable fields (slug immutable).
type UpdateAppInput struct {
	Label       string
	Description string
	Icon        string
	Color       string
	IsDefault   bool
	SortOrder   int
	ActorID     string
	// HomeDashboardID: nil = keep the current home dashboard (an old client that never sends the
	// key must not wipe it on this full-replace PUT); "" = clear; otherwise must be one of the
	// tenant's dashboards (APP_HOME_DASHBOARD_INVALID).
	HomeDashboardID *string
	// HomeQueueIDs: the work queues the app home shows. nil = keep; empty = clear (home derives the
	// queues from the menu tables again); otherwise each id must be a queue of the tenant.
	HomeQueueIDs *[]string
	// DefaultRoleIDs: roles that land in this app at sign-in. nil = keep; empty = clear; otherwise
	// each id must be a role of the tenant.
	DefaultRoleIDs *[]string
}

// AppNavItemInput is one nav reference in a replace-all write.
type AppNavItemInput struct {
	ItemType string
	RefID    string
	Label    string
}

// AppNavItemView is one resolved, ACL-visible nav entry returned by GetNav.
type AppNavItemView struct {
	Type  string `json:"type"`
	RefID string `json:"refId"`
	Label string `json:"label"`
	Icon  string `json:"icon,omitempty"`
	Href  string `json:"href"`
	// RefName is the referenced object's stable name (entity: table name; dashboard: its name).
	// Never serialized — GET /apps/:name/nav is unchanged; the AI context resolver reads it.
	RefName string `json:"-"`
}

// CreateApp validates and inserts an app. The slug is lowercased then validated
// (APP_NAME_INVALID); a taken slug is APP_ALREADY_EXISTS; label is required.
func (s *AppService) CreateApp(ctx context.Context, tenantID string, in CreateAppInput) (*appmeta.App, error) {
	if err := ValidateAppConfig(in.Name, in.Label, in.Icon, in.Color); err != nil {
		return nil, err
	}
	if err := validAppSortOrder(in.SortOrder); err != nil {
		return nil, err
	}
	name := strings.ToLower(strings.TrimSpace(in.Name))
	label := strings.TrimSpace(in.Label)
	id, err := s.repo.Create(ctx, appmeta.CreateInput{
		TenantID:    tenantID,
		Name:        name,
		Label:       label,
		Description: strings.TrimSpace(in.Description),
		Icon:        strings.TrimSpace(in.Icon),
		Color:       strings.TrimSpace(in.Color),
		IsDefault:   in.IsDefault,
		SortOrder:   in.SortOrder,
		CreatedBy:   strings.TrimSpace(in.ActorID),
	})
	if err != nil {
		if errors.Is(err, appmeta.ErrAppDuplicateName) {
			return nil, invalid("APP_ALREADY_EXISTS", "app %q đã tồn tại", name)
		}
		return nil, fmt.Errorf("services.AppService.CreateApp: %w", err)
	}
	app, err := s.repo.GetByID(ctx, tenantID, id)
	emitDocAudit(ctx, s.auditor, nil /* create applies at once */, tenantID, audit.AppMeta, id, audit.ActionCreate, nil, appAuditSnapshot(app))
	if err != nil {
		return nil, fmt.Errorf("services.AppService.CreateApp: read back: %w", err)
	}
	s.rec.RecordDirect(ctx, tenantID, "", DirectDoc{DocType: "app", DocKey: id, Label: app.Label, Op: "create", After: appAuditSnapshot(app)})
	return app, nil
}

// ListApps returns every app in the tenant in App Switcher order (admin list AND the
// non-admin /apps catalog — apps are not an access boundary, so no ACL filter here; the nav
// INSIDE an app is what gets filtered, INV-3).
func (s *AppService) ListApps(ctx context.Context, tenantID string) ([]appmeta.App, error) {
	apps, err := s.repo.List(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.AppService.ListApps: %w", err)
	}
	return apps, nil
}

// ListAppsForCaller is the catalog behind GET /apps. Admins see every app. Anyone else sees only
// the apps in which they can open something (an ACL-visible entity or any dashboard) — so a
// receptionist is not offered the tenant's CRM, and an empty app is not a dead end. The app is
// still a lens, not a boundary: entity ACL stays the only access control (INV-3); this only
// stops the switcher from advertising lenses that would render empty for the caller.
func (s *AppService) ListAppsForCaller(ctx context.Context, scope RecordScope, tenantID string) ([]appmeta.App, error) {
	apps, err := s.ListApps(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.AppService.ListAppsForCaller: %w", err)
	}
	for i := range apps {
		apps[i].DefaultForCaller = scope.RoleID != "" && idListHas(apps[i].DefaultRoleIDs, scope.RoleID)
	}
	if scope.IsAdmin {
		return apps, nil
	}
	out := make([]appmeta.App, 0, len(apps))
	for i := range apps {
		// An app whose front page is a dashboard is openable even before it has a menu entry.
		if apps[i].HomeDashboardID != "" {
			out = append(out, apps[i])
			continue
		}
		// ponytail: one GetNav per app (O(apps), each resolves the whole nav). Fine for the handful of
		// apps a workspace has; replace with a single "any visible item" query if a tenant passes ~50.
		_, items, nerr := s.GetNav(ctx, scope, tenantID, apps[i].Name)
		if nerr != nil {
			var ve *ValidationError
			if errors.As(nerr, &ve) && ve.Code == "APP_NOT_FOUND" {
				continue // deleted between List and GetNav: drop it, do not fail the whole catalog
			}
			return nil, fmt.Errorf("services.AppService.ListAppsForCaller: %w", nerr)
		}
		if len(items) > 0 {
			out = append(out, apps[i])
		}
	}
	return out, nil
}

// GetApp returns one tenant-scoped app plus its raw nav rows (admin detail).
func (s *AppService) GetApp(ctx context.Context, tenantID, id string) (*appmeta.App, []appmeta.NavItem, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil, invalid("APP_NOT_FOUND", "không tìm thấy app")
	}
	app, err := s.repo.GetByID(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, appmeta.ErrAppNotFound) {
			return nil, nil, invalid("APP_NOT_FOUND", "không tìm thấy app")
		}
		return nil, nil, fmt.Errorf("services.AppService.GetApp: %w", err)
	}
	items, err := s.repo.ListNav(ctx, tenantID, app.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("services.AppService.GetApp: %w", err)
	}
	return app, items, nil
}

// AppReadinessView is the "App sẵn sàng thao tác" honesty gate (TRỤ 2 mục 4 item 5,
// docs/NEXUS-MOT-XUONG-2026-07-29.md). The FE previously inferred readiness from a raw FIELD
// COUNT alone (AppOverview.tsx), which can read "ready" while every non-admin still hits a 403
// wall — exactly the G3 gap the rest of this slice closes. Ready is true ONLY when all three
// hold: every nav entity has ≥1 field, some non-admin role can view at least one of them, AND
// some non-admin user actually holds that role. An app with ZERO entities in its nav is NEVER
// ready — EntityCount gates Ready so an empty app cannot vacuously report AllEntitiesHaveFields.
type AppReadinessView struct {
	EntityCount             int      `json:"entityCount"`
	EntitiesMissingFields   []string `json:"entitiesMissingFields"`
	AllEntitiesHaveFields   bool     `json:"allEntitiesHaveFields"`
	HasNonAdminViewRole     bool     `json:"hasNonAdminViewRole"`
	HasNonAdminUserAssigned bool     `json:"hasNonAdminUserAssigned"`
	Ready                   bool     `json:"ready"`
}

// Readiness computes the AppReadinessView for an app's nav items (pass the SAME slice GetApp
// already returned — no second app/nav read). Returns (nil, nil) when WithReadiness was never
// called (readiness feature not wired for this caller) so existing GetApp-only call sites are
// unaffected. A read failure (fields/roles/users query) is returned as an error — the HANDLER
// decides whether to fail the whole app-detail response or degrade gracefully (see
// admin_app_handler.go AdminGet), matching this being a pure read with no write to compensate.
func (s *AppService) Readiness(ctx context.Context, tenantID string, items []appmeta.NavItem) (*AppReadinessView, error) {
	if s.readinessRoles == nil || s.readinessUsers == nil || s.readinessFields == nil {
		return nil, nil
	}

	ents, err := s.entities.ListEntities(tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.AppService.Readiness: list entities: %w", err)
	}
	byID := make(map[string]metadata.EntityDefinition, len(ents))
	for _, e := range ents {
		byID[e.ID] = e
	}

	view := &AppReadinessView{EntitiesMissingFields: []string{}}
	names := make([]string, 0, len(items))
	for _, it := range items {
		if it.ItemType != appmeta.ItemTypeEntity {
			continue
		}
		def, ok := byID[it.RefID]
		if !ok {
			continue // orphan ref (INV-4 cascade should have purged it) — skip like GetNav does
		}
		view.EntityCount++
		names = append(names, def.Name)
		flds, ferr := s.readinessFields.ListFields(ctx, tenantID, def.ID)
		if ferr != nil {
			return nil, fmt.Errorf("services.AppService.Readiness: list fields of %q: %w", def.Name, ferr)
		}
		if len(flds) == 0 {
			view.EntitiesMissingFields = append(view.EntitiesMissingFields, def.Name)
		}
	}
	view.AllEntitiesHaveFields = view.EntityCount > 0 && len(view.EntitiesMissingFields) == 0

	roleIDs, rerr := s.readinessRoles.RolesGrantingView(ctx, tenantID, names)
	if rerr != nil {
		return nil, fmt.Errorf("services.AppService.Readiness: roles granting view: %w", rerr)
	}
	view.HasNonAdminViewRole = len(roleIDs) > 0

	if view.HasNonAdminViewRole {
		roleSet := make(map[string]struct{}, len(roleIDs))
		for _, id := range roleIDs {
			roleSet[id] = struct{}{}
		}
		users, uerr := s.readinessUsers.ListUsers(ctx, tenantID)
		if uerr != nil {
			return nil, fmt.Errorf("services.AppService.Readiness: list users: %w", uerr)
		}
		for _, u := range users {
			// A customer owner carries an all-access role by design; counting it would turn
			// Readiness green without a single staff member who can open the app (specs/065 D).
			if u.IsAdmin || u.CanManageUsers || u.Status != statusActive || u.RoleID == nil {
				continue
			}
			if _, ok := roleSet[*u.RoleID]; ok {
				view.HasNonAdminUserAssigned = true
				break
			}
		}
	}

	view.Ready = view.EntityCount > 0 && view.AllEntitiesHaveFields && view.HasNonAdminViewRole && view.HasNonAdminUserAssigned
	return view, nil
}

// ACLBackfillFailure is one nav entity SeedForEntity's fail-open contract reported as unseeded
// (details.reason is the human-readable Vietnamese warning it returned) — the loop continues
// past it (fail-open PER entity: one bad entity never blocks the rest), but the caller must be
// able to see it; it is never silently dropped.
type ACLBackfillFailure struct {
	Entity string `json:"entity"`
	Reason string `json:"reason"`
}

// ACLBackfillResult is the outcome of POST /admin/apps/:id/acl-backfill (TRỤ 2 mục 4 gap-closer,
// docs/NEXUS-MOT-XUONG-2026-07-29.md §4 "11 bảng cũ đang 403"): SeedForEntity previously ran
// ONLY at entity-CREATE time (app_acl_seed.go, wired from admin_entity_handler.go), so any
// entity that predates that wiring — or was created without an appId — never received its app's
// default roles. Seeded/Skipped are entity NAMES; Skipped additionally covers nav items with
// nothing to seed (type-prefixed: "dashboard:<refId>" for a dashboard nav item, "orphan:<refId>"
// for an entity ref whose sys_entities row is gone — the INV-4 cascade should have purged it).
type ACLBackfillResult struct {
	Seeded  []string             `json:"seeded"`
	Skipped []string             `json:"skipped"`
	Failed  []ACLBackfillFailure `json:"failed"`
}

// ACLBackfill walks EVERY item in appID's nav and, for each one that resolves to a live entity,
// re-applies the SAME idempotent seed CreateEntity would have run at entity-creation time
// (AppACLSeedService.SeedForEntity — AddGrants never duplicates an existing grant row, so a
// re-run over an already-seeded entity is a safe no-op). Nav items with nothing to seed
// (dashboards, orphaned entity refs) land in Skipped, never Failed.
//
// dryRun=true computes the EXACT same Seeded/Skipped classification WITHOUT calling
// SeedForEntity at all — no role/grant write, no audit row. Because dryRun makes no attempt,
// Failed is always empty in that mode (nothing was attempted, so nothing could fail); the real
// run's per-entity failure (an unknown/cross-tenant appID mid-race, or a role-store error) is
// reported in Failed rather than aborting the remaining entities.
//
// Authorization is the caller's responsibility (the route sits behind RequireAdmin, like every
// other /admin/apps endpoint) — this method does not re-check admin itself.
func (s *AppService) ACLBackfill(ctx context.Context, tenantID, appID, actorID string, dryRun bool) (*ACLBackfillResult, error) {
	if s.aclSeeder == nil {
		return nil, invalid("APP_ACL_BACKFILL_UNAVAILABLE", "chức năng cấp quyền hàng loạt hiện không khả dụng")
	}
	// FR-032: this writes LIVE grants, so it decides from the LIVE menu — never from a menu draft
	// the builder has not published.
	app, items, err := s.GetApp(metaversion.WithLiveRead(ctx), tenantID, appID)
	if err != nil {
		return nil, err
	}

	ents, err := s.entities.ListEntities(tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.AppService.ACLBackfill: list entities: %w", err)
	}
	byID := make(map[string]metadata.EntityDefinition, len(ents))
	for _, e := range ents {
		byID[e.ID] = e
	}

	result := &ACLBackfillResult{Seeded: []string{}, Skipped: []string{}, Failed: []ACLBackfillFailure{}}
	for _, it := range items {
		if it.ItemType != appmeta.ItemTypeEntity {
			result.Skipped = append(result.Skipped, "dashboard:"+it.RefID)
			continue
		}
		def, ok := byID[it.RefID]
		if !ok {
			result.Skipped = append(result.Skipped, "orphan:"+it.RefID) // INV-4 cascade should have purged this
			continue
		}
		if dryRun {
			result.Seeded = append(result.Seeded, def.Name)
			continue
		}
		seeded, warning, serr := s.aclSeeder.SeedForEntity(ctx, tenantID, app.ID, def.Name)
		if serr != nil {
			s.logger.Warn("services.AppService.ACLBackfill",
				zap.String("tenant_id", tenantID), zap.String("app_id", app.ID), zap.String("entity", def.Name), zap.Error(serr))
		}
		if !seeded {
			reason := warning
			if reason == "" {
				reason = "không rõ lý do"
			}
			result.Failed = append(result.Failed, ACLBackfillFailure{Entity: def.Name, Reason: reason})
			continue
		}
		result.Seeded = append(result.Seeded, def.Name)
	}

	if !dryRun {
		// RM-19: a bulk grant-expansion action always leaves a trail, even when the app's nav
		// had nothing to seed. One row per call (not per entity) — see ACLBackfillResult doc.
		s.auditor.Record(ctx, audit.Event{
			TenantID:    tenantID,
			ActorUserID: actorID,
			EntityName:  audit.AppACLMeta,
			Action:      audit.ActionUpdate,
			Diff: audit.ComputeDiff(nil, map[string]any{
				"appId": app.ID, "appName": app.Name,
				"seeded": result.Seeded, "skipped": result.Skipped, "failed": result.Failed,
			}),
			At: time.Now().UTC(),
		})
	}
	return result, nil
}

// UpdateApp applies the mutable fields (label/description/icon/color/isDefault/sortOrder) to
// a tenant-scoped app. Setting isDefault clears the tenant's previous default in the same
// repo transaction (≤1 default per tenant — no APP_DEFAULT_CONFLICT, by design the service
// resolves it). Unknown id → APP_NOT_FOUND.
func (s *AppService) UpdateApp(ctx context.Context, tenantID, id string, in UpdateAppInput) (*appmeta.App, error) {
	id = strings.TrimSpace(id)
	if err := validAppMutable(in.Label, in.Icon, in.Color); err != nil {
		return nil, err
	}
	if err := validAppSortOrder(in.SortOrder); err != nil {
		return nil, err
	}
	label := strings.TrimSpace(in.Label)
	var home *string
	if in.HomeDashboardID != nil {
		h := strings.TrimSpace(*in.HomeDashboardID)
		if h != "" {
			ids, derr := s.tenantDashboardIDs(ctx, tenantID)
			if derr != nil {
				return nil, fmt.Errorf("services.AppService.UpdateApp: %w", derr)
			}
			if _, ok := ids[h]; !ok {
				return nil, invalid("APP_HOME_DASHBOARD_INVALID", "bảng điều khiển trang chủ không tồn tại")
			}
		}
		home = &h
	}
	queues, err := s.cleanIDs(in.HomeQueueIDs, "APP_HOME_QUEUE_INVALID", "hàng đợi trang chủ không tồn tại", func() (func(string) (bool, error), error) {
		if s.homeQueues == nil {
			return nil, nil
		}
		rows, qerr := s.homeQueues.ListAll(ctx, tenantID)
		if qerr != nil {
			return nil, qerr
		}
		have := make(map[string]struct{}, len(rows))
		for _, q := range rows {
			have[q.ID] = struct{}{}
		}
		return func(id string) (bool, error) { _, ok := have[id]; return ok, nil }, nil
	})
	if err != nil {
		return nil, err
	}
	roles, err := s.cleanIDs(in.DefaultRoleIDs, "APP_DEFAULT_ROLE_INVALID", "vai mặc định không tồn tại", func() (func(string) (bool, error), error) {
		if s.homeRoles == nil {
			return nil, nil
		}
		return func(id string) (bool, error) { return s.homeRoles.RoleExists(ctx, tenantID, id) }, nil
	})
	if err != nil {
		return nil, err
	}
	// Snapshot before the write (a repository may hand back the very row it then mutates); an unknown
	// id has no prior and 404s on the write below.
	beforeSnap := appAuditSnapshot(priorDoc(func() (*appmeta.App, error) { return s.repo.GetByID(ctx, tenantID, id) }))
	if err := s.repo.Update(ctx, tenantID, id, appmeta.UpdateInput{
		HomeDashboardID: home,
		HomeQueueIDs:    queues,
		DefaultRoleIDs:  roles,
		Label:           label,
		Description:     strings.TrimSpace(in.Description),
		Icon:            strings.TrimSpace(in.Icon),
		Color:           strings.TrimSpace(in.Color),
		IsDefault:       in.IsDefault,
		SortOrder:       in.SortOrder,
		UpdatedBy:       strings.TrimSpace(in.ActorID),
	}); err != nil {
		if errors.Is(err, appmeta.ErrAppNotFound) {
			return nil, invalid("APP_NOT_FOUND", "không tìm thấy app")
		}
		return nil, fmt.Errorf("services.AppService.UpdateApp: %w", err)
	}
	app, err := s.repo.GetByID(ctx, tenantID, id)
	emitDocAudit(ctx, s.auditor, s.draftMode, tenantID, audit.AppMeta, id, audit.ActionUpdate, beforeSnap, appAuditSnapshot(app))
	if err != nil {
		return nil, fmt.Errorf("services.AppService.UpdateApp: read back: %w", err)
	}
	return app, nil
}

// cleanIDs trims/dedupes a replace-list and checks every id exists via the checker built by mk
// (nil checker = the reader was never wired: any non-empty list is refused, fail closed). nil in =
// nil out (keep the stored list).
func (s *AppService) cleanIDs(in *[]string, code, msg string, mk func() (func(string) (bool, error), error)) (*[]string, error) {
	if in == nil {
		return nil, nil
	}
	out := make([]string, 0, len(*in))
	for _, id := range *in {
		if id = strings.TrimSpace(id); id != "" && !idListHas(out, id) {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return &out, nil
	}
	exists, err := mk()
	if err != nil {
		return nil, fmt.Errorf("services.AppService.cleanIDs: %w", err)
	}
	if exists == nil {
		return nil, invalid(code, "%s", msg)
	}
	for _, id := range out {
		ok, cerr := exists(id)
		if cerr != nil {
			return nil, fmt.Errorf("services.AppService.cleanIDs: %w", cerr)
		}
		if !ok {
			return nil, invalid(code, "%s", msg)
		}
	}
	return &out, nil
}

func idListHas(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// DeleteApp soft-deletes a tenant-scoped app (its nav rows are removed in the same repo
// transaction). Unknown id → APP_NOT_FOUND. Referenced entities/dashboards are untouched —
// an app is a lens, deleting it never deletes data.
func (s *AppService) DeleteApp(ctx context.Context, tenantID, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return invalid("APP_NOT_FOUND", "không tìm thấy app")
	}
	// Read the app BEFORE deleting: cleanup below needs its name to derive the seeded role names,
	// and after SoftDelete the row no longer resolves.
	app, err := s.repo.GetByID(ctx, tenantID, id)
	if err != nil && !errors.Is(err, appmeta.ErrAppNotFound) {
		return fmt.Errorf("services.AppService.DeleteApp: load app: %w", err)
	}
	if err := s.repo.SoftDelete(ctx, tenantID, id); err != nil {
		if errors.Is(err, appmeta.ErrAppNotFound) {
			return invalid("APP_NOT_FOUND", "không tìm thấy app")
		}
		return fmt.Errorf("services.AppService.DeleteApp: %w", err)
	}
	if app != nil {
		s.cleanupSeededRoles(ctx, tenantID, app.Name)
	}
	emitDocAudit(ctx, s.auditor, nil /* delete applies at once */, tenantID, audit.AppMeta, id, audit.ActionDelete, appAuditSnapshot(app), nil)
	label := id
	if app != nil {
		label = app.Label
	}
	s.rec.RecordDirect(ctx, tenantID, "", DirectDoc{DocType: "app", DocKey: id, Label: label, Op: "delete", Before: appAuditSnapshot(app)})
	return nil
}

// appRestorer is the repository slice RestoreApp needs; *appmeta.SQLRepository in production.
type appRestorer interface {
	Restore(ctx context.Context, tenantID, id, name string, in appmeta.UpdateInput) (bool, error)
}

// RestoreApp undoes an applied-at-once delete: the app comes back under its own id from the recorded
// properties (its menu does not: the delete purged it). A slug a live app took meanwhile is
// ROLLBACK_NAME_TAKEN and nothing changes (specs/063 FR-053, US9 #4).
func (s *AppService) RestoreApp(ctx context.Context, tenantID, id string, snap appAuditDoc) error {
	r, ok := s.repo.(appRestorer)
	if !ok {
		return errors.New("services.AppService.RestoreApp: store cannot restore")
	}
	home := snap.HomeDashboardID
	in := appmeta.UpdateInput{Label: snap.Label, Description: snap.Description, Icon: snap.Icon, Color: snap.Color,
		IsDefault: snap.IsDefault, SortOrder: snap.SortOrder, UpdatedBy: audit.ActorFromContext(ctx), HomeDashboardID: &home}
	restored, err := r.Restore(ctx, tenantID, id, snap.Name, in)
	if err != nil {
		if errors.Is(err, appmeta.ErrAppDuplicateName) {
			return invalid("ROLLBACK_NAME_TAKEN", "Tên app đã bị dùng cho app khác, không hoàn tác được")
		}
		return fmt.Errorf("services.AppService.RestoreApp: %w", err)
	}
	if !restored {
		return invalid("APP_NOT_FOUND", "không tìm thấy app")
	}
	app, err := s.repo.GetByID(ctx, tenantID, id)
	if err != nil {
		return fmt.Errorf("services.AppService.RestoreApp: read back: %w", err)
	}
	emitDocAudit(ctx, s.auditor, nil, tenantID, audit.AppMeta, id, audit.ActionCreate, nil, appAuditSnapshot(app))
	s.rec.RecordDirect(ctx, tenantID, "", DirectDoc{DocType: "app", DocKey: id, Label: app.Label, Op: "create", After: appAuditSnapshot(app)})
	return nil
}

// cleanupSeededRoles deletes the app's OWN auto-seeded roles (app_<name>_staff / _manager) when
// the app is deleted — creating an app seeds them (AppACLSeedService), so deleting the app must
// offer the symmetric teardown or every create-delete cycle racks up orphan roles (found live,
// WAVE 6: two orphans after one test app).
//
// Deliberately conservative, in order:
//   - Only the two DETERMINISTIC seeded names are considered — never a hand-made role.
//   - A role someone is STILL ASSIGNED TO is kept: users.role_id is ON DELETE SET NULL, so
//     deleting it would silently drop those users to deny-all. An admin gave people that role on
//     purpose; un-giving it is their decision, not a side effect of deleting an app. (The seeded
//     grants名 the app's entities, which outlive the app — the role keeps meaning something.)
//   - Best-effort fail-open with a warning: the app delete has already committed; a cleanup
//     failure must not turn a successful delete into a 500.
func (s *AppService) cleanupSeededRoles(ctx context.Context, tenantID, appName string) {
	if s.roleCleanup == nil || s.readinessUsers == nil {
		return // not wired (older harnesses / legacy callers): keep the pre-slice behaviour
	}
	users, err := s.readinessUsers.ListUsers(ctx, tenantID)
	if err != nil {
		s.logger.Warn("services.AppService.DeleteApp: role cleanup skipped (list users)",
			zap.String("tenant_id", tenantID), zap.String("app", appName), zap.Error(err))
		return
	}
	held := make(map[string]bool)
	for _, u := range users {
		if u.RoleID != nil {
			held[*u.RoleID] = true
		}
	}
	for _, suffix := range []string{"staff", "manager"} {
		name := appDefaultRoleName(appName, suffix)
		role, rerr := s.roleCleanup.GetRoleByName(ctx, tenantID, name)
		if rerr != nil || role == nil {
			continue // never seeded, already removed, or unreadable — nothing to do
		}
		if held[role.ID] {
			s.logger.Info("services.AppService.DeleteApp: seeded role kept (users still assigned)",
				zap.String("tenant_id", tenantID), zap.String("role", name))
			continue
		}
		if derr := s.roleCleanup.DeleteRole(ctx, tenantID, role.ID); derr != nil {
			s.logger.Warn("services.AppService.DeleteApp: seeded role cleanup failed",
				zap.String("tenant_id", tenantID), zap.String("role", name), zap.Error(derr))
			continue
		}
		s.logger.Info("services.AppService.DeleteApp: seeded role removed",
			zap.String("tenant_id", tenantID), zap.String("role", name))
	}
}

// ReplaceNav swaps an app's entire nav for the given ordered items. EVERY refId must resolve
// to a live object of the CALLER's tenant (entity or dashboard per itemType) — a cross-tenant
// or unknown ref, a bad type, or a duplicate (type, ref) pair is APP_NAV_REF_INVALID (422).
// Returns the saved nav rows in order.
func (s *AppService) ReplaceNav(ctx context.Context, tenantID, appID string, items []AppNavItemInput) ([]appmeta.NavItem, error) {
	app, oldNav, err := s.GetApp(ctx, tenantID, appID)
	if err != nil {
		return nil, err
	}
	beforeNav := appNavAuditSnapshot(oldNav)

	// Resolve the tenant's valid ref-id sets lazily per type (one registry read each).
	var entityIDs, dashboardIDs map[string]struct{}
	seen := make(map[string]struct{}, len(items))
	repoItems := make([]appmeta.NavItemInput, 0, len(items))
	for i, it := range items {
		itemType := strings.ToLower(strings.TrimSpace(it.ItemType))
		refID := strings.TrimSpace(it.RefID)
		if refID == "" {
			return nil, invalid("APP_NAV_REF_INVALID", "items[%d] refId là bắt buộc", i)
		}
		key := itemType + ":" + refID
		if _, dup := seen[key]; dup {
			return nil, invalid("APP_NAV_REF_INVALID", "items[%d] trùng lặp (%s %s)", i, itemType, refID)
		}
		if err := capRunes(fmt.Sprintf("items[%d].label", i), strings.TrimSpace(it.Label), maxAppLabelRunes); err != nil {
			return nil, err
		}
		seen[key] = struct{}{}
		switch itemType {
		case appmeta.ItemTypeEntity:
			if entityIDs == nil {
				if entityIDs, err = s.tenantEntityIDs(tenantID); err != nil {
					return nil, fmt.Errorf("services.AppService.ReplaceNav: %w", err)
				}
			}
			if _, ok := entityIDs[refID]; !ok {
				return nil, invalid("APP_NAV_REF_INVALID", "items[%d] refId %q không phải entity của tenant", i, refID)
			}
		case appmeta.ItemTypeDashboard:
			if dashboardIDs == nil {
				if dashboardIDs, err = s.tenantDashboardIDs(ctx, tenantID); err != nil {
					return nil, fmt.Errorf("services.AppService.ReplaceNav: %w", err)
				}
			}
			if _, ok := dashboardIDs[refID]; !ok {
				return nil, invalid("APP_NAV_REF_INVALID", "items[%d] refId %q không phải dashboard của tenant", i, refID)
			}
		default:
			return nil, invalid("APP_NAV_REF_INVALID", "items[%d] itemType %q không hợp lệ (chỉ entity/dashboard)", i, it.ItemType)
		}
		repoItems = append(repoItems, appmeta.NavItemInput{
			ItemType: itemType,
			RefID:    refID,
			Label:    strings.TrimSpace(it.Label),
		})
	}

	if err := s.repo.ReplaceNav(ctx, tenantID, app.ID, repoItems); err != nil {
		return nil, fmt.Errorf("services.AppService.ReplaceNav: %w", err)
	}
	saved, err := s.repo.ListNav(ctx, tenantID, app.ID)
	emitDocAudit(ctx, s.auditor, s.draftMode, tenantID, audit.AppMeta, app.ID, audit.ActionUpdate, beforeNav, appNavAuditSnapshot(saved))
	if err != nil {
		return nil, fmt.Errorf("services.AppService.ReplaceNav: read back: %w", err)
	}
	return saved, nil
}

// GetNav resolves an app's nav for the CALLER (GET /apps/:name/nav): entity items are
// filtered through the SAME per-entity "view" ACL the ⌘K assistant and middleware.RequireACL
// run (INV-3) — a denied entity is silently DROPPED (no existence leak), never an error.
// Admins bypass (D3). Dashboard items are visible to every tenant member (the dashboard
// catalog already is). Orphaned refs (object deleted after the nav write) are dropped
// defensively — the INV-4 cascade should have purged them.
func (s *AppService) GetNav(ctx context.Context, scope RecordScope, tenantID, name string) (*appmeta.App, []AppNavItemView, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	app, err := s.repo.GetByName(ctx, tenantID, name)
	if err != nil {
		if errors.Is(err, appmeta.ErrAppNotFound) {
			return nil, nil, invalid("APP_NOT_FOUND", "không tìm thấy app")
		}
		return nil, nil, fmt.Errorf("services.AppService.GetNav: %w", err)
	}
	items, err := s.repo.ListNav(ctx, tenantID, app.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("services.AppService.GetNav: %w", err)
	}

	// Resolve the referenced objects lazily per type (nav lists are small; registry reads
	// are one query each).
	var entityByID map[string]metadata.EntityDefinition
	var dashByID map[string]dashboard.Record
	out := make([]AppNavItemView, 0, len(items))
	for _, it := range items {
		switch it.ItemType {
		case appmeta.ItemTypeEntity:
			if entityByID == nil {
				ents, err := s.entities.ListEntities(tenantID)
				if err != nil {
					return nil, nil, fmt.Errorf("services.AppService.GetNav: list entities: %w", err)
				}
				entityByID = make(map[string]metadata.EntityDefinition, len(ents))
				for _, e := range ents {
					entityByID[e.ID] = e
				}
			}
			def, ok := entityByID[it.RefID]
			if !ok {
				continue // orphan ref — drop
			}
			if !scope.IsAdmin {
				allowed, aerr := s.acl.CanViewEntity(ctx, tenantID, scope.RoleID, def.Name)
				if aerr != nil {
					// Fail closed: an ACL infrastructure fault must never widen visibility.
					return nil, nil, fmt.Errorf("services.AppService.GetNav: acl: %w", aerr)
				}
				if !allowed {
					continue // hidden — no existence leak
				}
			}
			out = append(out, AppNavItemView{
				Type:    appmeta.ItemTypeEntity,
				RefID:   it.RefID,
				Label:   navLabel(it.Label, def.Label),
				Icon:    entityIconOf(def.Options),
				Href:    "/" + def.Name,
				RefName: def.Name,
			})
		case appmeta.ItemTypeDashboard:
			if dashByID == nil {
				dashes, derr := s.dashboards.List(ctx, tenantID)
				if derr != nil {
					return nil, nil, fmt.Errorf("services.AppService.GetNav: list dashboards: %w", derr)
				}
				dashByID = make(map[string]dashboard.Record, len(dashes))
				for _, d := range dashes {
					dashByID[d.ID] = d
				}
			}
			rec, ok := dashByID[it.RefID]
			if !ok || rec.DraftState == draftStateDeleted {
				continue // orphan ref (or deleted in the preview) — drop
			}
			out = append(out, AppNavItemView{
				Type:    appmeta.ItemTypeDashboard,
				RefID:   it.RefID,
				Label:   navLabel(it.Label, rec.Name),
				Href:    "/dashboards/" + rec.ID,
				RefName: rec.Name,
			})
		default:
			// Unknown persisted type (future v2 rows read by a v1 binary) — drop, don't error.
			continue
		}
	}
	return app, out, nil
}

// tenantEntityIDs returns the id-set of the tenant's live entities.
func (s *AppService) tenantEntityIDs(tenantID string) (map[string]struct{}, error) {
	ents, err := s.entities.ListEntities(tenantID)
	if err != nil {
		return nil, fmt.Errorf("list entities: %w", err)
	}
	ids := make(map[string]struct{}, len(ents))
	for _, e := range ents {
		ids[e.ID] = struct{}{}
	}
	return ids, nil
}

// tenantDashboardIDs returns the id-set of the tenant's live dashboards.
func (s *AppService) tenantDashboardIDs(ctx context.Context, tenantID string) (map[string]struct{}, error) {
	dashes, err := s.dashboards.List(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list dashboards: %w", err)
	}
	ids := make(map[string]struct{}, len(dashes))
	for _, d := range dashes {
		if d.DraftState == draftStateDeleted { // marked for deletion in the draft: a menu must not point at it (S7-4)
			continue
		}
		ids[d.ID] = struct{}{}
	}
	return ids, nil
}

// navLabel picks the per-app override when set, else the referenced object's own label.
func navLabel(override *string, fallback string) string {
	if override != nil && strings.TrimSpace(*override) != "" {
		return *override
	}
	return fallback
}

// entityIconOf extracts options.icon from a sys_entities options JSON blob ("" when unset
// or unparsable — the icon is cosmetic, never worth failing a nav read over).
func entityIconOf(options *string) string {
	if options == nil || *options == "" {
		return ""
	}
	var opts struct {
		Icon string `json:"icon"`
	}
	if err := json.Unmarshal([]byte(*options), &opts); err != nil {
		return ""
	}
	return opts.Icon
}
