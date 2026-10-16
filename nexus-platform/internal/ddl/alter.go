package ddl

import "fmt"

// uniqueGenColDef is the canonical column definition of a unique field's generated column —
// ONE body shared by BuildAddUniqueColumn (ADD), BuildGuardUniqueColumn (MODIFY, Doctor A2a
// repair = migration 000015's ALTER shape) and BuildCreateTable's unique branch, so
// create-time, add-field-time and repair-time columns can never drift apart (the
// validator-parity discipline applied to DDL).
func uniqueGenColDef(fname string) string {
	return fmt.Sprintf(
		"`%s` VARCHAR(255) COLLATE utf8mb4_bin GENERATED ALWAYS AS (CASE WHEN `deleted_at` IS NULL THEN JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.%s')) ELSE NULL END) STORED",
		fname, fname)
}

// oneToOneRelGenColDef is the canonical column definition of a 1-1 relation field's
// CASE-guarded `<f>_id` FK column — shared by BuildAddOneToOneRelationColumn,
// BuildGuardOneToOneRelationColumn (Doctor A2a) and BuildCreateTable's 1-1 branch.
func oneToOneRelGenColDef(fname string) string {
	return fmt.Sprintf(
		"`%s_id` CHAR(36) GENERATED ALWAYS AS (CASE WHEN `deleted_at` IS NULL THEN JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.%s')) ELSE NULL END) STORED",
		fname, fname)
}

// polymorphicTypeColDef / polymorphicIDColDef are the canonical column definitions of a
// `polymorphic_relation` field's two STORED generated sub-columns — ONE body shared by
// BuildAddPolymorphicColumns (ADD) and BuildCreateTable's polymorphic branch, so create-time
// and add-field-time columns can never drift (the validator-parity discipline applied to DDL).
// `<f>_type` (VARCHAR(100)) projects the target entity name and `<f>_id` (CHAR(36)) the target
// record id, both from the JSON object stored under the bare field name
// ({"<f>":{"type":...,"id":...}}). Both are plain STORED (no `CASE WHEN deleted_at IS NULL`
// guard): unlike a unique/1-1 column they back a NON-unique composite index, so a soft-deleted
// row projecting its old value is harmless (no unique slot to squat).
func polymorphicTypeColDef(fname string) string {
	return fmt.Sprintf(
		"`%s_type` VARCHAR(100) GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.%s.type'))) STORED",
		fname, fname)
}

func polymorphicIDColDef(fname string) string {
	return fmt.Sprintf(
		"`%s_id` CHAR(36) GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.%s.id'))) STORED",
		fname, fname)
}

// BuildAddPolymorphicColumns returns the ALTER that adds a `polymorphic_relation` field
// (ACTIVITY-CORE #3a) to an existing entity's obj_* table: TWO STORED generated columns
// (`<field>_type` VARCHAR(100), `<field>_id` CHAR(36)) projecting the JSON {type,id} object,
// plus a NON-unique composite tenant-scoped index `idx_obj_<entity>_<field>` over
// (tenant_id, <field>_type, <field>_id) — the timeline query index. It mirrors
// BuildAddRelationColumn's shape (both columns + index in ONE statement) so a polymorphic field
// added later is indexed identically to one declared at create time. Both identifiers are
// sanitized; nothing is interpolated from raw input. The index name (9 + entity + field ≤ 61
// chars) fits MySQL's 64-char limit under SanitizeIdentifier's 26-char cap.
func BuildAddPolymorphicColumns(entity, field string) (string, error) {
	name, err := SanitizeIdentifier(entity)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildAddPolymorphicColumns: entity: %w", err)
	}
	fname, err := SanitizeIdentifier(field)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildAddPolymorphicColumns: field %q: %w", field, err)
	}
	table := "obj_" + name
	return fmt.Sprintf(
		"ALTER TABLE `%s` "+
			"ADD COLUMN %s, "+
			"ADD COLUMN %s, "+
			"ADD INDEX `idx_%s_%s` (`tenant_id`, `%s_type`, `%s_id`);",
		table, polymorphicTypeColDef(fname), polymorphicIDColDef(fname),
		table, fname, fname, fname), nil
}

// BuildDropPolymorphicColumns returns the ALTER that reverses BuildAddPolymorphicColumns (drops
// the composite index then both generated columns) — used only on the field/entity service's
// compensation path when the ALTER auto-committed but the metadata write failed afterward. Both
// identifiers are sanitized; nothing is interpolated from raw input.
func BuildDropPolymorphicColumns(entity, field string) (string, error) {
	name, err := SanitizeIdentifier(entity)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildDropPolymorphicColumns: entity: %w", err)
	}
	fname, err := SanitizeIdentifier(field)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildDropPolymorphicColumns: field %q: %w", field, err)
	}
	table := "obj_" + name
	return fmt.Sprintf(
		"ALTER TABLE `%s` "+
			"DROP INDEX `idx_%s_%s`, "+
			"DROP COLUMN `%s_type`, "+
			"DROP COLUMN `%s_id`;",
		table, table, fname, fname, fname), nil
}

// BuildAddUniqueColumn returns the ALTER statement that adds a unique field to an existing
// entity's obj_* table: a STORED generated column projecting the JSON value plus a
// tenant-scoped unique index. It mirrors the unique-field handling in BuildCreateTable, so a
// field added later is indexed identically to one declared at create time. Both identifiers
// are sanitized; nothing is interpolated from raw input.
func BuildAddUniqueColumn(entity, field string) (string, error) {
	name, err := SanitizeIdentifier(entity)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildAddUniqueColumn: entity: %w", err)
	}
	fname, err := SanitizeIdentifier(field)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildAddUniqueColumn: field %q: %w", field, err)
	}
	table := "obj_" + name
	// COLLATE utf8mb4_bin forces a byte-distinct unique index: without it the column
	// inherits the table's utf8mb4_unicode_ci collation, which is case- AND accent-
	// insensitive, so 'ABC1' == 'abc1' == 'àbc1' would collapse into one key and reject
	// distinct code/SKU values with a spurious 409 (GD1 bug #12). Mirror this in
	// BuildCreateTable's unique branch so create-time and add-field columns stay identical.
	//
	// W5-E1 conditional-NULL: the generated value is the field value ONLY while the row is
	// live (deleted_at IS NULL); a soft-deleted row projects NULL, which MySQL unique indexes
	// treat as distinct — so a soft-deleted record no longer SQUATS the unique slot and the
	// same email/sku can be re-created after a soft-delete (broken CRM delete→re-add flow
	// fixed). Two live rows with the same value still collide (both non-NULL). Kept identical
	// to BuildCreateTable's unique branch.
	return fmt.Sprintf(
		"ALTER TABLE `%s` "+
			"ADD COLUMN %s, "+
			"ADD UNIQUE KEY `uq_%s_%s` (`tenant_id`, `%s`);",
		table, uniqueGenColDef(fname), table, fname, fname), nil
}

// BuildGuardUniqueColumn returns the ALTER that rebuilds an EXISTING unique generated column
// into the W5-E1 conditional-NULL form — the per-column Go equivalent of migration 000015's
// stored-procedure sweep (ALTER ... MODIFY COLUMN back to the canonical definition, which
// recomputes the STORED values and rebuilds the unique index in place; proven idempotent).
// Used by the Doctor A2a fix when a column's GENERATION_EXPRESSION lost the
// `CASE WHEN deleted_at IS NULL` guard (the squat-bug regression class). If two LIVE rows
// already share a value the MODIFY's index rebuild fails LOUD with 1062 — the correct
// fail-fast, mirrored from 000015. Both identifiers are sanitized.
func BuildGuardUniqueColumn(entity, field string) (string, error) {
	name, err := SanitizeIdentifier(entity)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildGuardUniqueColumn: entity: %w", err)
	}
	fname, err := SanitizeIdentifier(field)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildGuardUniqueColumn: field %q: %w", field, err)
	}
	return fmt.Sprintf("ALTER TABLE `obj_%s` MODIFY COLUMN %s;", name, uniqueGenColDef(fname)), nil
}

// BuildDropUniqueColumn returns the ALTER that removes a unique field's generated column
// and its tenant-scoped unique index — the inverse of BuildAddUniqueColumn. It is used only
// on the compensation path when AddField persists the ALTER but then fails to write metadata
// (MySQL auto-commits DDL, so the column cannot roll back inside the metadata transaction).
// Dropping the index alongside the column keeps the table identical to its pre-ALTER state.
// Both identifiers are sanitized; nothing is interpolated from raw input.
func BuildDropUniqueColumn(entity, field string) (string, error) {
	name, err := SanitizeIdentifier(entity)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildDropUniqueColumn: entity: %w", err)
	}
	fname, err := SanitizeIdentifier(field)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildDropUniqueColumn: field %q: %w", field, err)
	}
	table := "obj_" + name
	return fmt.Sprintf(
		"ALTER TABLE `%s` "+
			"DROP INDEX `uq_%s_%s`, "+
			"DROP COLUMN `%s`;",
		table, table, fname, fname), nil
}

// BuildAddUniqueGencolNonUnique returns the ALTER statement that materializes a NEW `is_unique`
// field's physical column post RM-28 Slice B: the IDENTICAL byte-for-byte generated column
// BuildAddUniqueColumn emits (uniqueGenColDef — STORED, VARCHAR(255) COLLATE utf8mb4_bin,
// CASE-guarded against deleted_at) but a NON-unique tenant-scoped index (`idx_`, not `uq_`).
//
// WHY: after Slice B, a plain unique field's uniqueness is enforced SOLELY by the
// sys_unique_index pivot (dynrepo/pivot.go) — a physical UNIQUE index on obj_* would be ONE
// object shared by every tenant registering this entity name, re-introducing the exact
// cross-tenant coupling RM-28 exists to remove (../specs/019-rm28-pivot-uniqueness/spec.md §1.2). The
// physical column still exists and is still indexed — sort/filter/backfill-readback (§3.2's
// value-from-gencol invariant) are unaffected; only the CONSTRAINT moves off this column.
//
// The index name (`idx_obj_<entity>_<field>`) matches EXACTLY what migration 000051's demote
// sweep produces for an EXISTING field and what the A1 doctor repair now produces (one naming
// convention across create/add-field/migrate/repair, so a field's physical shape never depends
// on WHEN it was materialized). Used by AddField's unique branch (field_orchestration.go) and
// CreateEntity's shared-table reconcile (entity_orchestration.go) — CreateTable's OWN inline
// unique branch renders the equivalent shape directly (create_table.go).
//
// BuildAddUniqueColumn (ADD UNIQUE KEY) and its BuildDropUniqueColumn/BuildGuardUniqueColumn
// siblings are kept, UNCHANGED, as a tested DDL primitive — no production runtime path calls
// them anymore for a plain unique field (a 1-1 relation's own UNIQUE pair,
// BuildAddOneToOneRelationColumn, is a SEPARATE function and is NOT retired). They remain the
// documented shape a manual down-migration recovery (§2.2 down runbook) re-issues by hand.
//
// Both identifiers are sanitized; nothing is interpolated from raw input.
func BuildAddUniqueGencolNonUnique(entity, field string) (string, error) {
	name, err := SanitizeIdentifier(entity)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildAddUniqueGencolNonUnique: entity: %w", err)
	}
	fname, err := SanitizeIdentifier(field)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildAddUniqueGencolNonUnique: field %q: %w", field, err)
	}
	table := "obj_" + name
	idxName := fmt.Sprintf("idx_%s_%s", table, fname)
	return fmt.Sprintf(
		"ALTER TABLE `%s` ADD COLUMN %s, ADD INDEX `%s` (`tenant_id`, `%s`);",
		table, uniqueGenColDef(fname), idxName, fname), nil
}

// BuildDropUniqueGencolNonUnique reverses BuildAddUniqueGencolNonUnique (drops the non-unique
// `idx_` index then the generated column) — used only on the compensation path when the ALTER
// auto-committed but the metadata write failed afterward. Both identifiers are sanitized;
// nothing is interpolated from raw input.
func BuildDropUniqueGencolNonUnique(entity, field string) (string, error) {
	name, err := SanitizeIdentifier(entity)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildDropUniqueGencolNonUnique: entity: %w", err)
	}
	fname, err := SanitizeIdentifier(field)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildDropUniqueGencolNonUnique: field %q: %w", field, err)
	}
	table := "obj_" + name
	idxName := fmt.Sprintf("idx_%s_%s", table, fname)
	return fmt.Sprintf(
		"ALTER TABLE `%s` DROP INDEX `%s`, DROP COLUMN `%s`;",
		table, idxName, fname), nil
}

// BuildAddRelationColumn returns the ALTER that adds a 1-N `relation` field (FK-on-child,
// M3) to an existing entity's obj_* table: a STORED CHAR(36) `<field>_id` column projecting
// the JSON value, plus a NON-unique tenant-scoped index `idx_obj_<entity>_<field>_id`. It
// mirrors the unique-field handling in BuildCreateTable's relation branch, so a relation
// field added later is indexed identically to one declared at create time. FK columns must
// be indexed (rules/database.md) but the index is NOT unique — in a 1-N relation many child
// rows reference the same parent. The JSON key is the bare field name; the materialized
// column carries the `_id` suffix. Both identifiers are sanitized; nothing is interpolated
// from raw input.
func BuildAddRelationColumn(entity, field string) (string, error) {
	name, err := SanitizeIdentifier(entity)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildAddRelationColumn: entity: %w", err)
	}
	fname, err := SanitizeIdentifier(field)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildAddRelationColumn: field %q: %w", field, err)
	}
	table := "obj_" + name
	col := fname + "_id"
	return fmt.Sprintf(
		"ALTER TABLE `%s` "+
			"ADD COLUMN `%s` CHAR(36) GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.%s'))) STORED, "+
			"ADD INDEX `idx_%s_%s` (`tenant_id`, `%s`);",
		table, col, fname, table, col, col), nil
}

// BuildDropRelationColumn returns the ALTER that reverses BuildAddRelationColumn (drops the
// FK index then the generated `<field>_id` column) — the inverse used only on the field
// service's compensation path when the ALTER auto-committed but the metadata write failed
// afterward. Both identifiers are sanitized; nothing is interpolated from raw input.
func BuildDropRelationColumn(entity, field string) (string, error) {
	name, err := SanitizeIdentifier(entity)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildDropRelationColumn: entity: %w", err)
	}
	fname, err := SanitizeIdentifier(field)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildDropRelationColumn: field %q: %w", field, err)
	}
	table := "obj_" + name
	col := fname + "_id"
	return fmt.Sprintf(
		"ALTER TABLE `%s` "+
			"DROP INDEX `idx_%s_%s`, "+
			"DROP COLUMN `%s`;",
		table, table, col, col), nil
}

// BuildAddOneToOneRelationColumn returns the ALTER that adds a 1-1 `relation` field
// (ENG-REL2 #5, FK-on-child + tenant-scoped UNIQUE) to an existing entity's obj_* table:
// a STORED CHAR(36) `<field>_id` column CASE-guarded against deleted_at, plus a
// tenant-scoped UNIQUE key `uq_obj_<entity>_<field>_id` — in ONE statement, mirroring
// BuildAddRelationColumn's shape. The CASE guard (W5-E1 style) makes a soft-deleted row
// project NULL instead of squatting the unique slot forever, so re-pointing a NEW child
// record at the same parent after the old one is soft-deleted is not spuriously rejected.
// Both identifiers are sanitized; nothing is interpolated from raw input.
func BuildAddOneToOneRelationColumn(entity, field string) (string, error) {
	name, err := SanitizeIdentifier(entity)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildAddOneToOneRelationColumn: entity: %w", err)
	}
	fname, err := SanitizeIdentifier(field)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildAddOneToOneRelationColumn: field %q: %w", field, err)
	}
	table := "obj_" + name
	col := fname + "_id"
	return fmt.Sprintf(
		"ALTER TABLE `%s` "+
			"ADD COLUMN %s, "+
			"ADD UNIQUE KEY `uq_%s_%s` (`tenant_id`, `%s`);",
		table, oneToOneRelGenColDef(fname), table, col, col), nil
}

// BuildGuardOneToOneRelationColumn returns the ALTER that rebuilds an EXISTING 1-1 relation
// `<field>_id` generated column into the CASE-guarded form — the 1-1 sibling of
// BuildGuardUniqueColumn (Doctor A2a). Same MODIFY-in-place semantics and 1062 fail-fast as
// the unique variant. Both identifiers are sanitized.
func BuildGuardOneToOneRelationColumn(entity, field string) (string, error) {
	name, err := SanitizeIdentifier(entity)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildGuardOneToOneRelationColumn: entity: %w", err)
	}
	fname, err := SanitizeIdentifier(field)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildGuardOneToOneRelationColumn: field %q: %w", field, err)
	}
	return fmt.Sprintf("ALTER TABLE `obj_%s` MODIFY COLUMN %s;", name, oneToOneRelGenColDef(fname)), nil
}

// BuildDropOneToOneRelationColumn returns the ALTER that reverses
// BuildAddOneToOneRelationColumn (drops the UNIQUE index then the generated `<field>_id`
// column) — used only on the field/entity service's compensation path when the ALTER
// auto-committed but the metadata write failed afterward. Both identifiers are sanitized;
// nothing is interpolated from raw input.
func BuildDropOneToOneRelationColumn(entity, field string) (string, error) {
	name, err := SanitizeIdentifier(entity)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildDropOneToOneRelationColumn: entity: %w", err)
	}
	fname, err := SanitizeIdentifier(field)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildDropOneToOneRelationColumn: field %q: %w", field, err)
	}
	table := "obj_" + name
	col := fname + "_id"
	return fmt.Sprintf(
		"ALTER TABLE `%s` "+
			"DROP INDEX `uq_%s_%s`, "+
			"DROP COLUMN `%s`;",
		table, table, col, col), nil
}

// indexedColExpr returns the generated column type and expression for a given field type
// (D1 design table). The expression is embedded verbatim into ALTER/CREATE TABLE DDL; it
// never comes from raw client input (only the sanitized field name is interpolated).
//
//   - text / email / picklist / auto_number → VARCHAR(255) COLLATE utf8mb4_unicode_ci
//     (case-insensitive comparison via collation; unique columns use utf8mb4_bin instead, so the
//     two differ on purpose — indexed ≠ unique). auto_number (ENG #6) shares this shape because
//     its per-record value is a formatted string.
//   - number / rating → DECIMAL(38,10) via CAST, so sort/filter are arithmetic not lexicographic.
//   - currency → DECIMAL(38,10) on the $.f.amount sub-path (B1 fix: currency is stored as
//     {"amount":N,...}; the index must project the scalar amount, not the whole object).
//   - date → DATETIME via CAST; YYYY-MM-DD is safe for MySQL's DATETIME cast (S3).
//
// NOTE: datetime (RFC3339 "T"/"Z" format) is intentionally NOT handled here. CAST of an
// RFC3339 string to DATETIME fails under MySQL STRICT_TRANS_TABLES (Error 1292). The service
// layer (validateFieldConfig) rejects indexed=true on datetime fields before any DDL runs,
// so this case is unreachable from production code paths.
//
// Unknown field types return an error; the caller bubbles it up as a 422 before any DDL runs.
//
// The generated columns are VIRTUAL, not STORED (O4): they exist only to back a secondary index,
// and STORED would materialize them into the clustered index (heap bloat + full-COPY rebuild on
// ALTER). VIRTUAL is safe here despite "computed on read" because a secondary index on a VIRTUAL
// column materializes the expression INTO the index at WRITE time — so the CAST/length backstop
// STORED gave is PRESERVED: a value that fails CAST(...AS DECIMAL/DATETIME) or overflows VARCHAR(255)
// is still rejected at INSERT/UPDATE (Error 1264/1366/1406), never deferred to read (gate-verified,
// STORED↔VIRTUAL byte-parity over 5000 rows). Do NOT "fix" this back to STORED. (Unique/relation
// gen-cols stay STORED — a real FOREIGN KEY cannot reference a VIRTUAL column.)
func indexedColExpr(fname, fieldType string) (colDef, indexName string, err error) {
	switch fieldType {
	case "text", "email", "picklist", "auto_number":
		// auto_number (ENG #6): the per-record value is a formatted string (prefix + zero-padded
		// sequence number, e.g. "INV-00042") assigned server-side — same JSON-string shape as
		// text, so it shares the identical VARCHAR(255) VIRTUAL projection.
		colDef = fmt.Sprintf(
			"`%s` VARCHAR(255) COLLATE utf8mb4_unicode_ci GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.%s'))) VIRTUAL",
			fname, fname)
	case "number", "rating":
		colDef = fmt.Sprintf(
			"`%s` DECIMAL(38,10) GENERATED ALWAYS AS (CAST(JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.%s')) AS DECIMAL(38,10))) VIRTUAL",
			fname, fname)
	case "currency":
		// B1: currency stores {"amount":N,...}; project only the amount sub-path.
		colDef = fmt.Sprintf(
			"`%s` DECIMAL(38,10) GENERATED ALWAYS AS (CAST(JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.%s.amount')) AS DECIMAL(38,10))) VIRTUAL",
			fname, fname)
	case "date":
		// date values are stored as YYYY-MM-DD strings, which MySQL casts to DATETIME safely.
		colDef = fmt.Sprintf(
			"`%s` DATETIME GENERATED ALWAYS AS (CAST(JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.%s')) AS DATETIME)) VIRTUAL",
			fname, fname)
	default:
		return "", "", fmt.Errorf("field type %q không hỗ trợ indexed generated column (D1 hỗ trợ: text/email/picklist/auto_number/number/rating/currency/date)", fieldType)
	}
	return colDef, "", nil
}

// BuildAddIndexedColumn returns the ALTER statement that adds a non-unique indexed field to
// an existing entity's obj_* table: a VIRTUAL generated column typed per the field_type
// design table (D1) plus a NON-unique tenant-scoped index idx_<table>_<field>.
//
// Key differences from BuildAddUniqueColumn:
//   - Column type depends on fieldType (VARCHAR/DECIMAL/DATETIME), not always VARCHAR utf8mb4_bin.
//   - Index is ADD INDEX (not ADD UNIQUE KEY) — multiple rows may share the same value.
//   - Index name is idx_<table>_<field>, matching the relation-column pattern.
//
// Shared-table note: obj_<name> is shared across all tenants that register an entity with
// this name. Dropping a generated column (toggle-off via EditField) therefore affects ALL
// tenants sharing that entity-name — this is a known shared-table trade-off (§8 ARCHITECTURE).
// The toggle is scoped at the entity-definition level, not per-tenant.
//
// Both identifiers are sanitized; nothing is interpolated from raw input.
func BuildAddIndexedColumn(entity, field, fieldType string) (string, error) {
	name, err := SanitizeIdentifier(entity)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildAddIndexedColumn: entity: %w", err)
	}
	fname, err := SanitizeIdentifier(field)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildAddIndexedColumn: field %q: %w", field, err)
	}
	colDef, _, err := indexedColExpr(fname, fieldType)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildAddIndexedColumn: %w", err)
	}
	table := "obj_" + name
	idxName := fmt.Sprintf("idx_%s_%s", table, fname)
	return fmt.Sprintf(
		"ALTER TABLE `%s` ADD COLUMN %s, ADD INDEX `%s` (`tenant_id`, `%s`);",
		table, colDef, idxName, fname), nil
}

// BuildDropIndexedColumn returns the ALTER that reverses BuildAddIndexedColumn: drops the
// non-unique index then the generated column — the inverse used on the compensation path
// (metadata write failed after ALTER) or when EditField toggles indexed false.
//
// Shared-table caveat: dropping the generated column affects ALL tenants sharing this
// entity-name. See BuildAddIndexedColumn comment for details.
//
// Both identifiers are sanitized; nothing is interpolated from raw input.
func BuildDropIndexedColumn(entity, field string) (string, error) {
	name, err := SanitizeIdentifier(entity)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildDropIndexedColumn: entity: %w", err)
	}
	fname, err := SanitizeIdentifier(field)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildDropIndexedColumn: field %q: %w", field, err)
	}
	table := "obj_" + name
	idxName := fmt.Sprintf("idx_%s_%s", table, fname)
	return fmt.Sprintf(
		"ALTER TABLE `%s` DROP INDEX `%s`, DROP COLUMN `%s`;",
		table, idxName, fname), nil
}
