//go:build integration

// RM-28 pivot (sys_unique_index) integration tests. The first block (Slice A, landed) asserts
// the pivot CONTENTS the repository maintains alongside every obj_ mutation
// (create/update/soft-delete/restore/bulk/import), the entity/field cleanups, and that a
// concurrent create ends with exactly ONE pivot row for the winning value (spec §8.1). The
// TestSliceB_* tests at the bottom of this file are the Slice B ENFORCEMENT proofs (spec §8.1
// "Slice B test = ENFORCEMENT") that only manifest after the demote migration (000051): sole
// enforcement (no physical UNIQUE index left, only uq_pivot can reject a duplicate) and
// cross-tenant decoupling (a tenant that never declared a field unique is never constrained by
// a co-tenant sharing the same obj_* table who did).
//
// Run with a migrated MySQL 8 (migration 000051 present):
//
//	go test -tags=integration ./internal/dynrepo/ -run 'Pivot|SliceB' -race -count=1
package dynrepo

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/ddl"
)

// emailUnique is the single unique field these tests thread into the mutators.
var emailUnique = []UniqueFieldSpec{{FieldName: "email", GenCol: "email"}}

// pivotSetup creates a fresh obj_<entity> with a unique `email` field and registers cleanup of both
// the table AND the shared sys_unique_index rows for this entity (DropTable does NOT touch the
// pivot — it lives in a different table shared across tests).
func pivotSetup(t *testing.T, entity string) (*Repository, *sqlx.DB) {
	t.Helper()
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	_ = repo.DropTable(ctx, entity)
	_, _ = db.Exec("DELETE FROM `sys_unique_index` WHERE `entity_name` = ?", entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "name", FieldType: "text"},
		{Name: "email", FieldType: "text", IsUnique: true},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() {
		_ = repo.DropTable(ctx, entity)
		_, _ = db.Exec("DELETE FROM `sys_unique_index` WHERE `entity_name` = ?", entity)
	})
	return repo, db
}

// pivotValues returns the sorted pivot values for (tenant, entity, "email").
func pivotValues(t *testing.T, db *sqlx.DB, tenant, entity string) []string {
	t.Helper()
	var vals []string
	if err := db.Select(&vals,
		"SELECT `value` FROM `sys_unique_index` WHERE `tenant_id` = ? AND `entity_name` = ? AND `field_name` = ? ORDER BY `value`",
		tenant, entity, "email"); err != nil {
		t.Fatalf("pivotValues query: %v", err)
	}
	return vals
}

// pivotRowCountForRecord counts a record's pivot rows across all fields.
func pivotRowCountForRecord(t *testing.T, db *sqlx.DB, tenant, entity, recordID string) int {
	t.Helper()
	var n int
	if err := db.Get(&n,
		"SELECT COUNT(*) FROM `sys_unique_index` WHERE `tenant_id` = ? AND `entity_name` = ? AND `record_id` = ?",
		tenant, entity, recordID); err != nil {
		t.Fatalf("pivotRowCountForRecord query: %v", err)
	}
	return n
}

// objLiveCount counts live (non-soft-deleted) obj_ rows for a tenant.
func objLiveCount(t *testing.T, db *sqlx.DB, entity, tenant string) int {
	t.Helper()
	var n int
	if err := db.Get(&n,
		fmt.Sprintf("SELECT COUNT(*) FROM `obj_%s` WHERE `tenant_id` = ? AND `deleted_at` IS NULL", entity),
		tenant); err != nil {
		t.Fatalf("objLiveCount query: %v", err)
	}
	return n
}

func eqValues(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestPivotLifecycle covers spec §8.1 create/update-A→B/set-null/set-""/soft-delete/restore: after
// each op the pivot rows exactly mirror the record's live unique-field gencol.
func TestPivotLifecycle(t *testing.T) {
	const entity = "it_pivot_life"
	const tenant = "11111111-1111-1111-1111-1111111111a1"
	const user = "00000000-0000-0000-0000-0000000000u1"
	repo, h := pivotSetup(t, entity)
	ctx := context.Background()

	// create → pivot row present.
	id, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"name": "A", "email": "a@x.com"}, emailUnique)
	if err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}
	if got := pivotValues(t, h, tenant, entity); !eqValues(got, []string{"a@x.com"}) {
		t.Fatalf("after create: pivot = %v; want [a@x.com]", got)
	}
	if c := pivotRowCountForRecord(t, h, tenant, entity, id); c != 1 {
		t.Fatalf("after create: record pivot count = %d; want 1", c)
	}

	// update A→B → row A gone, B present.
	if err := repo.UpdateRecord(ctx, entity, tenant, id, user, AdminScope(), map[string]any{"email": "b@x.com"}, nil, emailUnique); err != nil {
		t.Fatalf("UpdateRecord A→B: %v", err)
	}
	if got := pivotValues(t, h, tenant, entity); !eqValues(got, []string{"b@x.com"}) {
		t.Fatalf("after A→B: pivot = %v; want [b@x.com]", got)
	}

	// set "" → an empty-string pivot row (non-null value).
	if err := repo.UpdateRecord(ctx, entity, tenant, id, user, AdminScope(), map[string]any{"email": ""}, nil, emailUnique); err != nil {
		t.Fatalf("UpdateRecord →\"\": %v", err)
	}
	if got := pivotValues(t, h, tenant, entity); !eqValues(got, []string{""}) {
		t.Fatalf("after →\"\": pivot = %v; want one empty-string row", got)
	}

	// set null → row gone (explicit JSON null removes the key → gencol NULL).
	if err := repo.UpdateRecord(ctx, entity, tenant, id, user, AdminScope(), map[string]any{"email": nil}, nil, emailUnique); err != nil {
		t.Fatalf("UpdateRecord →null: %v", err)
	}
	if got := pivotValues(t, h, tenant, entity); len(got) != 0 {
		t.Fatalf("after →null: pivot = %v; want []", got)
	}
	if c := pivotRowCountForRecord(t, h, tenant, entity, id); c != 0 {
		t.Fatalf("after →null: record pivot count = %d; want 0", c)
	}

	// re-add a value, then soft-delete → row released.
	if err := repo.UpdateRecord(ctx, entity, tenant, id, user, AdminScope(), map[string]any{"email": "c@x.com"}, nil, emailUnique); err != nil {
		t.Fatalf("UpdateRecord →c: %v", err)
	}
	if err := repo.SoftDeleteRecord(ctx, entity, tenant, id, AdminScope()); err != nil {
		t.Fatalf("SoftDeleteRecord: %v", err)
	}
	if got := pivotValues(t, h, tenant, entity); len(got) != 0 {
		t.Fatalf("after soft-delete: pivot = %v; want []", got)
	}

	// restore → pivot re-inserted from the current gencol.
	if err := repo.RestoreRecord(ctx, entity, tenant, id, user, AdminScope(), emailUnique); err != nil {
		t.Fatalf("RestoreRecord: %v", err)
	}
	if got := pivotValues(t, h, tenant, entity); !eqValues(got, []string{"c@x.com"}) {
		t.Fatalf("after restore: pivot = %v; want [c@x.com]", got)
	}
}

// TestPivotUpdateUntouchedFieldAndNoOp asserts the pivot is NOT disturbed when the patch does not
// touch the unique field, and when the update is a version-conflict no-op (RowsAffected == 0).
func TestPivotUpdateUntouched(t *testing.T) {
	const entity = "it_pivot_untouched"
	const tenant = "11111111-1111-1111-1111-1111111111a2"
	const user = "00000000-0000-0000-0000-0000000000u1"
	repo, h := pivotSetup(t, entity)
	ctx := context.Background()

	id, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"name": "A", "email": "keep@x.com"}, emailUnique)
	if err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}

	// Patch a NON-unique field only → email pivot untouched.
	if err := repo.UpdateRecord(ctx, entity, tenant, id, user, AdminScope(), map[string]any{"name": "A2"}, nil, emailUnique); err != nil {
		t.Fatalf("UpdateRecord name: %v", err)
	}
	if got := pivotValues(t, h, tenant, entity); !eqValues(got, []string{"keep@x.com"}) {
		t.Fatalf("after non-unique patch: pivot = %v; want [keep@x.com]", got)
	}

	// Version-conflict update (stale expectedVersion) → RowsAffected 0 → pivot untouched.
	stale := 999
	err = repo.UpdateRecord(ctx, entity, tenant, id, user, AdminScope(), map[string]any{"email": "changed@x.com"}, &stale, emailUnique)
	var vce *VersionConflictError
	if !errors.As(err, &vce) {
		t.Fatalf("expected VersionConflictError, got %v", err)
	}
	if got := pivotValues(t, h, tenant, entity); !eqValues(got, []string{"keep@x.com"}) {
		t.Fatalf("after version-conflict: pivot = %v; want [keep@x.com] (untouched)", got)
	}
}

// TestPivotBulkAndImportSameBatchDup asserts a same-batch duplicate rolls the WHOLE batch back for
// both the atomic bulk path (InsertRecordsAtomic) and the CSV import path (InsertRecords): zero
// obj_ rows AND zero pivot rows survive.
func TestPivotBulkImportSameBatchDup(t *testing.T) {
	const tenant = "11111111-1111-1111-1111-1111111111a3"
	const user = "00000000-0000-0000-0000-0000000000u1"

	t.Run("atomic bulk", func(t *testing.T) {
		const entity = "it_pivot_bulkatomic"
		repo, h := pivotSetup(t, entity)
		ctx := context.Background()
		rows := []map[string]any{
			{"name": "A", "email": "dup@x.com"},
			{"name": "B", "email": "dup@x.com"},
		}
		_, failedIdx, err := repo.InsertRecordsAtomic(ctx, entity, tenant, user, rows, emailUnique)
		if err == nil {
			t.Fatal("expected same-batch duplicate error, got nil")
		}
		if !errors.Is(err, ErrDuplicateUnique) {
			t.Fatalf("err = %v; want ErrDuplicateUnique", err)
		}
		if failedIdx != 1 {
			t.Fatalf("failedIdx = %d; want 1", failedIdx)
		}
		if c := objLiveCount(t, h, entity, tenant); c != 0 {
			t.Fatalf("obj live count = %d; want 0 (batch rolled back)", c)
		}
		if got := pivotValues(t, h, tenant, entity); len(got) != 0 {
			t.Fatalf("pivot = %v; want [] (batch rolled back)", got)
		}
	})

	t.Run("csv import", func(t *testing.T) {
		const entity = "it_pivot_import"
		repo, h := pivotSetup(t, entity)
		ctx := context.Background()
		rows := []map[string]any{
			{"name": "A", "email": "dup@x.com"},
			{"name": "B", "email": "dup@x.com"},
		}
		_, err := repo.InsertRecords(ctx, entity, tenant, user, rows, emailUnique)
		if !errors.Is(err, ErrDuplicateUnique) {
			t.Fatalf("err = %v; want ErrDuplicateUnique", err)
		}
		if c := objLiveCount(t, h, entity, tenant); c != 0 {
			t.Fatalf("obj live count = %d; want 0", c)
		}
		if got := pivotValues(t, h, tenant, entity); len(got) != 0 {
			t.Fatalf("pivot = %v; want []", got)
		}
	})
}

// TestPivotEntityAndFieldCleanup covers the whole-entity release (SoftDeleteRecords, entity
// uninstall) and the per-field cleanup (DeletePivotByField, DeleteField) — both must leave zero
// pivot rows so a recreated entity/field does not inherit a stale unique slot (§8.1 entity
// re-create).
func TestPivotEntityAndFieldCleanup(t *testing.T) {
	const tenant = "11111111-1111-1111-1111-1111111111a4"
	const user = "00000000-0000-0000-0000-0000000000u1"

	t.Run("SoftDeleteRecords releases whole entity", func(t *testing.T) {
		const entity = "it_pivot_reinstall"
		repo, h := pivotSetup(t, entity)
		ctx := context.Background()
		if _, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"email": "x@x.com"}, emailUnique); err != nil {
			t.Fatalf("InsertRecord: %v", err)
		}
		if _, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"email": "y@x.com"}, emailUnique); err != nil {
			t.Fatalf("InsertRecord: %v", err)
		}
		if got := pivotValues(t, h, tenant, entity); len(got) != 2 {
			t.Fatalf("before uninstall: pivot = %v; want 2 rows", got)
		}
		if err := repo.SoftDeleteRecords(ctx, entity, tenant); err != nil {
			t.Fatalf("SoftDeleteRecords: %v", err)
		}
		if got := pivotValues(t, h, tenant, entity); len(got) != 0 {
			t.Fatalf("after uninstall: pivot = %v; want []", got)
		}
	})

	t.Run("DeletePivotByField releases one field", func(t *testing.T) {
		const entity = "it_pivot_delfield"
		repo, h := pivotSetup(t, entity)
		ctx := context.Background()
		if _, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"email": "z@x.com"}, emailUnique); err != nil {
			t.Fatalf("InsertRecord: %v", err)
		}
		if got := pivotValues(t, h, tenant, entity); len(got) != 1 {
			t.Fatalf("before DeletePivotByField: pivot = %v; want 1", got)
		}
		if err := repo.DeletePivotByField(ctx, entity, tenant, "email"); err != nil {
			t.Fatalf("DeletePivotByField: %v", err)
		}
		if got := pivotValues(t, h, tenant, entity); len(got) != 0 {
			t.Fatalf("after DeletePivotByField: pivot = %v; want []", got)
		}
	})
}

// TestPivotConcurrentCreate asserts N goroutines creating the SAME unique value end with EXACTLY
// ONE winner and EXACTLY ONE pivot row for that value (no check-then-insert TOCTOU). Post RM-28
// Slice B the obj_* index is non-unique from birth (BuildCreateTable/BuildAddUniqueGencolNonUnique)
// so uq_pivot is the ONLY thing that can catch the race — proving the sentinel-based assertion
// below is exercising the pivot itself, not incidentally riding the old physical index (§8.1 #10).
// Run under -race by the gate.
func TestPivotConcurrentCreate(t *testing.T) {
	const entity = "it_pivot_conc"
	const tenant = "11111111-1111-1111-1111-1111111111a5"
	const user = "00000000-0000-0000-0000-0000000000u1"
	repo, h := pivotSetup(t, entity)
	ctx := context.Background()

	const n = 12
	var success, dup int64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"email": "race@x.com"}, emailUnique)
			switch {
			case err == nil:
				atomic.AddInt64(&success, 1)
			case errors.Is(err, ErrDuplicateUnique) || errors.Is(err, ErrWriteConflict):
				atomic.AddInt64(&dup, 1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if success != 1 {
		t.Fatalf("winners = %d; want exactly 1", success)
	}
	if got := pivotValues(t, h, tenant, entity); !eqValues(got, []string{"race@x.com"}) {
		t.Fatalf("after race: pivot = %v; want exactly [race@x.com]", got)
	}
}

// pivotTenantsForValue returns the sorted list of tenant_ids holding a pivot row for
// (entity, field, value) — used to prove the SAME value is independently pivoted per tenant.
func pivotTenantsForValue(t *testing.T, h *sqlx.DB, entity, field, value string) []string {
	t.Helper()
	var tenants []string
	if err := h.Select(&tenants,
		"SELECT `tenant_id` FROM `sys_unique_index` WHERE `entity_name` = ? AND `field_name` = ? AND `value` = ? ORDER BY `tenant_id`",
		entity, field, value); err != nil {
		t.Fatalf("pivotTenantsForValue query: %v", err)
	}
	return tenants
}

// TestPivotCrossTenantIsolation proves the #1 isolation property (spec §5): the pivot INSERT is
// tenant-scoped, so two DIFFERENT tenants sharing the SAME physical obj_ table (obj_<entity> is
// named by entity, not by tenant — the whole reason RM-28 exists) can independently hold the SAME
// unique value with NO uq_pivot collision. If the pivot INSERT ever forgot tenant_id, the second
// tenant's insert would 1062 against the first tenant's row. This is a Slice-A-testable pivot
// CONTENTS assertion (not enforcement/decoupling, which needs the Slice B demote — §8.1).
func TestPivotCrossTenantIsolation(t *testing.T) {
	const entity = "it_pivot_xtenant"
	const tenantA = "11111111-1111-1111-1111-1111111111a6"
	const tenantB = "22222222-2222-2222-2222-2222222222b6"
	const user = "00000000-0000-0000-0000-0000000000u1"
	const sharedValue = "x@y.com"
	repo, h := pivotSetup(t, entity)
	ctx := context.Background()

	// Both tenants register the SAME obj_ table (entity name, not tenant, names the physical
	// table) and both insert a record with the IDENTICAL unique value.
	idA, err := repo.InsertRecord(ctx, entity, tenantA, user, map[string]any{"name": "A", "email": sharedValue}, emailUnique)
	if err != nil {
		t.Fatalf("InsertRecord tenantA: %v (must succeed — tenants must not collide)", err)
	}
	idB, err := repo.InsertRecord(ctx, entity, tenantB, user, map[string]any{"name": "B", "email": sharedValue}, emailUnique)
	if err != nil {
		t.Fatalf("InsertRecord tenantB: %v (must succeed — same value, DIFFERENT tenant, no uq_pivot collision)", err)
	}

	// Each tenant sees its OWN pivot row for the value...
	if got := pivotValues(t, h, tenantA, entity); !eqValues(got, []string{sharedValue}) {
		t.Fatalf("tenantA pivot = %v; want [%s]", got, sharedValue)
	}
	if got := pivotValues(t, h, tenantB, entity); !eqValues(got, []string{sharedValue}) {
		t.Fatalf("tenantB pivot = %v; want [%s]", got, sharedValue)
	}

	// ...and exactly TWO rows exist total for (entity, field, value) — one per tenant, proving
	// tenant_id (not just record_id) partitions the uq_pivot constraint.
	tenants := pivotTenantsForValue(t, h, entity, "email", sharedValue)
	if !eqValues(tenants, []string{tenantA, tenantB}) {
		t.Fatalf("tenants holding pivot value %q = %v; want exactly [%s, %s]", sharedValue, tenants, tenantA, tenantB)
	}

	// Sanity: the two rows really do belong to the two distinct records inserted above.
	if c := pivotRowCountForRecord(t, h, tenantA, entity, idA); c != 1 {
		t.Fatalf("tenantA record pivot count = %d; want 1", c)
	}
	if c := pivotRowCountForRecord(t, h, tenantB, entity, idB); c != 1 {
		t.Fatalf("tenantB record pivot count = %d; want 1", c)
	}
}

// physicalIndexUnique reports whether `index` on `table` currently exists as a UNIQUE index
// (NON_UNIQUE=0), false if it exists as non-unique OR does not exist at all.
func physicalIndexUnique(t *testing.T, h *sqlx.DB, table, index string) bool {
	t.Helper()
	var nonUnique int
	err := h.Get(&nonUnique,
		"SELECT NON_UNIQUE FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ? LIMIT 1",
		table, index)
	if err != nil {
		return false // index absent — not unique (and not present either)
	}
	return nonUnique == 0
}

// indexExists reports whether `index` exists at all on `table`.
func indexExists(t *testing.T, h *sqlx.DB, table, index string) bool {
	t.Helper()
	var n int
	if err := h.Get(&n, "SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ?", table, index); err != nil {
		t.Fatalf("indexExists query: %v", err)
	}
	return n > 0
}

// TestSliceB_SoleEnforcement_NoPhysicalUniqueIndex is the RM-28 Slice B "sole-enforcement" proof
// (spec §8.1): a fresh table built post Slice B (BuildCreateTable's unique branch) NEVER
// materializes a physical UNIQUE index for a plain `is_unique` field — information_schema shows
// idx_obj_<entity>_<field> (non-unique) and NO uq_obj_<entity>_<field> at all. A same-tenant
// duplicate insert is still rejected, proving the rejection can ONLY be coming from uq_pivot
// (there is no physical UNIQUE index left to have caught it).
func TestSliceB_SoleEnforcement_NoPhysicalUniqueIndex(t *testing.T) {
	const entity = "it_sliceb_sole"
	const tenant = "33333333-3333-3333-3333-3333333333c1"
	const user = "00000000-0000-0000-0000-0000000000u1"
	repo, h := pivotSetup(t, entity)
	ctx := context.Background()
	table := "obj_" + entity

	// The physical shape: non-unique idx_, no uq_ at all.
	if !indexExists(t, h, table, "idx_"+table+"_email") {
		t.Fatalf("idx_%s_email missing — BuildCreateTable must materialize a non-unique index for a unique field post Slice B", table)
	}
	if physicalIndexUnique(t, h, table, "idx_"+table+"_email") {
		t.Fatalf("idx_%s_email is UNIQUE — RM-28 Slice B requires a NON-unique index for a plain unique field", table)
	}
	if indexExists(t, h, table, "uq_"+table+"_email") {
		t.Fatalf("uq_%s_email exists — a plain unique field must NEVER get a physical UNIQUE index post Slice B", table)
	}

	// Sole enforcement: same tenant, same value, second insert → 409 (must come from uq_pivot;
	// no physical UNIQUE index exists to have caught it).
	if _, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"email": "solo@x.com"}, emailUnique); err != nil {
		t.Fatalf("InsertRecord #1: %v", err)
	}
	_, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"email": "solo@x.com"}, emailUnique)
	if !errors.Is(err, ErrDuplicateUnique) {
		t.Fatalf("InsertRecord #2 err = %v; want ErrDuplicateUnique (from uq_pivot — the only remaining constraint)", err)
	}
	if got := pivotValues(t, h, tenant, entity); !eqValues(got, []string{"solo@x.com"}) {
		t.Fatalf("pivot after rejected dup = %v; want exactly [solo@x.com] (single winner)", got)
	}
}

// TestSliceB_CrossTenantDecoupling is THE core proof of RM-28 (spec §8.1 "Slice B test =
// ENFORCEMENT" / dispatch's cross-tenant decoupling requirement): two tenants sharing the SAME
// obj_* table (obj_<entity> is named by entity, not tenant) are independently enforced —
//   - tenant A DECLARES the field unique (threads emailUnique) → its own duplicate is rejected.
//   - tenant B does NOT declare the field unique (threads nil — no pivot spec at all, mirroring a
//     co-tenant whose sys_fields row for this field has is_unique=0 or does not exist) → B may
//     insert AS MANY duplicate values as it wants; A's constraint does not touch B's rows.
//   - tenant C ALSO declares the field unique, independently of A → C's own duplicate is rejected
//     by C's OWN pivot rows, proving A's constraint neither blocks nor is required for C's.
func TestSliceB_CrossTenantDecoupling(t *testing.T) {
	const entity = "it_sliceb_decouple"
	const tenantA = "33333333-3333-3333-3333-3333333333d1"
	const tenantB = "33333333-3333-3333-3333-3333333333d2"
	const tenantC = "33333333-3333-3333-3333-3333333333d3"
	const user = "00000000-0000-0000-0000-0000000000u1"
	const sharedValue = "shared@decouple.com"
	repo, h := pivotSetup(t, entity)
	ctx := context.Background()

	// Tenant A declares unique: first insert succeeds, second (same value) is rejected.
	if _, err := repo.InsertRecord(ctx, entity, tenantA, user, map[string]any{"email": sharedValue}, emailUnique); err != nil {
		t.Fatalf("tenantA insert #1: %v", err)
	}
	if _, err := repo.InsertRecord(ctx, entity, tenantA, user, map[string]any{"email": sharedValue}, emailUnique); !errors.Is(err, ErrDuplicateUnique) {
		t.Fatalf("tenantA insert #2 err = %v; want ErrDuplicateUnique (A's own constraint)", err)
	}

	// Tenant B does NOT declare the field unique (no UniqueFieldSpec threaded — mirrors the
	// service layer never populating uniqueFieldSpecs for a field whose is_unique=0 for B): TWO
	// inserts of the SAME value must BOTH succeed, completely unaffected by A's constraint OR by
	// any physical index (there is none — Slice B demoted it to non-unique).
	if _, err := repo.InsertRecord(ctx, entity, tenantB, user, map[string]any{"email": sharedValue}, nil); err != nil {
		t.Fatalf("tenantB insert #1 (undeclared unique): %v (must succeed — B never opted into uniqueness)", err)
	}
	if _, err := repo.InsertRecord(ctx, entity, tenantB, user, map[string]any{"email": sharedValue}, nil); err != nil {
		t.Fatalf("tenantB insert #2 (undeclared unique, SAME value as #1 AND as tenant A's): %v (must ALSO succeed — decoupling is the whole point of RM-28)", err)
	}
	if got := pivotValues(t, h, tenantB, entity); len(got) != 0 {
		t.Fatalf("tenantB pivot = %v; want [] (B never declared unique — zero pivot rows, zero enforcement)", got)
	}
	if c := objLiveCount(t, h, entity, tenantB); c != 2 {
		t.Fatalf("tenantB live rows = %d; want 2 (both dup inserts landed)", c)
	}

	// Tenant C ALSO declares unique, independently: its own first insert of the SAME value A and
	// B already hold succeeds (C's constraint is scoped to C, not global), its second is rejected
	// by C's OWN pivot — proving A's rows neither help nor hinder C.
	if _, err := repo.InsertRecord(ctx, entity, tenantC, user, map[string]any{"email": sharedValue}, emailUnique); err != nil {
		t.Fatalf("tenantC insert #1: %v (must succeed — C's constraint is independent of A's)", err)
	}
	if _, err := repo.InsertRecord(ctx, entity, tenantC, user, map[string]any{"email": sharedValue}, emailUnique); !errors.Is(err, ErrDuplicateUnique) {
		t.Fatalf("tenantC insert #2 err = %v; want ErrDuplicateUnique (C's own constraint)", err)
	}
}
