// schema_repair.go — the Doctor DOCTOR-SCHEMA fix executors (../specs/008-nexus-doctor/spec.md §2.2
// A2a/A5). Every statement is built by a ddl.Build* helper (never bespoke SQL) and shares its
// definition body with BuildCreateTable, so a repaired table is byte-identical to a freshly
// created one. The A1/A6/B4 fixes reuse the pre-existing executors in dynrepo.go/relation.go
// (AddUniqueColumn, AddRelationColumn, AddOneToOneRelationColumn, AddIndexedColumn,
// CreateTable, CreateJoinTable) — nothing here duplicates them.
package dynrepo

import (
	"context"
	"fmt"

	"nexus/platform/internal/ddl"
)

// GuardUniqueColumn rebuilds a unique field's generated column into the W5-E1
// conditional-NULL form (Doctor A2a) — the per-column equivalent of migration 000015's
// stored-procedure sweep. MODIFY-in-place: the STORED values recompute and the existing
// unique index rebuilds under its unchanged name. ER_DUP_ENTRY (1062) means two LIVE rows
// already share a value so the unique index cannot rebuild — surfaced as the typed
// ErrDuplicateUnique sentinel (fail-loud, exactly like 000015), never retried blindly.
func (r *Repository) GuardUniqueColumn(ctx context.Context, entity, field string) error {
	stmt, err := ddl.BuildGuardUniqueColumn(entity, field)
	if err != nil {
		return fmt.Errorf("dynrepo.GuardUniqueColumn: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		if _, ok := duplicateIndexName(err); ok {
			return fmt.Errorf("dynrepo.GuardUniqueColumn: %w", ErrDuplicateUnique)
		}
		return fmt.Errorf("dynrepo.GuardUniqueColumn: exec: %w", err)
	}
	return nil
}

// GuardOneToOneRelationColumn rebuilds a 1-1 relation `<field>_id` generated column into the
// CASE-guarded form (Doctor A2a, 1-1 sibling of GuardUniqueColumn). Same MODIFY-in-place +
// 1062 → ErrDuplicateUnique semantics.
func (r *Repository) GuardOneToOneRelationColumn(ctx context.Context, entity, field string) error {
	stmt, err := ddl.BuildGuardOneToOneRelationColumn(entity, field)
	if err != nil {
		return fmt.Errorf("dynrepo.GuardOneToOneRelationColumn: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		if _, ok := duplicateIndexName(err); ok {
			return fmt.Errorf("dynrepo.GuardOneToOneRelationColumn: %w", ErrDuplicateUnique)
		}
		return fmt.Errorf("dynrepo.GuardOneToOneRelationColumn: exec: %w", err)
	}
	return nil
}

// AddMandatoryColumn adds ONE missing mandatory system column to an entity's obj_* table
// (Doctor A5 — e.g. `version` on a table predating migration 000013). MySQL 8 has no
// ADD COLUMN IF NOT EXISTS, so the caller re-detects against information_schema first; a
// lost race (ER_DUP_FIELDNAME 1060) maps to the ErrDuplicateKeyName sentinel → the Doctor
// counts it as already-clean instead of failed.
func (r *Repository) AddMandatoryColumn(ctx context.Context, entity, column string) error {
	stmt, err := ddl.BuildAddMandatoryColumn(entity, column)
	if err != nil {
		return fmt.Errorf("dynrepo.AddMandatoryColumn: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		if isDuplicateKeyName(err) {
			return fmt.Errorf("dynrepo.AddMandatoryColumn: %w", ErrDuplicateKeyName)
		}
		return fmt.Errorf("dynrepo.AddMandatoryColumn: exec: %w", err)
	}
	return nil
}

// EnsureMandatoryIndex (re)creates ONE mandatory index on an entity's obj_* table (Doctor
// A5 — pays down RM-47's composite (tenant_id, deleted_at, col, id) on pre-O1∪O6 tables).
// rebuild=true drops the wrong-shaped index in the same ALTER. Same re-detect-first contract
// and 1061 → ErrDuplicateKeyName mapping as AddMandatoryColumn.
func (r *Repository) EnsureMandatoryIndex(ctx context.Context, entity, index string, rebuild bool) error {
	stmt, err := ddl.BuildMandatoryIndexDDL(entity, index, rebuild)
	if err != nil {
		return fmt.Errorf("dynrepo.EnsureMandatoryIndex: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		if isDuplicateKeyName(err) {
			return fmt.Errorf("dynrepo.EnsureMandatoryIndex: %w", ErrDuplicateKeyName)
		}
		return fmt.Errorf("dynrepo.EnsureMandatoryIndex: exec: %w", err)
	}
	return nil
}
