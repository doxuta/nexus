package services

import (
	"context"
	"testing"

	"nexus/platform/internal/metadata"
)

// ENG-REL2 #4 unit tests: AddField's 1-N relationship registry write (name/child_fk_column/
// inverse-field naming rule) and DeleteField's registry cleanup. Mirrors the fakeFieldMeta /
// relMeta() harness in field_relation_orchestration_test.go.

// TestAddField_Relation_RegistersInverseRow_DefaultName is the core acceptance: adding a
// relation field with NO options.inverseField registers a companion 1-N sys_relationships row
// (via CreateFieldWithRelationship, atomically with the field) whose name is
// "<childEntity>_<fieldName>", child_fk_column is "<fieldName>_id", and inverse_field defaults
// to the owning (child) entity name.
func TestAddField_Relation_RegistersInverseRow_DefaultName(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := relMeta()
	svc := NewFieldService(schema, meta)

	fd, err := svc.AddField(context.Background(), "tenant-1", addRelationField())
	if err != nil {
		t.Fatalf("AddField(relation): %v", err)
	}
	if fd.FieldType != "relation" {
		t.Fatalf("returned field type = %q; want relation", fd.FieldType)
	}
	if len(meta.createdWithRel) != 1 {
		t.Fatalf("expected 1 CreateFieldWithRelationship call; got %d", len(meta.createdWithRel))
	}
	rel := meta.createdWithRel[0].rel
	if rel.Name != "courses_owner" {
		t.Errorf("rel.Name = %q; want courses_owner", rel.Name)
	}
	if rel.ParentEntityName != "accounts" {
		t.Errorf("rel.ParentEntityName = %q; want accounts", rel.ParentEntityName)
	}
	if rel.ChildEntityName != "courses" {
		t.Errorf("rel.ChildEntityName = %q; want courses", rel.ChildEntityName)
	}
	if rel.RelationshipType != "1-N" {
		t.Errorf("rel.RelationshipType = %q; want 1-N", rel.RelationshipType)
	}
	if rel.ChildFKColumn != "owner_id" {
		t.Errorf("rel.ChildFKColumn = %q; want owner_id", rel.ChildFKColumn)
	}
	if rel.InverseField != "courses" {
		t.Errorf("rel.InverseField = %q; want courses (default = child entity name)", rel.InverseField)
	}
	if rel.TenantID != "tenant-1" {
		t.Errorf("rel.TenantID = %q; want tenant-1", rel.TenantID)
	}
	if rel.JoinTableName != nil {
		t.Errorf("rel.JoinTableName = %v; want nil (1-N has no join table)", rel.JoinTableName)
	}
}

// TestAddField_Relation_InverseFieldOverrideHonored: options.inverseField, when a valid
// identifier and unclaimed, is used verbatim instead of the default child-entity name.
func TestAddField_Relation_InverseFieldOverrideHonored(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := relMeta()
	svc := NewFieldService(schema, meta)

	in := addRelationField()
	in.Field.Options["inverseField"] = "account_owner"

	if _, err := svc.AddField(context.Background(), "tenant-1", in); err != nil {
		t.Fatalf("AddField(relation, inverseField override): %v", err)
	}
	if len(meta.createdWithRel) != 1 {
		t.Fatalf("expected 1 CreateFieldWithRelationship call; got %d", len(meta.createdWithRel))
	}
	if got := meta.createdWithRel[0].rel.InverseField; got != "account_owner" {
		t.Errorf("rel.InverseField = %q; want account_owner (explicit override)", got)
	}
}

// TestAddField_Relation_InverseFieldInvalidIdentifier: an options.inverseField that fails
// ddl.SanitizeIdentifier is rejected 422 FIELD_RELATION_INVERSE_INVALID before any DDL or
// metadata write.
func TestAddField_Relation_InverseFieldInvalidIdentifier(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := relMeta()
	svc := NewFieldService(schema, meta)

	in := addRelationField()
	in.Field.Options["inverseField"] = "Bad Name!!"

	_, err := svc.AddField(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_RELATION_INVERSE_INVALID" {
		t.Fatalf("code = %q; want FIELD_RELATION_INVERSE_INVALID", codeOf(err))
	}
	if len(schema.relAdded) != 0 {
		t.Errorf("invalid inverseField must not run DDL; relAdded=%v", schema.relAdded)
	}
	if len(meta.created) != 0 || len(meta.createdWithRel) != 0 {
		t.Errorf("invalid inverseField must not persist metadata; created=%v createdWithRel=%v", meta.created, meta.createdWithRel)
	}
}

// TestAddField_Relation_InverseFieldExplicitCollision: an explicit options.inverseField that
// collides with an already-claimed (parent, inverse_field) pair is rejected 422
// FIELD_RELATION_INVERSE_TAKEN before any DDL or metadata write.
func TestAddField_Relation_InverseFieldExplicitCollision(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := relMeta()
	meta.claimedInverse = map[string]*metadata.RelationshipDefinition{
		"accounts|account_owner": {Name: "some_other_rel", ParentEntityName: "accounts", InverseField: "account_owner", RelationshipType: "1-N"},
	}
	svc := NewFieldService(schema, meta)

	in := addRelationField()
	in.Field.Options["inverseField"] = "account_owner"

	_, err := svc.AddField(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_RELATION_INVERSE_TAKEN" {
		t.Fatalf("code = %q; want FIELD_RELATION_INVERSE_TAKEN", codeOf(err))
	}
	if len(schema.relAdded) != 0 {
		t.Errorf("collision must not run DDL; relAdded=%v", schema.relAdded)
	}
	if len(meta.created) != 0 || len(meta.createdWithRel) != 0 {
		t.Errorf("collision must not persist metadata; created=%v createdWithRel=%v", meta.created, meta.createdWithRel)
	}
}

// TestAddField_Relation_DefaultInverseCollision_FallsBackToChildFieldName: when the default
// inverse name (the child entity name, "courses") is already claimed for this parent, AddField
// falls back to "<childEntity>_<fieldName>" ("courses_owner") instead of failing.
func TestAddField_Relation_DefaultInverseCollision_FallsBackToChildFieldName(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := relMeta()
	meta.claimedInverse = map[string]*metadata.RelationshipDefinition{
		"accounts|courses": {Name: "some_other_rel", ParentEntityName: "accounts", InverseField: "courses", RelationshipType: "1-N"},
	}
	svc := NewFieldService(schema, meta)

	if _, err := svc.AddField(context.Background(), "tenant-1", addRelationField()); err != nil {
		t.Fatalf("AddField(relation, default-collision fallback): %v", err)
	}
	if len(meta.createdWithRel) != 1 {
		t.Fatalf("expected 1 CreateFieldWithRelationship call; got %d", len(meta.createdWithRel))
	}
	if got := meta.createdWithRel[0].rel.InverseField; got != "courses_owner" {
		t.Errorf("rel.InverseField = %q; want courses_owner (fallback)", got)
	}
}

// TestAddField_Relation_DefaultAndFallbackBothTaken_Rejected: when BOTH the default name and
// its "<childEntity>_<fieldName>" fallback are already claimed, AddField rejects 422
// FIELD_RELATION_INVERSE_TAKEN before any DDL or metadata write.
func TestAddField_Relation_DefaultAndFallbackBothTaken_Rejected(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := relMeta()
	meta.claimedInverse = map[string]*metadata.RelationshipDefinition{
		"accounts|courses":       {Name: "rel-a", ParentEntityName: "accounts", InverseField: "courses", RelationshipType: "1-N"},
		"accounts|courses_owner": {Name: "rel-b", ParentEntityName: "accounts", InverseField: "courses_owner", RelationshipType: "1-N"},
	}
	svc := NewFieldService(schema, meta)

	_, err := svc.AddField(context.Background(), "tenant-1", addRelationField())
	if codeOf(err) != "FIELD_RELATION_INVERSE_TAKEN" {
		t.Fatalf("code = %q; want FIELD_RELATION_INVERSE_TAKEN", codeOf(err))
	}
	if len(schema.relAdded) != 0 {
		t.Errorf("collision must not run DDL; relAdded=%v", schema.relAdded)
	}
	if len(meta.created) != 0 || len(meta.createdWithRel) != 0 {
		t.Errorf("collision must not persist metadata; created=%v createdWithRel=%v", meta.created, meta.createdWithRel)
	}
}

// TestAddField_Relation_CompensatesRegistryFailure: the FK ALTER auto-commits, then the
// COMBINED field+relationship metadata write fails (e.g. an unexpected registry-insert error)
// -> the just-added FK column must still be dropped, exactly like a plain metadata failure.
func TestAddField_Relation_CompensatesRegistryFailure(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := relMeta()
	meta.createErr = context.DeadlineExceeded // any non-nil error simulates the combined tx failing
	svc := NewFieldService(schema, meta)

	if _, err := svc.AddField(context.Background(), "tenant-1", addRelationField()); err == nil {
		t.Fatal("expected error when the combined field+relationship write fails")
	}
	if len(schema.relAdded) != 1 {
		t.Errorf("FK ALTER should have run before metadata; relAdded=%v", schema.relAdded)
	}
	if len(schema.relDropped) != 1 || schema.relDropped[0] != "owner" {
		t.Errorf("metadata failure must DROP the added FK column; relDropped=%v", schema.relDropped)
	}
}
