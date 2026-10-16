package services

// HIDDEN-COL-SIGNAL: List must surface HOW MANY declared fields of the entity the caller's
// field-ACL hides (can_view=0) as ListResult.HiddenColumns, computed from the SAME hidden set
// computedHiddenFields already loads for the lookup/rollup gate (one ACL path — B6 anti-drift).
// The handler turns a count > 0 into the additive `meta.hiddenColumns` envelope key so the FE
// can render an honest "N cột ẩn bởi quyền" chip; admins and a nil enforcer must yield 0 so the
// chip can never be a false positive for a caller who actually sees every column.

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/dynrepo"
)

// fakeListHiddenACL is a fieldACLEnforcer double for the HIDDEN-COL-SIGNAL tests. Only
// HiddenFields carries behavior (per-entity hidden set or an injected error); the strip/guard
// methods are no-ops because the count is derived from the set, not from row mutation.
// hiddenCalls counts HiddenFields invocations so the admin test can prove the bypass never
// consults the enforcer at all.
type fakeListHiddenACL struct {
	hidden      map[string]map[string]struct{} // entity → hidden field names
	err         error                          // injected HiddenFields failure (fail-closed path)
	hiddenCalls int
}

func (f *fakeListHiddenACL) FilterReadFields(_ context.Context, _, _, _ string, _ bool, _ map[string]any) error {
	return nil
}
func (f *fakeListHiddenACL) FilterReadFieldsBatch(_ context.Context, _, _, _ string, _ bool, _ []map[string]any) error {
	return nil
}
func (f *fakeListHiddenACL) CheckWriteFields(_ context.Context, _, _, _ string, _ bool, _ map[string]any) error {
	return nil
}

// CheckWriteFieldsAgainst delegates to CheckWriteFields: these fakes have no notion of a
// record's current values, and the update-path rule (an unchanged locked field is a no-op)
// is exercised in the field-ACL tests that DO model them.
func (f *fakeListHiddenACL) CheckWriteFieldsAgainst(ctx context.Context, t, r, e string, isAdmin bool, payload, _ map[string]any) error {
	return f.CheckWriteFields(ctx, t, r, e, isAdmin, payload)
}
func (f *fakeListHiddenACL) AssertFieldVisible(_ context.Context, _, _, _ string, _ bool, _ string) error {
	return nil
}
func (f *fakeListHiddenACL) AssertFieldsVisible(_ context.Context, _, _, _ string, _ bool, _ []string) error {
	return nil
}
func (f *fakeListHiddenACL) HiddenFields(_ context.Context, _, _, entity string, _ bool) (map[string]struct{}, error) {
	f.hiddenCalls++
	if f.err != nil {
		return nil, f.err
	}
	if f.hidden == nil {
		return map[string]struct{}{}, nil
	}
	return f.hidden[entity], nil
}

// newHiddenListService builds a RecordService over the courses fixture (title/seats/level —
// record_list_test.go) with the given field-ACL double injected.
func newHiddenListService(facl *fakeListHiddenACL) *RecordService {
	store := &fakeRecordStore{}
	cat := &fakeRecordCatalog{entity: courseEntityFixture(), fields: courseFieldDefs()}
	return NewRecordService(cat, store).WithFieldACL(facl)
}

// memberScope is a non-admin caller with a role — the principal the field-ACL applies to.
func memberScope() RecordScope {
	return RecordScope{UserID: "u1", RoleID: "role-1", AccessLevel: "all", IsAdmin: false}
}

// Two declared fields hidden → HiddenColumns == 2 (offset mode). The core happy path.
func TestList_HiddenColumns_TwoHiddenFields(t *testing.T) {
	facl := &fakeListHiddenACL{hidden: map[string]map[string]struct{}{
		"courses": {"title": {}, "level": {}},
	}}
	svc := newHiddenListService(facl)
	res, err := svc.List(context.Background(), memberScope(), "tenant-1", "courses", ListQuery{Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.HiddenColumns != 2 {
		t.Errorf("HiddenColumns = %d; want 2 (title + level hidden)", res.HiddenColumns)
	}
	if res.CursorMode {
		t.Errorf("CursorMode = true; want false (offset mode)")
	}
}

// A stale ACL restriction naming a field NOT declared on the entity must not inflate the
// count: the FE chip promises "N cột ẩn" out of the columns it would otherwise render.
func TestList_HiddenColumns_StaleUndeclaredFieldIgnored(t *testing.T) {
	facl := &fakeListHiddenACL{hidden: map[string]map[string]struct{}{
		"courses": {"title": {}, "ghost_deleted_field": {}},
	}}
	svc := newHiddenListService(facl)
	res, err := svc.List(context.Background(), memberScope(), "tenant-1", "courses", ListQuery{Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.HiddenColumns != 1 {
		t.Errorf("HiddenColumns = %d; want 1 (ghost_deleted_field is not a declared field)", res.HiddenColumns)
	}
}

// A role with NO hidden fields → 0 (the handler then omits meta entirely).
func TestList_HiddenColumns_NoneHiddenIsZero(t *testing.T) {
	facl := &fakeListHiddenACL{hidden: map[string]map[string]struct{}{}}
	svc := newHiddenListService(facl)
	res, err := svc.List(context.Background(), memberScope(), "tenant-1", "courses", ListQuery{Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.HiddenColumns != 0 {
		t.Errorf("HiddenColumns = %d; want 0", res.HiddenColumns)
	}
}

// Admin bypass: even with a restriction set that would hide fields from a member role, an
// admin sees every column → count 0, and the enforcer is never consulted (computedHiddenFields
// short-circuits on IsAdmin — mirroring stripReadFields' bypass exactly).
func TestList_HiddenColumns_AdminZeroWithoutACLLoad(t *testing.T) {
	facl := &fakeListHiddenACL{hidden: map[string]map[string]struct{}{
		"courses": {"title": {}, "level": {}},
	}}
	svc := newHiddenListService(facl)
	res, err := svc.List(context.Background(), RecordScope{UserID: "admin", IsAdmin: true, AccessLevel: "all"},
		"tenant-1", "courses", ListQuery{Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.HiddenColumns != 0 {
		t.Errorf("HiddenColumns = %d; want 0 for admin (no false-positive chip)", res.HiddenColumns)
	}
	if facl.hiddenCalls != 0 {
		t.Errorf("admin path must not consult field-ACL; hiddenCalls = %d, want 0", facl.hiddenCalls)
	}
}

// Field-level security not wired (nil enforcer) → 0, exactly like every other RM-42 surface.
func TestList_HiddenColumns_NilEnforcerZero(t *testing.T) {
	svc, _ := newListService() // no WithFieldACL
	res, err := svc.List(context.Background(), memberScope(), "tenant-1", "courses", ListQuery{Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.HiddenColumns != 0 {
		t.Errorf("HiddenColumns = %d; want 0 with field-ACL disabled", res.HiddenColumns)
	}
}

// Cursor mode (D2) carries the same count — both List branches reuse the SAME hidden set.
func TestList_HiddenColumns_CursorModeCarriesCount(t *testing.T) {
	facl := &fakeListHiddenACL{hidden: map[string]map[string]struct{}{
		"courses": {"title": {}, "level": {}},
	}}
	svc := newHiddenListService(facl)
	cursor := dynrepo.EncodeCursor("created_at", "DESC", false, "2026-07-01 10:00:00", "00000000-0000-0000-0000-000000000001")
	res, err := svc.List(context.Background(), memberScope(), "tenant-1", "courses", ListQuery{
		Page: 1, Limit: 10, Sort: "created_at", Order: "desc", After: cursor,
	})
	if err != nil {
		t.Fatalf("List cursor mode: %v", err)
	}
	if !res.CursorMode {
		t.Fatalf("CursorMode = false; want true")
	}
	if res.HiddenColumns != 2 {
		t.Errorf("HiddenColumns = %d; want 2 in cursor mode", res.HiddenColumns)
	}
}

// Fail-closed: a HiddenFields load error aborts the read (the caller refuses the page rather
// than serving rows whose visibility it could not verify) — the pre-existing
// computedHiddenFields contract, locked here because the count now also rides on it.
func TestList_HiddenColumns_ACLLoadErrorFailsClosed(t *testing.T) {
	facl := &fakeListHiddenACL{err: errors.New("grant table unavailable")}
	svc := newHiddenListService(facl)
	_, err := svc.List(context.Background(), memberScope(), "tenant-1", "courses", ListQuery{Page: 1, Limit: 20})
	if err == nil {
		t.Fatalf("List must fail closed when the field-ACL hidden set cannot be loaded")
	}
}

// Compile-time drift guard: the count helper must intersect with the DECLARED specs — a
// direct table over the pure helper (cheap to extend when field types grow).
func TestCountHiddenColumns_Table(t *testing.T) {
	specs := []FieldSpec{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	tests := []struct {
		name   string
		hidden map[string]struct{}
		want   int
	}{
		{"nil set", nil, 0},
		{"empty set", map[string]struct{}{}, 0},
		{"one declared", map[string]struct{}{"b": {}}, 1},
		{"all declared", map[string]struct{}{"a": {}, "b": {}, "c": {}}, 3},
		{"stale only", map[string]struct{}{"zz": {}}, 0},
		{"mixed declared+stale", map[string]struct{}{"a": {}, "zz": {}}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countHiddenColumns(specs, tt.hidden); got != tt.want {
				t.Errorf("countHiddenColumns = %d; want %d", got, tt.want)
			}
		})
	}
}
