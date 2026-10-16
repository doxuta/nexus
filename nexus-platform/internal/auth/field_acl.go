package auth

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// FieldACL is one field-level grant row (docs RM-42 / A6, table sys_field_acl): for a
// (tenant_id, role_id, entity_name, field_name) it records whether the role may VIEW
// (CanView) and/or EDIT (CanEdit) that field. A4 (acl_role_actions) authorizes the (entity,
// action) at the MODULE level and A5 narrows ROWS; this adds the FIELD dimension on top.
//
// DEFAULT-ALLOW: a field with NO row here is fully visible+editable for the role — this table
// is a RESTRICTIVE OVERLAY, so admins only insert rows to hide/lock specific sensitive fields.
// The triple-plus (tenant, role, entity, field) is unique (uq_sfa_tenant_role_entity_field) so
// a field's visibility for a role is single-valued. Deleting the role cascades its field grants
// (fk_sfa_role ON DELETE CASCADE). GORM-managed; tenant scoping flows through tenant_id (always
// part of the WHERE) and the role_id FK to the tenant-scoped acl_roles row.
type FieldACL struct {
	ID         string `gorm:"type:char(36);primaryKey" json:"id"`
	TenantID   string `gorm:"type:char(36)" json:"tenantId"`
	RoleID     string `gorm:"type:char(36)" json:"roleId"`
	EntityName string `gorm:"type:varchar(100)" json:"entityName"`
	FieldName  string `gorm:"type:varchar(100)" json:"fieldName"`
	CanView    bool   `gorm:"column:can_view" json:"canView"`
	CanEdit    bool   `gorm:"column:can_edit" json:"canEdit"`
}

// TableName pins the fixed system table.
func (FieldACL) TableName() string { return "sys_field_acl" }

// FieldACLRepository reads the field-level ACL grants (sys_field_acl) over GORM (RM-42). Every
// query is parameterized and pinned to tenant_id AND reached through a tenant-scoped role, so a
// role_id from another tenant returns zero rows → default-allow on no row, but a forged tenant
// can never surface another tenant's field grants. It is consumed by the FieldACLService
// (read-strip + write-guard) on the dynamic /:entity surface.
type FieldACLRepository struct {
	db *gorm.DB
}

// NewFieldACLRepository wires a field-ACL repository over an existing GORM connection.
func NewFieldACLRepository(db *gorm.DB) *FieldACLRepository {
	return &FieldACLRepository{db: db}
}

// LoadFieldACLForEntity loads ALL field grants for a (tenant, role, entity) in ONE indexed read
// — the per-request hot path for the read-strip / write-guard. It is tenant-pinned via a JOIN to
// acl_roles (a role_id from another tenant returns ZERO rows → default-allow, matching the no-row
// case; D4 forge-tenant guard enforced in the query, not the caller) AND an explicit tenant_id
// predicate on sys_field_acl (defense in depth). An empty roleID returns nil (no role → no
// restrictions to load; the module ACL already denied a no-role non-admin upstream). It NEVER
// returns a not-found error: an empty slice simply means "no field is restricted for this role".
func (r *FieldACLRepository) LoadFieldACLForEntity(ctx context.Context, tenantID, roleID, entity string) ([]FieldACL, error) {
	if roleID == "" {
		return nil, nil
	}
	var rows []FieldACL
	err := r.db.WithContext(ctx).
		Model(&FieldACL{}).
		Joins("JOIN acl_roles ON acl_roles.id = sys_field_acl.role_id").
		Where("sys_field_acl.tenant_id = ? AND sys_field_acl.role_id = ? AND sys_field_acl.entity_name = ? AND acl_roles.tenant_id = ?",
			tenantID, roleID, entity, tenantID).
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("auth.FieldACLRepository.LoadFieldACLForEntity: %w", err)
	}
	return rows, nil
}
