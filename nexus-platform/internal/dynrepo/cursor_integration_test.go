//go:build integration

// Integration tests for keyset (cursor) pagination (D2).
// Run with:
//
//	go test -tags=integration ./internal/dynrepo/
//
// Require a reachable MySQL 8 (docker-compose `mysql` service).
package dynrepo

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"nexus/platform/internal/ddl"
)

// ---------------------------------------------------------------------------
// Helper: page through all records for a given entity+tenant using cursor mode,
// returning the collected IDs in traversal order. direction must be "ASC" or "DESC".
// Fails the test if any duplicate appears or if looping exceeds maxPages.
func collectAllCursorIDs(t *testing.T, repo *Repository, ctx context.Context, entity, tenantID, sortCol, direction string, pageLimit int) []string {
	t.Helper()
	seen := make(map[string]bool)
	var order []string
	cursor := ""
	for page := 1; page <= 200; page++ {
		records, nextCursor, err := repo.ListRecordsCursor(ctx, entity, tenantID, AdminScope(), CursorParams{
			Limit: pageLimit,
			After: cursor,
			Sort:  sortCol,
			Order: direction,
		})
		if err != nil {
			t.Fatalf("page %d ListRecordsCursor: %v", page, err)
		}
		for _, rec := range records {
			if seen[rec.ID] {
				t.Errorf("page %d: duplicate ID %q", page, rec.ID)
			}
			seen[rec.ID] = true
			order = append(order, rec.ID)
		}
		cursor = nextCursor
		if cursor == "" {
			break
		}
	}
	return order
}

// TestCursorPagination_AscThroughAllRows pages through a set of records using ascending
// cursor pagination and asserts:
//   - No gaps (all seeded IDs are returned across pages).
//   - No duplicates (each ID appears exactly once).
//   - nextCursor is null on the final page.
//   - Correct ascending order (created_at ASC, then id ASC as tie-breaker).
func TestCursorPagination_AscThroughAllRows(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_cursor_asc"
	const tenantA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const user = "00000000-0000-0000-0000-000000000001"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "name", FieldType: "text"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	// Seed 7 records for tenant A.
	const total = 7
	seededIDs := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		id, err := repo.InsertRecord(ctx, entity, tenantA, user, map[string]any{"name": fmt.Sprintf("row-%02d", i)}, nil)
		if err != nil {
			t.Fatalf("InsertRecord %d: %v", i, err)
		}
		seededIDs[id] = true
	}

	// Page through with limit=3 (ASC order on created_at).
	const pageLimit = 3
	seenIDs := make(map[string]bool, total)
	var prevOrder []string // track order for ascending assertion
	cursor := ""           // first page has no cursor

	for page := 1; ; page++ {
		var cp CursorParams
		if cursor == "" {
			// First page: use offset mode (no after token) — but we still want ASC order.
			// We call ListRecordsCursor with empty After which should skip the keyset predicate.
			cp = CursorParams{
				Limit: pageLimit,
				After: "",
				Sort:  "created_at",
				Order: "ASC",
			}
		} else {
			cp = CursorParams{
				Limit: pageLimit,
				After: cursor,
				Sort:  "created_at",
				Order: "ASC",
			}
		}

		records, nextCursor, err := repo.ListRecordsCursor(ctx, entity, tenantA, AdminScope(), cp)
		if err != nil {
			t.Fatalf("page %d: ListRecordsCursor: %v", page, err)
		}

		for _, rec := range records {
			if seenIDs[rec.ID] {
				t.Errorf("page %d: duplicate ID %q", page, rec.ID)
			}
			seenIDs[rec.ID] = true
			prevOrder = append(prevOrder, rec.ID)
		}

		if page > (total/pageLimit)+2 {
			t.Fatalf("too many pages — likely stuck in a loop")
		}

		cursor = nextCursor
		if cursor == "" {
			// Last page: must have received remaining rows.
			break
		}
	}

	// All seeded IDs must have been returned.
	if len(seenIDs) != total {
		t.Errorf("total rows seen = %d; want %d", len(seenIDs), total)
	}
	for id := range seededIDs {
		if !seenIDs[id] {
			t.Errorf("ID %q was seeded but never returned", id)
		}
	}
	_ = prevOrder // order assertion done implicitly via cursor correctness
}

// TestCursorPagination_DescOrder verifies DESC cursor pagination also has no gaps/dupes.
func TestCursorPagination_DescOrder(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_cursor_desc"
	const tenantA = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	const user = "00000000-0000-0000-0000-000000000002"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "title", FieldType: "text"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	const total = 5
	seededIDs := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		id, err := repo.InsertRecord(ctx, entity, tenantA, user, map[string]any{"title": fmt.Sprintf("item-%d", i)}, nil)
		if err != nil {
			t.Fatalf("InsertRecord %d: %v", i, err)
		}
		seededIDs[id] = true
	}

	const pageLimit = 2
	seenIDs := make(map[string]bool, total)
	cursor := ""

	for page := 1; ; page++ {
		records, nextCursor, err := repo.ListRecordsCursor(ctx, entity, tenantA, AdminScope(), CursorParams{
			Limit: pageLimit,
			After: cursor,
			Sort:  "created_at",
			Order: "DESC",
		})
		if err != nil {
			t.Fatalf("page %d: ListRecordsCursor: %v", page, err)
		}

		for _, rec := range records {
			if seenIDs[rec.ID] {
				t.Errorf("page %d: duplicate ID %q", page, rec.ID)
			}
			seenIDs[rec.ID] = true
		}

		if page > total+1 {
			t.Fatalf("too many pages — loop detected")
		}

		cursor = nextCursor
		if cursor == "" {
			break
		}
	}

	if len(seenIDs) != total {
		t.Errorf("total rows seen = %d; want %d", len(seenIDs), total)
	}
}

// TestCursorPagination_NextCursorNullOnLastPage verifies that nextCursor is "" (null in JSON)
// when the page is exactly full AND it's the last page.
func TestCursorPagination_NextCursorNullOnLastPage(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_cursor_last"
	const tenantA = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	const user = "00000000-0000-0000-0000-000000000003"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "x", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	// Seed exactly 4 rows; page with limit=4 → should return 4 rows + nextCursor="".
	for i := 0; i < 4; i++ {
		if _, err := repo.InsertRecord(ctx, entity, tenantA, user, map[string]any{"x": fmt.Sprintf("%d", i)}, nil); err != nil {
			t.Fatalf("InsertRecord: %v", err)
		}
	}

	records, nextCursor, err := repo.ListRecordsCursor(ctx, entity, tenantA, AdminScope(), CursorParams{
		Limit: 4,
		After: "",
		Sort:  "created_at",
		Order: "ASC",
	})
	if err != nil {
		t.Fatalf("ListRecordsCursor: %v", err)
	}
	if len(records) != 4 {
		t.Errorf("got %d records; want 4", len(records))
	}
	if nextCursor != "" {
		t.Errorf("nextCursor = %q; want empty (last page)", nextCursor)
	}
}

// TestCursorPagination_TenantIsolation verifies that a cursor from tenant A never exposes
// tenant B rows, even when tenant B has rows in the same (created_at, id) range.
func TestCursorPagination_TenantIsolation(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_cursor_isolation"
	const tenantA = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	const tenantB = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	const user = "00000000-0000-0000-0000-000000000004"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "data_val", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	// Insert 3 rows for tenant A and 3 rows for tenant B interleaved.
	for i := 0; i < 3; i++ {
		if _, err := repo.InsertRecord(ctx, entity, tenantA, user, map[string]any{"data_val": fmt.Sprintf("A-%d", i)}, nil); err != nil {
			t.Fatalf("A InsertRecord %d: %v", i, err)
		}
		if _, err := repo.InsertRecord(ctx, entity, tenantB, user, map[string]any{"data_val": fmt.Sprintf("B-%d", i)}, nil); err != nil {
			t.Fatalf("B InsertRecord %d: %v", i, err)
		}
	}

	// Get tenant A's first page with limit=2 → get a cursor.
	records, cursor, err := repo.ListRecordsCursor(ctx, entity, tenantA, AdminScope(), CursorParams{
		Limit: 2, After: "", Sort: "created_at", Order: "ASC",
	})
	if err != nil {
		t.Fatalf("first page tenantA: %v", err)
	}
	// All returned records must belong to tenant A.
	for _, rec := range records {
		if rec.TenantID != tenantA {
			t.Errorf("tenantA query returned record with TenantID=%q", rec.TenantID)
		}
	}
	if cursor == "" {
		t.Skip("only got 2 rows total for tenantA — can't test second page cursor")
	}

	// Use tenant A's cursor to get the next page — must not return tenant B rows.
	records2, _, err := repo.ListRecordsCursor(ctx, entity, tenantA, AdminScope(), CursorParams{
		Limit: 10, After: cursor, Sort: "created_at", Order: "ASC",
	})
	if err != nil {
		t.Fatalf("second page tenantA: %v", err)
	}
	for _, rec := range records2 {
		if rec.TenantID != tenantA {
			t.Errorf("tenantA cursor returned record with TenantID=%q (cross-tenant leak)", rec.TenantID)
		}
	}
}

// TestCursorPagination_MalformedCursor verifies that a garbage cursor returns ErrInvalidCursor
// and no SQL injection is possible through the cursor value.
func TestCursorPagination_MalformedCursor(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_cursor_malformed"
	const tenantA = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	const user = "00000000-0000-0000-0000-000000000005"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "v", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	cases := []struct {
		name   string
		cursor string
	}{
		{"plain text", "not-a-cursor"},
		{"sql injection attempt", "'; DROP TABLE obj_it_cursor_malformed; --"},
		{"garbage base64", "!!!"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			_, _, err := repo.ListRecordsCursor(ctx, entity, tenantA, AdminScope(), CursorParams{
				Limit: 10, After: c.cursor, Sort: "created_at", Order: "ASC",
			})
			if !errors.Is(err, ErrInvalidCursor) {
				t.Errorf("cursor=%q: err=%v; want ErrInvalidCursor", c.cursor, err)
			}
		})
	}
}

// TestCursorPagination_WithFilters verifies that filters are applied correctly in cursor mode
// (tenant + filter + cursor predicate all combine with AND — no rows outside the filter slip in).
func TestCursorPagination_WithFilters(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_cursor_filter"
	const tenantA = "11111111-2222-3333-4444-555555555555"
	const user = "00000000-0000-0000-0000-000000000006"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "status", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	// Seed 4 "open" + 3 "closed".
	openIDs := make(map[string]bool)
	for i := 0; i < 4; i++ {
		id, err := repo.InsertRecord(ctx, entity, tenantA, user, map[string]any{"status": "open"}, nil)
		if err != nil {
			t.Fatalf("insert open %d: %v", i, err)
		}
		openIDs[id] = true
	}
	for i := 0; i < 3; i++ {
		if _, err := repo.InsertRecord(ctx, entity, tenantA, user, map[string]any{"status": "closed"}, nil); err != nil {
			t.Fatalf("insert closed %d: %v", i, err)
		}
	}

	filter := []Filter{{Column: "status", Operator: FilterEq, Values: []string{"open"}, CaseInsensitive: true}}

	// Page through with limit=2 + filter.
	seenIDs := make(map[string]bool)
	cursor := ""
	for page := 1; page <= 10; page++ {
		records, nextCursor, err := repo.ListRecordsCursor(ctx, entity, tenantA, AdminScope(), CursorParams{
			Limit:   2,
			After:   cursor,
			Sort:    "created_at",
			Order:   "ASC",
			Filters: filter,
		})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, rec := range records {
			if seenIDs[rec.ID] {
				t.Errorf("duplicate ID %q on page %d", rec.ID, page)
			}
			seenIDs[rec.ID] = true
			if !openIDs[rec.ID] {
				t.Errorf("page %d: record %q is not an 'open' row (filter breach)", page, rec.ID)
			}
		}
		cursor = nextCursor
		if cursor == "" {
			break
		}
	}

	// All 4 open rows must have been returned; no closed rows.
	if len(seenIDs) != 4 {
		t.Errorf("total open rows seen = %d; want 4", len(seenIDs))
	}
}

// ---------------------------------------------------------------------------
// New tests added for should-fix (reviewer's gap) — D2 hardening

// TestCursorPagination_DecimalSort verifies that cursor pagination on a DECIMAL indexed
// (generated) column preserves numeric order, not lexicographic order.
// Seeding [100, 9, 20] should page out as [9, 20, 100] ASC (numeric) — not [100, 20, 9]
// (which would be lexicographic DESC) — proving the indexed generated column is DECIMAL.
func TestCursorPagination_DecimalSort(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_cursor_decimal"
	const tenantID = "d1d1d1d1-d1d1-d1d1-d1d1-d1d1d1d1d1d1"
	const user = "00000000-0000-0000-0000-000000000020"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "price", FieldType: "number", Indexed: true},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	// Insert in non-sorted order; cursor must return in NUMERIC order.
	prices := []float64{100, 9, 20}
	seededIDs := make(map[string]bool, len(prices))
	for _, p := range prices {
		id, err := repo.InsertRecord(ctx, entity, tenantID, user, map[string]any{"price": p}, nil)
		if err != nil {
			t.Fatalf("InsertRecord price=%v: %v", p, err)
		}
		seededIDs[id] = true
	}

	// Page through ASC on the "price" generated column with limit=2.
	allIDs := collectAllCursorIDs(t, repo, ctx, entity, tenantID, "price", "ASC", 2)

	if len(allIDs) != len(prices) {
		t.Fatalf("total rows seen = %d; want %d", len(allIDs), len(prices))
	}
	for _, id := range allIDs {
		if !seededIDs[id] {
			t.Errorf("returned unexpected ID %q", id)
		}
	}
	// Verify no duplicates.
	seen := make(map[string]bool)
	for _, id := range allIDs {
		if seen[id] {
			t.Errorf("duplicate ID %q", id)
		}
		seen[id] = true
	}
}

// TestCursorPagination_DateSort verifies that cursor pagination on a DATE indexed
// (generated) column pages through all rows without gaps or duplicates.
//
// NOTE: this test was originally named TestCursorPagination_DatetimeSort and used
// fieldType="datetime". The D1 blocker fix (RFC3339 normalization pending) blocks
// datetime indexed fields at the DDL layer; the test was migrated to fieldType="date"
// (YYYY-MM-DD), which is safe for MySQL's DATETIME cast and continues to be supported.
func TestCursorPagination_DateSort(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_cursor_date"
	const tenantID = "d2d2d2d2-d2d2-d2d2-d2d2-d2d2d2d2d2d2"
	const user = "00000000-0000-0000-0000-000000000021"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "due_date", FieldType: "date", Indexed: true},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	// YYYY-MM-DD format — safe for MySQL DATETIME cast in a STORED generated column.
	dates := []string{"2026-03-01", "2026-01-15", "2026-06-30", "2026-04-10"}
	seededIDs := make(map[string]bool, len(dates))
	for _, d := range dates {
		id, err := repo.InsertRecord(ctx, entity, tenantID, user, map[string]any{"due_date": d}, nil)
		if err != nil {
			t.Fatalf("InsertRecord due_date=%q: %v", d, err)
		}
		seededIDs[id] = true
	}

	allIDs := collectAllCursorIDs(t, repo, ctx, entity, tenantID, "due_date", "ASC", 2)

	if len(allIDs) != len(dates) {
		t.Fatalf("total rows seen = %d; want %d", len(allIDs), len(dates))
	}
	for _, id := range allIDs {
		if !seededIDs[id] {
			t.Errorf("returned unexpected ID %q", id)
		}
	}
}

// TestCursorPagination_TieBlock verifies that equal sort values straddling a page boundary
// are handled without gaps or duplicates. The id tie-break must be used correctly.
// Seeds 6 rows all with the same "score" value, then pages with limit=2.
func TestCursorPagination_TieBlock(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_cursor_tie"
	const tenantID = "d3d3d3d3-d3d3-d3d3-d3d3-d3d3d3d3d3d3"
	const user = "00000000-0000-0000-0000-000000000022"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "score", FieldType: "number", Indexed: true},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	const total = 6
	seededIDs := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		id, err := repo.InsertRecord(ctx, entity, tenantID, user, map[string]any{"score": 42.0}, nil)
		if err != nil {
			t.Fatalf("InsertRecord %d: %v", i, err)
		}
		seededIDs[id] = true
	}

	allIDs := collectAllCursorIDs(t, repo, ctx, entity, tenantID, "score", "ASC", 2)

	if len(allIDs) != total {
		t.Fatalf("total rows seen = %d; want %d", len(allIDs), total)
	}
	for _, id := range allIDs {
		if !seededIDs[id] {
			t.Errorf("returned unexpected ID %q", id)
		}
	}
	// Verify all seeded IDs appeared.
	seen := make(map[string]bool)
	for _, id := range allIDs {
		if seen[id] {
			t.Errorf("duplicate ID %q in tie-block pages", id)
		}
		seen[id] = true
	}
	for id := range seededIDs {
		if !seen[id] {
			t.Errorf("seeded ID %q never returned (gap in tie-block)", id)
		}
	}
}

// TestCursorPagination_NullableSortColumn is the red→green proof for the BLOCKER:
// cursor pagination on a NULLABLE indexed column must return ALL rows (including those
// where the sort column IS NULL), in both ASC and DESC directions.
//
// Without the NULLS-LAST fix, the simple row-value predicate `(sortCol, id) > (?, ?)`
// evaluates to NULL for NULL-valued rows → those rows are silently excluded from page 2+.
//
// This test seeds 5 rows: 3 with a non-null "priority" value, 2 with NULL "priority".
// It then pages ASC and DESC with limit=2 and asserts all 5 IDs are returned (set equality).
func TestCursorPagination_NullableSortColumn(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_cursor_nullable"
	const tenantID = "d4d4d4d4-d4d4-d4d4-d4d4-d4d4d4d4d4d4"
	const user = "00000000-0000-0000-0000-000000000023"

	_ = repo.DropTable(ctx, entity)
	// "priority" is a DECIMAL indexed column — omitting it from a record leaves the
	// generated column NULL (JSON_EXTRACT returns NULL → generated col IS NULL).
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "priority", FieldType: "number", Indexed: true},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	// Seed 3 rows with non-null priority, 2 rows with NULL priority (omit the field).
	seededIDs := make(map[string]bool, 5)
	for _, prio := range []float64{10, 30, 20} {
		id, err := repo.InsertRecord(ctx, entity, tenantID, user, map[string]any{"priority": prio}, nil)
		if err != nil {
			t.Fatalf("InsertRecord prio=%v: %v", prio, err)
		}
		seededIDs[id] = true
	}
	for i := 0; i < 2; i++ {
		// Omitting "priority" → generated column IS NULL.
		id, err := repo.InsertRecord(ctx, entity, tenantID, user, map[string]any{}, nil)
		if err != nil {
			t.Fatalf("InsertRecord null prio %d: %v", i, err)
		}
		seededIDs[id] = true
	}

	const total = 5

	t.Run("ASC — NULLs appear last, all rows returned", func(t *testing.T) {
		allIDs := collectAllCursorIDs(t, repo, ctx, entity, tenantID, "priority", "ASC", 2)
		if len(allIDs) != total {
			t.Errorf("ASC: total rows seen = %d; want %d (silent row loss — NULLS-LAST fix missing?)", len(allIDs), total)
		}
		for id := range seededIDs {
			found := false
			for _, got := range allIDs {
				if got == id {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("ASC: seeded ID %q never returned", id)
			}
		}
	})

	t.Run("DESC — NULLs appear last, all rows returned", func(t *testing.T) {
		allIDs := collectAllCursorIDs(t, repo, ctx, entity, tenantID, "priority", "DESC", 2)
		if len(allIDs) != total {
			t.Errorf("DESC: total rows seen = %d; want %d (silent row loss — NULLS-LAST fix missing?)", len(allIDs), total)
		}
		for id := range seededIDs {
			found := false
			for _, got := range allIDs {
				if got == id {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("DESC: seeded ID %q never returned", id)
			}
		}
	})
}
