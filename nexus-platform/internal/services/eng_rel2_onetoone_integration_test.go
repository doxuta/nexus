//go:build integration

// ENG-REL2 #5 (1-1 relationship, FK-on-child + tenant-scoped UNIQUE) end-to-end integration
// tests — exercised through EntityService/FieldService/RecordService over a freshly-migrated
// throwaway MySQL DB (engWire/o3aSetup harness from eng_computed_integration_test.go /
// metadata_cache_wired_integration_test.go / eng_rel2_registry_integration_test.go).
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestENGREL2OneToOne -race -v
package services

import (
	"context"
	"testing"
)

const eng11User = "11111111-1111-1111-1111-111111111102"

// setupOneToOneEntities registers "users" (parent) and "profiles" (child, with a 1-1
// `user` relation field) for the given tenant.
func setupOneToOneEntities(t *testing.T, entitySvc *EntityService, tenant string) {
	t.Helper()
	if _, _, err := entitySvc.CreateEntity(context.Background(), tenant, CreateEntityInput{
		Name:  "users",
		Label: "Users",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
		},
	}); err != nil {
		t.Fatalf("CreateEntity users: %v", err)
	}
	if _, _, err := entitySvc.CreateEntity(context.Background(), tenant, CreateEntityInput{
		Name:  "profiles",
		Label: "Profiles",
		Fields: []FieldInput{
			{Name: "bio", Label: "Bio", FieldType: "text"},
			{Name: "user", Label: "User", FieldType: "relation",
				Options: map[string]any{"relatedEntity": "users", "cardinality": "1-1"}},
		},
	}); err != nil {
		t.Fatalf("CreateEntity profiles: %v", err)
	}
}

// TestENGREL2OneToOne_BindThenSecondBindRejected_Integration is the core E2E acceptance: two
// profile records cannot both bind the SAME user (tenant-scoped UNIQUE on profiles.user_id
// enforces exclusivity) — the second insert is rejected 409 RECORD_RELATION_ALREADY_BOUND
// with details.field "user".
func TestENGREL2OneToOne_BindThenSecondBindRejected_Integration(t *testing.T) {
	entitySvc, _, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	setupOneToOneEntities(t, entitySvc, engTenantA)

	userID, err := recordSvc.Create(ctx, engAdmin(eng11User), engTenantA, eng11User, "users", map[string]any{
		"name": "Alice",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// First profile binds the user — must succeed.
	if _, err := recordSvc.Create(ctx, engAdmin(eng11User), engTenantA, eng11User, "profiles", map[string]any{
		"bio": "First profile", "user": userID,
	}); err != nil {
		t.Fatalf("create first profile (bind user): %v", err)
	}

	// Second profile binding the SAME user must be rejected — 1-1 exclusivity.
	_, err = recordSvc.Create(ctx, engAdmin(eng11User), engTenantA, eng11User, "profiles", map[string]any{
		"bio": "Second profile", "user": userID,
	})
	if err == nil {
		t.Fatal("expected the second profile bind to the SAME user to be rejected")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("err = %v (%T); want *ValidationError", err, err)
	}
	if ve.Code != "RECORD_RELATION_ALREADY_BOUND" {
		t.Errorf("code = %q; want RECORD_RELATION_ALREADY_BOUND", ve.Code)
	}
	if ve.Field != "user" {
		t.Errorf("Field = %q; want user", ve.Field)
	}
}

// TestENGREL2OneToOne_UpdateRebindRejected_Integration is the UPDATE-path parity case: the
// brief enforces 1-1 exclusivity on "INSERT/UPDATE" alike. Profile A is already bound to user
// P (live); a SEPARATE, unbound profile B is then UPDATEd (PUT) to bind the SAME user P — must
// be rejected the same way a colliding INSERT is (409 RECORD_RELATION_ALREADY_BOUND,
// details.field "user"), proving RecordService.Update's asDuplicate wrapping (record_
// orchestration.go ~line 1105) picks up the new 1-1 branch exactly like Create's.
func TestENGREL2OneToOne_UpdateRebindRejected_Integration(t *testing.T) {
	entitySvc, _, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	setupOneToOneEntities(t, entitySvc, engTenantA)

	userP, err := recordSvc.Create(ctx, engAdmin(eng11User), engTenantA, eng11User, "users", map[string]any{
		"name": "Carol",
	})
	if err != nil {
		t.Fatalf("create user P: %v", err)
	}
	userQ, err := recordSvc.Create(ctx, engAdmin(eng11User), engTenantA, eng11User, "users", map[string]any{
		"name": "Dave",
	})
	if err != nil {
		t.Fatalf("create user Q: %v", err)
	}

	// Profile A binds P — the live holder of P's 1-1 slot.
	if _, err := recordSvc.Create(ctx, engAdmin(eng11User), engTenantA, eng11User, "profiles", map[string]any{
		"bio": "Profile A", "user": userP,
	}); err != nil {
		t.Fatalf("create profile A (bind P): %v", err)
	}

	// Profile B starts bound to a DIFFERENT user (Q), so its own creation succeeds.
	profileB, err := recordSvc.Create(ctx, engAdmin(eng11User), engTenantA, eng11User, "profiles", map[string]any{
		"bio": "Profile B", "user": userQ,
	})
	if err != nil {
		t.Fatalf("create profile B (bind Q): %v", err)
	}

	// UPDATE profile B to rebind it to P — P's slot is already held (live) by profile A.
	err = recordSvc.Update(ctx, engAdmin(eng11User), engTenantA, "profiles", profileB, map[string]any{
		"user": userP,
	}, nil)
	if err == nil {
		t.Fatal("expected UPDATE rebinding profile B to an already-bound user P to be rejected")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("err = %v (%T); want *ValidationError", err, err)
	}
	if ve.Code != "RECORD_RELATION_ALREADY_BOUND" {
		t.Errorf("code = %q; want RECORD_RELATION_ALREADY_BOUND", ve.Code)
	}
	if ve.Field != "user" {
		t.Errorf("Field = %q; want user", ve.Field)
	}
}

// TestENGREL2OneToOne_SoftDeleteReleasesSlot_Integration proves the CASE-guard: soft-deleting
// the FIRST profile (which held the 1-1 slot for a user) releases it — a NEW profile can then
// bind the same user without hitting RECORD_RELATION_ALREADY_BOUND. Without the deleted_at
// CASE guard on the generated column, the soft-deleted row would keep squatting the unique
// slot forever.
func TestENGREL2OneToOne_SoftDeleteReleasesSlot_Integration(t *testing.T) {
	entitySvc, _, recordSvc, _, _ := engWire(t)
	ctx := context.Background()
	setupOneToOneEntities(t, entitySvc, engTenantA)

	userID, err := recordSvc.Create(ctx, engAdmin(eng11User), engTenantA, eng11User, "users", map[string]any{
		"name": "Bob",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	profileA, err := recordSvc.Create(ctx, engAdmin(eng11User), engTenantA, eng11User, "profiles", map[string]any{
		"bio": "Profile A", "user": userID,
	})
	if err != nil {
		t.Fatalf("create profile A (bind user): %v", err)
	}

	// A second bind attempt while A is live must still fail (sanity, mirrors the test above).
	if _, err := recordSvc.Create(ctx, engAdmin(eng11User), engTenantA, eng11User, "profiles", map[string]any{
		"bio": "Profile B (should fail)", "user": userID,
	}); err == nil {
		t.Fatal("expected bind to fail while profile A is still live")
	}

	// Soft-delete profile A — the CASE-guarded generated column must now project NULL for it,
	// releasing the unique slot.
	if err := recordSvc.Delete(ctx, engAdmin(eng11User), engTenantA, "profiles", profileA); err != nil {
		t.Fatalf("soft-delete profile A: %v", err)
	}

	// A NEW profile binding the SAME user must now succeed.
	profileB, err := recordSvc.Create(ctx, engAdmin(eng11User), engTenantA, eng11User, "profiles", map[string]any{
		"bio": "Profile B (after A soft-deleted)", "user": userID,
	})
	if err != nil {
		t.Fatalf("create profile B after A soft-deleted (slot must be released): %v", err)
	}
	if profileB == "" {
		t.Error("profile B should have been created with a non-empty id")
	}
}

// TestENGREL2OneToOne_RegistryRowType_Integration proves the sys_relationships row for a 1-1
// relation field is discoverable with RelationshipType "1-1" (not "1-N") through the SAME
// broadened GET /metadata/relationships lookups ENG-REL2 #4 wired.
func TestENGREL2OneToOne_RegistryRowType_Integration(t *testing.T) {
	entitySvc, _, _, _, metaRepo := engWire(t)
	ctx := context.Background()
	setupOneToOneEntities(t, entitySvc, engTenantA)

	rel, err := metaRepo.GetRelationshipByName(ctx, engTenantA, "profiles_user")
	if err != nil {
		t.Fatalf("GetRelationshipByName(profiles_user): %v", err)
	}
	if rel.RelationshipType != "1-1" {
		t.Errorf("RelationshipType = %q; want 1-1", rel.RelationshipType)
	}
	if rel.ChildFKColumn != "user_id" {
		t.Errorf("ChildFKColumn = %q; want user_id", rel.ChildFKColumn)
	}
	if rel.InverseField != "profiles" {
		t.Errorf("InverseField = %q; want profiles (default = child entity name)", rel.InverseField)
	}

	byInverse, err := metaRepo.GetRelationshipByInverseField(ctx, engTenantA, "users", "profiles")
	if err != nil {
		t.Fatalf("GetRelationshipByInverseField: %v", err)
	}
	if byInverse.Name != "profiles_user" || byInverse.RelationshipType != "1-1" {
		t.Errorf("GetRelationshipByInverseField = %+v; want profiles_user/1-1", byInverse)
	}
}

// TestENGREL2OneToOne_EntityRecreate_AfterSoftDelete_Succeeds_Integration mirrors ENG-REL2
// #4's entity-recreate regression test (TestENGREL2_EntityRecreate_AfterSoftDelete_Succeeds_
// Integration) for the 1-1 shape: soft-deleting the CHILD entity must ALSO tombstone the 1-1
// registry row it owns, so recreating the entity with the same 1-1 field succeeds instead of
// permanently bricking on 409 RELATIONSHIP_ALREADY_EXISTS.
func TestENGREL2OneToOne_EntityRecreate_AfterSoftDelete_Succeeds_Integration(t *testing.T) {
	entitySvc, _, _, _, metaRepo := engWire(t)
	ctx := context.Background()

	if _, _, err := entitySvc.CreateEntity(ctx, engTenantA, CreateEntityInput{
		Name:  "rc11_parents",
		Label: "RC11 Parents",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
		},
	}); err != nil {
		t.Fatalf("CreateEntity rc11_parents: %v", err)
	}
	childInput := CreateEntityInput{
		Name:  "rc11_children",
		Label: "RC11 Children",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
			{Name: "parent", Label: "Parent", FieldType: "relation",
				Options: map[string]any{"relatedEntity": "rc11_parents", "cardinality": "1-1"}},
		},
	}
	if _, _, err := entitySvc.CreateEntity(ctx, engTenantA, childInput); err != nil {
		t.Fatalf("CreateEntity rc11_children (first): %v", err)
	}
	if rel, err := metaRepo.GetRelationshipByName(ctx, engTenantA, "rc11_children_parent"); err != nil {
		t.Fatalf("registry row must exist after first create: %v", err)
	} else if rel.RelationshipType != "1-1" {
		t.Fatalf("registry row type = %q; want 1-1", rel.RelationshipType)
	}

	if err := entitySvc.SoftDeleteEntity(ctx, engTenantA, "rc11_children"); err != nil {
		t.Fatalf("SoftDeleteEntity rc11_children: %v", err)
	}
	if _, err := metaRepo.GetRelationshipByName(ctx, engTenantA, "rc11_children_parent"); err == nil {
		t.Error("GetRelationshipByName must not resolve the owned 1-1 row after entity soft-delete")
	}

	// Recreate with the SAME name + SAME 1-1 relation field must succeed (RED before the
	// SoftDeleteEntity broaden-to-1-1 fix: 409 RELATIONSHIP_ALREADY_EXISTS).
	if _, _, err := entitySvc.CreateEntity(ctx, engTenantA, childInput); err != nil {
		t.Fatalf("CreateEntity rc11_children (recreate after soft-delete) must succeed: %v", err)
	}
	rel, err := metaRepo.GetRelationshipByName(ctx, engTenantA, "rc11_children_parent")
	if err != nil {
		t.Fatalf("registry row must exist again after recreate: %v", err)
	}
	if rel.RelationshipType != "1-1" || rel.ChildFKColumn != "parent_id" || rel.InverseField != "rc11_children" {
		t.Errorf("recreated registry row = %+v; want 1-1 parent_id inverse=rc11_children", rel)
	}
}

// TestENGREL2OneToOne_DeleteField_CleansUpRegistryRow_Integration mirrors ENG-REL2 #4's field
// registry cleanup for the 1-1 shape: soft-deleting the `relation` field itself soft-deletes
// its 1-1 registry row.
func TestENGREL2OneToOne_DeleteField_CleansUpRegistryRow_Integration(t *testing.T) {
	entitySvc, fieldSvc, _, _, metaRepo := engWire(t)
	ctx := context.Background()
	setupOneToOneEntities(t, entitySvc, engTenantA)

	profilesEnt, err := metaRepo.GetEntityByName(ctx, engTenantA, "profiles")
	if err != nil {
		t.Fatalf("GetEntityByName profiles: %v", err)
	}
	fields, err := metaRepo.ListFields(ctx, engTenantA, profilesEnt.ID)
	if err != nil {
		t.Fatalf("ListFields profiles: %v", err)
	}
	var userFieldID string
	for _, f := range fields {
		if f.Name == "user" {
			userFieldID = f.ID
		}
	}
	if userFieldID == "" {
		t.Fatal("could not resolve profiles.user field id")
	}

	if err := fieldSvc.DeleteField(ctx, engTenantA, DeleteFieldInput{FieldID: userFieldID}); err != nil {
		t.Fatalf("DeleteField(profiles.user): %v", err)
	}
	if _, err := metaRepo.GetRelationshipByName(ctx, engTenantA, "profiles_user"); err == nil {
		t.Error("registry row must be gone after the owning 1-1 field is soft-deleted")
	}
}

// TestENGREL2OneToOne_TwoTenants_DoNotBlockEachOther_Integration (security audit LOW-2):
// the 1-1 UNIQUE key is (tenant_id, <f>_id) with tenant_id LEADING — two tenants sharing the
// physical obj_* table may each bind a child to the SAME parent id without colliding.
// Tenant isolation is the platform's #1 risk, so the structural guarantee (composite key)
// gets an empirical pin here: tenant B's bind must succeed even though tenant A already
// bound the same parent-id VALUE, and tenant B's second bind must still be rejected within
// its own tenant.
func TestENGREL2OneToOne_TwoTenants_DoNotBlockEachOther_Integration(t *testing.T) {
	entitySvc, _, recordSvc, dynRepo, _ := engWire(t)
	ctx := context.Background()
	setupOneToOneEntities(t, entitySvc, engTenantA)
	setupOneToOneEntities(t, entitySvc, engTenantB)

	userA, err := recordSvc.Create(ctx, engAdmin(eng11User), engTenantA, eng11User, "users", map[string]any{
		"name": "Alice A",
	})
	if err != nil {
		t.Fatalf("create user (tenant A): %v", err)
	}
	if _, err := recordSvc.Create(ctx, engAdmin(eng11User), engTenantA, eng11User, "profiles", map[string]any{
		"bio": "A's profile", "user": userA,
	}); err != nil {
		t.Fatalf("bind profile (tenant A): %v", err)
	}

	// The true pin needs the SAME <f>_id VALUE in both tenants: if the UNIQUE key were NOT
	// tenant-scoped (i.e. on user_id alone), a tenant-B row projecting user_id = userA would
	// trip 1062 against tenant A's live binding above. Service-layer relation validation
	// (correctly) refuses a cross-tenant target id, so this goes one layer down —
	// dynrepo.InsertRecord writes exactly the row shape the service would, and the UNIQUE
	// constraint lives at the DB layer, not the service layer.
	if _, err := dynRepo.InsertRecord(ctx, "profiles", engTenantB, eng11User, map[string]any{
		"bio": "B's row projecting the SAME user_id value", "user": userA,
	}, nil); err != nil {
		t.Fatalf("tenant B inserting the SAME user_id value must not be blocked by tenant A (tenant-scoped UNIQUE): %v", err)
	}

	// And in-tenant exclusivity still holds for tenant B: a SECOND tenant-B row projecting
	// the same value must trip the (tenant_id, user_id) key.
	if _, err := dynRepo.InsertRecord(ctx, "profiles", engTenantB, eng11User, map[string]any{
		"bio": "B's second row", "user": userA,
	}, nil); err == nil {
		t.Fatal("tenant B's second row with the same user_id must still be rejected (in-tenant 1-1)")
	}
}
