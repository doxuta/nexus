package bpm

import (
	"context"
	"fmt"
	"strings"
)

// DuplicatePendingRow is one record that holds several PENDING approvals — what the old start-up
// race left behind, and what Migration B (uq_ai_open) refuses to migrate over. InstanceIDs run
// oldest first.
type DuplicatePendingRow struct {
	EntityName  string
	RecordID    string
	InstanceIDs []string
}

// ListDuplicatePending returns up to limit records of the tenant with more than one PENDING
// instance. Read-only, tenant-pinned, and deliberately independent of the lane / open_key columns:
// it must work on a schema BEFORE Migration B (that is when Nexus runs it, on a copy, to find the
// pairs it has to withdraw by hand) and it returns nothing after it (the unique key forbids them).
//
// ponytail: GROUP_CONCAT stops at group_concat_max_len (1024 bytes ≈ 27 ids); a record with more
// pending copies than that lists the first 27 — enough to act on, and it still counts as one finding.
func (r *SQLRepository) ListDuplicatePending(ctx context.Context, tenantID string, limit int) ([]DuplicatePendingRow, error) {
	const q = `SELECT entity_name, record_id, GROUP_CONCAT(id ORDER BY created_at, id SEPARATOR ',') AS ids
		FROM sys_approval_instances
		WHERE tenant_id = ? AND status = 'pending'
		GROUP BY entity_name, record_id
		HAVING COUNT(*) > 1
		ORDER BY entity_name, record_id
		LIMIT ?`
	var rows []struct {
		EntityName string `db:"entity_name"`
		RecordID   string `db:"record_id"`
		IDs        string `db:"ids"`
	}
	if err := r.db.SelectContext(ctx, &rows, q, tenantID, limit); err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.ListDuplicatePending: %w", err)
	}
	out := make([]DuplicatePendingRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, DuplicatePendingRow{EntityName: row.EntityName, RecordID: row.RecordID, InstanceIDs: strings.Split(row.IDs, ",")})
	}
	return out, nil
}
