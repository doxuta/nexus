//go:build integration

package dynrepo

import (
	"context"
	"testing"

	"nexus/platform/internal/ddl"
)

// TestListRecords_FilterAndSearch proves the JSON filter/search predicates actually narrow
// the result set at the SQL layer (B-3, spec §3.2 ?field=value + ?q), stay tenant-scoped,
// and ignore soft-deleted rows.
func TestListRecords_FilterAndSearch(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_filter_people"
	const tenantA = "11111111-1111-1111-1111-1111111111aa"
	const tenantB = "22222222-2222-2222-2222-2222222222bb"
	const user = "00000000-0000-0000-0000-0000000000u1"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "name", FieldType: "text"},
		{Name: "city", FieldType: "text"},
		{Name: "age", FieldType: "number"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	mustInsert := func(tenant string, data map[string]any) string {
		id, err := repo.InsertRecord(ctx, entity, tenant, user, data, nil)
		if err != nil {
			t.Fatalf("InsertRecord: %v", err)
		}
		return id
	}
	mustInsert(tenantA, map[string]any{"name": "An", "city": "Hanoi", "age": float64(30)})
	mustInsert(tenantA, map[string]any{"name": "Anh", "city": "Hanoi", "age": float64(25)})
	mustInsert(tenantA, map[string]any{"name": "Binh", "city": "Saigon", "age": float64(40)})
	mustInsert(tenantB, map[string]any{"name": "An", "city": "Hanoi", "age": float64(30)})

	// Equality filter on a JSON field, tenant-scoped: tenant A has 2 in Hanoi.
	recs, total, err := repo.ListRecords(ctx, entity, tenantA, AdminScope(), ListParams{
		Page: 1, Limit: 20,
		Filters: []Filter{{Column: "city", Value: "Hanoi"}},
	})
	if err != nil {
		t.Fatalf("ListRecords filter: %v", err)
	}
	if total != 2 || len(recs) != 2 {
		t.Fatalf("city=Hanoi total=%d len=%d; want 2/2", total, len(recs))
	}

	// Multiple filters AND together.
	_, total, err = repo.ListRecords(ctx, entity, tenantA, AdminScope(), ListParams{
		Page: 1, Limit: 20,
		Filters: []Filter{{Column: "city", Value: "Hanoi"}, {Column: "name", Value: "An"}},
	})
	if err != nil {
		t.Fatalf("ListRecords multi-filter: %v", err)
	}
	if total != 1 {
		t.Fatalf("city=Hanoi&name=An total=%d; want 1", total)
	}

	// Substring search across string columns: "an" matches An + Anh (case-insensitive).
	_, total, err = repo.ListRecords(ctx, entity, tenantA, AdminScope(), ListParams{
		Page: 1, Limit: 20,
		Search: &Search{Term: "an", Columns: []string{"name", "city"}},
	})
	if err != nil {
		t.Fatalf("ListRecords search: %v", err)
	}
	// "an" appears in An, Anh (name) and Hanoi, Saigon? Saigon has no "an"; Hanoi has "an".
	// Rows: An(Hanoi), Anh(Hanoi), Binh(Saigon) -> An, Anh, and both Hanoi rows already
	// counted; Binh's city Saigon has no "an" -> Binh excluded. Expect 2.
	if total != 2 {
		t.Fatalf("search q=an total=%d; want 2", total)
	}

	// Filter + search combine (AND).
	_, total, err = repo.ListRecords(ctx, entity, tenantA, AdminScope(), ListParams{
		Page: 1, Limit: 20,
		Filters: []Filter{{Column: "city", Value: "Hanoi"}},
		Search:  &Search{Term: "anh", Columns: []string{"name"}},
	})
	if err != nil {
		t.Fatalf("ListRecords filter+search: %v", err)
	}
	if total != 1 {
		t.Fatalf("city=Hanoi&q=anh total=%d; want 1", total)
	}

	// Cross-tenant isolation holds with filters: tenant B sees its single Hanoi row only.
	_, total, err = repo.ListRecords(ctx, entity, tenantB, AdminScope(), ListParams{
		Page: 1, Limit: 20,
		Filters: []Filter{{Column: "city", Value: "Hanoi"}},
	})
	if err != nil {
		t.Fatalf("ListRecords tenantB: %v", err)
	}
	if total != 1 {
		t.Fatalf("tenantB city=Hanoi total=%d; want 1", total)
	}
}
