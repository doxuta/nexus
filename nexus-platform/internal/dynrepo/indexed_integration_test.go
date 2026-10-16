//go:build integration

// Integration tests for the D1 field-indexing feature (Wave 2B / D1).
//
// Run with:
//
//	go test -tags=integration ./internal/dynrepo/
//
// They require a reachable MySQL 8. Set TEST_DATABASE_URL to override the default DSN.
package dynrepo

import (
	"context"
	"testing"

	"nexus/platform/internal/ddl"
)

// TestAddDropIndexedColumn verifies that AddIndexedColumn materializes the correct typed
// generated column + non-unique index, and DropIndexedColumn reverses it exactly.
func TestAddDropIndexedColumn(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_d1_addrop"
	const table = "obj_it_d1_addrop"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "name", FieldType: "text"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	// --- ADD: number field indexed → DECIMAL(38,10) generated column + non-unique index ---
	if err := repo.AddIndexedColumn(ctx, entity, "score", "number"); err != nil {
		t.Fatalf("AddIndexedColumn: %v", err)
	}

	cols := columnSet(t, db, table)
	if _, ok := cols["score"]; !ok {
		t.Error("AddIndexedColumn: generated column 'score' not found in information_schema")
	}

	idxs := indexSet(t, db, table)
	const wantIdx = "idx_obj_it_d1_addrop_score"
	if _, ok := idxs[wantIdx]; !ok {
		t.Errorf("AddIndexedColumn: index %q not found; indexes: %v", wantIdx, idxs)
	}

	// --- DROP: compensation reverses add exactly ---
	if err := repo.DropIndexedColumn(ctx, entity, "score"); err != nil {
		t.Fatalf("DropIndexedColumn: %v", err)
	}

	cols = columnSet(t, db, table)
	if _, ok := cols["score"]; ok {
		t.Error("DropIndexedColumn: column 'score' still present after drop")
	}
	idxs = indexSet(t, db, table)
	if _, ok := idxs[wantIdx]; ok {
		t.Errorf("DropIndexedColumn: index %q still present after drop", wantIdx)
	}

	// Table itself must still exist (we only dropped the generated column).
	if !tableExists(t, db, table) {
		t.Error("DropIndexedColumn must not drop the table")
	}
}

// TestCreateTable_WithIndexedField verifies that BuildCreateTable emits the generated
// column + index when a FieldDef has Indexed=true at create time (no separate ALTER needed).
func TestCreateTable_WithIndexedField(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_d1_create"
	const table = "obj_it_d1_create"

	_ = repo.DropTable(ctx, entity)
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	// Create with an indexed number field and an indexed text field at create-time.
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "title", FieldType: "text"},
		{Name: "price", FieldType: "number", Indexed: true},
		{Name: "status", FieldType: "text", Indexed: true},
	}); err != nil {
		t.Fatalf("CreateTable with indexed fields: %v", err)
	}

	cols := columnSet(t, db, table)
	for _, col := range []string{"price", "status"} {
		if _, ok := cols[col]; !ok {
			t.Errorf("CreateTable: indexed generated column %q not found", col)
		}
	}
	idxs := indexSet(t, db, table)
	for _, idx := range []string{"idx_obj_it_d1_create_price", "idx_obj_it_d1_create_status"} {
		if _, ok := idxs[idx]; !ok {
			t.Errorf("CreateTable: index %q not found; got: %v", idx, idxs)
		}
	}
}

// TestIndexedColumn_Backfill verifies that adding an indexed column to a table that already
// has rows lets MySQL serve the generated column from the existing JSON data. VIRTUAL
// generated columns are not materialized on disk — the value is computed on read from the
// JSON `data` column, so existing rows expose the correct value immediately (no UPDATE,
// no STORED backfill needed).
func TestIndexedColumn_Backfill(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_d1_backfill"
	const tenantID = "aaaa0000-0000-0000-0000-000000000001"
	const userID = "bbbb0000-0000-0000-0000-000000000001"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "name", FieldType: "text"},
		{Name: "age", FieldType: "number"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	// Insert two rows BEFORE adding the indexed column.
	id1, err := repo.InsertRecord(ctx, entity, tenantID, userID, map[string]any{"name": "Alice", "age": float64(30)}, nil)
	if err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}
	_, err = repo.InsertRecord(ctx, entity, tenantID, userID, map[string]any{"name": "Bob", "age": float64(25)}, nil)
	if err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}

	// Add the indexed column AFTER rows exist — MySQL should backfill automatically.
	if err := repo.AddIndexedColumn(ctx, entity, "age", "number"); err != nil {
		t.Fatalf("AddIndexedColumn after rows exist: %v", err)
	}

	// Verify the generated column is now populated for the existing row.
	var val *float64
	err = db.QueryRowContext(ctx,
		"SELECT `age` FROM `obj_it_d1_backfill` WHERE `id` = ?", id1,
	).Scan(&val)
	if err != nil {
		t.Fatalf("query generated column: %v", err)
	}
	if val == nil || *val != 30 {
		t.Errorf("backfill: expected age=30 for Alice, got %v", val)
	}
}

// TestIndexedField_SortWorks verifies that after AddIndexedColumn, ListRecords with
// ?sort=<indexedField> returns rows in the correct order (was 422 SORT_NOT_ALLOWED before D1).
func TestIndexedField_SortWorks(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_d1_sort"
	const tenantID = "cccc0000-0000-0000-0000-000000000001"
	const userID = "dddd0000-0000-0000-0000-000000000001"

	_ = repo.DropTable(ctx, entity)
	// Create with indexed number field at create-time.
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "name", FieldType: "text"},
		{Name: "score", FieldType: "number", Indexed: true},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	// Insert rows with distinct scores.
	for _, row := range []map[string]any{
		{"name": "C", "score": float64(3)},
		{"name": "A", "score": float64(1)},
		{"name": "B", "score": float64(2)},
	} {
		if _, err := repo.InsertRecord(ctx, entity, tenantID, userID, row, nil); err != nil {
			t.Fatalf("InsertRecord: %v", err)
		}
	}

	// Sort ascending by the physical generated column "score".
	// SortAllowlist maps "score" → "score" (the physical column name).
	recs, _, err := repo.ListRecords(ctx, entity, tenantID, AdminScope(), ListParams{
		Page:  1,
		Limit: 10,
		Sort:  "score",
		Order: "asc",
		SortAllowlist: map[string]string{
			"score":      "score",
			"created_at": "created_at",
			"updated_at": "updated_at",
			"id":         "id",
		},
	})
	if err != nil {
		t.Fatalf("ListRecords with sort=score: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(recs))
	}
	names := []string{}
	for _, r := range recs {
		if n, ok := r.Data["name"].(string); ok {
			names = append(names, n)
		}
	}
	// Expect A, B, C (score 1, 2, 3).
	want := []string{"A", "B", "C"}
	for i, w := range want {
		if i >= len(names) || names[i] != w {
			t.Errorf("sort order: got %v, want %v", names, want)
			break
		}
	}
}

// TestIndexedField_FilterUsesColumn verifies that filtering on an indexed field returns
// the correct subset of rows (using the physical generated column path).
func TestIndexedField_FilterUsesColumn(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_d1_filter"
	const tenantID = "eeee0000-0000-0000-0000-000000000001"
	const userID = "ffff0000-0000-0000-0000-000000000001"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "city", FieldType: "text", Indexed: true},
		{Name: "age", FieldType: "number", Indexed: true},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	for _, row := range []map[string]any{
		{"city": "Hanoi", "age": float64(30)},
		{"city": "Hanoi", "age": float64(25)},
		{"city": "Saigon", "age": float64(40)},
	} {
		if _, err := repo.InsertRecord(ctx, entity, tenantID, userID, row, nil); err != nil {
			t.Fatalf("InsertRecord: %v", err)
		}
	}

	// Filter on text indexed column (PhysicalColumn set → uses generated column, not JSON_EXTRACT).
	recs, total, err := repo.ListRecords(ctx, entity, tenantID, AdminScope(), ListParams{
		Page:  1,
		Limit: 10,
		Filters: []Filter{{
			Column:         "city",
			Operator:       FilterEq,
			Value:          "Hanoi",
			Values:         []string{"Hanoi"},
			PhysicalColumn: "city", // D1: route to physical column
		}},
	})
	if err != nil {
		t.Fatalf("ListRecords filter by indexed text: %v", err)
	}
	if total != 2 || len(recs) != 2 {
		t.Errorf("city=Hanoi: got total=%d len=%d, want 2/2", total, len(recs))
	}

	// Filter on number indexed column with gt operator (CastNumeric=true, PhysicalColumn set).
	recs, total, err = repo.ListRecords(ctx, entity, tenantID, AdminScope(), ListParams{
		Page:  1,
		Limit: 10,
		Filters: []Filter{{
			Column:         "age",
			Operator:       FilterGt,
			Value:          "26",
			Values:         []string{"26"},
			CastNumeric:    true,
			PhysicalColumn: "age",
		}},
	})
	if err != nil {
		t.Fatalf("ListRecords filter by indexed number gt: %v", err)
	}
	if total != 2 || len(recs) != 2 {
		t.Errorf("age>26: got total=%d len=%d, want 2/2 (ages 30 and 40)", total, len(recs))
	}
}

// TestIndexedField_ToggleOff verifies that after AddIndexedColumn, a subsequent
// DropIndexedColumn removes the generated column and its index while preserving all rows.
func TestIndexedField_ToggleOff(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_d1_toggle"
	const tenantID = "1111aaaa-0000-0000-0000-000000000001"
	const userID = "2222bbbb-0000-0000-0000-000000000001"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "label", FieldType: "text"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	if _, err := repo.InsertRecord(ctx, entity, tenantID, userID, map[string]any{"label": "hello"}, nil); err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}

	// Toggle ON.
	if err := repo.AddIndexedColumn(ctx, entity, "label", "text"); err != nil {
		t.Fatalf("AddIndexedColumn (toggle on): %v", err)
	}
	if _, ok := columnSet(t, db, "obj_it_d1_toggle")["label"]; !ok {
		t.Fatal("toggle on: generated column 'label' not found")
	}

	// Toggle OFF.
	if err := repo.DropIndexedColumn(ctx, entity, "label"); err != nil {
		t.Fatalf("DropIndexedColumn (toggle off): %v", err)
	}
	if _, ok := columnSet(t, db, "obj_it_d1_toggle")["label"]; ok {
		t.Error("toggle off: generated column 'label' still present")
	}
	if _, ok := indexSet(t, db, "obj_it_d1_toggle")["idx_obj_it_d1_toggle_label"]; ok {
		t.Error("toggle off: index still present")
	}

	// Data rows must survive the column drop.
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM `obj_it_d1_toggle` WHERE `tenant_id` = ?", tenantID).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 1 {
		t.Errorf("toggle off dropped data rows: want 1, got %d", count)
	}
}
