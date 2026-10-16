// pivot_backfill.go — RM-28 Slice A2: the backfill routine (spec §2.3) and the read/write
// surface the Doctor D1 bidirectional reconcile check (spec §6) drives. ONE routine
// (BackfillUniqueField) is reused by BOTH callers — AddField's unique-conversion path
// (field_orchestration.go §4.1) and the Doctor D1 "fill" repair — per the ponytail directive
// not to write the backfill logic twice.
//
// Everything here operates on ONE (tenant, entity, field) at a time; every query is
// tenant-scoped (`tenant_id = ?`) and every identifier (table, generated column) goes through
// ddl.SanitizeIdentifier before being composed into SQL — the same discipline pivot.go's
// write-path helpers already follow.
package dynrepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/ddl"
)

// BackfillConflict is one dupe tuple the pre-scan found for a (tenant, entity, field) backfill:
// N (≥2) LIVE rows share this gencol value, so none of them can enter the pivot (which enforces
// (tenant,entity,field,value) uniqueness) until a human dedupes the tenant's data (RM-28 §2.3).
type BackfillConflict struct {
	Value string `db:"value"`
	Count int    `db:"cnt"`
}

// BackfillResult is what BackfillUniqueField did for ONE (tenant, entity, field).
type BackfillResult struct {
	// Inserted is the number of pivot rows the backfill actually wrote (the dupe-free
	// remainder — always safe to insert, never conflicts with an existing constraint).
	Inserted int64
	// Conflicts lists every dupe value tuple the pre-scan found (empty when this tenant's live
	// data for the field has no duplicates). A non-empty Conflicts means the backfill is
	// INCOMPLETE for those specific values — the rows sharing a conflicting value were
	// excluded from the insert, never partially/arbitrarily picked. Re-running after the
	// tenant dedupes completes the field.
	Conflicts []BackfillConflict
}

// BackfillUniqueField populates sys_unique_index from the LIVE obj_<entity> rows of ONE
// (tenant, field) — RM-28 §2.3. Idempotent + re-runnable: the INSERT is keyed on
// `record_id NOT EXISTS` (never `INSERT IGNORE`, which would silently swallow a genuine
// duplicate instead of reporting it). Only live rows contribute
// (`deleted_at IS NULL AND <gencol> IS NOT NULL`). The value is read FROM THE GENCOL — never
// re-derived from JSON — so it is byte-identical to what the old obj_ unique index enforces
// (§3.2's invariant, reused here for the backfill's own INSERT…SELECT).
//
// A dupe tuple (≥2 live rows sharing one value) is reported as a BackfillConflict and EXCLUDED
// from the INSERT — spec v0's literal INSERT…SELECT HAD no exclusion clause; run as-is, a real
// duplicate would trip uq_pivot's 1062 mid-statement and MySQL would abort the WHOLE
// INSERT…SELECT atomically — undoing rows that were NOT part of the conflict too. This
// implementation carries the `NOT IN (<conflict values>)` predicate the current spec §2.3
// mandates (its ⚠️ callout preserves the v0-mismatch history) so the
// dupe-free remainder is provably inserted in the same statement (../specs/019-rm28-pivot-uniqueness/spec.md
// §2.3 — flagged as a spec/SQL mismatch, not a silent deviation).
//
// Runs under REPEATABLE READ (the default isolation of BeginTxx(ctx, nil)) so the pre-scan and
// the INSERT observe a consistent snapshot (§2.3's isolation note) — owns its own tx,
// self-contained; safe to call standalone (AddField) or from a Doctor fix.
func (r *Repository) BackfillUniqueField(ctx context.Context, entity, tenantID, field string) (*BackfillResult, error) {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.BackfillUniqueField: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := BackfillUniqueFieldTx(ctx, tx, entity, tenantID, field)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("dynrepo.BackfillUniqueField: commit: %w", err)
	}
	return res, nil
}

// BackfillUniqueFieldTx is BackfillUniqueField on a transaction the caller owns and commits: the
// rollback of a deleted unique field / table (specs/063 S7-10) rebuilds the pivot INSIDE the rollback
// tx so a conflict undoes the whole restore. The caller decides what a non-empty Conflicts means.
func BackfillUniqueFieldTx(ctx context.Context, tx *sqlx.Tx, entity, tenantID, field string) (*BackfillResult, error) {
	entityName, err := entityNameForPivot(entity)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.BackfillUniqueField: %w", err)
	}
	genCol, err := ddl.SanitizeIdentifier(field)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.BackfillUniqueField: field %q: %w", field, err)
	}
	objTable := "obj_" + entityName

	dupeStmt := fmt.Sprintf(
		"SELECT `%s` AS value, COUNT(*) AS cnt FROM `%s` "+
			"WHERE `tenant_id` = ? AND `deleted_at` IS NULL AND `%s` IS NOT NULL "+
			"GROUP BY `%s` HAVING COUNT(*) > 1",
		genCol, objTable, genCol, genCol)
	var conflicts []BackfillConflict
	if err := tx.SelectContext(ctx, &conflicts, dupeStmt, tenantID); err != nil {
		return nil, fmt.Errorf("dynrepo.BackfillUniqueField: pre-scan: %w", err)
	}

	insertStmt := fmt.Sprintf(
		"INSERT INTO `%s` (`id`, `tenant_id`, `entity_name`, `field_name`, `value`, `record_id`) "+
			"SELECT UUID(), o.`tenant_id`, ?, ?, o.`%s`, o.`id` "+
			"FROM `%s` o "+
			"WHERE o.`tenant_id` = ? AND o.`deleted_at` IS NULL AND o.`%s` IS NOT NULL "+
			"AND NOT EXISTS (SELECT 1 FROM `%s` p WHERE p.`tenant_id` = o.`tenant_id` "+
			"AND p.`entity_name` = ? AND p.`field_name` = ? AND p.`record_id` = o.`id`)",
		pivotTable, genCol, objTable, genCol, pivotTable)
	args := []any{entityName, field, tenantID, entityName, field}
	if len(conflicts) > 0 {
		placeholders := make([]string, len(conflicts))
		for i, c := range conflicts {
			placeholders[i] = "?"
			args = append(args, c.Value)
		}
		insertStmt += fmt.Sprintf(" AND o.`%s` NOT IN (%s)", genCol, strings.Join(placeholders, ","))
	}
	res, err := tx.ExecContext(ctx, insertStmt, args...)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.BackfillUniqueField: insert: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("dynrepo.BackfillUniqueField: rows affected: %w", err)
	}
	return &BackfillResult{Inserted: inserted, Conflicts: conflicts}, nil
}

// CountMissingPivotRows reports how many LIVE obj_<entity> rows of (tenant, field) have NO
// sys_unique_index row matching BOTH their record_id AND their CURRENT gencol value — a "missed
// write" (a) per spec §6: either the mutator's pivot INSERT never ran (record_id absent
// entirely), OR the pivot row present for that record_id is STALE (holds a value the record no
// longer has — e.g. a squatting row the (b) orphan direction hasn't deleted yet). Matching on
// value too (not record_id alone) is what makes orphan-then-fill resolve in ONE Detect→Fix pass:
// a stale-value row would otherwise satisfy a record_id-only NOT EXISTS and hide the fill
// finding until a SECOND scan (after the orphan repair ran). The actual repair
// (BackfillUniqueField's INSERT) still keys its own NOT EXISTS on record_id alone, per spec
// §2.3 — that is correct BECAUSE the orphan direction runs first in the SAME Fix pass and
// deletes the stale row before the fill's INSERT ever executes (see doctor_pivot_checks.go's
// orphan-before-fill ordering). Used by the Doctor D1 reconcile Detect (which must stay
// read-only) to decide whether a fill finding is warranted, without mutating anything.
func (r *Repository) CountMissingPivotRows(ctx context.Context, entity, tenantID, field string) (int, error) {
	entityName, err := entityNameForPivot(entity)
	if err != nil {
		return 0, fmt.Errorf("dynrepo.CountMissingPivotRows: %w", err)
	}
	genCol, err := ddl.SanitizeIdentifier(field)
	if err != nil {
		return 0, fmt.Errorf("dynrepo.CountMissingPivotRows: field %q: %w", field, err)
	}
	objTable := "obj_" + entityName
	stmt := fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` o WHERE o.`tenant_id` = ? AND o.`deleted_at` IS NULL AND o.`%s` IS NOT NULL "+
			"AND NOT EXISTS (SELECT 1 FROM `%s` p WHERE p.`tenant_id` = o.`tenant_id` "+
			"AND p.`entity_name` = ? AND p.`field_name` = ? AND p.`record_id` = o.`id` AND p.`value` = o.`%s`)",
		objTable, genCol, pivotTable, genCol)
	var n int
	if err := r.db.GetContext(ctx, &n, stmt, tenantID, entityName, field); err != nil {
		return 0, fmt.Errorf("dynrepo.CountMissingPivotRows: %w", err)
	}
	return n, nil
}

// ListOrphanPivotIDs returns up to limit sys_unique_index row ids of (tenant, entity, field)
// that no LIVE obj_ row currently projects — the "orphan/ghost" direction of the Doctor D1
// reconcile (spec §6): a pivot row survives a soft-delete/value-change/DeleteField-window that
// should have released or moved it. Matching requires BOTH the record to be live AND its
// CURRENT gencol value to equal the pivot row's stored value (so a stale-value drift, not just
// a missing record, also counts as orphan). Read-only.
func (r *Repository) ListOrphanPivotIDs(ctx context.Context, entity, tenantID, field string, limit int) ([]string, error) {
	entityName, err := entityNameForPivot(entity)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.ListOrphanPivotIDs: %w", err)
	}
	genCol, err := ddl.SanitizeIdentifier(field)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.ListOrphanPivotIDs: field %q: %w", field, err)
	}
	objTable := "obj_" + entityName
	stmt := fmt.Sprintf(
		"SELECT p.`id` FROM `%s` p WHERE p.`tenant_id` = ? AND p.`entity_name` = ? AND p.`field_name` = ? "+
			"AND NOT EXISTS (SELECT 1 FROM `%s` o WHERE o.`tenant_id` = p.`tenant_id` AND o.`id` = p.`record_id` "+
			"AND o.`deleted_at` IS NULL AND o.`%s` = p.`value`) "+
			"ORDER BY p.`id` LIMIT ?",
		pivotTable, objTable, genCol)
	var ids []string
	if err := r.db.SelectContext(ctx, &ids, stmt, tenantID, entityName, field, limit); err != nil {
		return nil, fmt.Errorf("dynrepo.ListOrphanPivotIDs: %w", err)
	}
	return ids, nil
}

// PivotRow is one sys_unique_index row, projected for the Doctor D1 reconcile's per-item
// re-detect (GetPivotRow resolves a finding's owning entity/field before re-checking it).
type PivotRow struct {
	ID         string `db:"id"`
	EntityName string `db:"entity_name"`
	FieldName  string `db:"field_name"`
	RecordID   string `db:"record_id"`
	Value      string `db:"value"`
}

// GetPivotRow resolves one sys_unique_index row by id, tenant-scoped. Returns (nil, nil) when
// the row is gone (already deleted — a stale finding). Read-only.
func (r *Repository) GetPivotRow(ctx context.Context, tenantID, id string) (*PivotRow, error) {
	stmt := fmt.Sprintf(
		"SELECT `id`, `entity_name`, `field_name`, `record_id`, `value` FROM `%s` WHERE `tenant_id` = ? AND `id` = ?",
		pivotTable)
	var row PivotRow
	if err := r.db.GetContext(ctx, &row, stmt, tenantID, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("dynrepo.GetPivotRow: %w", err)
	}
	return &row, nil
}

// PivotRecordLive reports whether (tenant, entity, recordID) is a LIVE obj_ row whose <field>
// gencol currently equals value — i.e. whether a pivot row still legitimately projects that
// value. Used by the Doctor D1 orphan fix to re-detect a single item before deleting it (the
// record may have been restored, or its value may have converged back, between scan and fix).
// Read-only.
func (r *Repository) PivotRecordLive(ctx context.Context, tenantID, entity, field, recordID, value string) (bool, error) {
	entityName, err := entityNameForPivot(entity)
	if err != nil {
		return false, fmt.Errorf("dynrepo.PivotRecordLive: %w", err)
	}
	genCol, err := ddl.SanitizeIdentifier(field)
	if err != nil {
		return false, fmt.Errorf("dynrepo.PivotRecordLive: field %q: %w", field, err)
	}
	objTable := "obj_" + entityName
	stmt := fmt.Sprintf(
		"SELECT EXISTS(SELECT 1 FROM `%s` WHERE `tenant_id` = ? AND `id` = ? AND `deleted_at` IS NULL AND `%s` = ?)",
		objTable, genCol)
	var exists int
	if err := r.db.GetContext(ctx, &exists, stmt, tenantID, recordID, value); err != nil {
		return false, fmt.Errorf("dynrepo.PivotRecordLive: %w", err)
	}
	return exists == 1, nil
}

// DeletePivotRow removes ONE sys_unique_index row by id, tenant-scoped — the Doctor D1 orphan
// repair. A single statement (no obj_ coordination needed): deleting an orphan pivot row can
// only ever free a value slot, never lose live data.
func (r *Repository) DeletePivotRow(ctx context.Context, tenantID, id string) error {
	stmt := fmt.Sprintf("DELETE FROM `%s` WHERE `tenant_id` = ? AND `id` = ?", pivotTable)
	if _, err := r.db.ExecContext(ctx, stmt, tenantID, id); err != nil {
		return fmt.Errorf("dynrepo.DeletePivotRow: exec: %w", err)
	}
	return nil
}
