//go:build integration

// RM-28 Slice A2 — backfill (BackfillUniqueField) integration tests. Reuses pivotSetup/testDB
// from pivot_integration_test.go (Slice A). Run with a migrated MySQL 8 (migration 000050+
// present):
//
//	go test -tags=integration ./internal/dynrepo/ -run Backfill -race -count=1
package dynrepo

import (
	"context"
	"testing"
)

// TestBackfillUniqueField_Idempotent seeds obj_ rows WITHOUT pivot rows (insert normally, then
// delete the pivot rows behind the record's back — simulating a missed pivot write), runs the
// backfill, and asserts the pivot rows appear FROM THE GENCOL (exact bytes). A second run must
// insert nothing new and error nothing (the NOT-EXISTS-by-record_id predicate holds).
func TestBackfillUniqueField_Idempotent(t *testing.T) {
	const entity = "it_bf_idem"
	const tenant = "33333333-3333-3333-3333-3333333333b1"
	const user = "00000000-0000-0000-0000-0000000000u1"
	repo, h := pivotSetup(t, entity)
	ctx := context.Background()

	id1, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"name": "A", "email": "a@x.com"}, emailUnique)
	if err != nil {
		t.Fatalf("InsertRecord 1: %v", err)
	}
	id2, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"name": "B", "email": "b@x.com"}, emailUnique)
	if err != nil {
		t.Fatalf("InsertRecord 2: %v", err)
	}
	// Simulate a missed pivot write: delete BOTH pivot rows behind the mutator's back, leaving
	// the live obj_ rows (and their gencol values) untouched.
	if _, err := h.Exec("DELETE FROM `sys_unique_index` WHERE `tenant_id` = ? AND `entity_name` = ?", tenant, entity); err != nil {
		t.Fatalf("simulate missed pivot write: %v", err)
	}
	if got := pivotValues(t, h, tenant, entity); len(got) != 0 {
		t.Fatalf("pre-backfill sanity: pivot = %v; want [] (simulated missed write)", got)
	}

	res, err := repo.BackfillUniqueField(ctx, entity, tenant, "email")
	if err != nil {
		t.Fatalf("BackfillUniqueField: %v", err)
	}
	if res.Inserted != 2 || len(res.Conflicts) != 0 {
		t.Fatalf("BackfillUniqueField result = %+v; want Inserted=2, no conflicts", res)
	}
	if got := pivotValues(t, h, tenant, entity); !eqValues(got, []string{"a@x.com", "b@x.com"}) {
		t.Fatalf("after backfill: pivot = %v; want [a@x.com b@x.com]", got)
	}
	if c := pivotRowCountForRecord(t, h, tenant, entity, id1); c != 1 {
		t.Fatalf("record 1 pivot count = %d; want 1", c)
	}
	if c := pivotRowCountForRecord(t, h, tenant, entity, id2); c != 1 {
		t.Fatalf("record 2 pivot count = %d; want 1", c)
	}

	// Re-run: idempotent — NOT-EXISTS-by-record_id holds, zero new rows, zero error, zero dup.
	res2, err := repo.BackfillUniqueField(ctx, entity, tenant, "email")
	if err != nil {
		t.Fatalf("second BackfillUniqueField: %v", err)
	}
	if res2.Inserted != 0 || len(res2.Conflicts) != 0 {
		t.Fatalf("second BackfillUniqueField result = %+v; want Inserted=0, no conflicts (already backfilled)", res2)
	}
	if got := pivotValues(t, h, tenant, entity); !eqValues(got, []string{"a@x.com", "b@x.com"}) {
		t.Fatalf("after second backfill: pivot = %v; want unchanged [a@x.com b@x.com]", got)
	}
}

// TestBackfillUniqueField_WithDupes seeds a genuine drift: TWO live rows sharing the SAME email
// value with no pivot coverage (the scenario Doctor A1 exists for — is_unique=true but the
// index/gencol/pivot state let a dupe slip through). Backfill must: (1) report the conflict
// tuple, (2) still insert every OTHER (dupe-free) row in the SAME call, (3) complete cleanly on
// a re-run once the tenant dedupes (soft-deletes one of the pair).
func TestBackfillUniqueField_WithDupes(t *testing.T) {
	const entity = "it_bf_dupe"
	const tenant = "33333333-3333-3333-3333-3333333333b2"
	const user = "00000000-0000-0000-0000-0000000000u1"
	repo, h := pivotSetup(t, entity)
	ctx := context.Background()

	// A clean, dupe-free row that MUST still get backfilled alongside the conflict.
	cleanID, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"name": "C", "email": "clean@x.com"}, emailUnique)
	if err != nil {
		t.Fatalf("InsertRecord clean: %v", err)
	}

	// Simulate the DUPE drift the spec's §2.3 intro describes (Doctor A1's own class: DeleteField
	// no-DDL + restore, or an ALTER that raced a metadata-write failure, can leave is_unique=true
	// with the pivot rows ABSENT — a window where the tenant writes duplicates freely). Post RM-28
	// Slice B the obj_ index is non-unique from birth (BuildCreateTable), so no drift simulation
	// is even needed: these two raw inserts sharing a value succeed unassisted, reproducing the
	// exact live-duplicate condition BackfillUniqueField must handle.
	dupA := "11111111-aaaa-aaaa-aaaa-111111111111"
	dupB := "22222222-bbbb-bbbb-bbbb-222222222222"
	if _, err := h.Exec("INSERT INTO `obj_it_bf_dupe` (id, tenant_id, created_by, data) VALUES (?, ?, ?, JSON_OBJECT('name','D1','email','dup@x.com'))", dupA, tenant, user); err != nil {
		t.Fatalf("seed dupe A: %v", err)
	}
	if _, err := h.Exec("INSERT INTO `obj_it_bf_dupe` (id, tenant_id, created_by, data) VALUES (?, ?, ?, JSON_OBJECT('name','D2','email','dup@x.com'))", dupB, tenant, user); err != nil {
		t.Fatalf("seed dupe B: %v", err)
	}
	// No pivot rows exist yet for ANY of the three live rows (cleanID's pivot row from
	// InsertRecord above is deleted so the whole field starts from a clean slate for this test).
	if _, err := h.Exec("DELETE FROM `sys_unique_index` WHERE `tenant_id` = ? AND `entity_name` = ?", tenant, entity); err != nil {
		t.Fatalf("reset pivot: %v", err)
	}

	res, err := repo.BackfillUniqueField(ctx, entity, tenant, "email")
	if err != nil {
		t.Fatalf("BackfillUniqueField: %v", err)
	}
	if res.Inserted != 1 {
		t.Fatalf("BackfillUniqueField.Inserted = %d; want 1 (only the dupe-free 'clean@x.com' row)", res.Inserted)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0].Value != "dup@x.com" || res.Conflicts[0].Count != 2 {
		t.Fatalf("BackfillUniqueField.Conflicts = %+v; want [{dup@x.com 2}]", res.Conflicts)
	}
	if got := pivotValues(t, h, tenant, entity); !eqValues(got, []string{"clean@x.com"}) {
		t.Fatalf("after backfill: pivot = %v; want [clean@x.com] (dupe pair excluded, clean row inserted)", got)
	}
	if c := pivotRowCountForRecord(t, h, tenant, entity, cleanID); c != 1 {
		t.Fatalf("clean record pivot count = %d; want 1", c)
	}
	if c := pivotRowCountForRecord(t, h, tenant, entity, dupA); c != 0 {
		t.Fatalf("dupe A pivot count = %d; want 0 (excluded)", c)
	}
	if c := pivotRowCountForRecord(t, h, tenant, entity, dupB); c != 0 {
		t.Fatalf("dupe B pivot count = %d; want 0 (excluded)", c)
	}

	// Dedupe: soft-delete one of the pair (mirrors an admin cleanup) — re-run must complete.
	if _, err := h.Exec("UPDATE `obj_it_bf_dupe` SET deleted_at = NOW() WHERE id = ?", dupB); err != nil {
		t.Fatalf("soft-delete dupe B: %v", err)
	}
	res2, err := repo.BackfillUniqueField(ctx, entity, tenant, "email")
	if err != nil {
		t.Fatalf("re-run BackfillUniqueField: %v", err)
	}
	if res2.Inserted != 1 || len(res2.Conflicts) != 0 {
		t.Fatalf("re-run result = %+v; want Inserted=1 (dupA now unique-live), no conflicts", res2)
	}
	if got := pivotValues(t, h, tenant, entity); !eqValues(got, []string{"clean@x.com", "dup@x.com"}) {
		t.Fatalf("after dedupe+re-run: pivot = %v; want [clean@x.com dup@x.com]", got)
	}
	if c := pivotRowCountForRecord(t, h, tenant, entity, dupA); c != 1 {
		t.Fatalf("dupe A (now the sole live holder) pivot count = %d; want 1", c)
	}
}

// TestBackfillUniqueField_UnknownEntity asserts a bad identifier is rejected before any query
// (SanitizeIdentifier), never a raw driver error.
func TestBackfillUniqueField_UnknownEntity(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	if _, err := repo.BackfillUniqueField(context.Background(), "not a valid name!", "tenant-x", "email"); err == nil {
		t.Fatal("expected error for invalid entity identifier, got nil")
	}
}
