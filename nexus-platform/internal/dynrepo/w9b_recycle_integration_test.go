//go:build integration

// W9b integration repro: recycle-bin RESTORE + deleted-list on the REAL repository path.
//
// Run with:
//
//	go test -tags=integration -race ./internal/dynrepo/ -run TestW9bRecycle
//
// Requires a reachable MySQL 8 (TEST_DATABASE_URL or the local compose DSN). These tests drive
// CreateTable → InsertRecord → SoftDeleteRecord → RestoreRecord + ListRecords(OnlyDeleted), so they
// exercise the conditional-NULL unique generated column (ddl.BuildCreateTable) that makes the
// re-taken-slot collision only reproducible against real MySQL.
package dynrepo

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/ddl"
)

// TestW9bRecycleRestoreAndDeletedList covers the happy path: soft-delete moves a record into the
// recycle bin (ListRecords OnlyDeleted), restore brings it back to the live set, and the record's
// data survives the round-trip.
func TestW9bRecycleRestoreAndDeletedList(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_w9b_recycle"
	const tenant = "11111111-0000-0000-0000-0000000000a1"
	const user = "00000000-0000-0000-0000-0000000000aa"

	_ = repo.DropTable(ctx, entity)
	_, _ = db.Exec("DELETE FROM `sys_unique_index` WHERE `entity_name` = ?", entity)
	t.Cleanup(func() {
		_ = repo.DropTable(ctx, entity)
		_, _ = db.Exec("DELETE FROM `sys_unique_index` WHERE `entity_name` = ?", entity)
	})
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "name", FieldType: "text"},
		{Name: "email", FieldType: "email", IsUnique: true},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	id, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"name": "Alice", "email": "alice@example.com"}, emailUnique)
	if err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}

	// Before deletion: the recycle bin is empty, the live list has the row.
	if _, total, err := repo.ListRecords(ctx, entity, tenant, AdminScope(), ListParams{OnlyDeleted: true}); err != nil || total != 0 {
		t.Fatalf("deleted list before soft-delete: total=%d err=%v; want 0, nil", total, err)
	}
	if _, total, err := repo.ListRecords(ctx, entity, tenant, AdminScope(), ListParams{}); err != nil || total != 1 {
		t.Fatalf("live list before soft-delete: total=%d err=%v; want 1, nil", total, err)
	}

	if err := repo.SoftDeleteRecord(ctx, entity, tenant, id, AdminScope()); err != nil {
		t.Fatalf("SoftDeleteRecord: %v", err)
	}

	// After deletion: recycle bin has the row, live list is empty.
	recs, total, err := repo.ListRecords(ctx, entity, tenant, AdminScope(), ListParams{OnlyDeleted: true})
	if err != nil || total != 1 || len(recs) != 1 || recs[0].ID != id {
		t.Fatalf("deleted list after soft-delete: total=%d len=%d err=%v; want the 1 soft-deleted row", total, len(recs), err)
	}
	if _, total, err := repo.ListRecords(ctx, entity, tenant, AdminScope(), ListParams{}); err != nil || total != 0 {
		t.Fatalf("live list after soft-delete: total=%d err=%v; want 0, nil", total, err)
	}

	// Restore brings it back; the data survives.
	if err := repo.RestoreRecord(ctx, entity, tenant, id, user, AdminScope(), emailUnique); err != nil {
		t.Fatalf("RestoreRecord: %v", err)
	}
	if _, total, err := repo.ListRecords(ctx, entity, tenant, AdminScope(), ListParams{OnlyDeleted: true}); err != nil || total != 0 {
		t.Fatalf("deleted list after restore: total=%d err=%v; want 0, nil", total, err)
	}
	rec, err := repo.GetRecord(ctx, entity, tenant, id, AdminScope())
	if err != nil {
		t.Fatalf("GetRecord after restore: %v", err)
	}
	if rec.Data["email"] != "alice@example.com" {
		t.Fatalf("restored record lost its data: %v", rec.Data)
	}
}

// TestW9bRecycleRestoreNotFound proves restore reports not-found for a non-existent id, a LIVE
// (never-deleted) record, and a cross-tenant id — never silently "restoring" a live row.
func TestW9bRecycleRestoreNotFound(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_w9b_restore_404"
	const tenant = "22222222-0000-0000-0000-0000000000b1"
	const other = "22222222-0000-0000-0000-0000000000b2"
	const user = "00000000-0000-0000-0000-0000000000bb"

	_ = repo.DropTable(ctx, entity)
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "name", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	// (a) A non-existent id.
	if err := repo.RestoreRecord(ctx, entity, tenant, "does-not-exist", user, AdminScope(), nil); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("restore non-existent: want ErrRecordNotFound, got %v", err)
	}

	// (b) A LIVE, never-deleted record must NOT be restorable (deleted_at IS NOT NULL guard).
	id, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"name": "Live"}, nil)
	if err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}
	if err := repo.RestoreRecord(ctx, entity, tenant, id, user, AdminScope(), nil); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("restore a live record: want ErrRecordNotFound, got %v", err)
	}

	// (c) Cross-tenant: soft-delete under `tenant`, then a restore scoped to `other` must not see it.
	if err := repo.SoftDeleteRecord(ctx, entity, tenant, id, AdminScope()); err != nil {
		t.Fatalf("SoftDeleteRecord: %v", err)
	}
	if err := repo.RestoreRecord(ctx, entity, other, id, user, AdminScope(), nil); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("cross-tenant restore: want ErrRecordNotFound, got %v", err)
	}
	// The row is still soft-deleted under its real tenant (the cross-tenant attempt was a no-op).
	if _, total, err := repo.ListRecords(ctx, entity, tenant, AdminScope(), ListParams{OnlyDeleted: true}); err != nil || total != 1 {
		t.Fatalf("row must remain soft-deleted after failed cross-tenant restore: total=%d err=%v", total, err)
	}
}

// TestW9bRecycleRestoreOwnerScope proves the A5 row-scope applies to restore: an owner-scoped
// caller can only restore rows they created.
func TestW9bRecycleRestoreOwnerScope(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_w9b_restore_scope"
	const tenant = "33333333-0000-0000-0000-0000000000c1"
	const owner = "00000000-0000-0000-0000-0000000000c1"
	const stranger = "00000000-0000-0000-0000-0000000000c2"

	_ = repo.DropTable(ctx, entity)
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "name", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	id, err := repo.InsertRecord(ctx, entity, tenant, owner, map[string]any{"name": "Owned"}, nil)
	if err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}
	if err := repo.SoftDeleteRecord(ctx, entity, tenant, id, AdminScope()); err != nil {
		t.Fatalf("SoftDeleteRecord: %v", err)
	}
	// A stranger with owner-scope cannot restore it (not their row).
	strangerScope := RowScope{UserID: stranger, Level: AccessOwner}
	if err := repo.RestoreRecord(ctx, entity, tenant, id, stranger, strangerScope, nil); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("stranger owner-scope restore: want ErrRecordNotFound, got %v", err)
	}
	// The owner with owner-scope can.
	ownerScope := RowScope{UserID: owner, Level: AccessOwner}
	if err := repo.RestoreRecord(ctx, entity, tenant, id, owner, ownerScope, nil); err != nil {
		t.Fatalf("owner restore: want success, got %v", err)
	}
}

// TestW9bRecycleRestoreRetakenSlot is the B3/RM-28 trap: after a record is soft-deleted, a LIVE
// row can re-take its unique-value slot (the soft-deleted row's generated column projects NULL).
// Restoring the original must then FAIL CLEANLY with a *DuplicateUniqueError (→ 409 RECORD_DUPLICATE
// at the service), and must NOT leave a broken half-state — the original stays soft-deleted.
func TestW9bRecycleRestoreRetakenSlot(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_w9b_retaken"
	const tenant = "44444444-0000-0000-0000-0000000000d1"
	const user = "00000000-0000-0000-0000-0000000000dd"
	const email = "dup@example.com"

	_ = repo.DropTable(ctx, entity)
	_, _ = db.Exec("DELETE FROM `sys_unique_index` WHERE `entity_name` = ?", entity)
	t.Cleanup(func() {
		_ = repo.DropTable(ctx, entity)
		_, _ = db.Exec("DELETE FROM `sys_unique_index` WHERE `entity_name` = ?", entity)
	})
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "name", FieldType: "text"},
		{Name: "email", FieldType: "email", IsUnique: true},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	// A(email) → soft-delete → the unique slot is freed (gen-col NULL while deleted).
	// RM-28 Slice B: uniqueness is enforced solely by the sys_unique_index pivot now — thread
	// emailUnique (declared in pivot_integration_test.go, same package) exactly as a real
	// caller (RecordService) would, instead of silently exercising zero enforcement.
	idA, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"name": "A", "email": email}, emailUnique)
	if err != nil {
		t.Fatalf("InsertRecord A: %v", err)
	}
	if err := repo.SoftDeleteRecord(ctx, entity, tenant, idA, AdminScope()); err != nil {
		t.Fatalf("SoftDeleteRecord A: %v", err)
	}

	// B re-takes the SAME email while A is in the bin — succeeds (slot was free).
	idB, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{"name": "B", "email": email}, emailUnique)
	if err != nil {
		t.Fatalf("InsertRecord B (re-take freed slot) should succeed, got: %v", err)
	}

	// Restoring A now collides with the live B on the unique index → *DuplicateUniqueError.
	err = repo.RestoreRecord(ctx, entity, tenant, idA, user, AdminScope(), emailUnique)
	if err == nil {
		t.Fatalf("restore into a re-taken unique slot must fail, got nil")
	}
	if !errors.Is(err, ErrDuplicateUnique) {
		t.Fatalf("restore re-taken slot: want ErrDuplicateUnique, got %v", err)
	}
	var dup *DuplicateUniqueError
	if !errors.As(err, &dup) {
		t.Fatalf("restore re-taken slot: want *DuplicateUniqueError, got %v", err)
	}

	// No half-state: A must STILL be soft-deleted (the failed UPDATE rolled back atomically), and B
	// must still be live and hold the slot.
	if _, total, err := repo.ListRecords(ctx, entity, tenant, AdminScope(), ListParams{OnlyDeleted: true}); err != nil || total != 1 {
		t.Fatalf("A must remain soft-deleted after failed restore: total=%d err=%v; want 1", total, err)
	}
	recB, err := repo.GetRecord(ctx, entity, tenant, idB, AdminScope())
	if err != nil || recB.ID != idB {
		t.Fatalf("B must remain live and hold the slot: %v", err)
	}
}
