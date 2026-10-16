//go:build integration

// W5-E1 integration repro: soft-delete unique-value SQUAT fix (conditional-NULL gen-col).
//
// Run with:
//
//	go test -tags=integration -race ./internal/dynrepo/ -run TestSoftDeleteUniqueSquat
//
// Requires a reachable MySQL 8 (TEST_DATABASE_URL or the local compose DSN). The test drives
// the REAL repository path (CreateTable → InsertRecord → SoftDeleteRecord → InsertRecord), so
// it exercises the conditional-NULL unique generated column produced by ddl.BuildCreateTable.
// It is the empirical before=409/after=201 acceptance for the squat bug.
package dynrepo

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/ddl"
)

// TestSoftDeleteUniqueSquat proves a soft-deleted record no longer occupies its unique slot:
// after soft-deleting a record with a unique field, the same value can be inserted again
// (the broken CRM delete→re-add flow), while two LIVE rows with the same value still collide
// and cross-tenant values stay isolated.
func TestSoftDeleteUniqueSquat(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_squat_contacts"
	const t1 = "11111111-0000-0000-0000-000000000001"
	const t2 = "22222222-0000-0000-0000-000000000002"
	const user = "00000000-0000-0000-0000-0000000000aa"

	_ = repo.DropTable(ctx, entity) // clean slate from any prior failed run
	_, _ = db.Exec("DELETE FROM `sys_unique_index` WHERE `entity_name` = ?", entity)
	t.Cleanup(func() {
		_ = repo.DropTable(ctx, entity)
		_, _ = db.Exec("DELETE FROM `sys_unique_index` WHERE `entity_name` = ?", entity)
	})

	// Create the table with a UNIQUE `email` field — the engine materializes the W5-E1
	// conditional-NULL generated column + tenant-scoped unique index.
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "name", FieldType: "text"},
		{Name: "email", FieldType: "email", IsUnique: true},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	const email = "alice@example.com"
	// RM-28 Slice B: the physical index is non-unique from birth (CreateTable's unique branch);
	// uniqueness is enforced SOLELY by the sys_unique_index pivot, which only activates when the
	// caller threads UniqueFieldSpec — exactly what RecordService.Create/Update/RestoreRecord do
	// in production (record_orchestration.go's uniqueFieldSpecs). Thread it here too (emailUnique,
	// declared in pivot_integration_test.go, same package) so this low-level repo test mirrors a
	// real caller instead of silently exercising zero enforcement.

	// 1) Insert a live record, then soft-delete it.
	id1, err := repo.InsertRecord(ctx, entity, t1, user, map[string]any{"name": "Alice", "email": email}, emailUnique)
	if err != nil {
		t.Fatalf("InsertRecord (first): %v", err)
	}
	if err := repo.SoftDeleteRecord(ctx, entity, t1, id1, AdminScope()); err != nil {
		t.Fatalf("SoftDeleteRecord: %v", err)
	}

	// 2) THE FIX (after=201): re-create the SAME email after the soft-delete must SUCCEED.
	// Before W5-E1 this returned a DuplicateUniqueError (→ 409 RECORD_DUPLICATE) because the
	// soft-deleted row's value squatted the unique slot.
	id2, err := repo.InsertRecord(ctx, entity, t1, user, map[string]any{"name": "Alice 2", "email": email}, emailUnique)
	if err != nil {
		t.Fatalf("re-create same email after soft-delete should SUCCEED (squat fixed), got: %v", err)
	}
	if id2 == id1 {
		t.Fatalf("expected a NEW record id, got the soft-deleted one")
	}

	// 3) Live uniqueness STILL enforced: a SECOND live record with the same email must 409.
	_, err = repo.InsertRecord(ctx, entity, t1, user, map[string]any{"name": "Alice 3", "email": email}, emailUnique)
	if err == nil {
		t.Fatalf("a second LIVE record with the same email must violate uniqueness")
	}
	if !errors.Is(err, ErrDuplicateUnique) {
		t.Fatalf("expected ErrDuplicateUnique for a live collision, got: %v", err)
	}
	var dup *DuplicateUniqueError
	if !errors.As(err, &dup) {
		t.Fatalf("expected *DuplicateUniqueError, got: %v", err)
	}

	// 4) Cross-tenant isolation: tenant2 can use the same email independently.
	if _, err := repo.InsertRecord(ctx, entity, t2, user, map[string]any{"name": "Bob", "email": email}, emailUnique); err != nil {
		t.Fatalf("cross-tenant: tenant2 must be able to use the same email, got: %v", err)
	}

	// 5) The original soft-deleted row's value is preserved in the JSON `data` (restore-safe):
	// reading it back with an admin scope that INCLUDES deleted rows would show email intact.
	// We assert the raw column to avoid depending on a deleted-inclusive read API.
	var dataEmail string
	if err := db.GetContext(ctx, &dataEmail,
		"SELECT JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.email')) FROM `obj_it_squat_contacts` WHERE id = ?", id1); err != nil {
		t.Fatalf("read soft-deleted row data: %v", err)
	}
	if dataEmail != email {
		t.Fatalf("soft-deleted row should keep its original email in data JSON; got %q", dataEmail)
	}

	// And its generated unique column projects NULL while deleted (so it doesn't squat).
	var genEmail *string
	if err := db.GetContext(ctx, &genEmail,
		"SELECT `email` FROM `obj_it_squat_contacts` WHERE id = ?", id1); err != nil {
		t.Fatalf("read soft-deleted row gen-col: %v", err)
	}
	if genEmail != nil {
		t.Fatalf("soft-deleted row's unique gen-col should be NULL, got %q", *genEmail)
	}
}
