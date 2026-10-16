package auth

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// W7 governance — admin CRUD over sys_field_acl (the config surface for the A6 enforcement the
// FieldACLService applies on every non-admin record request). The enforcement read path
// (LoadFieldACLForEntity, field_acl.go) hits the DB per request with NO cache layer, so a
// mutation through these methods takes effect on the very next request — there is nothing to
// invalidate (proven by the W7 integration test: grant → hidden; clear → visible again).
//
// Multi-tenant invariant: every statement here pins tenant_id, and role ownership is verified
// by the SERVICE (GetRoleByID, tenant-scoped) before any write reaches this layer; the
// tenant_id predicate in each statement is defense in depth on top of that.

// FieldACLKey identifies one grant row within a tenant: (role, entity, field) — the unique key
// of sys_field_acl minus the tenant, which is always supplied server-side.
type FieldACLKey struct {
	RoleID     string
	EntityName string
	FieldName  string
}

// ListFieldACL returns the tenant's field-grant rows, optionally narrowed to one role and/or
// one entity. Rows are the RESTRICTIVE OVERLAY only (default-allow: a field with no row is
// fully visible+editable). Ordered stably for the admin matrix (role, entity, field).
func (r *FieldACLRepository) ListFieldACL(ctx context.Context, tenantID, roleID, entity string) ([]FieldACL, error) {
	q := r.db.WithContext(ctx).
		Model(&FieldACL{}).
		Where("tenant_id = ?", tenantID).
		Order("role_id ASC, entity_name ASC, field_name ASC")
	if roleID != "" {
		q = q.Where("role_id = ?", roleID)
	}
	if entity != "" {
		q = q.Where("entity_name = ?", entity)
	}
	var rows []FieldACL
	if err := q.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("auth.FieldACLRepository.ListFieldACL: %w", err)
	}
	return rows, nil
}

// ReplaceFieldACL applies one admin "save" in a SINGLE transaction (all-or-nothing, the
// matrix editor contract): upserts restriction rows (INSERT .. ON DUPLICATE KEY UPDATE on the
// (tenant, role, entity, field) unique key) and deletes the keys whose new state is
// "no restriction" (canView && canEdit — the service decides the split; storing such a row
// would be dead weight the enforcement ignores). Every row's TenantID is FORCE-SET to
// tenantID here (defense in depth — a grant can never be smuggled across tenants even if the
// service left it blank).
func (r *FieldACLRepository) ReplaceFieldACL(ctx context.Context, tenantID string, upserts []FieldACL, deletes []FieldACLKey) error {
	if len(upserts) == 0 && len(deletes) == 0 {
		return nil
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(upserts) > 0 {
			for i := range upserts {
				upserts[i].TenantID = tenantID
				if upserts[i].ID == "" {
					upserts[i].ID = uuid.NewString()
				}
			}
			// ON DUPLICATE KEY (uq_sfa_tenant_role_entity_field) → update the two grant bits;
			// created_at/updated_at ride the column defaults.
			if err := tx.Clauses(clause.OnConflict{
				DoUpdates: clause.AssignmentColumns([]string{"can_view", "can_edit"}),
			}).Create(&upserts).Error; err != nil {
				return fmt.Errorf("upsert: %w", err)
			}
		}
		for _, k := range deletes {
			if err := tx.
				Where("tenant_id = ? AND role_id = ? AND entity_name = ? AND field_name = ?",
					tenantID, k.RoleID, k.EntityName, k.FieldName).
				Delete(&FieldACL{}).Error; err != nil {
				return fmt.Errorf("delete %s/%s/%s: %w", k.RoleID, k.EntityName, k.FieldName, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("auth.FieldACLRepository.ReplaceFieldACL: %w", err)
	}
	return nil
}

// DeleteFieldACLWhere removes restriction rows by filter: all rows of a role, of a (role,
// entity), or the single (role, entity, field). Returns the number of rows removed (0 is not
// an error — deleting an absent restriction is idempotent). tenant_id pins the DELETE.
func (r *FieldACLRepository) DeleteFieldACLWhere(ctx context.Context, tenantID, roleID, entity, field string) (int64, error) {
	q := r.db.WithContext(ctx).Where("tenant_id = ? AND role_id = ?", tenantID, roleID)
	if entity != "" {
		q = q.Where("entity_name = ?", entity)
	}
	if field != "" {
		q = q.Where("field_name = ?", field)
	}
	res := q.Delete(&FieldACL{})
	if res.Error != nil {
		return 0, fmt.Errorf("auth.FieldACLRepository.DeleteFieldACLWhere: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// FieldACLRestriction is one RESTRICTIVE sys_field_acl row (hidden or read-only) joined to its
// role's name/label — what an admin needs to lift a lock: the matrix and the Doctor name roles,
// not role ids.
type FieldACLRestriction struct {
	RoleID     string `json:"roleId"`
	RoleName   string `json:"roleName"`
	RoleLabel  string `json:"roleLabel"`
	EntityName string `json:"entityName"`
	FieldName  string `json:"fieldName"`
	CanView    bool   `json:"canView"`
	CanEdit    bool   `json:"canEdit"`
}

// ListFieldRestrictions returns the tenant's restrictive rows (can_view=0 OR can_edit=0 — a stored
// full-access row restricts nothing), optionally narrowed to one entity and/or one field, each
// joined to its role. BOTH tables are pinned to tenant_id: a row whose role belongs to another
// tenant is never returned (the forge-tenant guard LoadFieldACLForEntity applies). Ordered for a
// stable answer (entity, field, role name).
func (r *FieldACLRepository) ListFieldRestrictions(ctx context.Context, tenantID, entity, field string) ([]FieldACLRestriction, error) {
	q := r.db.WithContext(ctx).
		Table("sys_field_acl").
		Select("sys_field_acl.role_id, acl_roles.name AS role_name, acl_roles.label AS role_label, "+
			"sys_field_acl.entity_name, sys_field_acl.field_name, sys_field_acl.can_view, sys_field_acl.can_edit").
		Joins("JOIN acl_roles ON acl_roles.id = sys_field_acl.role_id AND acl_roles.tenant_id = ?", tenantID).
		Where("sys_field_acl.tenant_id = ? AND (sys_field_acl.can_view = 0 OR sys_field_acl.can_edit = 0)", tenantID).
		Order("sys_field_acl.entity_name ASC, sys_field_acl.field_name ASC, acl_roles.name ASC")
	if entity != "" {
		q = q.Where("sys_field_acl.entity_name = ?", entity)
	}
	if field != "" {
		q = q.Where("sys_field_acl.field_name = ?", field)
	}
	var rows []FieldACLRestriction
	if err := q.Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("auth.FieldACLRepository.ListFieldRestrictions: %w", err)
	}
	return rows, nil
}
