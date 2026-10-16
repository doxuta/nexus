package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/metadata"
)

// relatedEntityFixture is the FK-target entity a relation field points to.
func relatedEntityFixture() *metadata.EntityDefinition {
	return &metadata.EntityDefinition{
		ID:        "ent-acc",
		TenantID:  "tenant-1",
		Name:      "accounts",
		DataTable: "obj_accounts",
	}
}

// addRelationField is a valid relation AddFieldInput (FK to "accounts").
func addRelationField() AddFieldInput {
	return AddFieldInput{
		EntityName: "courses",
		Field: FieldInput{
			Name:      "owner",
			Label:     "Chủ",
			FieldType: "relation",
			Options:   map[string]any{"relatedEntity": "accounts"},
		},
	}
}

// relMeta wires a fakeFieldMeta that resolves BOTH the host entity ("courses") and the FK
// target ("accounts") so AddField's relation branch can verify the target exists.
func relMeta() *fakeFieldMeta {
	return &fakeFieldMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{
			"courses":  entityFixture(),
			"accounts": relatedEntityFixture(),
		},
	}
}

// TestAddField_RelationAltersThenPersists is M3-T1's core unit acceptance: a relation field
// ALTERs the obj_* table to add the FK column (via AddRelationColumn, NOT AddUniqueColumn)
// exactly once, then persists the metadata row carrying options.relatedEntity. No unique
// ALTER and no compensation drop run on the happy path.
func TestAddField_RelationAltersThenPersists(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := relMeta()
	svc := NewFieldService(schema, meta)

	fd, err := svc.AddField(context.Background(), "tenant-1", addRelationField())
	if err != nil {
		t.Fatalf("AddField(relation): %v", err)
	}
	if len(schema.relAdded) != 1 || schema.relAdded[0] != "owner" {
		t.Errorf("relation field should ALTER once for owner; relAdded=%v", schema.relAdded)
	}
	if len(schema.added) != 0 {
		t.Errorf("relation field must NOT use the unique-column ALTER; added=%v", schema.added)
	}
	if len(schema.relDropped) != 0 {
		t.Errorf("no compensation on happy path; relDropped=%v", schema.relDropped)
	}
	if len(meta.created) != 1 || meta.created[0].FieldType != "relation" {
		t.Fatalf("relation metadata not persisted; created=%v", meta.created)
	}
	if meta.created[0].Options == nil || *meta.created[0].Options == "" {
		t.Error("relation field must persist options.relatedEntity in sys_fields.options")
	}
	if fd == nil || fd.FieldType != "relation" {
		t.Errorf("returned field def should be the persisted relation field; got %v", fd)
	}
}

// TestAddField_RelationCompensatesWhenMetaFails: the FK ALTER auto-commits, then the
// metadata write fails -> the just-added FK column must be DROPPED (via DropRelationColumn)
// so no orphan column/index is left behind.
func TestAddField_RelationCompensatesWhenMetaFails(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := relMeta()
	meta.createErr = errors.New("meta boom")
	svc := NewFieldService(schema, meta)

	if _, err := svc.AddField(context.Background(), "tenant-1", addRelationField()); err == nil {
		t.Fatal("expected error when metadata write fails")
	}
	if len(schema.relAdded) != 1 {
		t.Errorf("FK ALTER should have run before metadata; relAdded=%v", schema.relAdded)
	}
	if len(schema.relDropped) != 1 || schema.relDropped[0] != "owner" {
		t.Errorf("metadata failure must DROP the added FK column; relDropped=%v", schema.relDropped)
	}
}

// TestAddField_RelationTargetNotFound: when options.relatedEntity names an entity that does
// not exist for the tenant, AddField rejects with FIELD_RELATION_TARGET_NOT_FOUND (422) and
// runs NO DDL — never materializing an FK column pointing at a non-existent parent table.
func TestAddField_RelationTargetNotFound(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := &fakeFieldMeta{
		entitiesByName: map[string]*metadata.EntityDefinition{
			"courses": entityFixture(), // host exists; "accounts" deliberately absent
		},
	}
	svc := NewFieldService(schema, meta)

	_, err := svc.AddField(context.Background(), "tenant-1", addRelationField())
	if codeOf(err) != "FIELD_RELATION_TARGET_NOT_FOUND" {
		t.Fatalf("code = %q; want FIELD_RELATION_TARGET_NOT_FOUND", codeOf(err))
	}
	if len(schema.relAdded) != 0 {
		t.Errorf("missing target must not run DDL; relAdded=%v", schema.relAdded)
	}
	if len(meta.created) != 0 {
		t.Errorf("missing target must not persist metadata; created=%v", meta.created)
	}
}

// TestAddField_RelationMissingTargetRejectedBeforeLookup: a relation field with no
// options.relatedEntity is rejected by validation (FIELD_RELATION_TARGET_REQUIRED) before
// any entity lookup or DDL.
func TestAddField_RelationMissingTargetRejectedBeforeLookup(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := relMeta()
	svc := NewFieldService(schema, meta)

	in := addRelationField()
	in.Field.Options = nil
	_, err := svc.AddField(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_RELATION_TARGET_REQUIRED" {
		t.Fatalf("code = %q; want FIELD_RELATION_TARGET_REQUIRED", codeOf(err))
	}
	if len(schema.relAdded) != 0 || len(meta.created) != 0 {
		t.Errorf("invalid relation must not touch schema/metadata; relAdded=%v created=%v", schema.relAdded, meta.created)
	}
}

// TestAddField_RelationColumnConflict covers bug #14 (GD1-review-bugs): when the relation
// field's `<field>_id` physical column already exists on the obj_* table (e.g. another field
// already owns it), AddField must reject with FIELD_RELATION_COLUMN_CONFLICT (422) instead of
// silently skipping the ALTER and persisting metadata that hijacks a foreign column. No
// AddRelationColumn ALTER and no metadata write may run.
func TestAddField_RelationColumnConflict(t *testing.T) {
	schema := &fakeFieldSchema{colExists: true} // pretend owner_id already exists
	meta := relMeta()
	svc := NewFieldService(schema, meta)

	_, err := svc.AddField(context.Background(), "tenant-1", addRelationField())
	if codeOf(err) != "FIELD_RELATION_COLUMN_CONFLICT" {
		t.Fatalf("code = %q; want FIELD_RELATION_COLUMN_CONFLICT", codeOf(err))
	}
	// The precheck must have asked about the `<field>_id` column.
	if len(schema.colChecked) != 1 || schema.colChecked[0] != "owner_id" {
		t.Errorf("expected ColumnExists(owner_id) precheck; colChecked=%v", schema.colChecked)
	}
	if len(schema.relAdded) != 0 {
		t.Errorf("conflict must not run the relation ALTER; relAdded=%v", schema.relAdded)
	}
	if len(meta.created) != 0 {
		t.Errorf("conflict must not persist metadata; created=%v", meta.created)
	}
}
