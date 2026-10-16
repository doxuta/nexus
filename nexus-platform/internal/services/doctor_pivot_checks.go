// doctor_pivot_checks.go — RM-28 Slice A2 (../specs/019-rm28-pivot-uniqueness/spec.md §6): the bidirectional
// sys_unique_index reconcile check. The pivot is app-enforced (§1.4) — a code path that writes
// an obj_* row without also maintaining its pivot rows would silently break uniqueness, and
// this check is the ONE backstop invariant that catches it.
//
// TWO directions, both required (dropping either one leaves the other invisible in Slice A,
// per spec §6 — the demoted-index cutover in Slice B is what would turn a missed direction into
// live data corruption or false 409s):
//
//	(a) missed-write — a LIVE obj_ row whose gencol value has NO matching pivot row (the
//	    mutator's INSERT never ran, or was bypassed). Repair = re-run BackfillUniqueField for
//	    the (entity, field) — SAME routine AddField's unique-conversion path calls (A2 ponytail:
//	    write it once, share it).
//	(b) orphan/ghost — a pivot row whose (tenant, entity, field, record_id) has NO live obj_
//	    row currently projecting that value (soft-deleted, value drifted, or a DeleteField
//	    window). Repair = DELETE the stale pivot row.
//
// Detect emits orphan findings BEFORE missed-write findings on purpose: a stale pivot row
// squatting a record_id blocks BackfillUniqueField's NOT-EXISTS-by-record_id insert for that
// SAME record's current value, so a combined fix-all run must delete the orphan first.
//
// The demoted-index verification (idx_obj_<e>_<f> exists post-demote) is still OUT of this
// check's scope. It was descoped in A2 because nothing was demoted yet; Slice B (mig 000051)
// HAS since demoted, so that rationale is expired — the sub-check was simply never added.
// Recorded as open debt in spec §6 / "Nợ còn mở" (impact is perf-only: a hand-dropped idx_
// makes sort/filter full-scan; uniqueness itself stays with the pivot).
package services

import (
	"context"
	"fmt"
	"strings"

	"nexus/platform/internal/doctor"
)

// DoctorCheckPivotReconcile is the D1 check id (RM-28 A2).
const DoctorCheckPivotReconcile = "D1_PIVOT_RECONCILE"

// Finding key prefixes disambiguate the two repair kinds inside ONE check (mirrors A5's
// "<kind>:<name>" encoding — entity/field names never contain ':', from the identifier
// allowlist, so Cut/TrimPrefix never mis-splits).
const (
	pivotReconcileOrphanPrefix = "orphan:" // + sys_unique_index row id
	pivotReconcileFillPrefix   = "fill:"   // + entityName + ":" + fieldName
)

// checkPivotReconcile builds the D1 check. Only fields currently flagged is_unique=1 are in
// scope — the pivot NEVER tracks anything else (a 1-1 relation's cardinality uniqueness is a
// separate physical index, not pivot-backed; see uniqueFieldSpecs in record_orchestration.go).
func (s *DoctorService) checkPivotReconcile() doctor.Check {
	return doctor.Check{
		ID:          DoctorCheckPivotReconcile,
		Severity:    doctor.SeverityWarn, // app-enforced uniqueness has a gap until repaired; not yet a hard outage in Slice A (old obj_ index still enforces)
		AutoFixSafe: true,                // fill is purely additive (restores the invariant from data already live); orphan-delete only ever frees a slot — neither loses live data
		Detect: func(ctx context.Context, tenantID string) ([]doctor.Finding, error) {
			rows, err := s.store.ListPhysicalFieldRows(ctx, tenantID, doctorSchemaScanWindow)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.D1 detect: %w", err)
			}
			uniqueRows := make([]doctor.PhysicalFieldRow, 0, len(rows))
			for _, row := range rows {
				if row.IsUnique == 1 {
					uniqueRows = append(uniqueRows, row)
				}
			}
			tables := physFieldTables(uniqueRows)
			exists, err := s.store.ListExistingTables(ctx, tables)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.D1 detect: %w", err)
			}
			// D1 assumes the gencol PHYSICALLY EXISTS (its own scope is sys_unique_index vs.
			// obj_ CONTENTS, not the column itself) — a field whose gencol is missing is A1's
			// class (A1_MISSING_GEN_COLUMN), which can drift independently and, unfixed, would
			// make every query below fail "Unknown column". Load shapes and skip those rows;
			// A1's own scan surfaces them.
			shapes, err := s.loadTableShapes(ctx, tables)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.D1 detect: %w", err)
			}
			gencolReady := func(row doctor.PhysicalFieldRow) bool {
				table, ok := objTableOf(row.EntityName)
				if !ok || !exists[table] {
					return false // table gone — A6's class, disjoint on purpose
				}
				shape := shapes[table]
				if shape == nil {
					return false
				}
				_, ok = shape.columns[row.FieldName]
				return ok // false = gencol missing — A1's class, disjoint on purpose
			}
			findings := make([]doctor.Finding, 0)
			// (b) orphan FIRST — see file header for why the ordering matters to a combined fix-all.
			for _, row := range uniqueRows {
				if len(findings) > doctor.MaxFindingsPerCheck {
					break
				}
				if !gencolReady(row) {
					continue
				}
				orphanIDs, err := s.schema.ListOrphanPivotIDs(ctx, row.EntityName, tenantID, row.FieldName, doctor.MaxFindingsPerCheck+1)
				if err != nil {
					return nil, fmt.Errorf("services.Doctor.D1 detect: orphan %s.%s: %w", row.EntityName, row.FieldName, err)
				}
				for _, id := range orphanIDs {
					findings = append(findings, doctor.Finding{
						Key: pivotReconcileOrphanPrefix + id,
						Detail: fmt.Sprintf("pivot sys_unique_index dư trên %s.%s (record_id không còn sống, hoặc giá trị đã lệch) — sẽ xoá row pivot",
							row.EntityName, row.FieldName),
					})
					if len(findings) > doctor.MaxFindingsPerCheck {
						break
					}
				}
			}
			// (a) missed-write.
			for _, row := range uniqueRows {
				if len(findings) > doctor.MaxFindingsPerCheck {
					break
				}
				if !gencolReady(row) {
					continue
				}
				n, err := s.schema.CountMissingPivotRows(ctx, row.EntityName, tenantID, row.FieldName)
				if err != nil {
					return nil, fmt.Errorf("services.Doctor.D1 detect: missed-write %s.%s: %w", row.EntityName, row.FieldName, err)
				}
				if n == 0 {
					continue
				}
				findings = append(findings, doctor.Finding{
					Key: pivotReconcileFillPrefix + row.EntityName + ":" + row.FieldName,
					Detail: fmt.Sprintf("%d bản ghi sống của %s.%s thiếu row pivot sys_unique_index — sẽ backfill từ generated column",
						n, row.EntityName, row.FieldName),
				})
			}
			return findings, nil
		},
		Fix: func(ctx context.Context, tenantID string, f doctor.Finding) error {
			switch {
			case strings.HasPrefix(f.Key, pivotReconcileOrphanPrefix):
				return s.fixPivotOrphan(ctx, tenantID, strings.TrimPrefix(f.Key, pivotReconcileOrphanPrefix), f)
			case strings.HasPrefix(f.Key, pivotReconcileFillPrefix):
				rest := strings.TrimPrefix(f.Key, pivotReconcileFillPrefix)
				entityName, fieldName, ok := strings.Cut(rest, ":")
				if !ok {
					return fmt.Errorf("services.Doctor.D1 fix: malformed finding key %q", f.Key)
				}
				return s.fixPivotFill(ctx, tenantID, entityName, fieldName, f)
			default:
				return fmt.Errorf("services.Doctor.D1 fix: malformed finding key %q", f.Key)
			}
		},
	}
}

// gencolExists reports whether ONE table currently has a physical column named `column` — the
// per-item re-detect guard the D1 Fix helpers share (a unique field's gencol can be
// A1-drift-missing independently of anything D1 tracks; a raw "Unknown column" driver error
// must never leak through as an opaque 500).
func (s *DoctorService) gencolExists(ctx context.Context, table, column string) (bool, error) {
	shapes, err := s.loadTableShapes(ctx, []string{table})
	if err != nil {
		return false, err
	}
	shape := shapes[table]
	if shape == nil {
		return false, nil
	}
	_, ok := shape.columns[column]
	return ok, nil
}

// fixPivotOrphan re-detects ONE pivot row by id before deleting it: re-resolve the row (gone →
// already clean), confirm its obj_* table still exists (gone → A6's class now), then re-run the
// SAME live/value check Detect used — a concurrent restore or value-converge between scan and
// fix means the row is no longer orphaned. When the gencol itself is missing (A1's class,
// independent drift) no live row could possibly be projecting the pivot's value through a
// column that does not exist — so the item is still orphan, and PivotRecordLive is skipped
// rather than erroring on "Unknown column".
func (s *DoctorService) fixPivotOrphan(ctx context.Context, tenantID, pivotID string, f doctor.Finding) error {
	row, err := s.schema.GetPivotRow(ctx, tenantID, pivotID)
	if err != nil {
		return fmt.Errorf("services.Doctor.D1 re-detect: %w", err)
	}
	if row == nil {
		return doctor.ErrAlreadyClean
	}
	table, ok := objTableOf(row.EntityName)
	if !ok {
		return doctor.ErrAlreadyClean
	}
	exists, err := s.store.ListExistingTables(ctx, []string{table})
	if err != nil {
		return fmt.Errorf("services.Doctor.D1 re-detect: %w", err)
	}
	if !exists[table] {
		return doctor.ErrAlreadyClean // table gone — A6's class now
	}
	hasCol, err := s.gencolExists(ctx, table, row.FieldName)
	if err != nil {
		return fmt.Errorf("services.Doctor.D1 re-detect: %w", err)
	}
	live := false
	if hasCol {
		live, err = s.schema.PivotRecordLive(ctx, tenantID, row.EntityName, row.FieldName, row.RecordID, row.Value)
		if err != nil {
			return fmt.Errorf("services.Doctor.D1 re-detect: %w", err)
		}
	}
	if live {
		return doctor.ErrAlreadyClean // the record now legitimately projects this value
	}
	if err := s.schema.DeletePivotRow(ctx, tenantID, pivotID); err != nil {
		return fmt.Errorf("services.Doctor.D1 fix: %w", err)
	}
	s.recordFix(ctx, tenantID, DoctorCheckPivotReconcile, f)
	return nil
}

// fixPivotFill re-runs BackfillUniqueField for (entity, field) — the SAME routine AddField's
// unique-conversion path calls, idempotent by construction so re-running it IS the re-detect.
// A residual intra-tenant conflict (the tenant's own live data still has a duplicate) is a
// stable, non-retryable public verdict (RECORD_DUPLICATE, mirroring AddField's own mapping)
// rather than an opaque failure. A missing gencol (A1's independent drift class) is likewise a
// stable public verdict pointing at the check that owns the actual repair.
func (s *DoctorService) fixPivotFill(ctx context.Context, tenantID, entityName, fieldName string, f doctor.Finding) error {
	table, ok := objTableOf(entityName)
	if !ok {
		return doctor.ErrAlreadyClean
	}
	hasCol, err := s.gencolExists(ctx, table, fieldName)
	if err != nil {
		return fmt.Errorf("services.Doctor.D1 re-detect: %w", err)
	}
	if !hasCol {
		return doctorPublicErrf("backfill pivot %s.%s bị chặn: generated column chưa tồn tại — chạy fix %s trước",
			entityName, fieldName, DoctorCheckMissingGenColumn)
	}
	res, err := s.schema.BackfillUniqueField(ctx, entityName, tenantID, fieldName)
	if err != nil {
		return fmt.Errorf("services.Doctor.D1 fix: %w", err)
	}
	if len(res.Conflicts) > 0 {
		return doctorPublicErrf("backfill pivot %s.%s bị chặn: dữ liệu tenant có giá trị trùng %q (%d bản ghi) — xử lý dữ liệu trước, không tự retry",
			entityName, fieldName, res.Conflicts[0].Value, res.Conflicts[0].Count)
	}
	if res.Inserted == 0 {
		return doctor.ErrAlreadyClean // fixed concurrently, or the finding was already stale
	}
	s.recordFix(ctx, tenantID, DoctorCheckPivotReconcile, f)
	return nil
}
