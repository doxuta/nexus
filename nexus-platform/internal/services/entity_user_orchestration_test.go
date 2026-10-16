package services

import (
	"context"
	"testing"
)

// USER-FIELD slice: CreateEntity's shared-obj_*-table reconcile (bug3) must treat a `user`
// field EXACTLY like a plain 1-N `relation` field — same fakeSchema.addedRelation/AddRelationColumn
// bookkeeping (entity_orchestration_test.go) — since it reuses the identical DDL. Unlike relation,
// it needs NO target entity to resolve (fakeMeta stays empty — `users` is never a sys_entity).

// TestCreateEntity_ReconcileSharedTable_UserField mirrors TestCreateEntity_ReconcileSharedTable:
// on a pre-existing shared table missing the `assigned_to_id` column, CreateEntity ALTERs it in
// via the SAME AddRelationColumn call relation uses.
func TestCreateEntity_ReconcileSharedTable_UserField(t *testing.T) {
	schema := &fakeSchema{existed: true, existingCols: map[string]bool{"title": true}}
	meta := &fakeMeta{} // no target entity to resolve — `user` needs none
	svc := NewEntityService(schema, meta)

	in := validEntity() // fields: title(text), code(text,unique), level(picklist)
	in.Fields = append(in.Fields, FieldInput{Name: "assigned_to", Label: "Người phụ trách", FieldType: "user"})

	_, _, err := svc.CreateEntity(context.Background(), "tenant-2", in)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if len(schema.addedUnique) != 1 || schema.addedUnique[0] != "code" {
		t.Errorf("addedUnique = %v; want [code]", schema.addedUnique)
	}
	// The missing user-field FK column must be ALTERed in via the SAME reconcile path relation
	// uses (addedRelation, not a separate bookkeeping slice).
	if len(schema.addedRelation) != 1 || schema.addedRelation[0] != "assigned_to" {
		t.Errorf("addedRelation = %v; want [assigned_to] (user field reuses relation's reconcile)", schema.addedRelation)
	}
}

// TestCreateEntity_ReconcileSkipsExistingColumns_UserField mirrors
// TestCreateEntity_ReconcileSkipsExistingColumns: when assigned_to_id already exists on the
// shared table, reconcile must NOT re-ALTER it (ADD COLUMN onto an existing column errors 500).
func TestCreateEntity_ReconcileSkipsExistingColumns_UserField(t *testing.T) {
	schema := &fakeSchema{existed: true, existingCols: map[string]bool{
		"title": true, "code": true, "assigned_to_id": true,
	}}
	meta := &fakeMeta{}
	svc := NewEntityService(schema, meta)

	in := validEntity()
	in.Fields = append(in.Fields, FieldInput{Name: "assigned_to", Label: "Người phụ trách", FieldType: "user"})

	if _, _, err := svc.CreateEntity(context.Background(), "tenant-2", in); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if len(schema.addedRelation) != 0 {
		t.Errorf("addedRelation = %v; want none (column already present)", schema.addedRelation)
	}
}

// TestCreateEntity_UserField_FreshTable proves a `user` field declared inline at entity-create
// time (fresh, non-shared table) creates cleanly — CreateTable materializes the column, no
// reconcile runs, and no relation-target existence check fires (fakeMeta has zero known
// entities, which would reject a `relation` field but a `user` field needs no target at all).
func TestCreateEntity_UserField_FreshTable(t *testing.T) {
	schema := &fakeSchema{existed: false}
	meta := &fakeMeta{}
	svc := NewEntityService(schema, meta)

	in := validEntity()
	in.Fields = append(in.Fields, FieldInput{Name: "assigned_to", Label: "Người phụ trách", FieldType: "user"})

	entity, fields, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if err != nil {
		t.Fatalf("CreateEntity(user field, fresh table): %v", err)
	}
	if entity == nil {
		t.Fatal("entity is nil")
	}
	found := false
	for _, f := range fields {
		if f.Name == "assigned_to" {
			found = true
			if f.FieldType != "user" {
				t.Errorf("persisted FieldType = %q; want \"user\"", f.FieldType)
			}
		}
	}
	if !found {
		t.Error("assigned_to field not found in persisted fields")
	}
	// Fresh table -> CreateTable built every column inline, reconcile must not run.
	if len(schema.addedRelation) != 0 {
		t.Errorf("fresh table must not reconcile; addedRelation=%v", schema.addedRelation)
	}
}
