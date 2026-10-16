package ddl

import (
	"fmt"
	"strings"
)

// FieldDef is the minimal field description the DDL builder needs. The metadata
// service maps sys_fields rows onto this before building statements.
type FieldDef struct {
	Name      string // sanitized snake_case field name
	FieldType string // text|number|boolean|date|datetime|email|picklist|relation|user
	IsUnique  bool   // unique fields get a generated column + tenant-scoped unique index
	// RelatedEntity is the target entity name for a 1-N `relation` field (FK-on-child,
	// M3). It is read from sys_fields.options.relatedEntity. When FieldType=="relation"
	// the builder materializes a CHAR(36) `<name>_id` FK column + a non-unique
	// tenant-scoped index; ignored for every other field type.
	//
	// A `user` field (USER-FIELD slice) shares the IDENTICAL plain-1-N physical shape
	// (below) but has a FIXED, implicit target — the `users` system table, which is never
	// a sys_entity and therefore never has a RelatedEntity to carry. It reuses the
	// `relation` branch's DDL as-is (RelatedEntity/OneToOne stay their zero value).
	RelatedEntity string
	// OneToOne (ENG-REL2 #5) marks a `relation` field as 1-1 instead of the default 1-N.
	// Ignored for every non-relation field type. When true, BuildCreateTable materializes
	// the `<f>_id` FK column CASE-guarded against deleted_at (soft-deleted rows project
	// NULL instead of squatting the slot — mirrors the IsUnique W5-E1 guard above) plus a
	// tenant-scoped UNIQUE key (not the plain non-unique 1-N index) — enforcing at most one
	// LIVE child row per (tenant, parent). Read from sys_fields.options.cardinality == "1-1".
	OneToOne bool
	// Indexed, when true, materializes a VIRTUAL generated column typed per the D1 design
	// table (text→VARCHAR, number/currency/rating→DECIMAL, date/datetime→DATETIME) plus a
	// NON-unique tenant-scoped index idx_<table>_<field>. Ignored when IsUnique is also
	// true — the unique generated column already provides a physical index, so adding a
	// second column for the same field would cause a duplicate-column error.
	Indexed bool
	// Polymorphic (ACTIVITY-CORE #3a) marks a `polymorphic_relation` field: its per-record
	// value is a JSON object {type, id} stored under the bare field name, pointing at a record
	// in ANY entity of an allowlist (options.targetEntities). BuildCreateTable materializes TWO
	// STORED generated columns — `<f>_type` (VARCHAR(100)) and `<f>_id` (CHAR(36)) — plus a
	// NON-unique composite index (tenant_id, <f>_type, <f>_id) for the timeline query
	// `WHERE tenant_id=? AND <f>_type=? AND <f>_id=?`. Mutually exclusive with IsUnique/relation/
	// Indexed (a {type,id} object has no single scalar projection). Ignored for every other type.
	Polymorphic bool
}

// MandatoryColumn is one fixed system column every obj_* table carries (rules/database.md):
// Name is the physical column name; Def is the definition fragment as it appears verbatim in
// both CREATE TABLE (BuildCreateTable) and the Doctor A5 repair ALTER (BuildAddMandatoryColumn)
// — one definition body, so create-time and repair-time columns can never drift apart.
type MandatoryColumn struct {
	Name string
	Def  string
}

// MandatoryColumns is the single source of truth for the fixed obj_* columns. Dynamic field
// values live in `data`; only unique/relation/indexed fields get extra generated columns.
// The Doctor's A5 check diffs information_schema.COLUMNS against this list.
var MandatoryColumns = []MandatoryColumn{
	{Name: "id", Def: "`id` CHAR(36) NOT NULL"},
	{Name: "tenant_id", Def: "`tenant_id` CHAR(36) NOT NULL"},
	{Name: "team_set_id", Def: "`team_set_id` CHAR(36) DEFAULT NULL"},
	{Name: "created_by", Def: "`created_by` CHAR(36) NOT NULL"},
	// owner_id (TRỤ 2 mục 2 "Giao việc & đổi chủ", docs/NEXUS-MOT-XUONG-2026-07-29.md §3d mục 2):
	// the record's CURRENT, REASSIGNABLE owner — separate from `created_by`, which stays the
	// immutable creator (dynrepo.Record.CreatedBy feeds the workflow "$owner" notification
	// sentinel and every audit trail that reads it as "who made this record"; repurposing it
	// would silently corrupt both). Starts equal to the creator (every INSERT path writes it
	// alongside created_by) and is the ONLY column PUT /:entity/:id/owner
	// (services.RecordService.SetOwner) ever writes. Nullable — rowscope.go's AccessOwner/
	// AccessTeam predicates read COALESCE(owner_id, created_by), so a row whose owner_id is
	// still NULL (a legacy row before the migration 000061 backfill runs, or the Doctor's
	// generic ADD-COLUMN repair which does not backfill values) keeps EXACTLY today's
	// visibility — never less visible, so introducing this column can never itself leak data.
	{Name: "owner_id", Def: "`owner_id` CHAR(36) DEFAULT NULL"},
	{Name: "modified_by", Def: "`modified_by` CHAR(36) DEFAULT NULL"},
	// `version` backs RM-24 optimistic locking: an If-Match-guarded UPDATE bumps it
	// version+1 only when the caller's expected version matches, so a lost-update is
	// detected as RowsAffected==0 → 409. Defaults to 0 (the opt-in baseline FE first
	// sends). Migration 000013 backfills the same column onto pre-existing obj_* tables.
	{Name: "version", Def: "`version` INT NOT NULL DEFAULT 0"},
	{Name: "created_at", Def: "`created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP"},
	{Name: "updated_at", Def: "`updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP"},
	{Name: "deleted_at", Def: "`deleted_at` TIMESTAMP NULL DEFAULT NULL"},
	{Name: "data", Def: "`data` JSON DEFAULT NULL"},
}

// mandatoryColumns renders MandatoryColumns into the CREATE TABLE body segment.
var mandatoryColumns = func() string {
	parts := make([]string, len(MandatoryColumns))
	for i, c := range MandatoryColumns {
		parts[i] = "  " + c.Def
	}
	return strings.Join(parts, ",\n")
}()

// reservedColumns mirrors the physical mandatory columns above (W5-C1). A dynamic field
// must NOT be named after one of these system columns: a STORED generated column or JSON
// `data` field colliding with them either trips ER_DUP_FIELDNAME (1060 → opaque 500) or,
// worse, silently shadows a system column (a field literally named "version" defeats RM-24
// optimistic locking — every write looks like a 409). The validation layer rejects such
// names with 422 FIELD_NAME_RESERVED before any DDL runs. Derived from MandatoryColumns so
// the lockstep is by construction, not by discipline.
var reservedColumns = func() map[string]struct{} {
	m := make(map[string]struct{}, len(MandatoryColumns))
	for _, c := range MandatoryColumns {
		m[c.Name] = struct{}{}
	}
	return m
}()

// IsReservedColumn reports whether name (compared case-insensitively, after the same
// lowercasing SanitizeIdentifier applies) collides with a mandatory system column on every
// obj_* table. The validation layer uses it to reject reserved field names with 422
// FIELD_NAME_RESERVED before issuing any DDL (W5-C1).
func IsReservedColumn(name string) bool {
	_, ok := reservedColumns[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// MandatoryIndex is one fixed obj_* index: Name is the INDEX_NAME as information_schema
// reports it ("PRIMARY" for the PK); Columns is the ordered column list the index must cover
// (the Doctor A5 shape check — an index present under the right name but the WRONG columns,
// e.g. a pre-O1∪O6 idx_created_at over (created_at) alone, is drift too, RM-47); Def is the
// definition fragment shared verbatim by CREATE TABLE and the A5 repair ALTER
// (BuildMandatoryIndexDDL).
type MandatoryIndex struct {
	Name    string
	Columns []string
	Def     string
}

// MandatoryIndexes is the single source of truth for the fixed obj_* indexes.
var MandatoryIndexes = []MandatoryIndex{
	{Name: "PRIMARY", Columns: []string{"id"}, Def: "PRIMARY KEY (`id`)"},
	{Name: "idx_team_set", Columns: []string{"team_set_id"}, Def: "INDEX `idx_team_set` (`team_set_id`)"},
	{Name: "idx_created_by", Columns: []string{"created_by"}, Def: "INDEX `idx_created_by` (`created_by`)"},
	// idx_owner_id backs the current-owner lookup (rowscope.go COALESCE + a future direct
	// ?ownerId= filter) exactly like idx_created_by backs the creator lookup.
	{Name: "idx_owner_id", Columns: []string{"owner_id"}, Def: "INDEX `idx_owner_id` (`owner_id`)"},
	// Composite (tenant_id, deleted_at, col, id) — O1∪O6. EVERY obj_* read filters
	// `tenant_id = ? AND deleted_at IS NULL`, so deleted_at sits second (an IS-NULL equality on the
	// index) right after the tenant equality; the remaining (col, id) then serves the keyset
	// ORDER BY col [DIR], id [DIR] via a forward/REVERSE scan with no filesort — O(limit) at depth
	// AND soft-deleted rows are skipped inside the index (not read-then-discarded). The leading
	// (tenant_id) / (tenant_id, deleted_at) prefixes also serve tenant-only filters, COUNT, and the
	// bulk soft-delete predicate, so the old standalone idx_tenant + idx_deleted_at are redundant
	// (dropped). O6.
	{Name: "idx_created_at", Columns: []string{"tenant_id", "deleted_at", "created_at", "id"},
		Def: "INDEX `idx_created_at` (`tenant_id`, `deleted_at`, `created_at`, `id`)"},
	{Name: "idx_updated_at", Columns: []string{"tenant_id", "deleted_at", "updated_at", "id"},
		Def: "INDEX `idx_updated_at` (`tenant_id`, `deleted_at`, `updated_at`, `id`)"},
}

// mandatoryIndexes renders MandatoryIndexes into the CREATE TABLE body segment.
var mandatoryIndexes = func() string {
	parts := make([]string, len(MandatoryIndexes))
	for i, ix := range MandatoryIndexes {
		parts[i] = "  " + ix.Def
	}
	return strings.Join(parts, ",\n")
}()

// BuildCreateTable returns the CREATE TABLE statement for an entity's obj_* table.
// Every identifier (table name, generated column names) is run through
// SanitizeIdentifier first; values are never interpolated here. Returns an error if
// the entity or any field name fails the allowlist.
func BuildCreateTable(entity string, fields []FieldDef) (string, error) {
	name, err := SanitizeIdentifier(entity)
	if err != nil {
		return "", fmt.Errorf("ddl.BuildCreateTable: entity: %w", err)
	}
	table := "obj_" + name

	var genCols, uniqueIdx, relIdx []string
	for _, f := range fields {
		fname, err := SanitizeIdentifier(f.Name)
		if err != nil {
			return "", fmt.Errorf("ddl.BuildCreateTable: field %q: %w", f.Name, err)
		}
		// Ordinary fields stay in the JSON `data` column — no schema change. Only unique
		// fields need a materialized, indexable generated column.
		if f.IsUnique {
			// COLLATE utf8mb4_bin keeps the unique index byte-distinct. The table default
			// (utf8mb4_unicode_ci, below) is case- AND accent-insensitive, so without an
			// explicit binary collation 'ABC1' == 'abc1' == 'àbc1' would collide under
			// the sys_unique_index pivot value (RM-28 Slice B §3.2) and reject legitimately distinct code/SKU values with a
			// spurious 409 (GD1 bug #12). BuildAddUniqueGencolNonUnique applies the same collation.
			//
			// W5-E1 conditional-NULL: the generated value is the field value ONLY while the row
			// is live (deleted_at IS NULL); a soft-deleted row projects NULL. MySQL unique
			// indexes do NOT collide on NULL (each NULL is distinct), and MySQL 8 has no partial
			// indexes, so this is the canonical way to make a unique constraint ignore
			// soft-deleted rows. Without it a soft-deleted record's value SQUATS the unique slot
			// forever — re-creating the same email/sku after a soft-delete trips a 409
			// RECORD_DUPLICATE against an invisible row (broken CRM delete→re-add flow). Two LIVE
			// rows with the same value still collide correctly (both project the same non-NULL
			// value); a restored row recomputes from `data` (the original value is preserved in
			// JSON, untouched). We deliberately do NOT use (tenant_id, field, deleted_at): two
			// live rows both have deleted_at=NULL and NULLs are distinct there, so live
			// uniqueness would be lost.
			//
			// RM-28 Slice B: the physical index is NON-unique (`idx_`, not `uq_`) — a physical
			// UNIQUE index on a SHARED obj_* table would couple every tenant that registers
			// this entity name (../specs/019-rm28-pivot-uniqueness/spec.md §1.2). Uniqueness for an
			// `is_unique` field is enforced SOLELY by the sys_unique_index pivot
			// (dynrepo/pivot.go); this column + index remain only for sort/filter/
			// backfill-readback. Mirrors BuildAddUniqueGencolNonUnique's shape exactly
			// (alter.go) so create-time and add-field-time unique columns never diverge.
			genCols = append(genCols, "  "+uniqueGenColDef(fname))
			relIdx = append(relIdx, fmt.Sprintf(
				"  INDEX `idx_%s_%s` (`tenant_id`, `%s`)", table, fname, fname))
		}
		// A 1-N `relation` field (FK-on-child) materializes a CHAR(36) `<f>_id` FK column
		// projecting the JSON value, plus a NON-unique tenant-scoped index (many children
		// may reference one parent). The JSON key is the bare field name; the column gets
		// the `_id` suffix (reports_to -> reports_to_id).
		//
		// USER-FIELD: a `user` field reuses this EXACT branch — its target (`users`, a fixed
		// system table, never a sys_entity) makes the FK-on-child shape correct: many records
		// may be assigned to the same user, exactly like many children pointing at one parent.
		// f.OneToOne is always false for a `user` field (cardinality/1-1 is not offered for it
		// in this slice), so it always takes the plain, non-unique-index branch below.
		if f.FieldType == "relation" || f.FieldType == "user" {
			col := fname + "_id"
			if f.OneToOne {
				// ENG-REL2 #5: 1-1 relation. CASE-guard (W5-E1 style) so a soft-deleted row
				// projects NULL instead of squatting the unique slot forever — otherwise
				// soft-deleting the child and re-creating a new one pointing at the same
				// parent would spuriously 409. UNIQUE(tenant_id, <f>_id) enforces at most
				// one LIVE child row per (tenant, parent) — the exclusivity a 1-1 relation
				// promises. Naming mirrors the unique-field convention: uq_<table>_<col>.
				genCols = append(genCols, "  "+oneToOneRelGenColDef(fname))
				uniqueIdx = append(uniqueIdx, fmt.Sprintf(
					"  UNIQUE KEY `uq_%s_%s` (`tenant_id`, `%s`)", table, col, col))
			} else {
				genCols = append(genCols, fmt.Sprintf(
					"  `%s` CHAR(36) GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.%s'))) STORED",
					col, fname))
				relIdx = append(relIdx, fmt.Sprintf(
					"  INDEX `idx_%s_%s` (`tenant_id`, `%s`)", table, col, col))
			}
		}
		// A `polymorphic_relation` field (ACTIVITY-CORE #3a) materializes TWO STORED generated
		// columns — `<f>_type` (target entity name) and `<f>_id` (target record id) — projecting
		// the JSON object stored under the bare field name, plus a NON-unique composite index
		// (tenant_id, <f>_type, <f>_id). The column defs are shared verbatim with
		// BuildAddPolymorphicColumns (alter.go) so create-time and add-field-time columns can
		// never drift. The composite index name `idx_<table>_<f>` is 9 + len(entity) + len(field)
		// ≤ 61 chars under SanitizeIdentifier's 26-char cap — shorter than the relation FK index
		// (12 + entity + field) that binds MySQL's 64-char identifier limit, so it always fits.
		if f.Polymorphic {
			genCols = append(genCols, "  "+polymorphicTypeColDef(fname))
			genCols = append(genCols, "  "+polymorphicIDColDef(fname))
			relIdx = append(relIdx, fmt.Sprintf(
				"  INDEX `idx_%s_%s` (`tenant_id`, `%s_type`, `%s_id`)", table, fname, fname, fname))
		}
		// An `indexed` field (D1) materializes a VIRTUAL generated column typed per the D1
		// design table + a NON-unique tenant-scoped index. Skip when IsUnique is also true:
		// the unique generated column already carries a physical index (adding a second
		// column with the same name would cause a MySQL duplicate-column error).
		if f.Indexed && !f.IsUnique {
			colDef, _, err := indexedColExpr(fname, f.FieldType)
			if err != nil {
				return "", fmt.Errorf("ddl.BuildCreateTable: field %q indexed: %w", f.Name, err)
			}
			genCols = append(genCols, "  "+colDef)
			relIdx = append(relIdx, fmt.Sprintf(
				"  INDEX `idx_%s_%s` (`tenant_id`, `%s`)", table, fname, fname))
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS `%s` (\n", table)
	b.WriteString(mandatoryColumns)
	for _, c := range genCols {
		b.WriteString(",\n")
		b.WriteString(c)
	}
	b.WriteString(",\n")
	b.WriteString(mandatoryIndexes)
	for _, idx := range uniqueIdx {
		b.WriteString(",\n")
		b.WriteString(idx)
	}
	for _, idx := range relIdx {
		b.WriteString(",\n")
		b.WriteString(idx)
	}
	b.WriteString("\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;")
	return b.String(), nil
}
