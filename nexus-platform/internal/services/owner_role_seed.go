package services

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
)

// ownerRoleLabel is the display label of the reserved owner role.
const ownerRoleLabel = "Chủ khách — toàn quyền dữ liệu"

// SeedOwnerRole makes sure the tenant has the reserved customer-owner role (auth.OwnerRoleName)
// with view/create/edit/delete on every entity at access level "all", and returns its id.
// created reports whether THIS call made it; an existing role is returned as-is and its grants are
// never touched (a builder may have narrowed them on purpose).
//
// Race-safe by construction (specs/065 plan §"Vai dành riêng"): the unique key
// uq_acl_roles_tenant_name serialises two concurrent callers on the INSERT … ON DUPLICATE KEY, and
// the follow-up SELECT … FOR SHARE is a LOCKING read so it sees the committed winner (a plain
// SELECT would read this tx's REPEATABLE READ snapshot and miss it). created is derived from the id
// (never RowsAffected: the production DSN has clientFoundRows=true, so a no-op upsert reports 1).
//
// Allowed callers (nobody else writes the "tenant_owner" string or creates this role):
// SetOwnerTier, Provision, and specs/068-tenant-backup-restore D1 (recreate the role after a restore).
// tx MUST be a transaction handle so the role and its grants commit or roll back with the caller.
func SeedOwnerRole(ctx context.Context, tx *gorm.DB, tenantID string) (roleID string, created bool, err error) {
	newID := uuid.NewString()
	now := time.Now().UTC()
	if err := tx.WithContext(ctx).Exec(
		`INSERT INTO acl_roles (id, tenant_id, name, label, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE id = id`,
		newID, tenantID, auth.OwnerRoleName, ownerRoleLabel, now, now).Error; err != nil {
		return "", false, fmt.Errorf("services.SeedOwnerRole: insert role: %w", err)
	}
	if err := tx.WithContext(ctx).Raw(
		`SELECT id FROM acl_roles WHERE tenant_id = ? AND name = ? FOR SHARE`,
		tenantID, auth.OwnerRoleName).Scan(&roleID).Error; err != nil {
		return "", false, fmt.Errorf("services.SeedOwnerRole: read role: %w", err)
	}
	if roleID == "" {
		return "", false, fmt.Errorf("services.SeedOwnerRole: role missing right after insert (tenant %s)", tenantID)
	}
	if roleID != newID {
		return roleID, false, nil
	}
	for _, action := range []string{"view", "create", "edit", "delete"} {
		if err := tx.WithContext(ctx).Exec(
			`INSERT INTO acl_role_actions (id, role_id, entity_name, action, access_level, created_at, updated_at)
			 VALUES (?, ?, '*', ?, 'all', ?, ?)`,
			uuid.NewString(), roleID, action, now, now).Error; err != nil {
			return "", false, fmt.Errorf("services.SeedOwnerRole: grant %s: %w", action, err)
		}
	}
	return roleID, true, nil
}
