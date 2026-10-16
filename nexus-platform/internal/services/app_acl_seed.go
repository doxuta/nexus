package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/metaversion"
)

// Package note (TRỤ 2 mục 4, docs/NEXUS-MOT-XUONG-2026-07-29.md — "quyền không còn vô hình"):
// before this file, NO production code path seeded acl_role_actions when an entity was
// created — a hand-built entity was invisible (403) to every non-admin until someone manually
// configured a role, while a vertical-pack entity worked only because the pack itself ships its
// own Roles (vertical_pack/builtin.go). AppACLSeedService closes that gap for the ONE case its
// safety argument covers: an entity that is being attached to an app's workspace AT THE MOMENT
// IT IS CREATED. It never touches an existing entity's grants, never runs for a pack install
// (packs never pass an app id into this path — see admin_entity_handler.go), and never assigns a
// user to a role (assigning people is a deliberate, separate admin action).

// seedAppLookup resolves a tenant-scoped app by id (for its Name/Label, used to derive the two
// deterministic default-role identities). Satisfied by *appmeta.SQLRepository (the same repo
// AppService.repo already wraps) via structural typing — no adapter needed.
type seedAppLookup interface {
	GetByID(ctx context.Context, tenantID, id string) (*appmeta.App, error)
}

// seedRoleStore is the subset of auth.RoleRepository the seeder needs: look up a deterministic
// role by name, create it (with its first entity's grants attached), or append grants to one
// that already exists. Declared consumer-side so this service stays unit-testable with a fake.
type seedRoleStore interface {
	GetRoleByName(ctx context.Context, tenantID, name string) (*auth.Role, error)
	CreateRole(ctx context.Context, role *auth.Role, actions []auth.RoleAction) error
	AddGrants(ctx context.Context, tenantID, roleID string, actions []auth.RoleAction) error
	ListActionsForRole(ctx context.Context, tenantID, roleID string) ([]auth.RoleAction, error)
}

// AppACLSeedService ensures a newly created, app-scoped entity is immediately visible to the
// app's non-admin staff instead of defaulting to invisible (TRỤ 2 mục 4). It is invoked ONLY
// when the caller explicitly names an app (POST /admin/entities body field "appId") — creating
// an entity with no app, or a vertical-pack install, never reaches this service, so neither
// loses its existing behavior.
type AppACLSeedService struct {
	apps  seedAppLookup
	roles seedRoleStore
	newID func() string
	// aclCache, when set, is the shared ACL grants cache (O3). A freshly seeded grant is
	// invalidated from it AFTER commit so the entity is visible on the VERY NEXT request from a
	// staff/manager principal, not after the cache TTL. Nil-safe (Invalidate no-ops on nil).
	aclCache *CachedACLLoader
	logger   *zap.Logger
	rec      *VersionRecorder // S7-5: direct entry per role a seed creates / adds grants to; nil = none
}

// WithRecorder arms the direct version entries (specs/063 S7-5, FR-032): on a delivered workspace
// every default role this seed creates or extends leaves a before/after entry, recorded HERE (the
// one place) so table creation and acl-backfill are both covered. Chainable.
func (s *AppACLSeedService) WithRecorder(r *VersionRecorder) *AppACLSeedService {
	s.rec = r
	return s
}

// NewAppACLSeedService wires the seeder over an app lookup and the shared role repository.
func NewAppACLSeedService(apps seedAppLookup, roles seedRoleStore) *AppACLSeedService {
	return &AppACLSeedService{apps: apps, roles: roles, newID: uuid.NewString, logger: zap.NewNop()}
}

// WithACLCache attaches the shared ACL grants cache (O3 invalidation after commit). Chainable.
func (s *AppACLSeedService) WithACLCache(c *CachedACLLoader) *AppACLSeedService {
	s.aclCache = c
	return s
}

// WithLogger attaches a structured logger for the success path (zap.Info — the failure path is
// logged by the CALLER, which also owns the fail-open decision; see admin_entity_handler.go).
// Chainable; a nil logger is ignored.
func (s *AppACLSeedService) WithLogger(l *zap.Logger) *AppACLSeedService {
	if l != nil {
		s.logger = l
	}
	return s
}

// appDefaultRoleName derives the deterministic, tenant-unique (uq_acl_roles_tenant_name) role
// name for an app's default staff/manager role. Keying off the app's SLUG (not its label, which
// is free-text and mutable) makes SeedForEntity idempotent per (tenant, app) without a lookup
// table: the second entity created under the same app resolves the SAME role by name instead of
// creating a duplicate.
func appDefaultRoleName(appName, suffix string) string {
	return "app_" + appName + "_" + suffix
}

// appDefaultRoleLabel builds the admin-facing role label per the TRỤ 2 mục 4 spec: "<Tên app> —
// Nhân viên" / "<Tên app> — Quản lý". appLabel is capped at 100 runes by AppService's
// capRunes(maxAppLabelRunes) at app-create time, so the suffix always fits acl_roles.label
// VARCHAR(150).
func appDefaultRoleLabel(appLabel, suffix string) string {
	return appLabel + " — " + suffix
}

// staffGrants is the "<app> — Nhân viên" role's access to a freshly created entity.
//
// FOUNDER DECISION 2026-07-29: "Thấy hết, chỉ sửa được của mình" — view is tenant-wide ("all"),
// while create/edit stay scoped to the principal's OWN records ("owner"); delete is never granted.
// The first cut scoped view to "owner" too, which is right for a sales pipeline (reps shouldn't
// browse each other's accounts) but WRONG for the shared tables most apps are built around: a
// project board, a task list or a ticket queue renders EMPTY for a new joiner, which reads as
// "the app is broken" rather than "you own nothing yet". Seeing the whole board while only being
// able to edit your own rows is the default that fits both, and it is the customer's call to
// tighten per app in Vai trò & quyền.
func staffGrants(newID func() string, entityName string) []auth.RoleAction {
	levels := map[string]string{"view": "all", "create": "owner", "edit": "owner"}
	out := make([]auth.RoleAction, 0, len(levels))
	for _, a := range []string{"view", "create", "edit"} { // deterministic order
		out = append(out, auth.RoleAction{ID: newID(), EntityName: entityName, Action: a, AccessLevel: levels[a]})
	}
	return out
}

// managerGrants is the "<app> — Quản lý" role's access to a freshly created entity: full
// view/create/edit/delete over every record in the tenant (access_level "all").
func managerGrants(newID func() string, entityName string) []auth.RoleAction {
	actions := []string{"view", "create", "edit", "delete"}
	out := make([]auth.RoleAction, len(actions))
	for i, a := range actions {
		out[i] = auth.RoleAction{ID: newID(), EntityName: entityName, Action: a, AccessLevel: "all"}
	}
	return out
}

// SeedForEntity ensures appID's two default roles exist (creating whichever are missing) and
// grants entityName into BOTH — and ONLY those two (spec §1: "không vai nào khác"). It is
// idempotent and safe to call once per (app, entity) pair: a second entity created later under
// the same app reuses the SAME two roles (by deterministic name) and only ADDS its own grant
// rows, never touching the grants a prior entity already received (AddGrants, not Replace).
//
// It is FAIL-OPEN BY CONTRACT: seeded=false + a human-readable Vietnamese warning is returned
// for every failure mode (unknown/cross-tenant appID, a role-store error), never an error the
// caller must abort on — entity creation has already committed by the time this runs, so there
// is nothing safe to roll back. err, when non-nil, is the wrapped low-level cause for the
// caller's zap.Warn; the caller decides whether/how to surface it further (see
// admin_entity_handler.go CreateEntity).
func (s *AppACLSeedService) SeedForEntity(ctx context.Context, tenantID, appID, entityName string) (seeded bool, warning string, err error) {
	appID = strings.TrimSpace(appID)
	entityName = strings.TrimSpace(entityName)
	app, aerr := s.apps.GetByID(ctx, tenantID, appID)
	if aerr != nil {
		if errors.Is(aerr, appmeta.ErrAppNotFound) {
			return false, "app đã cho không tồn tại — bỏ qua cấp quyền mặc định, cấp quyền thủ công trong Vai trò & quyền", nil
		}
		return false, "không thể đọc app để cấp quyền mặc định", fmt.Errorf("services.AppACLSeedService.SeedForEntity: get app: %w", aerr)
	}

	trace := s.rec.Active(ctx, tenantID)
	staffBefore, managerBefore := s.roleImage(ctx, trace, tenantID, app, "staff"), s.roleImage(ctx, trace, tenantID, app, "manager")
	staffRole, serr := s.ensureRole(ctx, tenantID, appDefaultRoleName(app.Name, "staff"), appDefaultRoleLabel(app.Label, "Nhân viên"))
	if serr != nil {
		return false, "không thể tạo/tìm vai Nhân viên mặc định", fmt.Errorf("services.AppACLSeedService.SeedForEntity: ensure staff role: %w", serr)
	}
	managerRole, merr := s.ensureRole(ctx, tenantID, appDefaultRoleName(app.Name, "manager"), appDefaultRoleLabel(app.Label, "Quản lý"))
	if merr != nil {
		return false, "không thể tạo/tìm vai Quản lý mặc định", fmt.Errorf("services.AppACLSeedService.SeedForEntity: ensure manager role: %w", merr)
	}

	if err := s.roles.AddGrants(ctx, tenantID, staffRole.ID, staffGrants(s.newID, entityName)); err != nil {
		return false, "không thể cấp quyền cho vai Nhân viên", fmt.Errorf("services.AppACLSeedService.SeedForEntity: grant staff: %w", err)
	}
	if err := s.roles.AddGrants(ctx, tenantID, managerRole.ID, managerGrants(s.newID, entityName)); err != nil {
		return false, "không thể cấp quyền cho vai Quản lý", fmt.Errorf("services.AppACLSeedService.SeedForEntity: grant manager: %w", err)
	}

	if trace {
		s.record(ctx, tenantID, staffRole, staffBefore, appDefaultRoleLabel(app.Label, "Nhân viên"))
		s.record(ctx, tenantID, managerRole, managerBefore, appDefaultRoleLabel(app.Label, "Quản lý"))
	}

	// O3: invalidate AFTER both grants commit so the very next request from a principal holding
	// either role reloads fresh grants instead of a pre-seed cached empty/short list.
	s.aclCache.Invalidate(tenantID, staffRole.ID)
	s.aclCache.Invalidate(tenantID, managerRole.ID)

	s.logger.Info("services.AppACLSeedService.SeedForEntity: seeded default ACL",
		zap.String("tenant_id", tenantID), zap.String("app_id", appID), zap.String("entity_name", entityName),
		zap.String("staff_role_id", staffRole.ID), zap.String("manager_role_id", managerRole.ID))
	return true, "", nil
}

// ensureRole resolves the tenant's role by its deterministic name, creating it (with no initial
// grants — SeedForEntity adds the entity's grants right after, via AddGrants either way) if
// absent. A CreateRole race (two entities created concurrently under an app with no roles yet)
// is resolved by re-reading the name: the loser simply reuses the winner's row.
func (s *AppACLSeedService) ensureRole(ctx context.Context, tenantID, name, label string) (*auth.Role, error) {
	existing, err := s.roles.GetRoleByName(ctx, tenantID, name)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, auth.ErrRoleNotFound) {
		return nil, err
	}
	role := &auth.Role{ID: s.newID(), TenantID: tenantID, Name: name, Label: label}
	if cerr := s.roles.CreateRole(ctx, role, nil); cerr != nil {
		if errors.Is(cerr, auth.ErrRoleAlreadyExists) {
			return s.roles.GetRoleByName(ctx, tenantID, name)
		}
		return nil, cerr
	}
	return role, nil
}

// roleImage is a default role's view BEFORE the seed (nil when it does not exist yet = a create).
func (s *AppACLSeedService) roleImage(ctx context.Context, trace bool, tenantID string, app *appmeta.App, suffix string) *RoleView {
	if !trace {
		return nil
	}
	r, err := s.roles.GetRoleByName(ctx, tenantID, appDefaultRoleName(app.Name, suffix))
	if err != nil {
		return nil
	}
	acts, err := s.roles.ListActionsForRole(ctx, tenantID, r.ID)
	if err != nil {
		return nil
	}
	v := roleToView(r, acts)
	return &v
}

func (s *AppACLSeedService) record(ctx context.Context, tenantID string, role *auth.Role, before *RoleView, label string) {
	acts, err := s.roles.ListActionsForRole(ctx, tenantID, role.ID)
	if err != nil {
		s.logger.Error("services.AppACLSeedService.record", zap.String("tenant_id", tenantID), zap.String("role_id", role.ID), zap.Error(err))
		metaversion.AddWarning(ctx, VersionWarningMessage)
		return
	}
	d := DirectDoc{DocType: "role", DocKey: role.ID, Label: label, Op: "update", After: roleToView(role, acts)}
	if before == nil {
		d.Op = "create"
	} else {
		d.Before = *before
	}
	s.rec.RecordDirect(ctx, tenantID, "", d)
}
