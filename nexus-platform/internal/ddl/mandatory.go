package ddl

import "fmt"

// mandatory.go — the Doctor A5 repair builders (../specs/008-nexus-doctor/spec.md §2.2): re-issue the
// EXACT mandatory column/index definitions BuildCreateTable uses (MandatoryColumns /
// MandatoryIndexes are the single source both render from) as targeted ALTERs for a
// pre-existing obj_* table that predates them. This pays down RM-47: pre-O1∪O6 tables carry
// the old 1-column idx_created_at (or none) instead of the composite
// (tenant_id, deleted_at, created_at, id).
//
// MySQL 8 has no `ADD COLUMN IF NOT EXISTS` / `ADD INDEX IF NOT EXISTS` (that is MariaDB), so
// idempotency is the CALLER's contract: the Doctor re-detects each item against
// information_schema immediately before executing, and the dynrepo exec maps a lost race
// (1060/1061 duplicate name) to the ErrDuplicateKeyName sentinel → skipped, never failed.

// BuildAddMandatoryColumn returns the ALTER that adds ONE missing mandatory system column to
// an entity's obj_* table (e.g. `version` on a table created before migration 000013 —
// same semantics as that sweep). The column name must be a member of MandatoryColumns;
// anything else errors before any DDL could run.
func BuildAddMandatoryColumn(entity, column string) (string, error) {
	name, err := SanitizeIdentifier(entity)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildAddMandatoryColumn: entity: %w", err)
	}
	for _, c := range MandatoryColumns {
		if c.Name == column {
			return fmt.Sprintf("ALTER TABLE `obj_%s` ADD COLUMN %s;", name, c.Def), nil
		}
	}
	return "", fmt.Errorf("ddl.BuildAddMandatoryColumn: %q không phải mandatory column", column)
}

// BuildMandatoryIndexDDL returns the ALTER that (re)creates ONE mandatory index on an
// entity's obj_* table. rebuild=false emits a plain ADD (index absent); rebuild=true emits
// DROP INDEX + ADD in one statement for an index that exists under the right NAME but the
// wrong COLUMN SHAPE (the RM-47 class). Rebuilding the PRIMARY KEY is refused: DROP PRIMARY
// KEY on a shared obj_* table is destructive territory the Doctor's additive-only guardrail
// (spec §3) does not cover — a mis-shaped PK (never produced by any engine version) needs a
// human. The index name must be a member of MandatoryIndexes.
func BuildMandatoryIndexDDL(entity, index string, rebuild bool) (string, error) {
	name, err := SanitizeIdentifier(entity)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildMandatoryIndexDDL: entity: %w", err)
	}
	for _, ix := range MandatoryIndexes {
		if ix.Name != index {
			continue
		}
		if !rebuild {
			return fmt.Sprintf("ALTER TABLE `obj_%s` ADD %s;", name, ix.Def), nil
		}
		if ix.Name == "PRIMARY" {
			return "", fmt.Errorf("ddl.BuildMandatoryIndexDDL: không rebuild PRIMARY KEY (DROP PK trên bảng dùng chung là destructive)")
		}
		return fmt.Sprintf("ALTER TABLE `obj_%s` DROP INDEX `%s`, ADD %s;", name, ix.Name, ix.Def), nil
	}
	return "", fmt.Errorf("ddl.BuildMandatoryIndexDDL: %q không phải mandatory index", index)
}
