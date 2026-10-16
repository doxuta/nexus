package auth

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrRoleNotFound is returned when no acl_roles row matches a (tenant_id, id) pair (unknown
// or cross-tenant id). The role service maps it to a 404 ROLE_NOT_FOUND.
var ErrRoleNotFound = errors.New("auth: role not found")

// ErrRoleAlreadyExists is returned when creating/renaming a role would violate the
// uq_acl_roles_tenant_name unique index (a role with that name already exists in the tenant).
// The role service maps it to a 409 ROLE_ALREADY_EXISTS.
var ErrRoleAlreadyExists = errors.New("auth: role already exists")

// RoleRepository persists and reads the RBAC tables (acl_roles, acl_role_actions) over GORM
// (../specs/001-auth-security/spec.md §A4). Every query is parameterized and pinned to tenant_id: a role
// from tenant A can never be read or mutated under tenant B, and the action rows are reached
// only through a tenant-scoped role (or, for the ACL hot path, a tenant-pinned JOIN), so they
// inherit the same isolation (R2/D4 forge-tenant guard lives here in the data layer). It is the
// shared A4 data layer consumed by the RoleService (CRUD) and the ACLService (decision lookup).
type RoleRepository struct {
	db *gorm.DB
}

// NewRoleRepository wires a role repository over an existing GORM connection.
func NewRoleRepository(db *gorm.DB) *RoleRepository {
	return &RoleRepository{db: db}
}

// ListRoles returns every role in the tenant ordered by name for a stable page. Always pinned
// to tenant_id so it can never return another tenant's roles.
func (r *RoleRepository) ListRoles(ctx context.Context, tenantID string) ([]Role, error) {
	var roles []Role
	err := r.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("name ASC").
		Find(&roles).Error
	if err != nil {
		return nil, fmt.Errorf("auth.RoleRepository.ListRoles: %w", err)
	}
	return roles, nil
}

// GetRoleByID returns the (tenant_id, id) role or ErrRoleNotFound. Pinned to tenant_id so a
// guessed/cross-tenant id misses.
// RoleExists reports whether a role id exists in the tenant (VIEC-HOM-NAY queue visibility
// validation — cheaper than loading the full role + grants).
func (r *RoleRepository) RoleExists(ctx context.Context, tenantID, id string) (bool, error) {
	var n int64
	if err := r.db.WithContext(ctx).Model(&Role{}).
		Where("tenant_id = ? AND id = ?", tenantID, id).Count(&n).Error; err != nil {
		return false, fmt.Errorf("auth.RoleRepository.RoleExists: %w", err)
	}
	return n > 0, nil
}

func (r *RoleRepository) GetRoleByID(ctx context.Context, tenantID, id string) (*Role, error) {
	var role Role
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		First(&role).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrRoleNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth.RoleRepository.GetRoleByID: %w", err)
	}
	return &role, nil
}

// GetRoleByName returns the (tenant_id, name) role or ErrRoleNotFound. Mirrors GetRoleByID but
// keyed by the tenant-unique name (uq_acl_roles_tenant_name) — the deterministic lookup the
// TRỤ-2-mục-4 app ACL seeder (services.AppACLSeedService) uses to reuse an already-seeded app
// role idempotently instead of trying to create a duplicate.
func (r *RoleRepository) GetRoleByName(ctx context.Context, tenantID, name string) (*Role, error) {
	var role Role
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND name = ?", tenantID, name).
		First(&role).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrRoleNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth.RoleRepository.GetRoleByName: %w", err)
	}
	return &role, nil
}

// AddGrants appends grant rows to an EXISTING tenant-scoped role WITHOUT touching any grant it
// already holds (unlike UpdateRole's Actions, which REPLACES the whole set) — the shape
// services.AppACLSeedService needs: a role seeded for entity A must keep entity A's grants
// intact when entity B is later added to the same app. The role is first verified to belong to
// the tenant (ErrRoleNotFound otherwise, mirroring ListActionsForRole). Each action's RoleID is
// FORCE-SET to id (R3 defense in depth). A row that would collide with an existing
// (role_id, entity_name, action) grant (uq_ara_role_entity_action) is silently skipped
// (ON CONFLICT DO NOTHING) rather than erroring — the caller only ever asks to grant a
// brand-new entity name, so a collision here means a harmless re-run, not a real conflict.
func (r *RoleRepository) AddGrants(ctx context.Context, tenantID, id string, actions []RoleAction) error {
	if len(actions) == 0 {
		return nil
	}
	if _, err := r.GetRoleByID(ctx, tenantID, id); err != nil {
		return err
	}
	for i := range actions {
		actions[i].RoleID = id
	}
	if err := r.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&actions).Error; err != nil {
		return fmt.Errorf("auth.RoleRepository.AddGrants: %w", err)
	}
	return nil
}

// RolesGrantingView returns the DISTINCT role ids in the tenant holding a "view" grant on ANY of
// entityNames (exact match) or the '*' wildcard. Every acl_roles row is inherently non-admin (an
// admin's is_admin super-gate never consults this table — middleware/acl.go), so any match here
// answers the TRỤ-2-mục-4 app-readiness question "does some non-admin role see this app's data?"
// (services.AppService.Readiness). entityNames empty → ([], nil) without a query.
func (r *RoleRepository) RolesGrantingView(ctx context.Context, tenantID string, entityNames []string) ([]string, error) {
	if len(entityNames) == 0 {
		return nil, nil
	}
	names := append([]string{wildcardRoleEntity}, entityNames...)
	var roleIDs []string
	err := r.db.WithContext(ctx).
		Model(&RoleAction{}).
		Joins("JOIN acl_roles ON acl_roles.id = acl_role_actions.role_id").
		// The reserved owner role holds '*' by design; counting it would make every converted
		// workspace look like it already has a staff role that can see the app (specs/065 lát D).
		Where("acl_roles.tenant_id = ? AND acl_roles.name <> ? AND acl_role_actions.action = ? AND acl_role_actions.entity_name IN ?", tenantID, OwnerRoleName, "view", names).
		Distinct("acl_role_actions.role_id").
		Pluck("acl_role_actions.role_id", &roleIDs).Error
	if err != nil {
		return nil, fmt.Errorf("auth.RoleRepository.RolesGrantingView: %w", err)
	}
	return roleIDs, nil
}

// wildcardRoleEntity mirrors services.wildcardEntity ('*' = every entity, RoleAction.EntityName
// doc comment) — duplicated here (not imported) because internal/auth must not depend on
// internal/services (services already depends on auth; the reverse would be a cycle).
const wildcardRoleEntity = "*"

// CreateRole inserts a role and its initial grants in ONE transaction. role must already carry
// a server-derived ID and TenantID. Each grant's RoleID is FORCE-SET to role.ID here (defense
// in depth: even if the service left it blank or wrong, a grant can never be smuggled onto a
// different role, R3). A duplicate (tenant_id, name) surfaces as ErrRoleAlreadyExists.
func (r *RoleRepository) CreateRole(ctx context.Context, role *Role, actions []RoleAction) error {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(role).Error; err != nil {
			if isDuplicateEntry(err) {
				return ErrRoleAlreadyExists
			}
			return fmt.Errorf("insert role: %w", err)
		}
		if len(actions) > 0 {
			for i := range actions {
				actions[i].RoleID = role.ID
			}
			if err := tx.Create(&actions).Error; err != nil {
				return fmt.Errorf("insert grants: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrRoleAlreadyExists) {
			return err
		}
		return fmt.Errorf("auth.RoleRepository.CreateRole: %w", err)
	}
	return nil
}

// RecreateRole puts a deleted role back under its own id in ONE transaction: the role row, its grants,
// its field-permission rows, and only then the users who held it (specs/063 FR-056). Only users of the
// tenant whose role_id is still NULL are given the role back: someone assigned another role since keeps
// it. Any step failing rolls everything back, so no user is ever handed a role that lacks its field
// restrictions. Returns how many users got the role. Every statement is pinned to tenantID.
func (r *RoleRepository) RecreateRole(ctx context.Context, role *Role, actions []RoleAction, fieldACL []FieldACL, userIDs []string) (int64, error) {
	var reassigned int64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(role).Error; err != nil {
			if isDuplicateEntry(err) {
				return ErrRoleAlreadyExists
			}
			return fmt.Errorf("insert role: %w", err)
		}
		if len(actions) > 0 {
			for i := range actions {
				actions[i].RoleID = role.ID
			}
			if err := tx.Create(&actions).Error; err != nil {
				return fmt.Errorf("insert grants: %w", err)
			}
		}
		if len(fieldACL) > 0 {
			for i := range fieldACL {
				fieldACL[i].TenantID, fieldACL[i].RoleID = role.TenantID, role.ID
			}
			if err := tx.Create(&fieldACL).Error; err != nil {
				return fmt.Errorf("insert field permissions: %w", err)
			}
		}
		if len(userIDs) > 0 {
			res := tx.Model(&User{}).
				Where("tenant_id = ? AND id IN ? AND role_id IS NULL", role.TenantID, userIDs).
				Update("role_id", role.ID)
			if res.Error != nil {
				return fmt.Errorf("reassign users: %w", res.Error)
			}
			reassigned = res.RowsAffected
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrRoleAlreadyExists) {
			return 0, err
		}
		return 0, fmt.Errorf("auth.RoleRepository.RecreateRole: %w", err)
	}
	return reassigned, nil
}

// UpdateRole applies the non-empty columns in fields to the (tenant_id, id) role and, when
// actions is non-nil, REPLACES its grant set (delete-then-insert) — all in ONE transaction so a
// failed grant insert never leaves the role half-updated. A nil actions slice leaves grants
// untouched; a non-nil empty slice clears them. Each grant's RoleID is force-set to id (R3).
// The WHERE is pinned to (tenant_id, id); an unknown/cross-tenant id is ErrRoleNotFound and a
// name clash is ErrRoleAlreadyExists. Returns the refreshed role row.
func (r *RoleRepository) UpdateRole(ctx context.Context, tenantID, id string, fields map[string]any, actions []RoleAction) (*Role, error) {
	var updated *Role
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Confirm the role exists in THIS tenant first (so a no-op fields + nil actions update,
		// or an actions-only replace, still returns not-found for a bad id).
		var existing Role
		if err := tx.Where("tenant_id = ? AND id = ?", tenantID, id).First(&existing).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrRoleNotFound
			}
			return fmt.Errorf("load role: %w", err)
		}

		if len(fields) > 0 {
			res := tx.Model(&Role{}).
				Where("tenant_id = ? AND id = ?", tenantID, id).
				Updates(fields)
			if res.Error != nil {
				if isDuplicateEntry(res.Error) {
					return ErrRoleAlreadyExists
				}
				return fmt.Errorf("update role: %w", res.Error)
			}
		}

		if actions != nil {
			if err := tx.Where("role_id = ?", id).Delete(&RoleAction{}).Error; err != nil {
				return fmt.Errorf("clear grants: %w", err)
			}
			if len(actions) > 0 {
				for i := range actions {
					actions[i].RoleID = id
				}
				if err := tx.Create(&actions).Error; err != nil {
					return fmt.Errorf("insert grants: %w", err)
				}
			}
		}

		var refreshed Role
		if err := tx.Where("tenant_id = ? AND id = ?", tenantID, id).First(&refreshed).Error; err != nil {
			return fmt.Errorf("reload role: %w", err)
		}
		updated = &refreshed
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrRoleNotFound) || errors.Is(err, ErrRoleAlreadyExists) {
			return nil, err
		}
		return nil, fmt.Errorf("auth.RoleRepository.UpdateRole: %w", err)
	}
	return updated, nil
}

// DeleteRole hard-deletes the (tenant_id, id) role; the acl_role_actions FK (ON DELETE CASCADE)
// drops its grants atomically and users.role_id FKs ON DELETE SET NULL, so any user holding the
// role is detached rather than removed. An unknown/cross-tenant id is ErrRoleNotFound.
func (r *RoleRepository) DeleteRole(ctx context.Context, tenantID, id string) error {
	res := r.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		Delete(&Role{})
	if res.Error != nil {
		return fmt.Errorf("auth.RoleRepository.DeleteRole: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrRoleNotFound
	}
	return nil
}

// ListActionsForRole returns the grant rows for a tenant-scoped role, ordered for a stable view.
// The role is first verified to belong to the tenant (GetRoleByID) so action rows never leak
// across tenants via a guessed role_id. Returns ErrRoleNotFound for an unknown/cross-tenant id.
func (r *RoleRepository) ListActionsForRole(ctx context.Context, tenantID, roleID string) ([]RoleAction, error) {
	if _, err := r.GetRoleByID(ctx, tenantID, roleID); err != nil {
		return nil, err
	}
	var actions []RoleAction
	err := r.db.WithContext(ctx).
		Where("role_id = ?", roleID).
		Order("entity_name ASC, action ASC").
		Find(&actions).Error
	if err != nil {
		return nil, fmt.Errorf("auth.RoleRepository.ListActionsForRole: %w", err)
	}
	return actions, nil
}

// LoadActionsForACL loads a role's grants for the per-request ACL decision. It is the HOT PATH
// (runs on every dynamic /:entity request for a non-admin) and is tenant-pinned via a JOIN to
// acl_roles: a role_id from another tenant returns ZERO grants → default-deny (D4 forge-tenant
// guard enforced in the query, not the caller). It does NOT return ErrRoleNotFound — an unknown
// or cross-tenant role simply yields an empty slice, which the ACLService reads as "no grants".
func (r *RoleRepository) LoadActionsForACL(ctx context.Context, tenantID, roleID string) ([]RoleAction, error) {
	var actions []RoleAction
	err := r.db.WithContext(ctx).
		Model(&RoleAction{}).
		Joins("JOIN acl_roles ON acl_roles.id = acl_role_actions.role_id").
		Where("acl_role_actions.role_id = ? AND acl_roles.tenant_id = ?", roleID, tenantID).
		Find(&actions).Error
	if err != nil {
		return nil, fmt.Errorf("auth.RoleRepository.LoadActionsForACL: %w", err)
	}
	return actions, nil
}
