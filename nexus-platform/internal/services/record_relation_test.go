package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// fakeRelStore is a RecordStore whose BatchRecordExists answers from a configurable set of
// "(entity,tenant,id) -> live" facts, so the relation FK-existence validation (M3-T2 + RM-11)
// can be exercised without a database. It also records Insert/Update calls so a test can
// confirm an invalid FK blocks the write entirely.
//
// validateRelations now calls BatchRecordExists (not RecordExists), so batchArgs tracks
// the individual ids probed and batchCalls counts how many times the method was invoked
// (one per unique target entity — RM-11 anti-N+1 property, S1).
type fakeRelStore struct {
	live       map[string]bool // key: entity|tenant|id
	batchErr   error
	inserted   int
	updated    int
	batchArgs  []string // "entity|tenant|id" probed via BatchRecordExists, for assertion
	batchCalls int      // number of BatchRecordExists invocations (S1: must be ≤ unique targets)
}

func relKey(entity, tenant, id string) string { return entity + "|" + tenant + "|" + id }

func (f *fakeRelStore) InsertRecord(_ context.Context, _, _, _ string, _ map[string]any, _ []dynrepo.UniqueFieldSpec) (string, error) {
	f.inserted++
	return "new-id", nil
}
func (f *fakeRelStore) GetRecord(_ context.Context, _, _, _ string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	return nil, nil
}
func (f *fakeRelStore) ListRecords(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.ListParams) ([]dynrepo.Record, int, error) {
	return nil, 0, nil
}
func (f *fakeRelStore) ListRecordsCursor(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.CursorParams) ([]dynrepo.Record, string, error) {
	return nil, "", nil
}
func (f *fakeRelStore) UpdateRecord(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, _ map[string]any, _ *int, _ []dynrepo.UniqueFieldSpec) error {
	f.updated++
	return nil
}
func (f *fakeRelStore) SoftDeleteRecord(_ context.Context, _, _, _ string, _ dynrepo.RowScope) error {
	return nil
}
func (f *fakeRelStore) RestoreRecord(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, _ []dynrepo.UniqueFieldSpec) error {
	return nil
}
func (f *fakeRelStore) RecordExists(_ context.Context, entity, tenantID, id string) (bool, error) {
	// Kept to satisfy the interface; validateRelations now calls BatchRecordExists.
	return f.live[relKey(entity, tenantID, id)], nil
}
func (f *fakeRelStore) BatchRecordExists(_ context.Context, entity, tenantID string, ids []string) (map[string]bool, error) {
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	f.batchCalls++ // S1: count invocations — must equal number of unique target entities
	result := make(map[string]bool, len(ids))
	for _, id := range ids {
		key := relKey(entity, tenantID, id)
		f.batchArgs = append(f.batchArgs, key)
		result[id] = f.live[key]
	}
	return result, nil
}
func (f *fakeRelStore) AggregateScalar(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.AggregateParams) (dynrepo.AggregateScalar, error) {
	return dynrepo.AggregateScalar{}, nil
}
func (f *fakeRelStore) AggregateGrouped(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.AggregateParams) ([]dynrepo.AggregateGroup, error) {
	return nil, nil
}
func (f *fakeRelStore) AggregateGrid(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.GridParams) ([]dynrepo.GridRow, []dynrepo.GridRow, error) {
	return nil, nil, nil
}
func (f *fakeRelStore) SetOwner(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, _ string) error {
	return nil
}
func ticketEntity() *metadata.EntityDefinition {
	return &metadata.EntityDefinition{ID: "ent-tic", TenantID: "tenant-1", Name: "tickets", DataTable: "obj_tickets"}
}

func ticketFieldDefs() []metadata.FieldDefinition {
	rel := `{"relatedEntity":"accounts"}`
	return []metadata.FieldDefinition{
		{Name: "subject", FieldType: "text", IsRequired: true},
		{Name: "account", FieldType: "relation", Options: &rel},
	}
}

func newRelService(store *fakeRelStore) *RecordService {
	cat := &fakeRecordCatalog{entity: ticketEntity(), fields: ticketFieldDefs()}
	return NewRecordService(cat, store)
}

// TestCreate_RelationFKValid: a relation value pointing at a live record in the related
// entity (same tenant) passes validation and the insert runs.
func TestCreate_RelationFKValid(t *testing.T) {
	store := &fakeRelStore{live: map[string]bool{relKey("accounts", "tenant-1", "acc-1"): true}}
	svc := newRelService(store)

	id, err := svc.Create(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", "user-1", "tickets",
		map[string]any{"subject": "Help", "account": "acc-1"})
	if err != nil {
		t.Fatalf("Create(valid FK): %v", err)
	}
	if id != "new-id" || store.inserted != 1 {
		t.Errorf("valid FK should insert; id=%q inserted=%d", id, store.inserted)
	}
	// The batch FK probe must target the RELATED entity, same tenant, with the FK value.
	if len(store.batchArgs) != 1 || store.batchArgs[0] != relKey("accounts", "tenant-1", "acc-1") {
		t.Errorf("FK probe args = %v; want [accounts|tenant-1|acc-1]", store.batchArgs)
	}
}

// TestCreate_RelationFKMissingTarget: a relation value pointing at a non-existent (or
// cross-tenant) parent is rejected 422 RECORD_RELATION_INVALID and NO insert runs.
func TestCreate_RelationFKMissingTarget(t *testing.T) {
	store := &fakeRelStore{live: map[string]bool{}} // nothing is a valid target
	svc := newRelService(store)

	_, err := svc.Create(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", "user-1", "tickets",
		map[string]any{"subject": "Help", "account": "ghost"})
	if codeOf(err) != "RECORD_RELATION_INVALID" {
		t.Fatalf("code = %q (err=%v); want RECORD_RELATION_INVALID", codeOf(err), err)
	}
	if store.inserted != 0 {
		t.Errorf("invalid FK must block the insert; inserted=%d", store.inserted)
	}
}

// TestCreate_RelationFKOmittedOK: an optional relation field left unset skips the FK probe
// entirely (no probe, insert proceeds) — relation values are validated only when present.
func TestCreate_RelationFKOmittedOK(t *testing.T) {
	store := &fakeRelStore{live: map[string]bool{}}
	svc := newRelService(store)

	if _, err := svc.Create(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", "user-1", "tickets",
		map[string]any{"subject": "Help"}); err != nil {
		t.Fatalf("Create(no FK): %v", err)
	}
	if len(store.batchArgs) != 0 {
		t.Errorf("omitted relation must not probe; args=%v", store.batchArgs)
	}
	if store.inserted != 1 {
		t.Errorf("omitted optional relation should still insert; inserted=%d", store.inserted)
	}
}

// TestUpdate_RelationFKMissingTarget: Update enforces the same FK rule as Create.
func TestUpdate_RelationFKMissingTarget(t *testing.T) {
	store := &fakeRelStore{live: map[string]bool{}}
	svc := newRelService(store)

	err := svc.Update(context.Background(), RecordScope{UserID: "user-1", AccessLevel: "all"}, "tenant-1", "tickets", "rec-1",
		map[string]any{"subject": "Help", "account": "ghost"}, nil)
	if codeOf(err) != "RECORD_RELATION_INVALID" {
		t.Fatalf("code = %q (err=%v); want RECORD_RELATION_INVALID", codeOf(err), err)
	}
	if store.updated != 0 {
		t.Errorf("invalid FK must block the update; updated=%d", store.updated)
	}
}

// TestCreate_RelationFKProbeErrorSurfaces: a failure of the FK-existence probe is surfaced
// (wrapped), not swallowed, and the insert does not run.
func TestCreate_RelationFKProbeErrorSurfaces(t *testing.T) {
	store := &fakeRelStore{batchErr: errors.New("db boom")}
	svc := newRelService(store)

	_, err := svc.Create(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", "user-1", "tickets",
		map[string]any{"subject": "Help", "account": "acc-1"})
	if err == nil {
		t.Fatal("expected error when the FK probe fails")
	}
	if store.inserted != 0 {
		t.Errorf("probe failure must block the insert; inserted=%d", store.inserted)
	}
}

// TestCreate_TwoRelationFieldsSameTarget_OneBatchCall proves the RM-11 anti-N+1 property
// (S1): when a record has TWO relation fields pointing at the SAME target entity, the
// service issues exactly ONE BatchRecordExists call, not two. The fake's batchCalls
// counter distinguishes "number of calls" from "number of ids probed".
func TestCreate_TwoRelationFieldsSameTarget_OneBatchCall(t *testing.T) {
	// Entity with two relation fields both targeting "accounts".
	rel := `{"relatedEntity":"accounts"}`
	store := &fakeRelStore{
		live: map[string]bool{
			relKey("accounts", "tenant-1", "acc-primary"):   true,
			relKey("accounts", "tenant-1", "acc-secondary"): true,
		},
	}
	cat := &fakeRecordCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-multi", TenantID: "tenant-1", Name: "deals", DataTable: "obj_deals"},
		fields: []metadata.FieldDefinition{
			{Name: "title", FieldType: "text", IsRequired: true},
			{Name: "primary_account", FieldType: "relation", Options: &rel},
			{Name: "secondary_account", FieldType: "relation", Options: &rel},
		},
	}
	svc := NewRecordService(cat, store)

	_, err := svc.Create(context.Background(), RecordScope{IsAdmin: true}, "tenant-1", "user-1", "deals",
		map[string]any{
			"title":             "Big Deal",
			"primary_account":   "acc-primary",
			"secondary_account": "acc-secondary",
		})
	if err != nil {
		t.Fatalf("Create(two relation fields, same target): %v", err)
	}
	// S1: two relation fields, same target entity "accounts" → exactly ONE batch call.
	if store.batchCalls != 1 {
		t.Errorf("batchCalls = %d; want 1 (both fields grouped into one IN query per target)", store.batchCalls)
	}
	// Both ids must have been probed in that single call.
	if len(store.batchArgs) != 2 {
		t.Errorf("batchArgs len = %d; want 2", len(store.batchArgs))
	}
	if store.inserted != 1 {
		t.Errorf("inserted = %d; want 1", store.inserted)
	}
}
