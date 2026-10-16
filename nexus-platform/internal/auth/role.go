package auth

import (
	"strings"
	"time"
)

// OwnerRoleName is the reserved name of the customer-owner role (specs/065-customer-owner-tier).
// Only the tenant seed creates it; a pack upgrade never creates, edits, revokes from or adopts it
// (specs/069 plan.md §5). Compare with this constant, never a bare string.
const OwnerRoleName = "tenant_owner"

// IsOwnerRoleName reports whether name is the reserved owner role name. acl_roles is
// utf8mb4_unicode_ci, so the DB treats "Tenant_Owner" and "tenant_owner" as one name; the guard must too.
func IsOwnerRoleName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), OwnerRoleName)
}

// Role is a named, tenant-scoped RBAC role (../specs/001-auth-security/spec.md §A4, table acl_roles).
// A user references at most one role via users.role_id; the role's grants (RoleAction rows)
// decide which (entity, action) pairs the user may perform. Role names are unique within a
// tenant (uq_acl_roles_tenant_name) but may repeat across tenants. This is a fixed system
// table → GORM-managed (rules/database.md) and always carries tenant_id.
type Role struct {
	ID        string    `gorm:"type:char(36);primaryKey" json:"id"`
	TenantID  string    `gorm:"type:char(36);index" json:"tenantId"`
	Name      string    `gorm:"type:varchar(100)" json:"name"`
	Label     string    `gorm:"type:varchar(150)" json:"label"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// TableName pins the fixed system table.
func (Role) TableName() string { return "acl_roles" }

// RoleAction is one grant row for a role (../specs/001-auth-security/spec.md §A4, table acl_role_actions):
// it says role_id may perform `action` on `entity_name` ('*' = every entity), with the
// row-level scope `access_level` (none/owner/team/all) consumed by the A5 team filter. The
// triple (role_id, entity_name, action) is unique (uq_ara_role_entity_action), so a role
// holds at most one grant per (entity, action). Deleting the role cascades its grants
// (fk_ara_role ON DELETE CASCADE). GORM-managed; tenant scoping flows through role_id.
type RoleAction struct {
	ID          string    `gorm:"type:char(36);primaryKey" json:"id"`
	RoleID      string    `gorm:"type:char(36);index:idx_ara_role" json:"roleId"`
	EntityName  string    `gorm:"type:varchar(100)" json:"entityName"`             // '*' = every entity
	Action      string    `gorm:"type:varchar(20)" json:"action"`                  // view/create/edit/delete
	AccessLevel string    `gorm:"type:varchar(40);default:all" json:"accessLevel"` // none/owner/team/all/field:<user field>
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// TableName pins the fixed system table.
func (RoleAction) TableName() string { return "acl_role_actions" }
