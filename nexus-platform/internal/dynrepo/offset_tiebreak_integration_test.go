//go:build integration

package dynrepo

import (
	"context"
	"fmt"
	"testing"

	"nexus/platform/internal/ddl"
)

// Offset paging must produce a TOTAL order (MỘT XƯỞNG, 2026-07-30).
//
// `ORDER BY <non-unique column>` leaves tied rows in an undefined order, and OFFSET paging re-runs
// the query for every page — so two pages can return the same row twice while skipping another.
// `created_at` is a second-resolution TIMESTAMP and a seed or an import writes many rows inside one
// second: on the demo tenant, 10 of 14 leads share a single `created_at`.
//
// Honest about what was and was not observed: repeated 3-page reads on that live table came back
// clean, because MySQL's plan happens to be stable at that size. This test does not rely on luck —
// it forces the tie (every row shares one created_at) and asserts the ordering CONTRACT, which is
// what the fix actually supplies. The cursor path has always tie-broken on `id`; the offset path
// did not, so the two paging modes disagreed about whether row order was even defined.
func TestOffsetPaging_TotalOrderWhenSortValuesTie(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_offset_tie"
	const tenantA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const user = "00000000-0000-0000-0000-000000000001"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "name", FieldType: "text"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	const total = 9
	seeded := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		id, err := repo.InsertRecord(ctx, entity, tenantA, user, map[string]any{"name": fmt.Sprintf("row-%02d", i)}, nil)
		if err != nil {
			t.Fatalf("InsertRecord %d: %v", i, err)
		}
		seeded[id] = true
	}

	// Collapse every created_at onto ONE value: this is the situation a seed/import creates, and
	// the situation in which an untie-broken ORDER BY has no defined answer.
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf("UPDATE `obj_%s` SET `created_at` = '2026-07-12 16:22:38' WHERE `tenant_id` = ?", entity),
		tenantA); err != nil {
		t.Fatalf("flatten created_at: %v", err)
	}

	// Both directions: the tie-break must follow `order`, not be hard-coded.
	for _, order := range []string{"desc", "asc"} {
		t.Run(order, func(t *testing.T) {
			const limit = 4
			seen := make(map[string]int, total)
			var pageOrder []string
			for page := 1; page <= 3; page++ {
				recs, gotTotal, err := repo.ListRecords(ctx, entity, tenantA, AdminScope(), ListParams{
					Page: page, Limit: limit, Sort: "created_at", Order: order,
				})
				if err != nil {
					t.Fatalf("page %d: %v", page, err)
				}
				if gotTotal != total {
					t.Fatalf("page %d total = %d; want %d", page, gotTotal, total)
				}
				for _, rec := range recs {
					seen[rec.ID]++
					pageOrder = append(pageOrder, rec.ID)
				}
			}

			// No row twice, no row missing — the property OFFSET paging silently lacked.
			for id, n := range seen {
				if n != 1 {
					t.Errorf("record %s returned %d times across pages; want exactly 1 (rows tie on created_at ⇒ order must be total)", id, n)
				}
			}
			if len(seen) != total {
				t.Errorf("saw %d distinct records across 3 pages; want all %d (a repeat means another row was skipped)", len(seen), total)
			}

			// And the order is the id order, since every sort value is identical.
			for i := 1; i < len(pageOrder); i++ {
				prev, cur := pageOrder[i-1], pageOrder[i]
				if order == "desc" && prev < cur {
					t.Errorf("desc: %s came before %s", prev, cur)
				}
				if order == "asc" && prev > cur {
					t.Errorf("asc: %s came before %s", prev, cur)
				}
			}
		})
	}
}
