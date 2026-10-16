package services

import (
	"context"
	"errors"
	"testing"
)

// USER-FIELD slice (../specs/017-user-field-type/spec.md): AddField orchestration for a `user` field. It
// reuses relation's plain 1-N DDL pair (AddRelationColumn/DropRelationColumn — same
// fakeFieldSchema.relAdded/relDropped tracking, field_orchestration_test.go) but skips ALL of
// relation's target-existence + sys_relationships registry machinery: `users` is a fixed system
// table, never a sys_entity. These tests mirror field_relation_orchestration_test.go's shape.

// addUserField is a valid AddFieldInput for a `user` field — no options needed (unlike relation,
// which requires options.relatedEntity).
func addUserField() AddFieldInput {
	return AddFieldInput{
		EntityName: "courses",
		Field:      FieldInput{Name: "assigned_to", Label: "Người phụ trách", FieldType: "user"},
	}
}

// TestAddField_UserAltersThenPersists mirrors TestAddField_RelationAltersThenPersists: a `user`
// field ALTERs the obj_* table via AddRelationColumn (the SAME DDL relation uses — reused
// as-is), then persists plain metadata — NO sys_relationships registry row (unlike relation).
func TestAddField_UserAltersThenPersists(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := &fakeFieldMeta{entity: entityFixture()} // single-entity fixture — no target to resolve
	svc := NewFieldService(schema, meta)

	fd, err := svc.AddField(context.Background(), "tenant-1", addUserField())
	if err != nil {
		t.Fatalf("AddField(user): %v", err)
	}
	if len(schema.relAdded) != 1 || schema.relAdded[0] != "assigned_to" {
		t.Errorf("user field should ALTER via AddRelationColumn once; relAdded=%v", schema.relAdded)
	}
	if len(schema.added) != 0 {
		t.Errorf("user field must NOT use the unique-column ALTER; added=%v", schema.added)
	}
	if len(schema.relDropped) != 0 {
		t.Errorf("no compensation on happy path; relDropped=%v", schema.relDropped)
	}
	if len(meta.created) != 1 || meta.created[0].FieldType != "user" {
		t.Fatalf("user metadata not persisted; created=%v", meta.created)
	}
	// USER-FIELD: unlike relation, no sys_relationships registry row is ever created.
	if len(meta.createdWithRel) != 0 {
		t.Errorf("user field must NOT register a relationship row; createdWithRel=%v", meta.createdWithRel)
	}
	if fd == nil || fd.FieldType != "user" {
		t.Errorf("returned field def should be the persisted user field; got %v", fd)
	}
}

// TestAddField_UserCompensatesWhenMetaFails: the FK ALTER auto-commits, then the metadata write
// fails -> the just-added column must be DROPPED (DropRelationColumn), same as relation.
func TestAddField_UserCompensatesWhenMetaFails(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := &fakeFieldMeta{entity: entityFixture()}
	meta.createErr = errors.New("meta boom")
	svc := NewFieldService(schema, meta)

	if _, err := svc.AddField(context.Background(), "tenant-1", addUserField()); err == nil {
		t.Fatal("expected error when metadata write fails")
	}
	if len(schema.relAdded) != 1 {
		t.Errorf("FK ALTER should have run before metadata; relAdded=%v", schema.relAdded)
	}
	if len(schema.relDropped) != 1 || schema.relDropped[0] != "assigned_to" {
		t.Errorf("metadata failure must DROP the added column; relDropped=%v", schema.relDropped)
	}
}

// TestAddField_UserColumnConflict: when the user field's `<field>_id` physical column already
// exists, AddField rejects with FIELD_RELATION_COLUMN_CONFLICT (422) — never skip-and-persist
// (mirrors TestAddField_RelationColumnConflict).
func TestAddField_UserColumnConflict(t *testing.T) {
	schema := &fakeFieldSchema{colExists: true} // pretend assigned_to_id already exists
	meta := &fakeFieldMeta{entity: entityFixture()}
	svc := NewFieldService(schema, meta)

	_, err := svc.AddField(context.Background(), "tenant-1", addUserField())
	if codeOf(err) != "FIELD_RELATION_COLUMN_CONFLICT" {
		t.Fatalf("code = %q; want FIELD_RELATION_COLUMN_CONFLICT", codeOf(err))
	}
	if len(schema.colChecked) != 1 || schema.colChecked[0] != "assigned_to_id" {
		t.Errorf("expected ColumnExists(assigned_to_id) precheck; colChecked=%v", schema.colChecked)
	}
	if len(schema.relAdded) != 0 {
		t.Errorf("conflict must not run the ALTER; relAdded=%v", schema.relAdded)
	}
	if len(meta.created) != 0 {
		t.Errorf("conflict must not persist metadata; created=%v", meta.created)
	}
}

// TestAddField_UserRejectsUnique proves validateUserFieldFlags runs from AddField's validation
// entry point (ValidateAddField) BEFORE any DDL — mirrors relation's equivalent contract.
func TestAddField_UserRejectsUnique(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := &fakeFieldMeta{entity: entityFixture()}
	svc := NewFieldService(schema, meta)

	in := addUserField()
	in.Field.IsUnique = true
	if _, err := svc.AddField(context.Background(), "tenant-1", in); codeOf(err) != "FIELD_USER_UNIQUE_UNSUPPORTED" {
		t.Fatalf("code = %q; want FIELD_USER_UNIQUE_UNSUPPORTED", codeOf(err))
	}
	if len(schema.relAdded) != 0 || len(meta.created) != 0 {
		t.Errorf("invalid user+unique must not touch schema/metadata; relAdded=%v created=%v", schema.relAdded, meta.created)
	}
}
