//go:build integration

// RM-28 Slice A2 (§6) — the D1 bidirectional pivot reconcile check, over real MySQL 8 via the
// o3a throwaway-DB harness (drWire). Proofs a unit fake cannot give:
//  1. missed-write: a live obj_ row whose pivot row was deleted behind the mutator's back is
//     detected and the fix (backfill) restores it from the gencol.
//  2. orphan/ghost: a bogus pivot row with no matching live obj_ record is detected and the fix
//     deletes it.
//  3. fix-all resolves BOTH in one pass and a re-scan is clean (idempotent QRR contract).
//  4. tenant isolation: a co-tenant sharing the SAME physical obj_ table (same entity name) with
//     its own CLEAN pivot state is never flagged, before or after the other tenant's fix runs.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestDoctorPivotReconcile -race -v
package services

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/dynrepo"
)

func TestDoctorPivotReconcile_BidirectionalFixAllRescanClean(t *testing.T) {
	svc, db := drWire(t)
	dyn := dynrepo.New(db)
	ctx := context.Background()
	const tenantA = "d1a1a1a1-0000-0000-0000-00000000000a"
	const tenantB = "d1b1b1b1-0000-0000-0000-00000000000b"
	emailUnique := []dynrepo.UniqueFieldSpec{{FieldName: "email", GenCol: "email"}}

	// --- Seed: shared entity d1_accounts, unique `email`, active for BOTH tenants. -------------
	entA := dsSeedEntity(t, db, tenantA, "d1_accounts")
	dsSeedField(t, db, tenantA, entA, "email", "text", true, "")
	entB := dsSeedEntity(t, db, tenantB, "d1_accounts")
	dsSeedField(t, db, tenantB, entB, "email", "text", true, "")
	if err := dyn.CreateTable(ctx, "d1_accounts", []ddl.FieldDef{
		{Name: "email", FieldType: "text", IsUnique: true},
	}); err != nil {
		t.Fatalf("create obj_d1_accounts: %v", err)
	}

	// Tenant A: a live record whose pivot write we'll delete (missed-write fixture).
	idMissed, err := dyn.InsertRecord(ctx, "d1_accounts", tenantA, "u-a", map[string]any{"email": "missed@x.com"}, emailUnique)
	if err != nil {
		t.Fatalf("insert A missed-write source row: %v", err)
	}
	dsExec(t, db, "DELETE FROM `sys_unique_index` WHERE `tenant_id` = ? AND `entity_name` = ? AND `field_name` = ? AND `record_id` = ?",
		tenantA, "d1_accounts", "email", idMissed)

	// Tenant A: a bogus orphan pivot row — record_id with NO live obj_ row at all. orphanPivotID
	// is the pivot row's OWN id (the Finding.Key the D1 orphan direction reports and repairs by);
	// orphanRecordID is the fake record_id it claims to project (never a real obj_ row).
	orphanPivotID := uuid.NewString()
	orphanRecordID := uuid.NewString()
	dsExec(t, db, "INSERT INTO `sys_unique_index` (id, tenant_id, entity_name, field_name, value, record_id) VALUES (?, ?, ?, ?, ?, ?)",
		orphanPivotID, tenantA, "d1_accounts", "email", "ghost@x.com", orphanRecordID)

	// Tenant B: an UNRELATED, perfectly healthy record on the SAME physical table — must never
	// be touched by tenant A's scan/fix (tenant isolation).
	idHealthyB, err := dyn.InsertRecord(ctx, "d1_accounts", tenantB, "u-b", map[string]any{"email": "healthy@x.com"}, emailUnique)
	if err != nil {
		t.Fatalf("insert B healthy row: %v", err)
	}

	// --- 1. Scan tenant A finds EXACTLY the two seeded findings. ------------------------------
	scanA := drScanCounts(t, svc, tenantA)
	d1A := scanA[DoctorCheckPivotReconcile]
	if d1A.Count != 2 {
		t.Fatalf("D1 tenant A scan = %+v; want 2 findings (1 missed-write + 1 orphan)", d1A)
	}
	var sawOrphan, sawFill bool
	for _, it := range d1A.Items {
		switch {
		case it.Key == pivotReconcileOrphanPrefix+orphanPivotID:
			sawOrphan = true
		case it.Key == pivotReconcileFillPrefix+"d1_accounts:email":
			sawFill = true
		default:
			t.Fatalf("D1 tenant A unexpected finding key %q (detail=%q)", it.Key, it.Detail)
		}
	}
	if !sawOrphan || !sawFill {
		t.Fatalf("D1 tenant A findings = %+v; want both orphan(%s) and fill(d1_accounts:email)", d1A.Items, orphanPivotID)
	}

	// --- 2. Tenant B's scan is completely clean (isolation, BEFORE A's fix). ------------------
	if got := drScanCounts(t, svc, tenantB)[DoctorCheckPivotReconcile]; got.Count != 0 {
		t.Fatalf("D1 tenant B scan (before A's fix) = %+v; want clean (isolation)", got)
	}

	// --- 3. Fix all findings for tenant A. -----------------------------------------------------
	results, err := svc.Fix(ctx, tenantA, []string{DoctorCheckPivotReconcile}, false)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if len(results) != 1 || results[0].Fixed != 2 || results[0].Failed != 0 {
		t.Fatalf("Fix result = %+v; want 1 check result, Fixed=2, Failed=0", results)
	}

	// --- 4. Re-scan tenant A -> clean (idempotent QRR contract). --------------------------------
	if got := drScanCounts(t, svc, tenantA)[DoctorCheckPivotReconcile]; got.Count != 0 {
		t.Fatalf("D1 tenant A re-scan = %+v; want clean", got)
	}
	// A second fix finds nothing to do.
	results2, err := svc.Fix(ctx, tenantA, []string{DoctorCheckPivotReconcile}, false)
	if err != nil {
		t.Fatalf("second Fix: %v", err)
	}
	if results2[0].Fixed != 0 || results2[0].Failed != 0 {
		t.Fatalf("second fix = %+v; want all-zero (already clean)", results2[0])
	}

	// --- 5. DB state really changed shape. -------------------------------------------------------
	// missed-write repaired: the live record's pivot row is back, value read from the gencol.
	var missedVal string
	if err := db.Get(&missedVal, "SELECT `value` FROM `sys_unique_index` WHERE `tenant_id` = ? AND `entity_name` = ? AND `field_name` = ? AND `record_id` = ?",
		tenantA, "d1_accounts", "email", idMissed); err != nil {
		t.Fatalf("read repaired pivot row: %v (missed-write fix did not insert it back)", err)
	}
	if missedVal != "missed@x.com" {
		t.Fatalf("repaired pivot value = %q; want missed@x.com (byte-identical to gencol)", missedVal)
	}
	// orphan repaired: the bogus row is gone.
	var n int
	if err := db.Get(&n, "SELECT COUNT(*) FROM `sys_unique_index` WHERE `tenant_id` = ? AND `record_id` = ?", tenantA, orphanRecordID); err != nil {
		t.Fatalf("count orphan row: %v", err)
	}
	if n != 0 {
		t.Fatalf("orphan pivot row survives fix: count = %d; want 0", n)
	}

	// --- 6. Tenant B is STILL untouched (isolation, AFTER A's fix). ----------------------------
	if got := drScanCounts(t, svc, tenantB)[DoctorCheckPivotReconcile]; got.Count != 0 {
		t.Fatalf("D1 tenant B scan (after A's fix) = %+v; want still clean (isolation)", got)
	}
	var bVal string
	if err := db.Get(&bVal, "SELECT `value` FROM `sys_unique_index` WHERE `tenant_id` = ? AND `entity_name` = ? AND `field_name` = ? AND `record_id` = ?",
		tenantB, "d1_accounts", "email", idHealthyB); err != nil {
		t.Fatalf("tenant B healthy pivot row disturbed by A's fix: %v", err)
	}
	if bVal != "healthy@x.com" {
		t.Fatalf("tenant B pivot value = %q; want unchanged healthy@x.com", bVal)
	}
}

// TestDoctorPivotReconcile_StaleValueOrphanThenFillSamePass is the TARGETED regression for why
// Detect emits orphan findings before fill findings: unlike the previous test (where the orphan
// and the missed-write sit on DIFFERENT records, so the assertions hold regardless of ordering),
// this seeds ONE record whose pivot row is STALE — same record_id, WRONG value — so that
// record_id is simultaneously (b) an orphan (its pivot row projects a value the record no longer
// holds) AND (a) missing a fill (no pivot row carries its CURRENT value). A single Fix call must
// resolve BOTH in the SAME pass: orphan-delete first (freeing record_id), THEN fill-insert
// (BackfillUniqueField's own NOT-EXISTS-by-record_id, spec §2.3, only succeeds because the stale
// row is already gone) — leaving EXACTLY ONE pivot row for the record, carrying the CURRENT
// value. Fill-before-orphan would insert nothing that pass (the stale row still satisfies
// record_id NOT-EXISTS) and need a second scan/fix cycle — which is exactly what this test would
// catch if the ordering (or CountMissingPivotRows' value-aware match) ever regressed.
func TestDoctorPivotReconcile_StaleValueOrphanThenFillSamePass(t *testing.T) {
	svc, db := drWire(t)
	dyn := dynrepo.New(db)
	ctx := context.Background()
	const tenant = "d1c1c1c1-0000-0000-0000-00000000000c"
	const entity = "d1_stale"
	emailUnique := []dynrepo.UniqueFieldSpec{{FieldName: "email", GenCol: "email"}}

	ent := dsSeedEntity(t, db, tenant, entity)
	dsSeedField(t, db, tenant, ent, "email", "text", true, "")
	if err := dyn.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "email", FieldType: "text", IsUnique: true},
	}); err != nil {
		t.Fatalf("create obj_%s: %v", entity, err)
	}

	// Insert normally — the mutator writes a CORRECT pivot row (value = "fresh@x.com").
	recID, err := dyn.InsertRecord(ctx, entity, tenant, "u1", map[string]any{"email": "fresh@x.com"}, emailUnique)
	if err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}
	var stalePivotID string
	if err := db.Get(&stalePivotID, "SELECT `id` FROM `sys_unique_index` WHERE `tenant_id` = ? AND `entity_name` = ? AND `field_name` = ? AND `record_id` = ?",
		tenant, entity, "email", recID); err != nil {
		t.Fatalf("read pivot row id: %v", err)
	}

	// Simulate the drift directly (bypassing UpdateRecord, whose reconcile would keep the pivot
	// row in sync): the record's live value moves to "current@x.com", but the pivot row is left
	// behind still holding "fresh@x.com" — same record_id, stale value.
	dsExec(t, db, "UPDATE `obj_"+entity+"` SET `data` = JSON_SET(`data`, '$.email', ?) WHERE `id` = ? AND `tenant_id` = ?",
		"current@x.com", recID, tenant)

	// --- Scan finds BOTH directions for the SAME record in one pass. --------------------------
	d1 := drScanCounts(t, svc, tenant)[DoctorCheckPivotReconcile]
	if d1.Count != 2 {
		t.Fatalf("D1 scan = %+v; want 2 findings (orphan on the stale row + fill for the current value)", d1)
	}
	var sawOrphan, sawFill bool
	for _, it := range d1.Items {
		switch it.Key {
		case pivotReconcileOrphanPrefix + stalePivotID:
			sawOrphan = true
		case pivotReconcileFillPrefix + entity + ":email":
			sawFill = true
		default:
			t.Fatalf("D1 unexpected finding key %q (detail=%q)", it.Key, it.Detail)
		}
	}
	if !sawOrphan || !sawFill {
		t.Fatalf("D1 findings = %+v; want both orphan(%s) and fill(%s:email) for the SAME record", d1.Items, stalePivotID, entity)
	}

	// --- ONE Fix call (single Detect internally) must resolve both in the same pass. -----------
	results, err := svc.Fix(ctx, tenant, []string{DoctorCheckPivotReconcile}, false)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if len(results) != 1 || results[0].Fixed != 2 || results[0].Failed != 0 {
		t.Fatalf("Fix result = %+v; want 1 check result, Fixed=2, Failed=0 (both resolved in ONE pass)", results)
	}

	// --- Exactly ONE pivot row survives for the record, carrying the CURRENT value. -------------
	var rows []string
	if err := db.Select(&rows, "SELECT `value` FROM `sys_unique_index` WHERE `tenant_id` = ? AND `entity_name` = ? AND `field_name` = ? AND `record_id` = ?",
		tenant, entity, "email", recID); err != nil {
		t.Fatalf("query pivot rows for record: %v", err)
	}
	if len(rows) != 1 || rows[0] != "current@x.com" {
		t.Fatalf("pivot rows for record = %v; want exactly [current@x.com] (stale deleted, fresh inserted, SAME pass)", rows)
	}

	// --- Re-scan is clean (nothing left for a second pass — proves it really was ONE pass). -----
	if got := drScanCounts(t, svc, tenant)[DoctorCheckPivotReconcile]; got.Count != 0 {
		t.Fatalf("re-scan = %+v; want clean (a second pass would mean ordering did NOT help)", got)
	}
}
