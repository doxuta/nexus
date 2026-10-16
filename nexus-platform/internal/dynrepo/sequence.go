package dynrepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ResetSequenceIfEmpty starts an auto_number counter over — but only when the table holds no row of
// the tenant at all (recycle bin included), so a number that a surviving row carries is never reused.
// ONE transaction: the FOR UPDATE on the counter row is the lock ClaimSequence's UPSERT waits on, and
// the emptiness check runs while it is held (specs/064 FR-036). No counter row ⇒ nothing to reset.
// ponytail: ClaimSequence hands the number out BEFORE the row is inserted (assignSequences), so a
// creator between those two steps is invisible to the check; the cost is a later duplicate number, only
// when the table is empty at exactly that moment. Upgrade: claim inside the insert transaction.
func (r *Repository) ResetSequenceIfEmpty(ctx context.Context, tenantID, entity, field string) (bool, error) {
	table, err := tableFor(entity)
	if err != nil {
		return false, fmt.Errorf("dynrepo.ResetSequenceIfEmpty: %w", err)
	}
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("dynrepo.ResetSequenceIfEmpty: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var next int64
	err = tx.GetContext(ctx, &next, "SELECT `next_val` FROM `sys_sequences` WHERE `tenant_id` = ? AND `entity_name` = ? AND `field_name` = ? FOR UPDATE",
		tenantID, entity, field)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("dynrepo.ResetSequenceIfEmpty: lock counter: %w", err)
	}
	var has bool
	if err := tx.GetContext(ctx, &has, fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM `%s` WHERE `tenant_id` = ?)", table), tenantID); err != nil {
		return false, fmt.Errorf("dynrepo.ResetSequenceIfEmpty: probe rows: %w", err)
	}
	if has {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM `sys_sequences` WHERE `tenant_id` = ? AND `entity_name` = ? AND `field_name` = ?",
		tenantID, entity, field); err != nil {
		return false, fmt.Errorf("dynrepo.ResetSequenceIfEmpty: delete counter: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("dynrepo.ResetSequenceIfEmpty: commit: %w", err)
	}
	return true, nil
}

// ClaimSequence atomically claims and advances the next_val counter in sys_sequences for a
// (tenantID, entityName, fieldName) triple — the per-tenant, per-entity, per-field auto_number
// sequence (ENG #6, ../specs/006-auto-number/spec.md §"#6 auto_number"). It returns the CLAIMED number
// (the value this caller should use), leaving the row's next_val one ahead for the next claim.
//
// The single-statement claim uses MySQL's INSERT ... ON DUPLICATE KEY UPDATE against the
// PRIMARY KEY(tenant_id, entity_name, field_name) (migration 000029) combined with the
// LAST_INSERT_ID(expr) side-effect function, so the claimed value is read off the driver's
// sql.Result.LastInsertId() with NO follow-up SELECT and no extra round trip:
//
//	INSERT INTO sys_sequences (tenant_id, entity_name, field_name, next_val)
//	VALUES (?, ?, ?, LAST_INSERT_ID(1) + 1)
//	ON DUPLICATE KEY UPDATE next_val = LAST_INSERT_ID(next_val) + 1
//
// Why LAST_INSERT_ID(1) and not the task-sketch's literal "VALUES (?,?,?,2)": sys_sequences has
// no AUTO_INCREMENT column, so the ONLY way to make the driver's LastInsertId() report a value is
// to explicitly call the LAST_INSERT_ID(expr) function somewhere in the executed statement — MySQL
// then sets the connection's last-insert-id state to expr and returns expr. A plain literal (no
// LAST_INSERT_ID() call) on the fresh-INSERT branch would leave that state at its default 0,
// because the ON DUPLICATE KEY UPDATE clause (the only place LAST_INSERT_ID appears in the naive
// version) never executes when the row didn't already exist. So the naive statement returns 1 on
// every claim AFTER the first, but 0 (not 1) on the very first claim for a brand-new triple — a
// real bug empirically caught by TestClaimSequence_FirstClaimReturnsOne (sequence_integration_test.go)
// before this fix. Wrapping the INSERT branch's own value in LAST_INSERT_ID(1) forces the
// side-effect on that branch too:
//   - INSERT branch (no existing row): LAST_INSERT_ID(1) sets state=1 and returns 1; the stored
//     next_val becomes 1+1=2. Claimed value = 1 (correct: first record gets 1).
//   - UPDATE branch (row exists, next_val=V): LAST_INSERT_ID(next_val) sets state=V (the value
//     about to be handed out) and returns V; the stored next_val becomes V+1. Claimed value = V.
//
// Concurrency safety: LAST_INSERT_ID() is a per-CONNECTION (session) value, so under concurrent
// claims on the SAME triple from different pooled connections, each caller's driver reads back
// ONLY its own statement's result — InnoDB's row-level lock on the UPSERT serializes the
// increments, and there is no cross-goroutine read of a shared "last insert id". Proven by
// TestClaimSequence_ConcurrentClaims_NoDuplicates (8 goroutines, same triple, all distinct values).
//
// Tenant isolation falls out of the composite key: a different tenantID is a different PRIMARY
// KEY row, so two tenants sharing an entity+field NAME (the shared obj_* table model) count
// independently — proven by TestClaimSequence_TenantIsolation.
func (r *Repository) ClaimSequence(ctx context.Context, tenantID, entityName, fieldName string) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO sys_sequences (tenant_id, entity_name, field_name, next_val)
		 VALUES (?, ?, ?, LAST_INSERT_ID(1) + 1)
		 ON DUPLICATE KEY UPDATE next_val = LAST_INSERT_ID(next_val) + 1`,
		tenantID, entityName, fieldName)
	if err != nil {
		return 0, fmt.Errorf("dynrepo.ClaimSequence: exec: %w", err)
	}
	claimed, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("dynrepo.ClaimSequence: last insert id: %w", err)
	}
	return claimed, nil
}
