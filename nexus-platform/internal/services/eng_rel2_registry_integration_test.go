//go:build integration

// ENG-REL2 #4 (bidirectional / inverse-field relationship registry, migration 000028)
// end-to-end integration tests — exercised through EntityService/FieldService/RelationService
// over a freshly-migrated throwaway MySQL DB (engWire/o3aSetup harness from
// eng_computed_integration_test.go / metadata_cache_wired_integration_test.go).
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestENGREL2 -race -v
package services

import (
	"context"
	"testing"

	"nexus/platform/internal/metadata"
)

// containsRelName reports whether rels includes a row with the given name — a local helper
// mirroring internal/metadata's test-only helper of the same purpose (different package, no
// shared symbol).
func containsRelName(rels []metadata.RelationshipDefinition, name string) bool {
	for _, r := range rels {
		if r.Name == name {
			return true
		}
	}
	return false
}

// TestENGREL2_AddField_RegistersOneToNRelationship_Integration proves the full stack: a
// `relation` field added via AddField (real MySQL, real GORM transaction) is discoverable
// through GetRelationshipByName / ListRelationshipsForEntity / ListAllRelationships — the SAME
// broadened lookups the handler layer (GET /metadata/relationships) uses.
func TestENGREL2_AddField_RegistersOneToNRelationship_Integration(t *testing.T) {
	entitySvc, fieldSvc, _, _, metaRepo := engWire(t)
	ctx := context.Background()

	if _, _, err := entitySvc.CreateEntity(ctx, engTenantA, CreateEntityInput{
		Name:  "accounts",
		Label: "Accounts",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
		},
	}); err != nil {
		t.Fatalf("CreateEntity accounts: %v", err)
	}
	if _, _, err := entitySvc.CreateEntity(ctx, engTenantA, CreateEntityInput{
		Name:  "contacts",
		Label: "Contacts",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
		},
	}); err != nil {
		t.Fatalf("CreateEntity contacts: %v", err)
	}

	if _, err := fieldSvc.AddField(ctx, engTenantA, AddFieldInput{
		EntityName: "contacts",
		Field: FieldInput{
			Name: "account", Label: "Account", FieldType: "relation",
			Options: map[string]any{"relatedEntity": "accounts"},
		},
	}); err != nil {
		t.Fatalf("AddField(relation): %v", err)
	}

	rel, err := metaRepo.GetRelationshipByName(ctx, engTenantA, "contacts_account")
	if err != nil {
		t.Fatalf("GetRelationshipByName(contacts_account): %v", err)
	}
	if rel.RelationshipType != "1-N" {
		t.Errorf("RelationshipType = %q; want 1-N", rel.RelationshipType)
	}
	if rel.ParentEntityName != "accounts" || rel.ChildEntityName != "contacts" {
		t.Errorf("Parent/Child = %q/%q; want accounts/contacts", rel.ParentEntityName, rel.ChildEntityName)
	}
	if rel.ChildFKColumn != "account_id" {
		t.Errorf("ChildFKColumn = %q; want account_id", rel.ChildFKColumn)
	}
	if rel.InverseField != "contacts" {
		t.Errorf("InverseField = %q; want contacts (default = child entity name)", rel.InverseField)
	}
	if rel.JoinTableName != nil {
		t.Errorf("JoinTableName = %v; want nil (1-N has no join table)", rel.JoinTableName)
	}

	byEntity, err := metaRepo.ListRelationshipsForEntity(ctx, engTenantA, "accounts")
	if err != nil {
		t.Fatalf("ListRelationshipsForEntity: %v", err)
	}
	if !containsRelName(byEntity, "contacts_account") {
		t.Errorf("ListRelationshipsForEntity(accounts) did not surface the 1-N row")
	}

	all, err := metaRepo.ListAllRelationships(ctx, engTenantA)
	if err != nil {
		t.Fatalf("ListAllRelationships: %v", err)
	}
	if !containsRelName(all, "contacts_account") {
		t.Errorf("ListAllRelationships did not surface the 1-N row")
	}

	byInverse, err := metaRepo.GetRelationshipByInverseField(ctx, engTenantA, "accounts", "contacts")
	if err != nil {
		t.Fatalf("GetRelationshipByInverseField: %v", err)
	}
	if byInverse.Name != "contacts_account" {
		t.Errorf("GetRelationshipByInverseField.Name = %q; want contacts_account", byInverse.Name)
	}
}

// TestENGREL2_RegisterRelationship_NameCollidesWithOneToNRow_Rejected proves the existing
// (tenant_id, name) unique index (migration 000025) already serializes a name collision
// between an admin-registered N-N relationship and a 1-N registry row written by AddField —
// the error surface must stay the existing 409 RELATIONSHIP_ALREADY_EXISTS, not a raw 500.
func TestENGREL2_RegisterRelationship_NameCollidesWithOneToNRow_Rejected(t *testing.T) {
	entitySvc, fieldSvc, _, dynRepo, metaRepo := engWire(t)
	ctx := context.Background()

	for _, name := range []string{"accounts", "contacts", "deals"} {
		if _, _, err := entitySvc.CreateEntity(ctx, engTenantA, CreateEntityInput{
			Name:  name,
			Label: name,
			Fields: []FieldInput{
				{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
			},
		}); err != nil {
			t.Fatalf("CreateEntity %s: %v", name, err)
		}
	}

	// AddField registers the 1-N row "contacts_account" (parent=accounts, child=contacts).
	if _, err := fieldSvc.AddField(ctx, engTenantA, AddFieldInput{
		EntityName: "contacts",
		Field: FieldInput{
			Name: "account", Label: "Account", FieldType: "relation",
			Options: map[string]any{"relatedEntity": "accounts"},
		},
	}); err != nil {
		t.Fatalf("AddField(relation): %v", err)
	}

	// Now register an UNRELATED N-N relationship (deals <-> contacts) but reuse the SAME name
	// the 1-N row already claimed ("contacts_account"). The unique index on (tenant_id, name)
	// must reject this as a duplicate — the same 409 code as a name collision between two N-N
	// relationships.
	relSvc := NewRelationService(dynRepo, metaRepo)
	_, err := relSvc.RegisterRelationship(ctx, engTenantA, RegisterRelationshipInput{
		Name: "contacts_account", EntityA: "deals", EntityB: "contacts",
	})
	if err == nil {
		t.Fatal("expected an error registering an N-N relationship whose name collides with an existing 1-N registry row")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("err = %v (%T); want *ValidationError", err, err)
	}
	if ve.Code != "RELATIONSHIP_ALREADY_EXISTS" {
		t.Errorf("code = %q; want RELATIONSHIP_ALREADY_EXISTS", ve.Code)
	}

	// The pre-existing 1-N row must be untouched by the rejected attempt.
	rel, err := metaRepo.GetRelationshipByName(ctx, engTenantA, "contacts_account")
	if err != nil {
		t.Fatalf("GetRelationshipByName after rejected collision: %v", err)
	}
	if rel.RelationshipType != "1-N" {
		t.Errorf("existing row's type changed to %q after rejected collision; want unchanged 1-N", rel.RelationshipType)
	}
}

// TestENGREL2_EntityRecreate_AfterSoftDelete_Succeeds_Integration (security audit MED):
// soft-deleting an entity must ALSO tombstone + soft-delete the 1-N registry rows OWNED by
// that entity's relation fields — otherwise the leftover active row keeps the
// (tenant_id, name) slot of uq_sysrel_tenant_name occupied forever and recreating the entity
// with the same relation field (the vertical-pack rollback→retry path, or a plain admin
// delete→recreate) is permanently bricked with 409 RELATIONSHIP_ALREADY_EXISTS.
func TestENGREL2_EntityRecreate_AfterSoftDelete_Succeeds_Integration(t *testing.T) {
	entitySvc, _, _, _, metaRepo := engWire(t)
	ctx := context.Background()

	if _, _, err := entitySvc.CreateEntity(ctx, engTenantA, CreateEntityInput{
		Name:  "rc_parents",
		Label: "RC Parents",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
		},
	}); err != nil {
		t.Fatalf("CreateEntity rc_parents: %v", err)
	}
	childInput := CreateEntityInput{
		Name:  "rc_children",
		Label: "RC Children",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
			{Name: "parent", Label: "Parent", FieldType: "relation",
				Options: map[string]any{"relatedEntity": "rc_parents"}},
		},
	}
	if _, _, err := entitySvc.CreateEntity(ctx, engTenantA, childInput); err != nil {
		t.Fatalf("CreateEntity rc_children (first): %v", err)
	}
	// Sanity: the registry row landed.
	if _, err := metaRepo.GetRelationshipByName(ctx, engTenantA, "rc_children_parent"); err != nil {
		t.Fatalf("registry row must exist after first create: %v", err)
	}

	if err := entitySvc.SoftDeleteEntity(ctx, engTenantA, "rc_children"); err != nil {
		t.Fatalf("SoftDeleteEntity rc_children: %v", err)
	}

	// The owned 1-N row must be gone from every ACTIVE lookup surface...
	if _, err := metaRepo.GetRelationshipByName(ctx, engTenantA, "rc_children_parent"); err == nil {
		t.Error("GetRelationshipByName must not resolve the owned 1-N row after entity soft-delete")
	}
	if _, err := metaRepo.GetRelationshipByInverseField(ctx, engTenantA, "rc_parents", "rc_children"); err == nil {
		t.Error("inverse-field claim must be released after entity soft-delete")
	}
	all, err := metaRepo.ListAllRelationships(ctx, engTenantA)
	if err != nil {
		t.Fatalf("ListAllRelationships: %v", err)
	}
	if containsRelName(all, "rc_children_parent") {
		t.Error("ListAllRelationships must not advertise a phantom edge for the deleted entity's owned relation")
	}

	// ...and the recreate with the SAME name + SAME relation field must succeed (this is the
	// exact vertical-pack retry repro; RED before the SoftDeleteEntity cleanup fix: 409
	// RELATIONSHIP_ALREADY_EXISTS from uq_sysrel_tenant_name).
	if _, _, err := entitySvc.CreateEntity(ctx, engTenantA, childInput); err != nil {
		t.Fatalf("CreateEntity rc_children (recreate after soft-delete) must succeed: %v", err)
	}
	rel, err := metaRepo.GetRelationshipByName(ctx, engTenantA, "rc_children_parent")
	if err != nil {
		t.Fatalf("registry row must exist again after recreate: %v", err)
	}
	if rel.RelationshipType != "1-N" || rel.ChildFKColumn != "parent_id" || rel.InverseField != "rc_children" {
		t.Errorf("recreated registry row = %+v; want 1-N parent_id inverse=rc_children", rel)
	}
}

// TestENGREL2_EntitySoftDelete_SweepsParentSideRows_Integration (REL-RM21-NN — behavior
// FLIPPED from the original KeepsParentSideRows contract): rows where the deleted entity is
// the PARENT (some OTHER entity's relation field points at it) are now tombstoned +
// soft-deleted WITH the entity. Keeping them active was the phantom-edge bug: the row kept
// surfacing through ListAllRelationships → GET /metadata/relationships → a ghost ERD/Related
// edge pointing at an entity that no longer exists. What IS still deferred (by design) is the
// sys_fields side: the owning relation FIELD on the other entity (ps_owners.target) survives
// as a harmless orphan — writing a value into it fails record validation (target not found)
// and an admin can delete it.
func TestENGREL2_EntitySoftDelete_SweepsParentSideRows_Integration(t *testing.T) {
	entitySvc, _, _, _, metaRepo := engWire(t)
	ctx := context.Background()

	if _, _, err := entitySvc.CreateEntity(ctx, engTenantA, CreateEntityInput{
		Name:  "ps_targets",
		Label: "PS Targets",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
		},
	}); err != nil {
		t.Fatalf("CreateEntity ps_targets: %v", err)
	}
	if _, _, err := entitySvc.CreateEntity(ctx, engTenantA, CreateEntityInput{
		Name:  "ps_owners",
		Label: "PS Owners",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
			{Name: "target", Label: "Target", FieldType: "relation",
				Options: map[string]any{"relatedEntity": "ps_targets"}},
		},
	}); err != nil {
		t.Fatalf("CreateEntity ps_owners: %v", err)
	}

	// Delete the PARENT (target) entity: the registry row must go with it (no ghost edge).
	if err := entitySvc.SoftDeleteEntity(ctx, engTenantA, "ps_targets"); err != nil {
		t.Fatalf("SoftDeleteEntity ps_targets: %v", err)
	}
	if _, err := metaRepo.GetRelationshipByName(ctx, engTenantA, "ps_owners_target"); err == nil {
		t.Error("parent-side row must be swept with the deleted parent entity (phantom ERD edge otherwise)")
	}
	all, err := metaRepo.ListAllRelationships(ctx, engTenantA)
	if err != nil {
		t.Fatalf("ListAllRelationships: %v", err)
	}
	if containsRelName(all, "ps_owners_target") {
		t.Error("ListAllRelationships must not advertise a ghost edge toward the deleted ps_targets")
	}

	// The honest leftover: the owning relation FIELD on ps_owners still exists (sys_fields
	// orphan cleanup deferred by design).
	owners, err := metaRepo.GetEntityByName(ctx, engTenantA, "ps_owners")
	if err != nil {
		t.Fatalf("GetEntityByName ps_owners: %v", err)
	}
	fields, err := metaRepo.ListFields(ctx, engTenantA, owners.ID)
	if err != nil {
		t.Fatalf("ListFields ps_owners: %v", err)
	}
	foundTarget := false
	for _, f := range fields {
		if f.Name == "target" {
			foundTarget = true
		}
	}
	if !foundTarget {
		t.Error("ps_owners.target relation field should survive the parent delete (documented deferred orphan)")
	}
}
