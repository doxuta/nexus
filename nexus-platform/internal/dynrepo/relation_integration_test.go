//go:build integration

// Integration tests for the M3 1-N relation engine at the dynrepo layer. Run with:
//
//	go test -tags=integration ./internal/dynrepo/
//
// They require a reachable MySQL 8 (the docker-compose `mysql` service); the harness
// (testDB, columnSet, indexSet, tableExists) lives in dynrepo_integration_test.go.
package dynrepo

import (
	"context"
	"testing"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/ddl"
)

// TestAddAndDropRelationColumn covers M3-T1's integration acceptance: adding a relation
// field (FK-on-child) to an existing entity materializes a CHAR(36) `<f>_id` column + a
// non-unique tenant-scoped index, and the compensation drop reverses it exactly.
func TestAddAndDropRelationColumn(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_rel_child"

	_ = repo.DropTable(ctx, entity)
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "title", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	const table = "obj_it_rel_child"
	if _, ok := columnSet(t, db, table)["account_id"]; ok {
		t.Fatal("account_id column should not exist before AddRelationColumn")
	}

	// Add a relation field -> FK column + non-unique tenant-scoped index appear.
	if err := repo.AddRelationColumn(ctx, entity, "account"); err != nil {
		t.Fatalf("AddRelationColumn: %v", err)
	}
	if _, ok := columnSet(t, db, table)["account_id"]; !ok {
		t.Error("AddRelationColumn did not create the `account_id` FK column")
	}
	if _, ok := indexSet(t, db, table)["idx_obj_it_rel_child_account_id"]; !ok {
		t.Error("AddRelationColumn did not create the FK index")
	}
	// The FK index must be NON-unique (1-N: many children -> one parent).
	if isUniqueIndex(t, db, table, "idx_obj_it_rel_child_account_id") {
		t.Error("relation FK index must be non-unique")
	}

	// Compensation drop -> column + index gone; table still present.
	if err := repo.DropRelationColumn(ctx, entity, "account"); err != nil {
		t.Fatalf("DropRelationColumn: %v", err)
	}
	if _, ok := columnSet(t, db, table)["account_id"]; ok {
		t.Error("DropRelationColumn did not remove the `account_id` column")
	}
	if _, ok := indexSet(t, db, table)["idx_obj_it_rel_child_account_id"]; ok {
		t.Error("DropRelationColumn did not remove the FK index")
	}
	if !tableExists(t, db, table) {
		t.Error("table should still exist after dropping just the column")
	}
}

// TestCreateTable_RelationFieldInline asserts a relation field declared at create time
// materializes the FK column + non-unique index inline (no separate ALTER needed).
func TestCreateTable_RelationFieldInline(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_rel_inline"

	_ = repo.DropTable(ctx, entity)
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "subject", FieldType: "text"},
		{Name: "parent", FieldType: "relation", RelatedEntity: "it_rel_parent"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	const table = "obj_it_rel_inline"
	if _, ok := columnSet(t, db, table)["parent_id"]; !ok {
		t.Error("create-time relation field did not produce the `parent_id` column")
	}
	if _, ok := indexSet(t, db, table)["idx_obj_it_rel_inline_parent_id"]; !ok {
		t.Error("create-time relation field did not produce the FK index")
	}
}

// TestRecordExists covers the FK-existence probe used by M3-T2 record validation: it
// reports true only for a live record in the SAME tenant; a different tenant's id, an
// absent id, and a soft-deleted record all report false (cross-tenant never leaks).
func TestRecordExists(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_rel_parent"
	const tenantA = "11111111-1111-1111-1111-111111111111"
	const tenantB = "22222222-2222-2222-2222-222222222222"
	const userID = "99999999-9999-9999-9999-999999999999"

	_ = repo.DropTable(ctx, entity)
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "name", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	// Insert a parent record under tenant A.
	parentID, err := repo.InsertRecord(ctx, entity, tenantA, userID, map[string]any{"name": "Acme"}, nil)
	if err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}

	// Same tenant + live id -> exists.
	if ok, err := repo.RecordExists(ctx, entity, tenantA, parentID); err != nil || !ok {
		t.Errorf("RecordExists(same tenant, live): ok=%v err=%v; want true,nil", ok, err)
	}
	// Cross-tenant -> must NOT see the record.
	if ok, err := repo.RecordExists(ctx, entity, tenantB, parentID); err != nil || ok {
		t.Errorf("RecordExists(cross tenant): ok=%v err=%v; want false,nil", ok, err)
	}
	// Absent id -> false.
	if ok, err := repo.RecordExists(ctx, entity, tenantA, "deadbeef-0000-0000-0000-000000000000"); err != nil || ok {
		t.Errorf("RecordExists(absent id): ok=%v err=%v; want false,nil", ok, err)
	}
	// Soft-deleted record -> no longer a valid FK target.
	if err := repo.SoftDeleteRecord(ctx, entity, tenantA, parentID, AdminScope()); err != nil {
		t.Fatalf("SoftDeleteRecord: %v", err)
	}
	if ok, err := repo.RecordExists(ctx, entity, tenantA, parentID); err != nil || ok {
		t.Errorf("RecordExists(soft-deleted): ok=%v err=%v; want false,nil", ok, err)
	}
}

// isUniqueIndex reports whether a named index on a table is unique (NON_UNIQUE = 0).
func isUniqueIndex(t *testing.T, db *sqlx.DB, table, index string) bool {
	t.Helper()
	var nonUnique int
	if err := db.Get(&nonUnique,
		`SELECT MIN(NON_UNIQUE) FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ?`,
		table, index); err != nil {
		t.Fatalf("query index uniqueness: %v", err)
	}
	return nonUnique == 0
}
