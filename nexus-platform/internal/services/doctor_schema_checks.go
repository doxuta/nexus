// doctor_schema_checks.go — DOCTOR-SCHEMA (../specs/008-nexus-doctor/spec.md §4 lát 2): the five
// information_schema checks. Declared-vs-actual diff, Nexus Quick-Repair style:
//
//	A1  active unique/relation/indexed field whose generated column is MISSING on obj_*   (auto)
//	A2a unique/1-1 generated column missing the `CASE WHEN deleted_at IS NULL` guard      (auto)
//	A5  obj_* missing mandatory columns/indexes (version — 000013; composite RM-47)       (auto)
//	A6  entity active but the obj_* table is GONE — every record op 500s                  (confirm)
//	B4  N-N relationship active but its rel_* join table is gone                          (confirm)
//
// Hard rules inherited from the framework (spec §3): Detect is READ-ONLY absolutely and
// batched (per-tenant sys_* list first, then IN-list information_schema probes); every Fix
// re-detects its single item, executes ONLY a ddl.Build*-shaped statement via dynrepo, writes
// an audit row, and the batch invalidates the MetaCache (DoctorService.Fix).
//
// SHARED-TABLE REASONING (the critical rule): obj_*/rel_* tables are shared across tenants.
//   - A1/A2a/A5 are additive/in-place-rebuild DDL — the repaired column/index/guard serves
//     every tenant sharing the table, and a co-tenant's identical finding re-detects clean
//     (skipped) instead of double-fixing: the finding key is a tenant-scoped sys_* row id but
//     the fix's precondition is the PHYSICAL state, re-read per item.
//   - A6/B4 recreates are CREATE TABLE IF NOT EXISTS (idempotent, race-safe) but they restore
//     an EMPTY table — old data/links do NOT come back, which is why both are needs-confirm
//     (AutoFixSafe=false → 422 DOCTOR_CONFIRM_REQUIRED without confirm=true).
//   - A backfill blocked by existing shared data (MySQL 1062) is reported as a FAILED item
//     with a stable public verdict (cardinality conflict for 1-1 relations) and never retried
//     blindly — data repair is out of the Doctor's v1 scope (spec §3). RM-28 Slice B retired
//     CROSS_TENANT_SHARED_UNIQUE for plain `is_unique` fields (A1's fix now materializes a
//     NON-unique idx_ index, which cannot 1062). A2a's GuardUniqueColumn MODIFY KEEPS the
//     mapping, retained as a documented guard for the narrow residual case of a field the Slice
//     B migration's coverage guard declined to demote (still physically `uq_` — see migration
//     000051) — in practice this path is vanishingly reachable (a still-live `uq_` index already
//     prevents two LIVE duplicate values from coexisting long enough to hit this MODIFY), but
//     keeping the mapping is the conservative call either way: removing a still-possibly-
//     reachable error would surface as an opaque 500 instead, while keeping an effectively-
//     unreachable one is harmless.
//
// SQL PREVIEW (spec §2.3 proposedSql): the WOULD-RUN statement is embedded in each finding's
// Detail after a "— SQL:" marker. Deliberate ponytail v1: the FE renders items[].detail
// monospace already, so no new wire field / FE change is needed; promote to a dedicated
// `proposedSql` field only if the FE ever needs to render it separately.
package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/doctor"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// Check IDs (spec §2.2 numbering). The FE CHECK_META lookup is prefix-tolerant on the part
// before the first underscore ("A1", "A2A", "A5", "A6", "B4").
const (
	DoctorCheckMissingGenColumn = "A1_MISSING_GEN_COLUMN"
	DoctorCheckUnguardedUnique  = "A2A_UNGUARDED_UNIQUE_COLUMN"
	DoctorCheckMandatoryDDL     = "A5_MISSING_MANDATORY_DDL"
	DoctorCheckMissingObjTable  = "A6_MISSING_OBJ_TABLE"
	DoctorCheckMissingJoinTable = "B4_MISSING_JOIN_TABLE"
	doctorGuardExpr             = "deleted_at" // 000015's predicate: guard present ⇔ expression references deleted_at
	doctorProposedSQLMark       = " — SQL: "
	doctorSchemaScanWindow      = 2000 // sys_* scan window, same rationale as doctorLayoutScanWindow
	doctorA5KeySep              = ":"  // "<entity>:column|index:<name>" — entity/column/index names never contain ':'
)

// doctorPublicError is a per-item fix error whose message is stable and client-safe (static
// text + identifiers we own — never driver internals). DoctorService.Fix's W-1 sanitizer
// detects it with errors.As on the RAW error chain (FixResult.ItemErrors) and passes the
// message through instead of replacing it with "xem server log". A typed sentinel, not a
// string marker: a coincidental "[public] " substring in wrapped driver text can never
// smuggle internals past the sanitizer (DOCTOR-SCHEMA audit LOW, closed in DOCTOR-OPS).
type doctorPublicError struct{ msg string }

func (e *doctorPublicError) Error() string { return e.msg }

// doctorPublicErrf builds a public per-item fix error (see doctorPublicError).
func doctorPublicErrf(format string, args ...any) error {
	return &doctorPublicError{msg: fmt.Sprintf(format, args...)}
}

// ---- shared detect plumbing -------------------------------------------------------------------

// doctorTableShape is the information_schema view of one physical table: column name →
// generation expression ("" for a plain column), index name → ordered column list.
type doctorTableShape struct {
	columns map[string]string
	indexes map[string][]string
}

// loadTableShapes batch-loads the shapes of the given tables (2–3 information_schema queries
// total, regardless of table count). A table absent from the result map does not exist —
// that is A6/B4's class, and the column-level checks skip it (disjoint classes, like B1/B2).
func (s *DoctorService) loadTableShapes(ctx context.Context, tables []string) (map[string]*doctorTableShape, error) {
	exists, err := s.store.ListExistingTables(ctx, tables)
	if err != nil {
		return nil, fmt.Errorf("loadTableShapes: %w", err)
	}
	existing := make([]string, 0, len(exists))
	for _, t := range tables {
		if exists[t] {
			existing = append(existing, t)
		}
	}
	shapes := make(map[string]*doctorTableShape, len(existing))
	for _, t := range existing {
		shapes[t] = &doctorTableShape{columns: map[string]string{}, indexes: map[string][]string{}}
	}
	cols, err := s.store.ListTableColumns(ctx, existing)
	if err != nil {
		return nil, fmt.Errorf("loadTableShapes: %w", err)
	}
	for _, c := range cols {
		if sh := shapes[c.TableName]; sh != nil {
			sh.columns[c.ColumnName] = c.GenerationExpression
		}
	}
	idx, err := s.store.ListTableIndexes(ctx, existing)
	if err != nil {
		return nil, fmt.Errorf("loadTableShapes: %w", err)
	}
	for _, ic := range idx { // rows arrive ordered by (table, index, seq_in_index)
		if sh := shapes[ic.TableName]; sh != nil {
			sh.indexes[ic.IndexName] = append(sh.indexes[ic.IndexName], ic.ColumnName)
		}
	}
	return shapes, nil
}

// physColSpec is the physical column a sys_fields row must own on the shared obj_* table,
// plus the ddl.Build* ALTER that materializes it (the A1 fix AND the SQL preview — one
// derivation for both, so the preview can never lie about what the fix would run).
type physColSpec struct {
	column string
	kind   string // "unique" | "relation" | "one_to_one" | "indexed"
	addSQL string
}

// resolvePhysColSpec derives the spec for a field row; ok=false when the row needs no
// physical column (malformed options, or an indexed flag on a type outside the D1 set —
// neither is A1's class; config validation owns those).
func resolvePhysColSpec(row doctor.PhysicalFieldRow) (physColSpec, bool) {
	opts, err := parseOptionsJSON(&row.Options)
	if err != nil {
		return physColSpec{}, false
	}
	switch {
	case row.IsUnique == 1:
		// RM-28 Slice B: the A1 fix (and this preview) now materialize a NON-unique idx_
		// index — see ddl.BuildAddUniqueGencolNonUnique's doc comment.
		sqlStr, err := ddl.BuildAddUniqueGencolNonUnique(row.EntityName, row.FieldName)
		if err != nil {
			return physColSpec{}, false
		}
		return physColSpec{column: row.FieldName, kind: "unique", addSQL: sqlStr}, true
	case row.FieldType == "relation":
		if relationCardinality(opts) == "1-1" {
			sqlStr, err := ddl.BuildAddOneToOneRelationColumn(row.EntityName, row.FieldName)
			if err != nil {
				return physColSpec{}, false
			}
			return physColSpec{column: row.FieldName + "_id", kind: "one_to_one", addSQL: sqlStr}, true
		}
		sqlStr, err := ddl.BuildAddRelationColumn(row.EntityName, row.FieldName)
		if err != nil {
			return physColSpec{}, false
		}
		return physColSpec{column: row.FieldName + "_id", kind: "relation", addSQL: sqlStr}, true
	case row.FieldType == "polymorphic_relation":
		// ACTIVITY-CORE #3a: the field materializes `<f>_type` + `<f>_id` (added/dropped together
		// in ONE ALTER). A1 keys the single-spec sentinel on `<f>_type`; the fix (addSQL) adds
		// BOTH columns + the composite index. Partial drift (one column present, one absent) is
		// only reachable by manual DBA surgery since the pair is atomic — accepted for slice 1.
		sqlStr, err := ddl.BuildAddPolymorphicColumns(row.EntityName, row.FieldName)
		if err != nil {
			return physColSpec{}, false
		}
		return physColSpec{column: row.FieldName + "_type", kind: "polymorphic", addSQL: sqlStr}, true
	case boolOpt(opts, "indexed"):
		sqlStr, err := ddl.BuildAddIndexedColumn(row.EntityName, row.FieldName, row.FieldType)
		if err != nil {
			return physColSpec{}, false
		}
		return physColSpec{column: row.FieldName, kind: "indexed", addSQL: sqlStr}, true
	}
	return physColSpec{}, false
}

// objTableOf returns the physical obj_* table name for an entity, or ok=false when the name
// fails the identifier allowlist (defensive — registration already enforces it).
func objTableOf(entityName string) (string, bool) {
	name, err := ddl.SanitizeIdentifier(entityName)
	if err != nil {
		return "", false
	}
	return "obj_" + name, true
}

// doctorFieldDefs maps live sys_fields rows onto ddl.FieldDef exactly the way CreateEntity
// does (entity_orchestration.go) — one derivation, so an A6-recreated table matches what a
// fresh create would build from today's metadata. Malformed options degrade to a plain JSON
// field (the record data still has a home; the physical extras surface later as A1 findings).
func doctorFieldDefs(fields []metadata.FieldDefinition) []ddl.FieldDef {
	defs := make([]ddl.FieldDef, 0, len(fields))
	for _, f := range fields {
		opts, err := parseOptionsJSON(f.Options)
		if err != nil {
			opts = nil
		}
		fd := ddl.FieldDef{Name: f.Name, FieldType: f.FieldType, IsUnique: f.IsUnique}
		if f.FieldType == "relation" {
			fd.RelatedEntity, _ = relatedEntityName(opts)
			fd.OneToOne = relationCardinality(opts) == "1-1"
		}
		// ACTIVITY-CORE #3a: a polymorphic_relation field materializes two gen-columns + a
		// composite index inline in CREATE TABLE — so an A6-recreated table matches a fresh create.
		if f.FieldType == "polymorphic_relation" {
			fd.Polymorphic = true
		}
		if !f.IsUnique {
			fd.Indexed = boolOpt(opts, "indexed")
		}
		defs = append(defs, fd)
	}
	return defs
}

// physFieldTables collects the deduped obj_* tables behind a batch of field rows.
func physFieldTables(rows []doctor.PhysicalFieldRow) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	for _, r := range rows {
		t, ok := objTableOf(r.EntityName)
		if !ok || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// ---- A1: missing generated column (bug3 residue / AddField-fail window) ------------------------

func (s *DoctorService) checkMissingGenColumns() doctor.Check {
	kindLabel := map[string]string{
		"unique": "unique", "relation": "relation 1-N", "one_to_one": "relation 1-1", "indexed": "indexed",
		"polymorphic": "polymorphic_relation",
	}
	return doctor.Check{
		ID:          DoctorCheckMissingGenColumn,
		Severity:    doctor.SeverityCritical, // unique unenforced + sort/filter on the column 500s
		AutoFixSafe: true,                    // additive ALTER, precedent: CreateEntity reconcile (bug3)
		Detect: func(ctx context.Context, tenantID string) ([]doctor.Finding, error) {
			rows, err := s.store.ListPhysicalFieldRows(ctx, tenantID, doctorSchemaScanWindow)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.A1 detect: %w", err)
			}
			shapes, err := s.loadTableShapes(ctx, physFieldTables(rows))
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.A1 detect: %w", err)
			}
			findings := make([]doctor.Finding, 0)
			for _, row := range rows {
				if len(findings) > doctor.MaxFindingsPerCheck {
					break
				}
				spec, ok := resolvePhysColSpec(row)
				if !ok {
					continue
				}
				table, ok := objTableOf(row.EntityName)
				if !ok {
					continue
				}
				shape := shapes[table]
				if shape == nil {
					continue // table gone — A6's class, disjoint on purpose
				}
				if _, ok := shape.columns[spec.column]; ok {
					continue
				}
				findings = append(findings, doctor.Finding{
					Key: row.FieldID,
					Detail: fmt.Sprintf("field %s.%s (%s) thiếu generated column %q trên %s%s%s",
						row.EntityName, row.FieldName, kindLabel[spec.kind], spec.column, table,
						doctorProposedSQLMark, spec.addSQL),
				})
			}
			return findings, nil
		},
		Fix: func(ctx context.Context, tenantID string, f doctor.Finding) error {
			row, err := s.store.GetPhysicalFieldRow(ctx, tenantID, f.Key)
			if err != nil {
				return fmt.Errorf("services.Doctor.A1 re-detect: %w", err)
			}
			if row == nil {
				return doctor.ErrAlreadyClean
			}
			spec, ok := resolvePhysColSpec(*row)
			if !ok {
				return doctor.ErrAlreadyClean
			}
			table, ok := objTableOf(row.EntityName)
			if !ok {
				return doctor.ErrAlreadyClean
			}
			shapes, err := s.loadTableShapes(ctx, []string{table})
			if err != nil {
				return fmt.Errorf("services.Doctor.A1 re-detect: %w", err)
			}
			shape := shapes[table]
			if shape == nil {
				return doctor.ErrAlreadyClean // table gone meanwhile — now A6's finding
			}
			if _, ok := shape.columns[spec.column]; ok {
				return doctor.ErrAlreadyClean
			}
			switch spec.kind {
			case "unique":
				// RM-28 Slice B: non-unique idx_ index — cannot 1062 (see below).
				err = s.schema.AddUniqueGencolNonUnique(ctx, row.EntityName, row.FieldName)
			case "one_to_one":
				err = s.schema.AddOneToOneRelationColumn(ctx, row.EntityName, row.FieldName)
			case "relation":
				err = s.schema.AddRelationColumn(ctx, row.EntityName, row.FieldName)
			case "polymorphic":
				err = s.schema.AddPolymorphicColumns(ctx, row.EntityName, row.FieldName)
			default: // indexed
				err = s.schema.AddIndexedColumn(ctx, row.EntityName, row.FieldName, row.FieldType)
			}
			if err != nil {
				// A concurrent fixer/AddField won the name race — the column/index is there now.
				if errors.Is(err, dynrepo.ErrDuplicateKeyName) {
					return doctor.ErrAlreadyClean
				}
				// 1062: RM-28 Slice B retired the "unique" kind's UNIQUE index (it is now
				// non-unique idx_, which can never 1062) — the ONLY kind that still reaches
				// this branch is "one_to_one" (its own SEPARATE physical UNIQUE index,
				// NOT pivot-backed, NOT retired — see AddOneToOneRelationColumn). Stable public
				// verdict, do NOT retry (spec §2.2 A1).
				if errors.Is(err, dynrepo.ErrDuplicateUnique) {
					return doctorPublicErrf("backfill 1-1 %s.%s bị chặn: dữ liệu bảng dùng chung vi phạm ràng buộc 1-1 (FIELD_RELATION_CARDINALITY_CONFLICT) — xử lý dữ liệu trước, không tự retry", row.EntityName, row.FieldName)
				}
				return fmt.Errorf("services.Doctor.A1 fix: %w", err)
			}
			s.recordFix(ctx, tenantID, DoctorCheckMissingGenColumn, f)
			return nil
		},
	}
}

// ---- A2a: unique/1-1 generated column missing the soft-delete guard (W5-E1 regression) ---------
//
// RM-28 Slice B note: for a plain `is_unique` field, the `CASE WHEN deleted_at IS NULL` guard
// this check repairs is NO LONGER load-bearing for UNIQUENESS — soft-delete slot release is now
// the sys_unique_index pivot's job (dynrepo/pivot.go deletePivotByRecord), independent of the
// physical column's value. The guard remains load-bearing for the column's OTHER purposes: the
// NULL projection sort/filter/backfill-readback (§3.2) depend on, and — for a field the Slice B
// migration's coverage guard left physically `uq_` (see migration 000051) — the guard STILL
// matters for uniqueness too, since that residual index is real. A future cleanup of this guard
// must NOT assume it is safe to drop just because the pivot exists.
func (s *DoctorService) checkUnguardedUniqueColumns() doctor.Check {
	// guardSpec resolves the A2a-relevant column + guard SQL for a field row, or ok=false
	// when the row is not A2a's class (only unique + 1-1 columns carry the guard).
	guardSpec := func(row doctor.PhysicalFieldRow) (column, guardSQL string, ok bool) {
		spec, ok := resolvePhysColSpec(row)
		if !ok {
			return "", "", false
		}
		switch spec.kind {
		case "unique":
			sqlStr, err := ddl.BuildGuardUniqueColumn(row.EntityName, row.FieldName)
			if err != nil {
				return "", "", false
			}
			return spec.column, sqlStr, true
		case "one_to_one":
			sqlStr, err := ddl.BuildGuardOneToOneRelationColumn(row.EntityName, row.FieldName)
			if err != nil {
				return "", "", false
			}
			return spec.column, sqlStr, true
		}
		return "", "", false
	}
	// unguarded re-checks one row against the live shape: the column exists, IS generated,
	// and its expression does not reference deleted_at (exactly migration 000015's predicate:
	// GENERATION_EXPRESSION <> '' AND NOT LIKE '%deleted_at%'). A plain (non-generated)
	// column is NOT flagged: MODIFY-to-generated is destructive territory, not this class.
	unguarded := func(shape *doctorTableShape, column string) bool {
		if shape == nil {
			return false
		}
		expr, ok := shape.columns[column]
		return ok && expr != "" && !strings.Contains(expr, doctorGuardExpr)
	}
	return doctor.Check{
		ID:          DoctorCheckUnguardedUnique,
		Severity:    doctor.SeverityWarn, // soft-deleted rows squat unique slots → spurious 409 on re-create
		AutoFixSafe: true,                // in-place MODIFY back to the canonical form, precedent: 000015 sweep
		Detect: func(ctx context.Context, tenantID string) ([]doctor.Finding, error) {
			rows, err := s.store.ListPhysicalFieldRows(ctx, tenantID, doctorSchemaScanWindow)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.A2a detect: %w", err)
			}
			shapes, err := s.loadTableShapes(ctx, physFieldTables(rows))
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.A2a detect: %w", err)
			}
			findings := make([]doctor.Finding, 0)
			for _, row := range rows {
				if len(findings) > doctor.MaxFindingsPerCheck {
					break
				}
				column, guardSQL, ok := guardSpec(row)
				if !ok {
					continue
				}
				table, ok := objTableOf(row.EntityName)
				if !ok || !unguarded(shapes[table], column) {
					continue
				}
				findings = append(findings, doctor.Finding{
					Key: row.FieldID,
					Detail: fmt.Sprintf("generated column %s.%s thiếu guard `CASE WHEN deleted_at IS NULL` — bản ghi xoá mềm đang chiếm chỗ unique%s%s",
						table, column, doctorProposedSQLMark, guardSQL),
				})
			}
			return findings, nil
		},
		Fix: func(ctx context.Context, tenantID string, f doctor.Finding) error {
			row, err := s.store.GetPhysicalFieldRow(ctx, tenantID, f.Key)
			if err != nil {
				return fmt.Errorf("services.Doctor.A2a re-detect: %w", err)
			}
			if row == nil {
				return doctor.ErrAlreadyClean
			}
			column, _, ok := guardSpec(*row)
			if !ok {
				return doctor.ErrAlreadyClean
			}
			table, ok := objTableOf(row.EntityName)
			if !ok {
				return doctor.ErrAlreadyClean
			}
			shapes, err := s.loadTableShapes(ctx, []string{table})
			if err != nil {
				return fmt.Errorf("services.Doctor.A2a re-detect: %w", err)
			}
			if !unguarded(shapes[table], column) {
				return doctor.ErrAlreadyClean
			}
			if row.IsUnique == 1 {
				err = s.schema.GuardUniqueColumn(ctx, row.EntityName, row.FieldName)
			} else {
				err = s.schema.GuardOneToOneRelationColumn(ctx, row.EntityName, row.FieldName)
			}
			if err != nil {
				if errors.Is(err, dynrepo.ErrDuplicateUnique) {
					// Two LIVE rows already share a value — the guarded index cannot rebuild
					// (000015's fail-fast). Stable public verdict, no blind retry. RM-28 Slice B:
					// for row.IsUnique==1 this branch requires the field's index to STILL be
					// physically `uq_` (the demote migration's coverage guard declined to touch
					// it — see migration 000051); a field already demoted to `idx_` can never
					// 1062 here. In practice even that residual case is vanishingly reachable — a
					// still-live `uq_` index already prevents two LIVE duplicate values from
					// coexisting, which is the very condition this branch checks for — so the
					// mapping is kept as a conservative, effectively-dead guard rather than
					// something expected to fire. For one_to_one it is the ordinary, still-live
					// 1-1 UNIQUE index (never retired), where this branch IS the normal path.
					return doctorPublicErrf("rebuild guard %s.%s bị chặn: bảng dùng chung có giá trị trùng giữa các bản ghi đang sống (CROSS_TENANT_SHARED_UNIQUE) — xử lý dữ liệu trước, không tự retry", row.EntityName, row.FieldName)
				}
				return fmt.Errorf("services.Doctor.A2a fix: %w", err)
			}
			s.recordFix(ctx, tenantID, DoctorCheckUnguardedUnique, f)
			return nil
		},
	}
}

// ---- A5: mandatory columns/indexes missing or mis-shaped (000013 residue + RM-47) --------------

// a5Key encodes an A5 finding as "<entity>:column|index:<name>"; entity/column/index names
// come from the identifier allowlist and never contain ':'.
func a5Key(entity, kind, name string) string {
	return entity + doctorA5KeySep + kind + doctorA5KeySep + name
}

// a5State classifies one mandatory item against a live table shape.
type a5State int

const (
	a5Clean   a5State = iota
	a5Missing         // column/index absent → additive ADD
	a5Reshape         // index present under the right name, wrong column list (RM-47) → DROP+ADD
)

// a5Classify evaluates one "<kind>:<name>" mandatory item against a shape.
func a5Classify(shape *doctorTableShape, kind, name string) a5State {
	if kind == "column" {
		if _, ok := shape.columns[name]; !ok {
			return a5Missing
		}
		return a5Clean
	}
	actual, ok := shape.indexes[name]
	if !ok {
		return a5Missing
	}
	for _, ix := range ddl.MandatoryIndexes {
		if ix.Name != name {
			continue
		}
		if len(actual) != len(ix.Columns) {
			return a5Reshape
		}
		for i := range actual {
			if actual[i] != ix.Columns[i] {
				return a5Reshape
			}
		}
	}
	return a5Clean
}

func (s *DoctorService) checkMandatoryDDL() doctor.Check {
	return doctor.Check{
		ID:          DoctorCheckMandatoryDDL,
		Severity:    doctor.SeverityWarn, // missing version breaks RM-24 writes; mis-shaped composite = full-scan timelines (RM-47)
		AutoFixSafe: true,                // additive ADD / in-place index rebuild, precedent: sweeps 000013 + O1∪O6
		Detect: func(ctx context.Context, tenantID string) ([]doctor.Finding, error) {
			ents, err := s.store.ListActiveEntities(ctx, tenantID, doctorSchemaScanWindow)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.A5 detect: %w", err)
			}
			tables := make([]string, 0, len(ents))
			tableOf := make(map[string]string, len(ents)) // entity name → table
			for _, e := range ents {
				if t, ok := objTableOf(e.Name); ok {
					tables = append(tables, t)
					tableOf[e.Name] = t
				}
			}
			shapes, err := s.loadTableShapes(ctx, tables)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.A5 detect: %w", err)
			}
			findings := make([]doctor.Finding, 0)
			for _, e := range ents {
				if len(findings) > doctor.MaxFindingsPerCheck {
					break
				}
				shape := shapes[tableOf[e.Name]]
				if shape == nil {
					continue // table gone — A6's class
				}
				for _, c := range ddl.MandatoryColumns {
					if a5Classify(shape, "column", c.Name) != a5Missing {
						continue
					}
					sqlStr, err := ddl.BuildAddMandatoryColumn(e.Name, c.Name)
					if err != nil {
						continue
					}
					findings = append(findings, doctor.Finding{
						Key: a5Key(e.Name, "column", c.Name),
						Detail: fmt.Sprintf("bảng %s thiếu cột hệ thống %q%s%s",
							tableOf[e.Name], c.Name, doctorProposedSQLMark, sqlStr),
					})
				}
				for _, ix := range ddl.MandatoryIndexes {
					state := a5Classify(shape, "index", ix.Name)
					if state == a5Clean {
						continue
					}
					if state == a5Reshape && ix.Name == "PRIMARY" {
						continue // never DROP PRIMARY KEY on a shared table — needs a human
					}
					sqlStr, err := ddl.BuildMandatoryIndexDDL(e.Name, ix.Name, state == a5Reshape)
					if err != nil {
						continue
					}
					verb := "thiếu index"
					if state == a5Reshape {
						verb = "index sai shape (RM-47) —"
					}
					findings = append(findings, doctor.Finding{
						Key: a5Key(e.Name, "index", ix.Name),
						Detail: fmt.Sprintf("bảng %s %s %q cần (%s)%s%s",
							tableOf[e.Name], verb, ix.Name, strings.Join(ix.Columns, ", "),
							doctorProposedSQLMark, sqlStr),
					})
				}
			}
			return findings, nil
		},
		Fix: func(ctx context.Context, tenantID string, f doctor.Finding) error {
			parts := strings.SplitN(f.Key, doctorA5KeySep, 3)
			if len(parts) != 3 || (parts[1] != "column" && parts[1] != "index") {
				return fmt.Errorf("services.Doctor.A5 fix: malformed finding key %q", f.Key)
			}
			entityName, kind, name := parts[0], parts[1], parts[2]
			// Tenant pin: the entity must still be ACTIVE for THIS tenant (a key can never
			// reach another tenant's table — GetEntityByName is tenant-scoped).
			if _, err := s.meta.GetEntityByName(ctx, tenantID, entityName); err != nil {
				if errors.Is(err, metadata.ErrEntityNotFound) {
					return doctor.ErrAlreadyClean
				}
				return fmt.Errorf("services.Doctor.A5 re-detect: %w", err)
			}
			table, ok := objTableOf(entityName)
			if !ok {
				return doctor.ErrAlreadyClean
			}
			shapes, err := s.loadTableShapes(ctx, []string{table})
			if err != nil {
				return fmt.Errorf("services.Doctor.A5 re-detect: %w", err)
			}
			shape := shapes[table]
			if shape == nil {
				return doctor.ErrAlreadyClean // table gone — A6's finding now
			}
			state := a5Classify(shape, kind, name)
			if state == a5Clean {
				return doctor.ErrAlreadyClean
			}
			if kind == "column" {
				err = s.schema.AddMandatoryColumn(ctx, entityName, name)
			} else {
				err = s.schema.EnsureMandatoryIndex(ctx, entityName, name, state == a5Reshape)
			}
			if err != nil {
				if errors.Is(err, dynrepo.ErrDuplicateKeyName) {
					return doctor.ErrAlreadyClean // concurrent fixer won the race
				}
				return fmt.Errorf("services.Doctor.A5 fix: %w", err)
			}
			s.recordFix(ctx, tenantID, DoctorCheckMandatoryDDL, f)
			return nil
		},
	}
}

// ---- A6: entity active but obj_* table gone (manual DROP) — needs confirm ----------------------

func (s *DoctorService) checkMissingObjTables() doctor.Check {
	return doctor.Check{
		ID:          DoctorCheckMissingObjTable,
		Severity:    doctor.SeverityCritical, // every record op on the entity 500s
		AutoFixSafe: false,                   // recreate restores an EMPTY table — old data does NOT come back
		Detect: func(ctx context.Context, tenantID string) ([]doctor.Finding, error) {
			ents, err := s.store.ListActiveEntities(ctx, tenantID, doctorSchemaScanWindow)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.A6 detect: %w", err)
			}
			tables := make([]string, 0, len(ents))
			tableOf := make(map[string]string, len(ents))
			for _, e := range ents {
				if t, ok := objTableOf(e.Name); ok {
					tables = append(tables, t)
					tableOf[e.Name] = t
				}
			}
			exists, err := s.store.ListExistingTables(ctx, tables)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.A6 detect: %w", err)
			}
			findings := make([]doctor.Finding, 0)
			for _, e := range ents {
				if len(findings) > doctor.MaxFindingsPerCheck {
					break
				}
				table, ok := tableOf[e.Name]
				if !ok || exists[table] {
					continue
				}
				detail := fmt.Sprintf("entity %q đang hoạt động nhưng bảng %s đã biến mất — mọi thao tác record trả 500. Fix tạo lại bảng RỖNG từ metadata; dữ liệu cũ KHÔNG quay lại", e.Name, table)
				// SQL preview from the SAME derivation the fix runs (doctorFieldDefs →
				// BuildCreateTable). Preview failure is not detect failure — the finding
				// stands, the fix will surface the real error per item.
				if fields, err := s.meta.ListFields(ctx, tenantID, e.ID); err == nil {
					if sqlStr, err := ddl.BuildCreateTable(e.Name, doctorFieldDefs(fields)); err == nil {
						detail += doctorProposedSQLMark + strings.ReplaceAll(sqlStr, "\n", " ")
					}
				}
				findings = append(findings, doctor.Finding{Key: e.Name, Detail: detail})
			}
			return findings, nil
		},
		Fix: func(ctx context.Context, tenantID string, f doctor.Finding) error {
			ent, err := s.meta.GetEntityByName(ctx, tenantID, f.Key)
			if err != nil {
				if errors.Is(err, metadata.ErrEntityNotFound) {
					return doctor.ErrAlreadyClean // entity deleted meanwhile — drift gone
				}
				return fmt.Errorf("services.Doctor.A6 re-detect: %w", err)
			}
			table, ok := objTableOf(ent.Name)
			if !ok {
				return doctor.ErrAlreadyClean
			}
			exists, err := s.store.ListExistingTables(ctx, []string{table})
			if err != nil {
				return fmt.Errorf("services.Doctor.A6 re-detect: %w", err)
			}
			if exists[table] {
				return doctor.ErrAlreadyClean // recreated concurrently (or by a co-tenant's doctor)
			}
			fields, err := s.meta.ListFields(ctx, tenantID, ent.ID)
			if err != nil {
				return fmt.Errorf("services.Doctor.A6 fix: list fields: %w", err)
			}
			// CREATE TABLE IF NOT EXISTS — idempotent and race-safe on the shared table. A
			// co-tenant sharing this entity name gets service back too; any physical column
			// THEIR fields need beyond this tenant's shows up as THEIR A1 findings (additive).
			if err := s.schema.CreateTable(ctx, ent.Name, doctorFieldDefs(fields)); err != nil {
				return fmt.Errorf("services.Doctor.A6 fix: %w", err)
			}
			s.recordFix(ctx, tenantID, DoctorCheckMissingObjTable, f)
			return nil
		},
	}
}

// ---- B4: N-N relationship active but rel_* join table gone — needs confirm ---------------------

func (s *DoctorService) checkMissingJoinTables() doctor.Check {
	// canonicalJoin re-derives the deterministic join-table name; mismatch with the stored
	// join_table_name means a hand-edited registry row the Doctor must NOT "fix" by creating
	// a table the engine would never address (Link/Unlink resolve via JoinParts).
	canonicalJoin := func(rel doctor.NNRelationshipRow) (string, bool) {
		name, _, _, err := ddl.JoinParts(rel.ParentEntityName, rel.ChildEntityName)
		return name, err == nil && name == rel.JoinTableName
	}
	return doctor.Check{
		ID:          DoctorCheckMissingJoinTable,
		Severity:    doctor.SeverityCritical, // Link/Unlink/ListLinked on the relationship 500s
		AutoFixSafe: false,                   // recreate restores an EMPTY join table — old links do NOT come back
		Detect: func(ctx context.Context, tenantID string) ([]doctor.Finding, error) {
			rels, err := s.store.ListActiveNNRelationships(ctx, tenantID, doctorSchemaScanWindow)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.B4 detect: %w", err)
			}
			tables := make([]string, 0, len(rels))
			for _, r := range rels {
				tables = append(tables, r.JoinTableName)
			}
			exists, err := s.store.ListExistingTables(ctx, tables)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.B4 detect: %w", err)
			}
			findings := make([]doctor.Finding, 0)
			for _, rel := range rels {
				if len(findings) > doctor.MaxFindingsPerCheck {
					break
				}
				if exists[rel.JoinTableName] {
					continue
				}
				detail := fmt.Sprintf("quan hệ N-N %q (%s ↔ %s) mất bảng join %q — Link/Unlink gãy. Fix tạo lại bảng RỖNG; liên kết cũ KHÔNG quay lại",
					rel.Name, rel.ParentEntityName, rel.ChildEntityName, rel.JoinTableName)
				if _, ok := canonicalJoin(rel); ok {
					if sqlStr, err := ddl.BuildCreateJoinTable(rel.ParentEntityName, rel.ChildEntityName); err == nil {
						detail += doctorProposedSQLMark + strings.ReplaceAll(sqlStr, "\n", " ")
					}
				} else {
					detail += " — join_table_name lệch tên chuẩn, fix sẽ từ chối (xử lý tay)"
				}
				findings = append(findings, doctor.Finding{Key: rel.ID, Detail: detail})
			}
			return findings, nil
		},
		Fix: func(ctx context.Context, tenantID string, f doctor.Finding) error {
			rel, err := s.store.GetActiveNNRelationship(ctx, tenantID, f.Key)
			if err != nil {
				return fmt.Errorf("services.Doctor.B4 re-detect: %w", err)
			}
			if rel == nil {
				return doctor.ErrAlreadyClean
			}
			exists, err := s.store.ListExistingTables(ctx, []string{rel.JoinTableName})
			if err != nil {
				return fmt.Errorf("services.Doctor.B4 re-detect: %w", err)
			}
			if exists[rel.JoinTableName] {
				return doctor.ErrAlreadyClean
			}
			if _, ok := canonicalJoin(*rel); !ok {
				return doctorPublicErrf("join_table_name %q không khớp tên chuẩn của cặp (%s, %s) — registry row bị sửa tay, cần xử lý thủ công", rel.JoinTableName, rel.ParentEntityName, rel.ChildEntityName)
			}
			// CREATE TABLE IF NOT EXISTS, deterministic name — idempotent + race-safe.
			if err := s.schema.CreateJoinTable(ctx, rel.ParentEntityName, rel.ChildEntityName); err != nil {
				return fmt.Errorf("services.Doctor.B4 fix: %w", err)
			}
			s.recordFix(ctx, tenantID, DoctorCheckMissingJoinTable, f)
			return nil
		},
	}
}
