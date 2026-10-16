package metadata

import (
	"context"
	"fmt"
)

// restore_exec.go — specs/063-draft-publish S7-10: the inverse of the soft deletes a published
// field / table delete performs, as plain DML on the rollback transaction. Every statement re-pins
// BOTH the id AND the tenant_id (a forged id of another tenant matches 0 rows) and only touches a row
// that IS soft-deleted, so a restore of something already alive is a visible no-op, never a second write.

// RestoreFieldExec brings a soft-deleted field back. The field row was never renamed (a field delete
// is a plain deleted_at), so its (tenant, entity, name) slot is still its own: there is no name to
// clash with. 0 rows -> ErrFieldNotFound (alive already, unknown, or another tenant's).
func RestoreFieldExec(ctx context.Context, ex Execer, tenantID, fieldID string) error {
	res, err := ex.ExecContext(ctx, `UPDATE sys_fields SET deleted_at = NULL, updated_at = NOW()
		WHERE id = ? AND tenant_id = ? AND deleted_at IS NOT NULL`, fieldID, tenantID)
	if err != nil {
		return fmt.Errorf("metadata.RestoreFieldExec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("metadata.RestoreFieldExec: rows affected: %w", err)
	}
	if n == 0 {
		return ErrFieldNotFound
	}
	return nil
}

// RestoreEntityExec brings a soft-deleted table back under its ORIGINAL name (the delete renamed it to
// "<name>__deleted_<id8>"). If a live table took the name meanwhile, uq_sys_entities_tenant_name fires
// (MySQL 1062) and the caller maps it to ROLLBACK_NAME_TAKEN. label / description are the values of
// the version being restored. 0 rows -> ErrEntityNotFound.
func RestoreEntityExec(ctx context.Context, ex Execer, tenantID, entityID, name, label, description string) error {
	res, err := ex.ExecContext(ctx, `UPDATE sys_entities SET name = ?, label = ?, description = ?, deleted_at = NULL, updated_at = NOW()
		WHERE id = ? AND tenant_id = ? AND deleted_at IS NOT NULL`, name, label, description, entityID, tenantID)
	if err != nil {
		return fmt.Errorf("metadata.RestoreEntityExec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("metadata.RestoreEntityExec: rows affected: %w", err)
	}
	if n == 0 {
		return ErrEntityNotFound
	}
	return nil
}

// RelationshipRestore is one sys_relationships row as it was before the delete tombstoned it.
type RelationshipRestore struct {
	ID            string
	Name          string
	JoinTableName *string
}

// RestoreRelationshipsExec un-tombstones relationship rows: the original name and join table name come
// back and deleted_at clears. A row that is alive already (or gone) matches 0 rows and is skipped; a
// name a live relationship holds meanwhile fires uq_sysrel_tenant_name / uq_sysrel_tenant_join_table
// (1062, mapped by the caller to ROLLBACK_NAME_TAKEN). Returns how many rows came back.
func RestoreRelationshipsExec(ctx context.Context, ex Execer, tenantID string, rels []RelationshipRestore) (int, error) {
	restored := 0
	for _, r := range rels {
		res, err := ex.ExecContext(ctx, `UPDATE sys_relationships SET name = ?, join_table_name = ?, deleted_at = NULL, updated_at = NOW()
			WHERE id = ? AND tenant_id = ? AND deleted_at IS NOT NULL`, r.Name, r.JoinTableName, r.ID, tenantID)
		if err != nil {
			return restored, fmt.Errorf("metadata.RestoreRelationshipsExec: %q: %w", r.Name, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return restored, fmt.Errorf("metadata.RestoreRelationshipsExec: rows affected: %w", err)
		}
		restored += int(n)
	}
	return restored, nil
}
