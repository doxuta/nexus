package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// fakeUserStore is a RecordStore whose BatchActiveUserExists answers from a configurable set of
// "(tenant,id) -> ACTIVE user" facts, so the `user` field FK-existence validation (USER-FIELD
// slice, mirrors M3-T2's checkRelationTargets) can be exercised without a database. It mirrors
// fakeRelStore (record_relation_test.go) shape-for-shape.
//
// The fake collapses every "doesn't count" reason (unknown id, cross-tenant id, disabled user)
// into a single boolean per (tenant,id) key — exactly what checkUserTargets sees from the real
// SQL (BatchActiveUserExists filters status='active' AND deleted_at IS NULL AND tenant_id=?).
// WHICH reason applies for a given id is a SQL-shape concern, covered by the dynrepo integration
// test (user_field_integration_test.go), not this unit test.
type fakeUserStore struct {
	activeUsers map[string]bool // key: tenant|id -> true when the fake considers it a live ACTIVE user
	batchErr    error
	inserted    int
	updated     int
	batchArgs   []string // "tenant|id" probed via BatchActiveUserExists, for assertion
	batchCalls  int
	// setOwnerErr / setOwnerCalls / gotNewOwnerID / gotSetOwnerScope back TestRecordService_SetOwner
	// (record_owner_test.go) — configurable failure + call-args capture for the SetOwner path.
	setOwnerErr      error
	setOwnerCalls    int
	gotNewOwnerID    string
	gotSetOwnerScope dynrepo.RowScope
}

func userKey(tenant, id string) string { return tenant + "|" + id }

func (f *fakeUserStore) InsertRecord(_ context.Context, _, _, _ string, _ map[string]any, _ []dynrepo.UniqueFieldSpec) (string, error) {
	f.inserted++
	return "new-id", nil
}
func (f *fakeUserStore) GetRecord(_ context.Context, _, _, _ string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	return nil, nil
}
func (f *fakeUserStore) ListRecords(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.ListParams) ([]dynrepo.Record, int, error) {
	return nil, 0, nil
}
func (f *fakeUserStore) ListRecordsCursor(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.CursorParams) ([]dynrepo.Record, string, error) {
	return nil, "", nil
}
func (f *fakeUserStore) UpdateRecord(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, _ map[string]any, _ *int, _ []dynrepo.UniqueFieldSpec) error {
	f.updated++
	return nil
}
func (f *fakeUserStore) SoftDeleteRecord(_ context.Context, _, _, _ string, _ dynrepo.RowScope) error {
	return nil
}
func (f *fakeUserStore) RestoreRecord(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, _ []dynrepo.UniqueFieldSpec) error {
	return nil
}
func (f *fakeUserStore) RecordExists(_ context.Context, _, _, _ string) (bool, error) {
	return true, nil
}
func (f *fakeUserStore) BatchRecordExists(_ context.Context, _, _ string, _ []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}
func (f *fakeUserStore) AggregateScalar(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.AggregateParams) (dynrepo.AggregateScalar, error) {
	return dynrepo.AggregateScalar{}, nil
}
func (f *fakeUserStore) AggregateGrouped(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.AggregateParams) ([]dynrepo.AggregateGroup, error) {
	return nil, nil
}
func (f *fakeUserStore) AggregateGrid(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.GridParams) ([]dynrepo.GridRow, []dynrepo.GridRow, error) {
	return nil, nil, nil
}
func (f *fakeUserStore) SetOwner(_ context.Context, _, _, _, _ string, scope dynrepo.RowScope, newOwnerID string) error {
	f.setOwnerCalls++
	f.gotNewOwnerID = newOwnerID
	f.gotSetOwnerScope = scope
	if f.setOwnerErr != nil {
		return f.setOwnerErr
	}
	return nil
}
func (f *fakeUserStore) BatchActiveUserExists(_ context.Context, tenantID string, ids []string) (map[string]bool, error) {
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	f.batchCalls++
	result := make(map[string]bool, len(ids))
	for _, id := range ids {
		key := userKey(tenantID, id)
		f.batchArgs = append(f.batchArgs, key)
		result[id] = f.activeUsers[key]
	}
	return result, nil
}

func ticketWithAssigneeEntity() *metadata.EntityDefinition {
	return &metadata.EntityDefinition{ID: "ent-tic2", TenantID: "tenant-1", Name: "tickets", DataTable: "obj_tickets"}
}

func ticketWithAssigneeFieldDefs() []metadata.FieldDefinition {
	return []metadata.FieldDefinition{
		{Name: "subject", FieldType: "text", IsRequired: true},
		{Name: "assigned_to", FieldType: "user"},
	}
}

func newUserFieldService(store *fakeUserStore) *RecordService {
	cat := &fakeRecordCatalog{entity: ticketWithAssigneeEntity(), fields: ticketWithAssigneeFieldDefs()}
	return NewRecordService(cat, store)
}

// TestCreate_UserFieldValid: a `user` value that IS an active user of the SAME tenant passes
// validation and the insert runs; the probe targets the right (tenant,id).
func TestCreate_UserFieldValid(t *testing.T) {
	store := &fakeUserStore{activeUsers: map[string]bool{userKey("tenant-1", "u-1"): true}}
	svc := newUserFieldService(store)

	id, err := svc.Create(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", "user-1", "tickets",
		map[string]any{"subject": "Help", "assigned_to": "u-1"})
	if err != nil {
		t.Fatalf("Create(valid user FK): %v", err)
	}
	if id != "new-id" || store.inserted != 1 {
		t.Errorf("valid user FK should insert; id=%q inserted=%d", id, store.inserted)
	}
	if len(store.batchArgs) != 1 || store.batchArgs[0] != userKey("tenant-1", "u-1") {
		t.Errorf("user probe args = %v; want [tenant-1|u-1]", store.batchArgs)
	}
}

// TestCreate_UserFieldGarbageID: a `user` value that resolves to NO active user of this tenant
// (garbage id — the fake never registered it) is rejected 422 RECORD_USER_INVALID and no insert
// runs. details.field must name the offending field.
func TestCreate_UserFieldGarbageID(t *testing.T) {
	store := &fakeUserStore{activeUsers: map[string]bool{}}
	svc := newUserFieldService(store)

	_, err := svc.Create(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", "user-1", "tickets",
		map[string]any{"subject": "Help", "assigned_to": "ghost"})
	if codeOf(err) != "RECORD_USER_INVALID" {
		t.Fatalf("code = %q (err=%v); want RECORD_USER_INVALID", codeOf(err), err)
	}
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != "assigned_to" {
		t.Fatalf("details.field = %+v; want \"assigned_to\"", ve)
	}
	if store.inserted != 0 {
		t.Errorf("invalid user FK must block the insert; inserted=%d", store.inserted)
	}
}

// TestCreate_UserFieldCrossTenant: an id that IS an active user, but of a DIFFERENT tenant, is
// rejected with the SAME RECORD_USER_INVALID (anti-enumeration — no distinct "wrong tenant" code).
func TestCreate_UserFieldCrossTenant(t *testing.T) {
	store := &fakeUserStore{activeUsers: map[string]bool{userKey("tenant-OTHER", "u-1"): true}}
	svc := newUserFieldService(store)

	_, err := svc.Create(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", "user-1", "tickets",
		map[string]any{"subject": "Help", "assigned_to": "u-1"})
	if codeOf(err) != "RECORD_USER_INVALID" {
		t.Fatalf("code = %q (err=%v); want RECORD_USER_INVALID (cross-tenant id must not resolve)", codeOf(err), err)
	}
	if store.inserted != 0 {
		t.Errorf("cross-tenant user FK must block the insert; inserted=%d", store.inserted)
	}
}

// TestCreate_UserFieldDisabled: a KNOWN id (registered in the fake at all) that is explicitly
// marked NOT-active — the fake's stand-in for a disabled user, since BatchActiveUserExists'
// real SQL filters status='active' — is rejected with the SAME RECORD_USER_INVALID as a garbage
// id (TestCreate_UserFieldGarbageID) or a cross-tenant id (TestCreate_UserFieldCrossTenant):
// disabled/unknown/cross-tenant are ALL indistinguishable "doesn't count" outcomes by design
// (anti-enumeration). Which SQL predicate excluded a given real row is covered by the dynrepo
// integration test, not this fake.
func TestCreate_UserFieldDisabled(t *testing.T) {
	store := &fakeUserStore{activeUsers: map[string]bool{userKey("tenant-1", "u-1"): false}}
	svc := newUserFieldService(store)

	_, err := svc.Create(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", "user-1", "tickets",
		map[string]any{"subject": "Help", "assigned_to": "u-1"})
	if codeOf(err) != "RECORD_USER_INVALID" {
		t.Fatalf("code = %q (err=%v); want RECORD_USER_INVALID (disabled user must not resolve)", codeOf(err), err)
	}
	if store.inserted != 0 {
		t.Errorf("disabled user FK must block the insert; inserted=%d", store.inserted)
	}
}

// TestCreate_UserFieldOmittedOK: an optional `user` field left unset skips the probe entirely.
func TestCreate_UserFieldOmittedOK(t *testing.T) {
	store := &fakeUserStore{activeUsers: map[string]bool{}}
	svc := newUserFieldService(store)

	if _, err := svc.Create(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", "user-1", "tickets",
		map[string]any{"subject": "Help"}); err != nil {
		t.Fatalf("Create(no assigned_to): %v", err)
	}
	if len(store.batchArgs) != 0 {
		t.Errorf("omitted user field must not probe; args=%v", store.batchArgs)
	}
	if store.inserted != 1 {
		t.Errorf("omitted optional user field should still insert; inserted=%d", store.inserted)
	}
}

// TestUpdate_UserFieldGarbageID: Update enforces the same rule as Create (partial-PUT may
// set/replace the assignee).
func TestUpdate_UserFieldGarbageID(t *testing.T) {
	store := &fakeUserStore{activeUsers: map[string]bool{}}
	svc := newUserFieldService(store)

	err := svc.Update(context.Background(), RecordScope{UserID: "user-1", AccessLevel: "all"}, "tenant-1", "tickets", "rec-1",
		map[string]any{"assigned_to": "ghost"}, nil)
	if codeOf(err) != "RECORD_USER_INVALID" {
		t.Fatalf("code = %q (err=%v); want RECORD_USER_INVALID", codeOf(err), err)
	}
	if store.updated != 0 {
		t.Errorf("invalid user FK must block the update; updated=%d", store.updated)
	}
}

// TestCreate_UserFieldProbeErrorSurfaces: a failure of the existence probe is surfaced
// (wrapped), not swallowed, and the insert does not run.
func TestCreate_UserFieldProbeErrorSurfaces(t *testing.T) {
	store := &fakeUserStore{batchErr: errors.New("db boom")}
	svc := newUserFieldService(store)

	_, err := svc.Create(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", "user-1", "tickets",
		map[string]any{"subject": "Help", "assigned_to": "u-1"})
	if err == nil {
		t.Fatal("expected error when the user-existence probe fails")
	}
	if store.inserted != 0 {
		t.Errorf("probe failure must block the insert; inserted=%d", store.inserted)
	}
}

// TestCreate_UserField_StoreUnavailable_FailClosed proves checkUserTargets FAILS CLOSED
// (RECORD_USER_UNAVAILABLE) when the wired store does not implement userExister — but ONLY
// because THIS payload actually touches the `user` field with a non-empty value. fakeRecordStore
// (record_list_test.go) implements RecordStore but NOT BatchActiveUserExists, exactly the
// "hand-rolled fake that predates this slice" scenario checkUserTargets' doc describes.
func TestCreate_UserField_StoreUnavailable_FailClosed(t *testing.T) {
	store := &fakeRecordStore{}
	cat := &fakeRecordCatalog{entity: ticketWithAssigneeEntity(), fields: ticketWithAssigneeFieldDefs()}
	svc := NewRecordService(cat, store) // store does NOT implement userExister -> s.userStore stays nil

	_, err := svc.Create(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", "user-1", "tickets",
		map[string]any{"subject": "Help", "assigned_to": "u-1"})
	if codeOf(err) != "RECORD_USER_UNAVAILABLE" {
		t.Fatalf("code = %q (err=%v); want RECORD_USER_UNAVAILABLE", codeOf(err), err)
	}
}

// TestCreate_UserField_StoreUnavailable_OmittedFieldStillOK proves the fail-closed guard is
// SCOPED to payloads that actually touch a `user` field — a create that never mentions
// assigned_to succeeds even though the store cannot verify user existence at all.
func TestCreate_UserField_StoreUnavailable_OmittedFieldStillOK(t *testing.T) {
	store := &fakeRecordStore{}
	cat := &fakeRecordCatalog{entity: ticketWithAssigneeEntity(), fields: ticketWithAssigneeFieldDefs()}
	svc := NewRecordService(cat, store)

	if _, err := svc.Create(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", "user-1", "tickets",
		map[string]any{"subject": "Help"}); err != nil {
		t.Fatalf("Create(no assigned_to, unwired user store) = %v; want nil", err)
	}
}
