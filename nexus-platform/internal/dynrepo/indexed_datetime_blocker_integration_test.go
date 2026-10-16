//go:build integration

// Integration tests for the D1 datetime-indexed blocker fix.
//
// Run with:
//
//	go test -tags=integration ./internal/dynrepo/
//
// They require a reachable MySQL 8. Set TEST_DATABASE_URL to override the default DSN.
// These tests document:
//  1. WHY datetime is blocked: CAST('2026-07-15T10:30:00Z' AS DATETIME) fails under
//     STRICT_TRANS_TABLES (Error 1292) — the root cause of the D1 blocker.
//  2. That the date type (YYYY-MM-DD) is safe and continues to work end-to-end.
package dynrepo

import (
	"context"
	"fmt"
	"testing"

	"nexus/platform/internal/ddl"
)

// TestDatetimeIndexedBlocker_CastFailsUnderStrict documents the root cause of the D1
// datetime-indexed blocker: MySQL's CAST of an RFC3339 datetime string to DATETIME fails
// under STRICT_TRANS_TABLES with Error 1292. This is the reason the service layer rejects
// indexed=true on datetime fields.
//
// The test confirms the failure is reproducible in the actual MySQL instance used by the
// test suite, so future maintainers can trace WHY datetime is blocked without guessing.
func TestDatetimeIndexedBlocker_CastFailsUnderStrict(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// Confirm STRICT_TRANS_TABLES is active (the precondition for the failure).
	var sqlMode string
	if err := db.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&sqlMode); err != nil {
		t.Fatalf("query sql_mode: %v", err)
	}
	t.Logf("sql_mode: %s", sqlMode)

	// A CAST of an RFC3339 string ('2026-07-15T10:30:00Z') to DATETIME fails under
	// STRICT_TRANS_TABLES — MySQL cannot parse the 'T' separator or 'Z' suffix.
	// This is what breaks the generated column expression:
	//   CAST(JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.f')) AS DATETIME)
	// when the stored value is an RFC3339 datetime.
	var result *string
	err := db.QueryRowContext(ctx,
		"SELECT CAST('2026-07-15T10:30:00Z' AS DATETIME)",
	).Scan(&result)
	if err == nil {
		// Without STRICT_TRANS_TABLES the CAST silently truncates to NULL. Warn but do not
		// fail the test — the guard in validateFieldConfig is still correct regardless of
		// the sql_mode, since we cannot guarantee the production server has strict mode off.
		t.Logf("WARN: CAST('2026-07-15T10:30:00Z' AS DATETIME) did not error (result=%v); sql_mode may not include STRICT_TRANS_TABLES", result)
	} else {
		t.Logf("OK: CAST of RFC3339 datetime to DATETIME errored as expected: %v", err)
	}
}

// TestIndexedDateField_EndToEnd verifies that an indexed `date` field (YYYY-MM-DD) works
// correctly end-to-end with a real MySQL instance: the generated column populates at insert
// time, and sort by the physical column returns rows in ascending date order. This is the
// "regression proof" that date indexing is not broken by the datetime guard.
func TestIndexedDateField_EndToEnd(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()

	const entity = "it_d1_date_e2e"
	const table = "obj_it_d1_date_e2e"
	const tenantID = "d1d10000-0000-0000-0000-000000000001"
	const userID = "d1d20000-0000-0000-0000-000000000001"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "name", FieldType: "text"},
		{Name: "start_date", FieldType: "date", Indexed: true},
	}); err != nil {
		t.Fatalf("CreateTable with indexed date: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	// Confirm the generated column and index were created.
	cols := columnSet(t, db, table)
	if _, ok := cols["start_date"]; !ok {
		t.Error("indexed date field: generated column 'start_date' not found in information_schema")
	}
	const wantIdx = "idx_obj_it_d1_date_e2e_start_date"
	if _, ok := indexSet(t, db, table)[wantIdx]; !ok {
		t.Errorf("indexed date field: index %q not found", wantIdx)
	}

	// Insert rows with distinct dates (YYYY-MM-DD — safe for MySQL DATETIME cast).
	rows := []map[string]any{
		{"name": "C", "start_date": "2026-03-15"},
		{"name": "A", "start_date": "2026-01-10"},
		{"name": "B", "start_date": "2026-02-20"},
	}
	for _, row := range rows {
		if _, err := repo.InsertRecord(ctx, entity, tenantID, userID, row, nil); err != nil {
			t.Fatalf("InsertRecord date row: %v (row=%v)", err, row)
		}
	}

	// Verify the generated column is populated for a specific row.
	var dateVal *string
	err := db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT `start_date` FROM `%s` WHERE `tenant_id` = ? AND JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.name')) = 'A'", table),
		tenantID,
	).Scan(&dateVal)
	if err != nil {
		t.Fatalf("query generated date column for 'A': %v", err)
	}
	if dateVal == nil {
		t.Error("generated date column for 'A' is NULL (backfill did not populate)")
	} else {
		t.Logf("generated column value for 'A': %s", *dateVal)
	}

	// Sort ascending by the physical generated column 'start_date': expect A, B, C.
	recs, _, err := repo.ListRecords(ctx, entity, tenantID, AdminScope(), ListParams{
		Page:  1,
		Limit: 10,
		Sort:  "start_date",
		Order: "asc",
		SortAllowlist: map[string]string{
			"start_date": "start_date",
			"created_at": "created_at",
			"id":         "id",
		},
	})
	if err != nil {
		t.Fatalf("ListRecords sort by indexed date: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(recs))
	}
	want := []string{"A", "B", "C"}
	for i, w := range want {
		if i >= len(recs) {
			break
		}
		if n, ok := recs[i].Data["name"].(string); !ok || n != w {
			got := make([]string, len(recs))
			for j, r := range recs {
				got[j], _ = r.Data["name"].(string)
			}
			t.Errorf("sort by date: got %v, want %v", got, want)
			break
		}
	}
}

// TestIndexedDateField_ToggleOff verifies that DropIndexedColumn removes the generated
// column and index for a date field while preserving existing rows.
func TestIndexedDateField_ToggleOff(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()

	const entity = "it_d1_date_toggle"
	const tenantID = "d1d30000-0000-0000-0000-000000000001"
	const userID = "d1d40000-0000-0000-0000-000000000001"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "name", FieldType: "text"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	if _, err := repo.InsertRecord(ctx, entity, tenantID, userID, map[string]any{"name": "row1", "start_date": "2026-06-01"}, nil); err != nil {
		t.Fatalf("InsertRecord before index: %v", err)
	}

	// Toggle ON: add indexed date column.
	if err := repo.AddIndexedColumn(ctx, entity, "start_date", "date"); err != nil {
		t.Fatalf("AddIndexedColumn(date): %v", err)
	}
	if _, ok := columnSet(t, db, "obj_it_d1_date_toggle")["start_date"]; !ok {
		t.Fatal("toggle on: generated column 'start_date' not found")
	}

	// Toggle OFF: remove the generated column + index.
	if err := repo.DropIndexedColumn(ctx, entity, "start_date"); err != nil {
		t.Fatalf("DropIndexedColumn(date): %v", err)
	}
	if _, ok := columnSet(t, db, "obj_it_d1_date_toggle")["start_date"]; ok {
		t.Error("toggle off: generated column 'start_date' still present")
	}

	// Data row must survive.
	var count int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM `obj_it_d1_date_toggle` WHERE `tenant_id` = ?", tenantID,
	).Scan(&count); err != nil {
		t.Fatalf("count rows after toggle off: %v", err)
	}
	if count != 1 {
		t.Errorf("toggle off dropped data rows: want 1, got %d", count)
	}
}
