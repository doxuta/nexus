//go:build integration

// Integration tests for the dynamic-record repository. Run with:
//
//	go test -tags=integration ./internal/dynrepo/
//
// They require a reachable MySQL 8 (the docker-compose `mysql` service). Set
// TEST_DATABASE_URL to override the DSN; otherwise the local compose DSN is used.
package dynrepo

import (
	"context"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/ddl"
)

// testDB connects to the throwaway migrated database provisioned by TestMain (see
// main_test.go). Before #84 this read TEST_DATABASE_URL directly, which broke against a db-less
// DSN ("No database selected"); TestMain now creates+migrates a throwaway DB and points testDSN
// at it.
func testDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("mysql", testDSN)
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping integration test", err)
	}
	return db
}

func columnSet(t *testing.T, db *sqlx.DB, table string) map[string]struct{} {
	t.Helper()
	var cols []string
	err := db.Select(&cols,
		`SELECT COLUMN_NAME FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table)
	if err != nil {
		t.Fatalf("query columns: %v", err)
	}
	set := make(map[string]struct{}, len(cols))
	for _, c := range cols {
		set[c] = struct{}{}
	}
	return set
}

func tableExists(t *testing.T, db *sqlx.DB, table string) bool {
	t.Helper()
	var n int
	if err := db.Get(&n,
		`SELECT COUNT(*) FROM information_schema.TABLES
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table); err != nil {
		t.Fatalf("query table existence: %v", err)
	}
	return n > 0
}

func TestCreateAndDropTable(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_courses"

	_ = repo.DropTable(ctx, entity) // clean slate from any prior failed run
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	fields := []ddl.FieldDef{
		{Name: "title", FieldType: "text"},
		{Name: "code", FieldType: "text", IsUnique: true},
	}
	if err := repo.CreateTable(ctx, entity, fields); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	cols := columnSet(t, db, "obj_it_courses")
	for _, want := range []string{"id", "tenant_id", "team_set_id", "created_by", "data", "code"} {
		if _, ok := cols[want]; !ok {
			t.Errorf("obj_it_courses missing column %q", want)
		}
	}

	if err := repo.DropTable(ctx, entity); err != nil {
		t.Fatalf("DropTable: %v", err)
	}
	if tableExists(t, db, "obj_it_courses") {
		t.Error("obj_it_courses still exists after DropTable")
	}
}

func indexSet(t *testing.T, db *sqlx.DB, table string) map[string]struct{} {
	t.Helper()
	var names []string
	err := db.Select(&names,
		`SELECT DISTINCT INDEX_NAME FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return set
}

// TestAddAndDropUniqueColumn covers M2-T1's integration acceptance: adding a UNIQUE field to
// an existing entity materializes a generated column + tenant-scoped unique index, and the
// compensation drop reverses it exactly (column + index gone), leaving the table intact.
func TestAddAndDropUniqueColumn(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_field_engine"

	_ = repo.DropTable(ctx, entity)
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	// Start with only ordinary fields — no extra columns, no unique index.
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "title", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	const table = "obj_it_field_engine"
	if _, ok := columnSet(t, db, table)["sku"]; ok {
		t.Fatal("sku column should not exist before AddUniqueColumn")
	}

	// Add a unique field -> generated column + tenant-scoped unique index appear.
	if err := repo.AddUniqueColumn(ctx, entity, "sku"); err != nil {
		t.Fatalf("AddUniqueColumn: %v", err)
	}
	if _, ok := columnSet(t, db, table)["sku"]; !ok {
		t.Error("AddUniqueColumn did not create the `sku` generated column")
	}
	if _, ok := indexSet(t, db, table)["uq_obj_it_field_engine_sku"]; !ok {
		t.Error("AddUniqueColumn did not create the unique index")
	}

	// Compensation drop -> column and index gone again; table still present.
	if err := repo.DropUniqueColumn(ctx, entity, "sku"); err != nil {
		t.Fatalf("DropUniqueColumn: %v", err)
	}
	if _, ok := columnSet(t, db, table)["sku"]; ok {
		t.Error("DropUniqueColumn did not remove the `sku` column")
	}
	if _, ok := indexSet(t, db, table)["uq_obj_it_field_engine_sku"]; ok {
		t.Error("DropUniqueColumn did not remove the unique index")
	}
	if !tableExists(t, db, table) {
		t.Error("table should still exist after dropping just the column")
	}
}

// TestAddAndDropUniqueGencolNonUnique covers the RM-28 Slice B runtime path: a NEW unique field
// materializes the SAME generated column as AddUniqueColumn, but a NON-unique tenant-scoped
// index (no uq_ at all) — uniqueness for it is enforced solely by the sys_unique_index pivot.
func TestAddAndDropUniqueGencolNonUnique(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_field_engine_b"

	_ = repo.DropTable(ctx, entity)
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "title", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	const table = "obj_it_field_engine_b"
	if _, ok := columnSet(t, db, table)["sku"]; ok {
		t.Fatal("sku column should not exist before AddUniqueGencolNonUnique")
	}

	if err := repo.AddUniqueGencolNonUnique(ctx, entity, "sku"); err != nil {
		t.Fatalf("AddUniqueGencolNonUnique: %v", err)
	}
	if _, ok := columnSet(t, db, table)["sku"]; !ok {
		t.Error("AddUniqueGencolNonUnique did not create the `sku` generated column")
	}
	idx := indexSet(t, db, table)
	if _, ok := idx["idx_obj_it_field_engine_b_sku"]; !ok {
		t.Error("AddUniqueGencolNonUnique did not create the non-unique index")
	}
	if _, ok := idx["uq_obj_it_field_engine_b_sku"]; ok {
		t.Error("AddUniqueGencolNonUnique must NOT create a UNIQUE index post RM-28 Slice B")
	}

	// Compensation drop -> column and index gone again; table still present.
	if err := repo.DropUniqueGencolNonUnique(ctx, entity, "sku"); err != nil {
		t.Fatalf("DropUniqueGencolNonUnique: %v", err)
	}
	if _, ok := columnSet(t, db, table)["sku"]; ok {
		t.Error("DropUniqueGencolNonUnique did not remove the `sku` column")
	}
	if _, ok := indexSet(t, db, table)["idx_obj_it_field_engine_b_sku"]; ok {
		t.Error("DropUniqueGencolNonUnique did not remove the index")
	}
	if !tableExists(t, db, table) {
		t.Error("table should still exist after dropping just the column")
	}
}

// TestColumnExists covers the B-1 precheck helper: before/after a unique field's generated
// column is materialized, ColumnExists must report false then true, and it must stay true
// after a soft-delete would have kept the column (the column is only physically gone after
// an explicit DropUniqueColumn). It also reports false for an entirely absent table.
func TestColumnExists(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_colexists"

	_ = repo.DropTable(ctx, entity)
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	// Absent table -> column cannot exist.
	if ok, err := repo.ColumnExists(ctx, entity, "sku"); err != nil || ok {
		t.Fatalf("ColumnExists on absent table: ok=%v err=%v; want false,nil", ok, err)
	}

	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "title", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	// Column not yet added.
	if ok, err := repo.ColumnExists(ctx, entity, "sku"); err != nil || ok {
		t.Fatalf("ColumnExists before AddUniqueColumn: ok=%v err=%v; want false,nil", ok, err)
	}

	// Add the unique field -> the generated column exists.
	if err := repo.AddUniqueColumn(ctx, entity, "sku"); err != nil {
		t.Fatalf("AddUniqueColumn: %v", err)
	}
	if ok, err := repo.ColumnExists(ctx, entity, "sku"); err != nil || !ok {
		t.Fatalf("ColumnExists after AddUniqueColumn: ok=%v err=%v; want true,nil", ok, err)
	}

	// A second AddUniqueColumn on the existing column must FAIL (this is exactly the 500
	// the B-1 precheck prevents) — proving ColumnExists is the right guard.
	if err := repo.AddUniqueColumn(ctx, entity, "sku"); err == nil {
		t.Error("re-adding an existing column should error (the 500 ColumnExists guards against)")
	}

	// After dropping the column, ColumnExists reports false again.
	if err := repo.DropUniqueColumn(ctx, entity, "sku"); err != nil {
		t.Fatalf("DropUniqueColumn: %v", err)
	}
	if ok, err := repo.ColumnExists(ctx, entity, "sku"); err != nil || ok {
		t.Fatalf("ColumnExists after DropUniqueColumn: ok=%v err=%v; want false,nil", ok, err)
	}
}
