//go:build integration

// Integration test for Wave-5 W5-D D3: keyset (cursor) pagination on a collation-insensitive
// (accent/case-folding) non-unique sort column — text/email/picklist (utf8mb4_unicode_ci) or
// relation (CHAR(36) default collation) — must NOT drop or duplicate rows across page boundaries.
//
// Background (red-team finding F1): under an accent/case-folding collation MANY distinct byte
// values compare EQUAL ("Café" == "café" == "cafe" == "CAFE"). The concern was that a keyset
// cursor whose tie-break runs under that same folding collation is value-NON-INJECTIVE and could
// skip collation-equal rows whose id sits before the page boundary.
//
// RECONCILED (2026-06-25): this is NOT a live bug. The keyset cursor is loss-free on a folding
// sort column WITHOUT any COLLATE override, because ListRecordsCursor's ORDER BY `(col, id)` and
// its keyset predicate `(col > ?) OR (col = ? AND id > ?)` both compare `col` under the SAME
// default collation, and the unique `id` tie-break FOLLOWS the sort direction — so `(col, id)` is
// a STRICT TOTAL ORDER and `id` absorbs every collation-tie regardless of col's non-injectivity.
// The red-team "row-loss" repro was hand-SQL whose ORDER BY omitted that `id` term (a model of the
// code, not the code). A `COLLATE utf8mb4_bin` override was rejected: it would change the visible
// sort to codepoint order (wrong for Vietnamese) and make cursor-mode disagree with offset-mode
// (which is ci). This test therefore pins the real invariant — every row appears exactly once,
// ASC and DESC, under the NATURAL ci collation — so a future refactor that breaks the
// predicate==ORDER-BY-collation or drops the id tie-break fails loudly.
//
// Run with:
//
//	go test -tags=integration -race ./internal/dynrepo/ -run CursorCollat
package dynrepo

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	"nexus/platform/internal/ddl"
)

// pageAllCursorCollated pages through every record via cursor mode and returns the distinct-id →
// count map. SortCollatedText mirrors what the service sets for a collation-folding sort column.
func pageAllCursorCollated(t *testing.T, repo *Repository, ctx context.Context, entity, tenant, sortCol, dir string, limit int) map[string]int {
	t.Helper()
	seen := make(map[string]int)
	after := ""
	for page := 1; page <= 500; page++ {
		records, next, err := repo.ListRecordsCursor(ctx, entity, tenant, AdminScope(), CursorParams{
			Limit: limit,
			After: after,
			Sort:  sortCol,
			Order: dir,
		})
		if err != nil {
			t.Fatalf("page %d ListRecordsCursor(dir=%s): %v", page, dir, err)
		}
		for _, rec := range records {
			seen[rec.ID]++
		}
		after = next
		if after == "" {
			break
		}
	}
	return seen
}

// seedCollationRow inserts a single record with an EXPLICIT id (so the test controls how ids
// interleave with collation-fold order) directly into the obj_* table. The `city` generated
// column derives from data->>'$.city' automatically.
func seedCollationRow(t *testing.T, repo *Repository, ctx context.Context, entity, tenant, id, city string) {
	t.Helper()
	table := "obj_" + entity
	payload, err := json.Marshal(map[string]any{"city": city})
	if err != nil {
		t.Fatalf("marshal city=%q: %v", city, err)
	}
	stmt := fmt.Sprintf("INSERT INTO `%s` (`id`, `tenant_id`, `created_by`, `data`) VALUES (?, ?, ?, ?)", table)
	if _, err := repo.db.ExecContext(ctx, stmt, id, tenant, "", payload); err != nil {
		t.Fatalf("seed id=%s city=%q: %v", id, city, err)
	}
}

// TestCursorCollation_NoRowLossOnCollatedText seeds rows whose indexed text values fold together
// under utf8mb4_unicode_ci ("Café"/"café"/"cafe"/"CAFE") with EXPLICIT ids arranged so the folded
// group's order runs OPPOSITE to id order — the adversarial case for a keyset tie-break. It pages
// with a small limit in BOTH directions and asserts every seeded row appears exactly once (no gap,
// no dup) under the NATURAL ci collation — the `(col, id)` total order is injective regardless of
// how the values fold or how ids interleave, with no COLLATE override needed.
func TestCursorCollation_NoRowLossOnCollatedText(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_d3_collate"
	const tenant = "d3000000-0000-0000-0000-000000000001"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "city", FieldType: "text", Indexed: true}, // utf8mb4_unicode_ci VARCHAR gen column
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	// Folded group: all four compare EQUAL under utf8mb4_unicode_ci. Their EXPLICIT ids are
	// assigned so the byte-distinct values do NOT line up with ascending id order — the worst
	// case for a boundary tie-break. Two distinct anchors bracket the group, and a couple of
	// extra folded variants stress the byte-exact ordering further.
	type row struct{ id, city string }
	rows := []row{
		{"00000000-0000-0000-0000-000000000001", "alpha"}, // distinct, sorts first
		{"00000000-0000-0000-0000-000000000050", "Café"},  // folded group (ids interleaved out of byte order)
		{"00000000-0000-0000-0000-000000000020", "café"},
		{"00000000-0000-0000-0000-000000000090", "cafe"},
		{"00000000-0000-0000-0000-000000000010", "CAFE"},
		{"00000000-0000-0000-0000-000000000070", "CAFÉ"},
		{"00000000-0000-0000-0000-000000000099", "zebra"}, // distinct, sorts last
	}
	seeded := make(map[string]bool, len(rows))
	for _, r := range rows {
		seedCollationRow(t, repo, ctx, entity, tenant, r.id, r.city)
		seeded[r.id] = true
	}

	for _, dir := range []string{"ASC", "DESC"} {
		for _, limit := range []int{1, 2, 3, 4, 5} {
			t.Run(fmt.Sprintf("collated dir=%s limit=%d", dir, limit), func(t *testing.T) {
				seen := pageAllCursorCollated(t, repo, ctx, entity, tenant, "city", dir, limit)
				if len(seen) != len(seeded) {
					missing := make([]string, 0)
					for id := range seeded {
						if seen[id] == 0 {
							missing = append(missing, id)
						}
					}
					sort.Strings(missing)
					t.Fatalf("dir=%s limit=%d: cursor returned %d distinct rows; want %d (DROPPED %d: %v)",
						dir, limit, len(seen), len(seeded), len(missing), missing)
				}
				for id, c := range seen {
					if c != 1 {
						t.Fatalf("dir=%s limit=%d: row %q appeared %d times; want exactly 1", dir, limit, id, c)
					}
				}
			})
		}
	}
}
