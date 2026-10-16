package dynrepo

import (
	"context"
	"database/sql"
	"fmt"

	"nexus/platform/internal/ddl"
)

// pivotTable is the physical name of the per-tenant uniqueness pivot (migration 000050, RM-28).
const pivotTable = "sys_unique_index"

// UniqueFieldSpec carries the minimal per-field metadata the repo needs to maintain a record's
// rows in the sys_unique_index pivot (RM-28 §3.1 B1). The repository cannot import services /
// metadata, so the SERVICE resolves the entity's active unique fields and threads them down — the
// same decoupling pattern as DedupFieldSpec (record_import.go). It is deliberately narrow.
//
//   - FieldName is the logical field (== the value stored in pivot.field_name and the key used for
//     per-field DELETE + error attribution).
//   - GenCol is the physical STORED generated column the value is READ BACK from IN-TX. For an
//     ordinary unique field this equals the field name (ddl.BuildAddUniqueColumn names the column
//     after the field); it is kept as its own field so the read-back SQL is self-documenting and
//     the value is NEVER computed app-side (§3.2).
type UniqueFieldSpec struct {
	FieldName string
	GenCol    string
}

// pivotExecer is the minimal write surface the pivot helpers need: ExecContext, satisfied by both
// *sqlx.DB (autocommit standalone cleanup) and *sqlx.Tx (the record mutators, which run pivot
// maintenance in the SAME transaction as the obj_ write). Declared consumer-side so the helpers are
// tx/db-agnostic and the lock order (obj_ before sys_unique_index) is preserved by call ordering.
type pivotExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// insertPivotRow inserts ONE (record, field) row into sys_unique_index by READING THE VALUE BACK
// FROM the generated column in the SAME transaction (RM-28 §3.2 — the load-bearing invariant). The
// value is never marshalled in Go: the SELECT copies the gencol verbatim, inheriting its
// utf8mb4_bin bytes, JSON coercion, NULL-on-soft-delete CASE guard and 255-truncation EXACTLY, so
// the pivot value can never diverge from what the obj_ unique index enforces.
//
// When the gencol is NULL (field absent / soft-deleted row) the SELECT matches nothing → no pivot
// row is written (null frees the slot; empty string "" is a non-null value → a row IS written).
//
// The INSERT is field-granular so a uq_pivot 1062 is attributable to THIS field directly:
// DuplicateUniqueError{Field} carries the name (no index-name parsing). Transient contention
// (1213/1205) surfaces as ErrWriteConflict. table/genCol are sanitized (defense-in-depth on top of
// the service's own SanitizeIdentifier); tenantID, entityName, fieldName, recordID bind via `?`.
func insertPivotRow(ctx context.Context, ex pivotExecer, table, tenantID, entityName string, uf UniqueFieldSpec, recordID string) error {
	genCol, err := ddl.SanitizeIdentifier(uf.GenCol)
	if err != nil {
		return fmt.Errorf("dynrepo.insertPivotRow: gencol %q: %w", uf.GenCol, err)
	}
	stmt := fmt.Sprintf(
		"INSERT INTO `%s` (`id`, `tenant_id`, `entity_name`, `field_name`, `value`, `record_id`) "+
			"SELECT UUID(), `tenant_id`, ?, ?, `%s`, `id` "+
			"FROM `%s` WHERE `tenant_id` = ? AND `id` = ? AND `%s` IS NOT NULL",
		pivotTable, genCol, table, genCol)
	if _, err := ex.ExecContext(ctx, stmt, entityName, uf.FieldName, tenantID, recordID); err != nil {
		return classifyPivotErr(err, uf.FieldName)
	}
	return nil
}

// insertPivotForRecord inserts the pivot rows for EVERY unique field of a newly-written record
// (create / restore). Fields whose gencol reads back NULL contribute no row (handled by
// insertPivotRow). The first duplicate/contention error short-circuits so the caller can roll the
// enclosing transaction back.
func insertPivotForRecord(ctx context.Context, ex pivotExecer, table, tenantID, entityName, recordID string, ufs []UniqueFieldSpec) error {
	for _, uf := range ufs {
		if err := insertPivotRow(ctx, ex, table, tenantID, entityName, uf, recordID); err != nil {
			return err
		}
	}
	return nil
}

// insertPivotForBatch inserts pivot rows for a SET of freshly-inserted record ids in one statement
// per unique field (CSV import — RM-28 §3.3). It mirrors insertPivotRow's read-back-gencol
// invariant but scopes the SELECT to `id IN (?,…)`. An intra-batch or vs-existing duplicate trips
// uq_pivot 1062 → the whole import batch rolls back (all-or-nothing). An empty id set is a no-op.
func insertPivotForBatch(ctx context.Context, ex pivotExecer, table, tenantID, entityName string, recordIDs []string, ufs []UniqueFieldSpec) error {
	if len(recordIDs) == 0 || len(ufs) == 0 {
		return nil
	}
	placeholders := "?"
	for i := 1; i < len(recordIDs); i++ {
		placeholders += ",?"
	}
	for _, uf := range ufs {
		genCol, err := ddl.SanitizeIdentifier(uf.GenCol)
		if err != nil {
			return fmt.Errorf("dynrepo.insertPivotForBatch: gencol %q: %w", uf.GenCol, err)
		}
		stmt := fmt.Sprintf(
			"INSERT INTO `%s` (`id`, `tenant_id`, `entity_name`, `field_name`, `value`, `record_id`) "+
				"SELECT UUID(), `tenant_id`, ?, ?, `%s`, `id` "+
				"FROM `%s` WHERE `tenant_id` = ? AND `id` IN (%s) AND `%s` IS NOT NULL",
			pivotTable, genCol, table, placeholders, genCol)
		args := make([]any, 0, 3+len(recordIDs))
		args = append(args, entityName, uf.FieldName, tenantID)
		for _, id := range recordIDs {
			args = append(args, id)
		}
		if _, err := ex.ExecContext(ctx, stmt, args...); err != nil {
			return classifyPivotErr(err, uf.FieldName)
		}
	}
	return nil
}

// reconcilePivotForRecord updates the pivot after an obj_ UPDATE (RM-28 §3.4). For each unique
// field the patch TOUCHED (its name is a key in `data`), it DELETEs the record's pivot row for that
// field BY record_id+field (never by value — the old value is stale under REPEATABLE READ) and then
// re-INSERTs the value READ BACK from the merged gencol. A field the patch did not touch keeps its
// pivot row untouched (no needless lock contention). Null-clear (`{f:nil}` → merge removes the key →
// gencol NULL → re-INSERT writes nothing) and empty-string (`{f:""}` → gencol "" → a row) both fall
// out of the read-back automatically. Must run only when the obj_ UPDATE affected a row.
func reconcilePivotForRecord(ctx context.Context, ex pivotExecer, table, tenantID, entityName, recordID string, data map[string]any, ufs []UniqueFieldSpec) error {
	for _, uf := range ufs {
		if _, patched := data[uf.FieldName]; !patched {
			continue
		}
		if err := deletePivotByRecordField(ctx, ex, tenantID, entityName, recordID, uf.FieldName); err != nil {
			return err
		}
		if err := insertPivotRow(ctx, ex, table, tenantID, entityName, uf, recordID); err != nil {
			return err
		}
	}
	return nil
}

// deletePivotByRecordField removes ONE record's pivot row for ONE field. Used by the UPDATE
// reconcile before re-inserting the fresh read-back value.
func deletePivotByRecordField(ctx context.Context, ex pivotExecer, tenantID, entityName, recordID, fieldName string) error {
	stmt := fmt.Sprintf(
		"DELETE FROM `%s` WHERE `tenant_id` = ? AND `entity_name` = ? AND `record_id` = ? AND `field_name` = ?",
		pivotTable)
	if _, err := ex.ExecContext(ctx, stmt, tenantID, entityName, recordID, fieldName); err != nil {
		return classifyPivotErr(err, fieldName)
	}
	return nil
}

// deletePivotByRecord removes ALL of one record's pivot rows (every field), releasing its unique
// slots. Used by single soft-delete — the record leaves the live set, mirroring the gencol → NULL
// projection the CASE guard produces.
func deletePivotByRecord(ctx context.Context, ex pivotExecer, tenantID, entityName, recordID string) error {
	stmt := fmt.Sprintf(
		"DELETE FROM `%s` WHERE `tenant_id` = ? AND `entity_name` = ? AND `record_id` = ?",
		pivotTable)
	if _, err := ex.ExecContext(ctx, stmt, tenantID, entityName, recordID); err != nil {
		return classifyPivotErr(err, "")
	}
	return nil
}

// deletePivotByEntity removes ALL pivot rows of ONE tenant's entity (whole-entity release). Used by
// entity uninstall's SoftDeleteRecords so a recreated entity of the same name does not inherit the
// old tenant's unique slots.
func deletePivotByEntity(ctx context.Context, ex pivotExecer, tenantID, entityName string) error {
	stmt := fmt.Sprintf(
		"DELETE FROM `%s` WHERE `tenant_id` = ? AND `entity_name` = ?",
		pivotTable)
	if _, err := ex.ExecContext(ctx, stmt, tenantID, entityName); err != nil {
		return classifyPivotErr(err, "")
	}
	return nil
}

// deletePivotByField removes ALL pivot rows of ONE tenant's (entity, field). Used by DeleteField so
// a soft-deleted-then-recreated unique field does not leak a value into the pivot (which would 409
// a record the tenant can no longer see).
func deletePivotByField(ctx context.Context, ex pivotExecer, tenantID, entityName, fieldName string) error {
	stmt := fmt.Sprintf(
		"DELETE FROM `%s` WHERE `tenant_id` = ? AND `entity_name` = ? AND `field_name` = ?",
		pivotTable)
	if _, err := ex.ExecContext(ctx, stmt, tenantID, entityName, fieldName); err != nil {
		return classifyPivotErr(err, fieldName)
	}
	return nil
}

// classifyPivotErr maps a pivot statement error to a typed sentinel: uq_pivot 1062 →
// *DuplicateUniqueError carrying the KNOWN field (no index-name parsing); transient 1213/1205 →
// ErrWriteConflict (retryable); anything else is wrapped verbatim. field is the pivot field the
// statement acted on ("" for record/entity-scoped deletes, which cannot 1062).
func classifyPivotErr(err error, field string) error {
	if idxName, ok := duplicateIndexName(err); ok {
		return fmt.Errorf("dynrepo.pivot: %w", &DuplicateUniqueError{IndexName: idxName, Field: field})
	}
	if isWriteConflict(err) {
		return fmt.Errorf("dynrepo.pivot: %w", ErrWriteConflict)
	}
	return fmt.Errorf("dynrepo.pivot: %w", err)
}

// entityNameForPivot returns the sanitized entity name used for the pivot's entity_name column
// (the same identifier that names the obj_ table). Kept as a helper so every mutator resolves it
// identically.
func entityNameForPivot(entity string) (string, error) {
	name, err := ddl.SanitizeIdentifier(entity)
	if err != nil {
		return "", fmt.Errorf("dynrepo.entityNameForPivot: %w", err)
	}
	return name, nil
}
