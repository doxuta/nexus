package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/dynrepo"
)

// ENG-REL2 #5 (1-1 relationship) unit tests for the CreateEntity path: registry row type,
// the shared-table reconcile branch (AddOneToOneRelationColumn / DropOneToOneRelationColumn),
// and its compensation. Mirrors entity_relation_registry_test.go / the bug3 reconcile tests
// in entity_orchestration_test.go.

func oneToOneEntityInput(parentKnown bool) CreateEntityInput {
	_ = parentKnown
	return CreateEntityInput{
		Name:  "profiles",
		Label: "Profiles",
		Fields: []FieldInput{
			{Name: "bio", Label: "Bio", FieldType: "text"},
			{Name: "user", Label: "User", FieldType: "relation",
				Options: map[string]any{"relatedEntity": "users", "cardinality": "1-1"}},
		},
	}
}

// TestCreateEntity_OneToOneRelationField_RegistersOneToOneRow: a relation field declared
// inline with cardinality "1-1" registers a companion sys_relationships row with
// RelationshipType "1-1" (not "1-N"), in the same CreateEntityWithFields transaction.
func TestCreateEntity_OneToOneRelationField_RegistersOneToOneRow(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{knownEntities: map[string]bool{"users": true}}
	svc := NewEntityService(schema, meta)

	if _, _, err := svc.CreateEntity(context.Background(), "tenant-1", oneToOneEntityInput(true)); err != nil {
		t.Fatalf("CreateEntity(1-1 relation field): %v", err)
	}
	if len(meta.createdRels) != 1 {
		t.Fatalf("expected 1 relationship row; got %d: %+v", len(meta.createdRels), meta.createdRels)
	}
	rel := meta.createdRels[0]
	if rel.RelationshipType != "1-1" {
		t.Errorf("rel.RelationshipType = %q; want 1-1", rel.RelationshipType)
	}
	if rel.ChildFKColumn != "user_id" {
		t.Errorf("rel.ChildFKColumn = %q; want user_id", rel.ChildFKColumn)
	}
	if rel.ParentEntityName != "users" || rel.ChildEntityName != "profiles" {
		t.Errorf("Parent/Child = %q/%q; want users/profiles", rel.ParentEntityName, rel.ChildEntityName)
	}
}

// TestCreateEntity_ReconcileSharedTable_OneToOne: obj_profiles pre-exists (another tenant
// registered it first) and lacks the `user_id` column. Reconcile must ALTER it in via
// AddOneToOneRelationColumn (the guarded+unique pair), NOT the plain AddRelationColumn.
func TestCreateEntity_ReconcileSharedTable_OneToOne(t *testing.T) {
	schema := &fakeSchema{existed: true, existingCols: map[string]bool{"bio": true}}
	meta := &fakeMeta{knownEntities: map[string]bool{"users": true}}
	svc := NewEntityService(schema, meta)

	if _, _, err := svc.CreateEntity(context.Background(), "tenant-2", oneToOneEntityInput(true)); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if len(schema.addedOneToOne) != 1 || schema.addedOneToOne[0] != "user" {
		t.Errorf("addedOneToOne = %v; want [user]", schema.addedOneToOne)
	}
	if len(schema.addedRelation) != 0 {
		t.Errorf("must NOT use the plain 1-N reconcile ALTER; addedRelation=%v", schema.addedRelation)
	}
}

// TestCreateEntity_ReconcileSkipsExistingColumn_OneToOne: when the `user_id` column is
// already present on the shared table, reconcile must skip the ALTER (idempotent — ADD
// COLUMN onto an existing column errors).
func TestCreateEntity_ReconcileSkipsExistingColumn_OneToOne(t *testing.T) {
	schema := &fakeSchema{existed: true, existingCols: map[string]bool{"bio": true, "user_id": true}}
	meta := &fakeMeta{knownEntities: map[string]bool{"users": true}}
	svc := NewEntityService(schema, meta)

	if _, _, err := svc.CreateEntity(context.Background(), "tenant-2", oneToOneEntityInput(true)); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if len(schema.addedOneToOne) != 0 {
		t.Errorf("addedOneToOne = %v; want none (column already present)", schema.addedOneToOne)
	}
}

// TestCreateEntity_ReconcileDuplicateUnique_OneToOne_Returns409: reconcile's
// AddOneToOneRelationColumn hits ER_DUP_ENTRY (dynrepo.ErrDuplicateUnique — existing rows on
// the shared table already violate 1-1 exclusivity for this tenant) -> 409
// FIELD_RELATION_CARDINALITY_CONFLICT, not a generic 500. No table drop (shared table).
func TestCreateEntity_ReconcileDuplicateUnique_OneToOne_Returns409(t *testing.T) {
	schema := &fakeSchema{
		existed:        true,
		existingCols:   map[string]bool{"bio": true},
		addOneToOneErr: map[string]error{"user": dynrepo.ErrDuplicateUnique},
	}
	meta := &fakeMeta{knownEntities: map[string]bool{"users": true}}
	svc := NewEntityService(schema, meta)

	_, _, err := svc.CreateEntity(context.Background(), "tenant-2", oneToOneEntityInput(true))
	if codeOf(err) != "FIELD_RELATION_CARDINALITY_CONFLICT" {
		t.Fatalf("code = %q; want FIELD_RELATION_CARDINALITY_CONFLICT", codeOf(err))
	}
	if meta.called {
		t.Error("metadata write must not run when reconcile fails")
	}
	if len(schema.dropped) != 0 {
		t.Errorf("dropped = %v; the shared table must never be dropped", schema.dropped)
	}
}

// TestCreateEntity_ReconcileCompensatesOnMetaFailure_OneToOne: on a pre-existing SHARED
// table, reconcile ALTERs the 1-1 column in, then the metadata write fails -> the column
// must be dropped via DropOneToOneRelationColumn (the matching pair).
func TestCreateEntity_ReconcileCompensatesOnMetaFailure_OneToOne(t *testing.T) {
	schema := &fakeSchema{existed: true, existingCols: map[string]bool{"bio": true}}
	meta := &fakeMeta{
		err:           errors.New("metadata write boom"),
		knownEntities: map[string]bool{"users": true},
	}
	svc := NewEntityService(schema, meta)

	_, _, err := svc.CreateEntity(context.Background(), "tenant-2", oneToOneEntityInput(true))
	if err == nil {
		t.Fatal("expected error when metadata write fails")
	}
	if len(schema.droppedOneToOne) != 1 || schema.droppedOneToOne[0] != "user" {
		t.Errorf("droppedOneToOne = %v; want [user]", schema.droppedOneToOne)
	}
	if len(schema.droppedRelation) != 0 {
		t.Errorf("must NOT use the plain 1-N drop; droppedRelation=%v", schema.droppedRelation)
	}
	if len(schema.dropped) != 0 {
		t.Errorf("dropped = %v; a pre-existing shared table must NOT be dropped", schema.dropped)
	}
}

// TestCreateEntity_RelationCardinalityInvalid_RejectsBeforeDDL: a bad options.cardinality on
// a relation field declared inline with a new entity is rejected 422
// FIELD_RELATION_CARDINALITY_INVALID before any DDL runs — no table created.
func TestCreateEntity_RelationCardinalityInvalid_RejectsBeforeDDL(t *testing.T) {
	schema := &fakeSchema{}
	meta := &fakeMeta{knownEntities: map[string]bool{"users": true}}
	svc := NewEntityService(schema, meta)

	in := oneToOneEntityInput(true)
	in.Fields[1].Options["cardinality"] = "N-N"

	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_RELATION_CARDINALITY_INVALID" {
		t.Fatalf("code = %q; want FIELD_RELATION_CARDINALITY_INVALID", codeOf(err))
	}
	if len(schema.created) != 0 {
		t.Errorf("no DDL should run when cardinality is invalid; created=%v", schema.created)
	}
	if meta.called {
		t.Error("metadata write must not run when cardinality is invalid")
	}
}
