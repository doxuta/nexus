//go:build integration

package dynrepo

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/ddl"
)

func TestRecordCRUD(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_people"
	const tenantA = "11111111-1111-1111-1111-1111111111aa"
	const tenantB = "22222222-2222-2222-2222-2222222222bb"
	const user = "00000000-0000-0000-0000-0000000000u1"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "name", FieldType: "text"},
		{Name: "age", FieldType: "number"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	// Insert two records for tenant A and one for tenant B.
	idA1, err := repo.InsertRecord(ctx, entity, tenantA, user, map[string]any{"name": "An", "age": float64(30)}, nil)
	if err != nil {
		t.Fatalf("InsertRecord A1: %v", err)
	}
	if _, err := repo.InsertRecord(ctx, entity, tenantA, user, map[string]any{"name": "Bình", "age": float64(25)}, nil); err != nil {
		t.Fatalf("InsertRecord A2: %v", err)
	}
	if _, err := repo.InsertRecord(ctx, entity, tenantB, user, map[string]any{"name": "Cường"}, nil); err != nil {
		t.Fatalf("InsertRecord B1: %v", err)
	}

	// Get returns the stored data for the right tenant.
	got, err := repo.GetRecord(ctx, entity, tenantA, idA1, AdminScope())
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	if got.Data["name"] != "An" {
		t.Errorf("Data[name] = %v; want An", got.Data["name"])
	}

	// Cross-tenant get must not leak: tenant B cannot read tenant A's record.
	if _, err := repo.GetRecord(ctx, entity, tenantB, idA1, AdminScope()); !errors.Is(err, ErrRecordNotFound) {
		t.Errorf("cross-tenant GetRecord err = %v; want ErrRecordNotFound", err)
	}

	// List is tenant-scoped: tenant A sees 2, tenant B sees 1.
	recs, total, err := repo.ListRecords(ctx, entity, tenantA, AdminScope(), ListParams{Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("ListRecords A: %v", err)
	}
	if total != 2 || len(recs) != 2 {
		t.Errorf("tenant A list total=%d len=%d; want 2/2", total, len(recs))
	}

	// Update is apply-when-present: a PARTIAL body merges only the supplied field and leaves
	// absent fields untouched (no full-replace data loss). Here we update only `name` and
	// assert `age` survives unchanged.
	if err := repo.UpdateRecord(ctx, entity, tenantA, idA1, user, AdminScope(), map[string]any{"name": "An Updated"}, nil, nil); err != nil {
		t.Fatalf("UpdateRecord (partial): %v", err)
	}
	got, _ = repo.GetRecord(ctx, entity, tenantA, idA1, AdminScope())
	if got.Data["name"] != "An Updated" {
		t.Errorf("after partial update Data[name] = %v; want An Updated", got.Data["name"])
	}
	if got.Data["age"] != float64(30) {
		t.Errorf("partial update wiped absent field: Data[age] = %v; want 30 preserved", got.Data["age"])
	}

	// A subsequent partial update of `age` merges over the already-merged record.
	if err := repo.UpdateRecord(ctx, entity, tenantA, idA1, user, AdminScope(), map[string]any{"age": float64(31)}, nil, nil); err != nil {
		t.Fatalf("UpdateRecord (partial age): %v", err)
	}
	got, _ = repo.GetRecord(ctx, entity, tenantA, idA1, AdminScope())
	if got.Data["name"] != "An Updated" || got.Data["age"] != float64(31) {
		t.Errorf("after second partial update Data = %v; want name=An Updated, age=31", got.Data)
	}

	// Soft delete: record disappears from get/list but stays in the table.
	if err := repo.SoftDeleteRecord(ctx, entity, tenantA, idA1, AdminScope()); err != nil {
		t.Fatalf("SoftDeleteRecord: %v", err)
	}
	if _, err := repo.GetRecord(ctx, entity, tenantA, idA1, AdminScope()); !errors.Is(err, ErrRecordNotFound) {
		t.Errorf("get after soft-delete err = %v; want ErrRecordNotFound", err)
	}
	_, total, _ = repo.ListRecords(ctx, entity, tenantA, AdminScope(), ListParams{Page: 1, Limit: 20})
	if total != 1 {
		t.Errorf("tenant A list total after soft-delete = %d; want 1", total)
	}
}

// TestUpdateRecord_NoOpVsNotFound is the #3 regression (round-4 MEDIUM): a PUT that re-sends
// identical values (so MySQL reports RowsAffected==0 with no CLIENT_FOUND_ROWS) must NOT be
// mistaken for a missing record. The repo disambiguates via a scope-aware existence probe:
//   - existing+visible record, no-op merge by the SAME user -> success (200), not 404.
//   - genuinely missing id                                  -> ErrRecordNotFound (404).
//   - existing record OUTSIDE the caller's row scope        -> ErrRecordNotFound (404).
func TestUpdateRecord_NoOpVsNotFound(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_noop"
	const tenant = "33333333-3333-3333-3333-3333333333cc"
	const owner = "00000000-0000-0000-0000-0000000000o1"
	const other = "00000000-0000-0000-0000-0000000000o2"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "name", FieldType: "text"},
		{Name: "age", FieldType: "number"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	id, err := repo.InsertRecord(ctx, entity, tenant, owner, map[string]any{"name": "An", "age": float64(30)}, nil)
	if err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}

	// First update by the owner changes modified_by from creator->owner (creator IS owner here,
	// but the very first UpdateRecord still writes modified_by, so use it to "settle" the row).
	if err := repo.UpdateRecord(ctx, entity, tenant, id, owner, AdminScope(), map[string]any{"name": "An"}, nil, nil); err != nil {
		t.Fatalf("UpdateRecord (settle): %v", err)
	}

	// NO-OP: same user re-sends the SAME values. data unchanged + modified_by unchanged ->
	// RowsAffected==0. Must be treated as an idempotent success, NOT ErrRecordNotFound.
	if err := repo.UpdateRecord(ctx, entity, tenant, id, owner, AdminScope(), map[string]any{"name": "An"}, nil, nil); err != nil {
		t.Fatalf("no-op UpdateRecord (same values, same user) err = %v; want nil (200, not 404)", err)
	}
	// The record must still be intact and readable after the no-op.
	got, err := repo.GetRecord(ctx, entity, tenant, id, AdminScope())
	if err != nil || got.Data["name"] != "An" || got.Data["age"] != float64(30) {
		t.Fatalf("after no-op, GetRecord err=%v data=%v; want intact {name:An, age:30}", err, got.Data)
	}

	// NOT FOUND: a genuinely missing id must still surface ErrRecordNotFound (404), even when
	// the merge body would be a no-op shape.
	const missing = "99999999-9999-9999-9999-999999999999"
	if err := repo.UpdateRecord(ctx, entity, tenant, missing, owner, AdminScope(), map[string]any{"name": "An"}, nil, nil); !errors.Is(err, ErrRecordNotFound) {
		t.Errorf("UpdateRecord(missing id) err = %v; want ErrRecordNotFound", err)
	}

	// OUT OF SCOPE: a no-op update of the owner's record by `other` under AccessOwner scope must
	// surface ErrRecordNotFound — the existence probe is scope-aware, so a user cannot probe (or
	// silently succeed against) rows outside their owner/team visibility.
	otherOwnerScope := RowScope{UserID: other, Level: AccessOwner}
	if err := repo.UpdateRecord(ctx, entity, tenant, id, other, otherOwnerScope, map[string]any{"name": "An"}, nil, nil); !errors.Is(err, ErrRecordNotFound) {
		t.Errorf("UpdateRecord(no-op, out-of-scope user) err = %v; want ErrRecordNotFound", err)
	}
}
