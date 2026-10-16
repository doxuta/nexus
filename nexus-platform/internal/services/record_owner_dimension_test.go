package services

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
)

// OWNER-DIMENSION (backlog "ai phụ trách biến khỏi MỌI list") — owner_id is a physical column on
// every obj_* table, readable end-to-end (serialized as ownerId on every row) and writable by
// "Đổi chủ", yet it was neither SORTABLE (buildSortAllowlist seeded only created_at/updated_at/id
// plus sys_fields-driven entries → 422 SORT_NOT_ALLOWED) nor FILTERABLE (filters are validated
// against sys_fields metadata and owner_id is never registered there → 422 LIST_UNKNOWN_FILTER).
// These tests pin BOTH gates open for the owner, and pin them shut for everything else.

const ownerUUID = "b1551a1a-5b97-4640-91b5-f49d13bb118f"

// Sorting by owner is accepted in BOTH wire spellings and resolves to the physical, indexed
// owner_id column. Before the change every case here returned 422 SORT_NOT_ALLOWED.
func TestList_SortByOwner_Accepted(t *testing.T) {
	for _, sortKey := range []string{"ownerId", "owner_id"} {
		t.Run(sortKey, func(t *testing.T) {
			svc, store := newListService()
			_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
				Page: 1, Limit: 20, Sort: sortKey, Order: "asc",
			})
			if err != nil {
				t.Fatalf("List sort=%s: %v", sortKey, err)
			}
			// The store resolves Sort through the allowlist it is handed (normalize()), so what
			// must hold is "the Sort key resolves to owner_id" — not "Sort already IS owner_id"
			// (that pre-translated form is not a key for relation/user fields and made the store
			// reject them; DAILY-QUEUE #4).
			if got := store.gotParams.SortAllowlist[store.gotParams.Sort]; got != "owner_id" {
				t.Errorf("SortAllowlist[Sort=%q] = %q; want owner_id (physical column, idx_owner_id)", store.gotParams.Sort, got)
			}
			if got := store.gotParams.SortAllowlist[sortKey]; got != "owner_id" {
				t.Errorf("SortAllowlist[%q] = %q; want owner_id", sortKey, got)
			}
		})
	}
}

// The allowlist did NOT become a wildcard: an undeclared field is still a hard 422, and so is a
// near-miss owner spelling ("owner" alone is not a column).
func TestList_SortUnknownField_StillRejected(t *testing.T) {
	for _, sortKey := range []string{"phu_trach", "owner", "ownerid"} {
		svc, _ := newListService()
		_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
			Page: 1, Limit: 20, Sort: sortKey,
		})
		if got := codeOf(err); got != "SORT_NOT_ALLOWED" {
			t.Errorf("sort=%s: code = %q (err=%v); want SORT_NOT_ALLOWED", sortKey, got, err)
		}
	}
}

// ?ownerId[eq]=<uuid> — the spelling a real client sends (the API returns the owner as `ownerId`
// on every row) — resolves to a parameterized predicate on the physical owner_id column, with the
// tenant and the A5 row scope both still in force. Before the change this was 422
// LIST_UNKNOWN_FILTER.
func TestList_FilterByOwner_Eq(t *testing.T) {
	svc, store := newListService()
	scope := RecordScope{UserID: "u1", AccessLevel: "owner"}
	_, err := svc.List(context.Background(), scope, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "ownerId", Operator: dynrepo.FilterEq, Values: []string{ownerUUID}}},
	})
	if err != nil {
		t.Fatalf("List ownerId[eq]: %v", err)
	}
	if len(store.gotParams.Filters) != 1 {
		t.Fatalf("Filters len = %d; want 1", len(store.gotParams.Filters))
	}
	f := store.gotParams.Filters[0]
	if f.PhysicalColumn != "owner_id" {
		t.Errorf("PhysicalColumn = %q; want owner_id (indexed physical column, not a JSON path)", f.PhysicalColumn)
	}
	// The camelCase wire key must NOT travel to the repo: dynrepo re-sanitizes Filter.Column with
	// ddl.SanitizeIdentifier (lowercase snake_case only), so "ownerId" there would be a 500.
	if f.Column != "owner_id" {
		t.Errorf("Column = %q; want owner_id (wire key normalized before dynrepo re-sanitizes it)", f.Column)
	}
	// Parameterized: the uuid is an OPERAND, never part of the column expression.
	if len(f.Values) != 1 || f.Values[0] != ownerUUID {
		t.Errorf("Values = %v; want [%s] as a placeholder operand", f.Values, ownerUUID)
	}
	if strings.Contains(f.Column, ownerUUID) || strings.Contains(f.PhysicalColumn, ownerUUID) {
		t.Errorf("uuid leaked into the column expression (Column=%q PhysicalColumn=%q)", f.Column, f.PhysicalColumn)
	}
	// A uuid is exact — case folding it would both miss and defeat idx_owner_id.
	if f.CaseInsensitive {
		t.Errorf("CaseInsensitive = true; want false for a uuid comparison")
	}
	// Tenant + A5 row scope are untouched by the new filter: the store still receives the caller's
	// tenant and resolved scope, so the owner filter can only NARROW what the row filter allows.
	if store.gotTenantID != "tenant-1" {
		t.Errorf("tenantID = %q; want tenant-1 (every entity query stays tenant-scoped)", store.gotTenantID)
	}
	if store.gotScope.Level != dynrepo.AccessOwner || store.gotScope.UserID != "u1" {
		t.Errorf("RowScope = %+v; want the caller's owner-level scope for u1", store.gotScope)
	}
}

// ?ownerId[in]=a,b — "records owned by my team" — is accepted with one placeholder per id, in
// BOTH wire spellings (the camelCase one must still be normalized before it reaches dynrepo).
func TestList_FilterByOwner_In(t *testing.T) {
	other := "0f7c2a10-1111-4222-8333-444455556666"
	for _, key := range []string{"ownerId", "owner_id"} {
		t.Run(key, func(t *testing.T) {
			svc, store := newListService()
			_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
				Page: 1, Limit: 20,
				Filters: []FilterInput{{Field: key, Operator: dynrepo.FilterIn, Values: []string{ownerUUID, other}}},
			})
			if err != nil {
				t.Fatalf("List %s[in]: %v", key, err)
			}
			f := store.gotParams.Filters[0]
			if f.Operator != dynrepo.FilterIn || len(f.Values) != 2 {
				t.Fatalf("Filter = %+v; want in with 2 values", f)
			}
			if f.PhysicalColumn != "owner_id" {
				t.Errorf("PhysicalColumn = %q; want owner_id", f.PhysicalColumn)
			}
			// Same normalization contract as eq: dynrepo re-sanitizes Column (lowercase
			// snake_case only), so "ownerId" must not survive this far.
			if f.Column != "owner_id" {
				t.Errorf("Column = %q; want owner_id", f.Column)
			}
		})
	}
}

// The owner key inherits `user` semantics, not a free-for-all: contains (a LIKE scan over a uuid)
// stays rejected, so admitting the key did not widen the operator surface.
func TestList_FilterByOwner_ContainsRejected(t *testing.T) {
	svc, _ := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "ownerId", Operator: dynrepo.FilterContains, Values: []string{"b155"}}},
	})
	if got := codeOf(err); got != "FILTER_OPERATOR_NOT_ALLOWED" {
		t.Fatalf("code = %q (err=%v); want FILTER_OPERATOR_NOT_ALLOWED", got, err)
	}
}

// An undeclared filter key is still a hard 422 — the owner key is a named exception, not a hole.
func TestList_FilterUnknownField_StillRejected(t *testing.T) {
	svc, _ := newListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "owner", Operator: dynrepo.FilterEq, Values: []string{ownerUUID}}},
	})
	if got := codeOf(err); got != "LIST_UNKNOWN_FILTER" {
		t.Fatalf("code = %q (err=%v); want LIST_UNKNOWN_FILTER", got, err)
	}
}

// TestOwnerDimension_ListAndAggregateAgree is the parity guard between the two halves of ONE
// screen. buildSortAllowlist admits the owner key, and resolveGroupByIndexed reuses that same map,
// so a dashboard can already GROUP BY owner. If only the List path also admitted the owner FILTER,
// the chart would group by owner and then 422 the instant the user filtered by the same owner —
// and the drill-down from that chart, which inherits the filter, would fail too.
//
// It asserts the two paths agree on the physical column AND on the accepted spellings, so a future
// edit that adds a key to one resolver and not the other fails here rather than in a dashboard.
func TestOwnerDimension_ListAndAggregateAgree(t *testing.T) {
	// A metadata set that does NOT declare an owner field — owner_id is a system column and is
	// never a sys_field, which is precisely why both resolvers need the reserved-key branch.
	byName := map[string]FieldSpec{
		"title": {Name: "title", FieldType: "text"},
	}

	for _, wire := range []string{"ownerId", "owner_id"} {
		t.Run(wire, func(t *testing.T) {
			resolved, err := resolveFiltersForAggregate(byName, []FilterInput{
				{Field: wire, Operator: dynrepo.FilterEq, Values: []string{ownerUUID}},
			})
			if err != nil {
				t.Fatalf("aggregate path refused the owner filter the List path accepts: %v", err)
			}
			if len(resolved) != 1 {
				t.Fatalf("resolved %d filters; want 1", len(resolved))
			}
			if resolved[0].PhysicalColumn != ownerPhysicalColumn {
				t.Errorf("aggregate routed %q to %q; want %q — the two paths must name the same column",
					wire, resolved[0].PhysicalColumn, ownerPhysicalColumn)
			}
			// The uuid must ride as a placeholder value, never folded into the column expression.
			if len(resolved[0].Values) != 1 || resolved[0].Values[0] != ownerUUID {
				t.Errorf("owner uuid did not travel as a bound value: %+v", resolved[0])
			}
		})
	}

	// And the same key must be a legal groupBy, or "group by owner" and "filter by owner" still
	// disagree — just in the other direction.
	allow := buildSortAllowlist([]FieldSpec{{Name: "title", FieldType: "text"}})
	if allow["ownerId"] != ownerPhysicalColumn {
		t.Errorf("sort/groupBy allowlist maps ownerId to %q; want %q", allow["ownerId"], ownerPhysicalColumn)
	}

	// A genuinely unknown key must still be refused by the aggregate path — the reserved branch
	// must not have become a hole.
	if _, err := resolveFiltersForAggregate(byName, []FilterInput{
		{Field: "owner", Operator: dynrepo.FilterEq, Values: []string{ownerUUID}},
	}); codeOf(err) != "LIST_UNKNOWN_FILTER" {
		t.Errorf("bare \"owner\" accepted by the aggregate path (code %q) — reserved keys leaked", codeOf(err))
	}
}
