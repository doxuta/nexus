package services

import (
	"context"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// USER-FIELD slice: sort/filter/groupBy parity with `relation` — a `user` field must be usable
// like any other physical column (RM-09 sort allowlist, RM-13 filter routing), reusing the SAME
// <field>_id generated column relation gets. Uses a LOCAL fixture (not courseFieldDefs(), shared
// by many other List tests in this package) to avoid any risk of side-effects on them.

func ticketsWithAssigneeEntityFixture() *metadata.EntityDefinition {
	return &metadata.EntityDefinition{ID: "ent-tuf", TenantID: "tenant-1", Name: "tickets", DataTable: "obj_tickets"}
}

func ticketsWithAssigneeFieldDefsForList() []metadata.FieldDefinition {
	return []metadata.FieldDefinition{
		{Name: "subject", FieldType: "text"},
		{Name: "assigned_to", FieldType: "user"},
	}
}

func newUserFieldListService() (*RecordService, *fakeRecordStore) {
	store := &fakeRecordStore{}
	cat := &fakeRecordCatalog{entity: ticketsWithAssigneeEntityFixture(), fields: ticketsWithAssigneeFieldDefsForList()}
	return NewRecordService(cat, store), store
}

// TestList_SortByUserField proves a `user` field is in the RM-09 sort allowlist and resolves to
// its <field>_id physical column — exactly like a `relation` field (wave2a_integration_test.go's
// "sort by relation _id column"). The resolution happens INSIDE the store (normalize() maps the
// wire name through SortAllowlist), so the contract to pin is "Sort is a key that resolves to the
// FK column" — pinning the already-translated name made the store reject it (DAILY-QUEUE #4).
func TestList_SortByUserField(t *testing.T) {
	svc, store := newUserFieldListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "tickets", ListQuery{
		Page: 1, Limit: 20, Sort: "assigned_to", Order: "asc",
	})
	if err != nil {
		t.Fatalf("List(sort=assigned_to): %v", err)
	}
	got := store.gotParams
	if phys, ok := got.SortAllowlist[got.Sort]; !ok || phys != "assigned_to_id" {
		t.Errorf("Sort %q resolves to %q (present=%v) via the store's SortAllowlist; want assigned_to_id (physical FK column)", got.Sort, phys, ok)
	}
}

// TestList_FilterEqUserField proves eq on a `user` field routes to the <field>_id physical
// column (RM-13) and is treated as exact (never case-insensitive) — same as relation.
func TestList_FilterEqUserField(t *testing.T) {
	svc, store := newUserFieldListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "tickets", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "assigned_to", Operator: dynrepo.FilterEq, Values: []string{"u-1"}}},
	})
	if err != nil {
		t.Fatalf("List(assigned_to=u-1): %v", err)
	}
	if len(store.gotParams.Filters) != 1 {
		t.Fatalf("Filters len = %d; want 1", len(store.gotParams.Filters))
	}
	f := store.gotParams.Filters[0]
	if f.PhysicalColumn != "assigned_to_id" {
		t.Errorf("PhysicalColumn = %q; want \"assigned_to_id\"", f.PhysicalColumn)
	}
	if f.CaseInsensitive {
		t.Errorf("CaseInsensitive = true; want false (user ids are exact)")
	}
}

// TestList_FilterInUserField proves the `in` operator is allowed on a `user` field (e.g.
// "assigned to any of my team").
func TestList_FilterInUserField(t *testing.T) {
	svc, store := newUserFieldListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "tickets", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "assigned_to", Operator: dynrepo.FilterIn, Values: []string{"u-1", "u-2"}}},
	})
	if err != nil {
		t.Fatalf("List(assigned_to in [u-1,u-2]): %v", err)
	}
	if len(store.gotParams.Filters) != 1 || store.gotParams.Filters[0].PhysicalColumn != "assigned_to_id" {
		t.Fatalf("Filters = %+v; want 1 filter routed to assigned_to_id", store.gotParams.Filters)
	}
}

// TestList_FilterGtUserFieldRejected proves an ordered comparison on a `user` field is rejected
// (a userId has no meaningful ordering) — same contract as `relation`.
func TestList_FilterGtUserFieldRejected(t *testing.T) {
	svc, _ := newUserFieldListService()
	_, err := svc.List(context.Background(), RecordScope{UserID: "u1", AccessLevel: "all"}, "tenant-1", "tickets", ListQuery{
		Page: 1, Limit: 20,
		Filters: []FilterInput{{Field: "assigned_to", Operator: dynrepo.FilterGt, Values: []string{"u-1"}}},
	})
	if got := codeOf(err); got != "FILTER_OPERATOR_NOT_ALLOWED" {
		t.Fatalf("code = %q (err=%v); want FILTER_OPERATOR_NOT_ALLOWED", got, err)
	}
}

// TestResolveGroupByIndexed_UserField proves a `user` field is groupable (kanban-by-assignee /
// report groupBy) for FREE via the shared RM-09 allowlist (buildSortAllowlist) — no separate
// code path to keep in sync.
func TestResolveGroupByIndexed_UserField(t *testing.T) {
	specs := []FieldSpec{{Name: "assigned_to", FieldType: "user"}}
	col, err := resolveGroupByIndexed("assigned_to", specs)
	if err != nil {
		t.Fatalf("resolveGroupByIndexed(assigned_to) = %v; want nil (user fields are groupable)", err)
	}
	if col != "assigned_to_id" {
		t.Errorf("physical column = %q; want \"assigned_to_id\"", col)
	}
}
