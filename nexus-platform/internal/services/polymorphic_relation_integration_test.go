//go:build integration

// ACTIVITY-CORE slice 1 — polymorphic_relation field type, end-to-end over real MySQL
// (engWire harness). These are the DISCRIMINATING tests the spec (§8) mandates: the cross-tenant
// existence probe is the bug-class gate (a record must NOT be able to relate to another tenant's
// record), plus target-allowlist membership, atomic shape, and the timeline filter over the two
// STORED generated sub-columns.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestPolymorphic -v
package services

import (
	"context"
	"testing"
)

// polyUser is the acting principal for these tests (admin scope via engAdmin).
const polyUser = "eeeeeeee-0e0e-0e0e-0e0e-eeeeeeeeeeee"

// setupPolymorphic creates `accounts` (target) + `tasks` (a NON-target entity, for the membership
// test) + `activities` (with a polymorphic `related_to` → [accounts]) in tenant A, and `accounts`
// in tenant B. Returns the record ids needed by the assertions.
func setupPolymorphic(t *testing.T, entitySvc *EntityService, recordSvc *RecordService) (accA, accB string) {
	t.Helper()
	ctx := context.Background()

	mkEntity := func(tenant, name string) {
		if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
			Name: name, Label: name,
			Fields: []FieldInput{{Name: "name", Label: "Name", FieldType: "text", IsRequired: true}},
		}); err != nil {
			t.Fatalf("CreateEntity %s/%s: %v", tenant, name, err)
		}
	}
	mkEntity(engTenantA, "accounts")
	mkEntity(engTenantA, "tasks")
	mkEntity(engTenantB, "accounts")

	// activities.related_to → polymorphic, targets [accounts] ONLY (tasks deliberately excluded).
	if _, _, err := entitySvc.CreateEntity(ctx, engTenantA, CreateEntityInput{
		Name: "activities", Label: "Activities",
		Fields: []FieldInput{
			{Name: "subject", Label: "Subject", FieldType: "text", IsRequired: true},
			{Name: "related_to", Label: "Related To", FieldType: "polymorphic_relation",
				Options: map[string]any{"targetEntities": []any{"accounts"}}},
		},
	}); err != nil {
		t.Fatalf("CreateEntity activities: %v", err)
	}

	var err error
	accA, err = recordSvc.Create(ctx, engAdmin(polyUser), engTenantA, polyUser, "accounts", map[string]any{"name": "Acme A"})
	if err != nil {
		t.Fatalf("create accA: %v", err)
	}
	accB, err = recordSvc.Create(ctx, engAdmin(polyUser), engTenantB, polyUser, "accounts", map[string]any{"name": "Acme B"})
	if err != nil {
		t.Fatalf("create accB: %v", err)
	}
	return accA, accB
}

// TestPolymorphic_CrossTenantExistence is THE bug-class gate: tenant A cannot attach an activity to
// tenant B's account, even with B's real account id. The existence probe is tenant-scoped.
func TestPolymorphic_CrossTenantExistence(t *testing.T) {
	entitySvc, _, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	accA, accB := setupPolymorphic(t, entitySvc, recordSvc)

	// A → B's account id (a REAL id, but belongs to tenant B) MUST be rejected.
	_, err := recordSvc.Create(ctx, engAdmin(polyUser), engTenantA, polyUser, "activities", map[string]any{
		"subject": "call", "related_to": map[string]any{"type": "accounts", "id": accB},
	})
	if codeOf(err) != "RECORD_POLYMORPHIC_TARGET_INVALID" {
		t.Fatalf("CROSS-TENANT LEAK: A relating to B's account id = %q; want RECORD_POLYMORPHIC_TARGET_INVALID", codeOf(err))
	}

	// A → its OWN account succeeds (control).
	id, err := recordSvc.Create(ctx, engAdmin(polyUser), engTenantA, polyUser, "activities", map[string]any{
		"subject": "call", "related_to": map[string]any{"type": "accounts", "id": accA},
	})
	if err != nil {
		t.Fatalf("A relating to its own account should succeed: %v", err)
	}
	if id == "" {
		t.Fatal("expected an activity id")
	}
}

// TestPolymorphic_TargetAllowlist proves a `type` outside options.targetEntities is rejected even
// when it is a real entity in the same tenant (tasks exists in A but is not a target).
func TestPolymorphic_TargetAllowlist(t *testing.T) {
	entitySvc, _, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	_, _ = setupPolymorphic(t, entitySvc, recordSvc)

	_, err := recordSvc.Create(ctx, engAdmin(polyUser), engTenantA, polyUser, "activities", map[string]any{
		"subject": "x", "related_to": map[string]any{"type": "tasks", "id": "11111111-1111-1111-1111-111111111111"},
	})
	if codeOf(err) != "RECORD_POLYMORPHIC_TARGET_INVALID" {
		t.Fatalf("type outside allowlist = %q; want RECORD_POLYMORPHIC_TARGET_INVALID", codeOf(err))
	}
}

// TestPolymorphic_AtomicShape proves a {type} without {id} is rejected (atomic-write contract).
func TestPolymorphic_AtomicShape(t *testing.T) {
	entitySvc, _, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	accA, _ := setupPolymorphic(t, entitySvc, recordSvc)

	_, err := recordSvc.Create(ctx, engAdmin(polyUser), engTenantA, polyUser, "activities", map[string]any{
		"subject": "x", "related_to": map[string]any{"type": "accounts"}, // id missing
	})
	if err == nil {
		t.Fatal("a {type} without {id} must be rejected (atomic)")
	}
	// A fully-absent related_to (not required) is fine.
	if _, err := recordSvc.Create(ctx, engAdmin(polyUser), engTenantA, polyUser, "activities", map[string]any{
		"subject": "no relation",
	}); err != nil {
		t.Fatalf("absent optional related_to should be OK: %v", err)
	}
	_ = accA
}

// TestFieldFlagGuards_EditPathParity proves EDITFIELD-FLAG-PARITY: the shared validateFieldFlagGuards
// now runs on the edit path for a SIBLING no-scalar type too, so editing a file field with
// options{indexed:true} returns 422 FIELD_FILE_NOT_INDEXED — not the opaque 500 the edit path
// produced before the guard was factored (the reviewer's out-of-scope sibling gap, now closed).
func TestFieldFlagGuards_EditPathParity(t *testing.T) {
	entitySvc, fieldSvc, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	_, _ = setupPolymorphic(t, entitySvc, recordSvc)

	fd, err := fieldSvc.AddField(ctx, engTenantA, AddFieldInput{
		EntityName: "tasks",
		Field:      FieldInput{Name: "attachment", Label: "Attachment", FieldType: "file"},
	})
	if err != nil {
		t.Fatalf("AddField(file): %v", err)
	}
	_, err = fieldSvc.EditField(ctx, engTenantA, EditFieldInput{
		FieldID: fd.ID,
		Options: map[string]any{"indexed": true},
	})
	if codeOf(err) != "FIELD_FILE_NOT_INDEXED" {
		t.Fatalf("edit file field with indexed:true code = %q; want FIELD_FILE_NOT_INDEXED (not opaque 500)", codeOf(err))
	}
}

// TestPolymorphic_TargetNotFound proves a targetEntities naming a non-existent entity is rejected
// at add-field time (config-time, tenant-scoped) — W1 coverage for the per-target existence loop.
func TestPolymorphic_TargetNotFound(t *testing.T) {
	entitySvc, fieldSvc, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	_, _ = setupPolymorphic(t, entitySvc, recordSvc)

	// accounts exists; "ghost" does not → the whole add is rejected.
	_, err := fieldSvc.AddField(ctx, engTenantA, AddFieldInput{
		EntityName: "tasks",
		Field: FieldInput{Name: "rel", Label: "Rel", FieldType: "polymorphic_relation",
			Options: map[string]any{"targetEntities": []any{"accounts", "ghost"}}},
	})
	if codeOf(err) != "FIELD_POLYMORPHIC_TARGET_NOT_FOUND" {
		t.Fatalf("code = %q; want FIELD_POLYMORPHIC_TARGET_NOT_FOUND", codeOf(err))
	}
}

// TestPolymorphic_EditIndexedRejected is the B1 regression: editing a polymorphic field with
// options{indexed:true} must return 422 FIELD_POLYMORPHIC_NOT_INDEXED, not an opaque 500 from the
// indexed-DDL branch. Exercises the EDIT path's flag guard.
func TestPolymorphic_EditIndexedRejected(t *testing.T) {
	entitySvc, fieldSvc, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	_, _ = setupPolymorphic(t, entitySvc, recordSvc)

	fd, err := fieldSvc.AddField(ctx, engTenantA, AddFieldInput{
		EntityName: "tasks",
		Field: FieldInput{Name: "linked", Label: "Linked", FieldType: "polymorphic_relation",
			Options: map[string]any{"targetEntities": []any{"accounts"}}},
	})
	if err != nil {
		t.Fatalf("AddField(polymorphic): %v", err)
	}
	_, err = fieldSvc.EditField(ctx, engTenantA, EditFieldInput{
		FieldID: fd.ID,
		Options: map[string]any{"targetEntities": []any{"accounts"}, "indexed": true},
	})
	if codeOf(err) != "FIELD_POLYMORPHIC_NOT_INDEXED" {
		t.Fatalf("edit with indexed:true code = %q; want FIELD_POLYMORPHIC_NOT_INDEXED (B1: not opaque 500)", codeOf(err))
	}
}

// TestPolymorphic_PutAtomic proves the atomic contract holds on PUT: a {type} without {id} is
// rejected on update, and omitting related_to preserves the stored value (W2 coverage).
func TestPolymorphic_PutAtomic(t *testing.T) {
	entitySvc, _, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	accA, _ := setupPolymorphic(t, entitySvc, recordSvc)

	id, err := recordSvc.Create(ctx, engAdmin(polyUser), engTenantA, polyUser, "activities", map[string]any{
		"subject": "a", "related_to": map[string]any{"type": "accounts", "id": accA},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// PUT with a half-pair → rejected (atomic).
	if err := recordSvc.Update(ctx, engAdmin(polyUser), engTenantA, "activities", id,
		map[string]any{"related_to": map[string]any{"type": "accounts"}}, nil); err == nil {
		t.Fatal("PUT with {type} but no {id} must be rejected (atomic)")
	}
	// PUT omitting related_to preserves the stored pair.
	if err := recordSvc.Update(ctx, engAdmin(polyUser), engTenantA, "activities", id,
		map[string]any{"subject": "renamed"}, nil); err != nil {
		t.Fatalf("PUT without related_to should succeed: %v", err)
	}
	rec, err := recordSvc.Get(ctx, engAdmin(polyUser), engTenantA, "activities", id, nil, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	rel, ok := rec.Data["related_to"].(map[string]any)
	if !ok || rel["id"] != accA {
		t.Fatalf("related_to not preserved across a PUT that omitted it: %v", rec.Data["related_to"])
	}
}

// TestPolymorphic_TimelineFilter proves the two STORED gen-columns materialize and are filterable —
// the "timeline for account X" query — and that it stays tenant-scoped.
func TestPolymorphic_TimelineFilter(t *testing.T) {
	entitySvc, _, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	accA, _ := setupPolymorphic(t, entitySvc, recordSvc)

	for i := 0; i < 3; i++ {
		if _, err := recordSvc.Create(ctx, engAdmin(polyUser), engTenantA, polyUser, "activities", map[string]any{
			"subject": "act", "related_to": map[string]any{"type": "accounts", "id": accA},
		}); err != nil {
			t.Fatalf("create activity %d: %v", i, err)
		}
	}
	// One activity attached to nothing (should NOT match the timeline filter).
	if _, err := recordSvc.Create(ctx, engAdmin(polyUser), engTenantA, polyUser, "activities", map[string]any{
		"subject": "orphan",
	}); err != nil {
		t.Fatalf("create orphan: %v", err)
	}

	// Timeline = List filtered by the two sub-columns.
	res, err := recordSvc.List(ctx, engAdmin(polyUser), engTenantA, "activities", ListQuery{
		Page: 1, Limit: 50,
		Filters: []FilterInput{
			{Field: "related_to_type", Operator: "eq", Values: []string{"accounts"}},
			{Field: "related_to_id", Operator: "eq", Values: []string{accA}},
		},
	})
	if err != nil {
		t.Fatalf("timeline List: %v", err)
	}
	if res.Total != 3 {
		t.Fatalf("timeline total = %d; want 3 (the orphan must not match)", res.Total)
	}

	// A non-matching id → zero.
	resNone, err := recordSvc.List(ctx, engAdmin(polyUser), engTenantA, "activities", ListQuery{
		Page: 1, Limit: 50,
		Filters: []FilterInput{
			{Field: "related_to_type", Operator: "eq", Values: []string{"accounts"}},
			{Field: "related_to_id", Operator: "eq", Values: []string{"99999999-9999-9999-9999-999999999999"}},
		},
	})
	if err != nil {
		t.Fatalf("timeline List (no match): %v", err)
	}
	if resNone.Total != 0 {
		t.Fatalf("timeline for a non-existent account total = %d; want 0", resNone.Total)
	}
}
