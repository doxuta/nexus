package services

import (
	"context"
	"testing"

	"nexus/platform/internal/metadata"
)

// TestEditField_UserField_IndexedToggleIsNoOp pins the load-bearing guard added to
// indexedDDLNeeded (field_orchestration.go): a `user` field ALREADY has its own permanent
// `<field>_id` FK column (materialized at AddField time), so toggling options.indexed on it
// must NOT attempt AddIndexedColumn — that call would fail (ddl.indexedColExpr does not
// recognize fieldType "user") and surface as an opaque 500 instead of being silently absorbed.
// This mirrors relation's existing (implicit, pre-dating this slice) protection.
func TestEditField_UserField_IndexedToggleIsNoOp(t *testing.T) {
	userField := &metadata.FieldDefinition{
		ID:        "fld-u1",
		EntityID:  "ent-1",
		Name:      "assigned_to",
		Label:     "Người phụ trách",
		FieldType: "user",
	}
	meta := &fakeEditMeta{field: userField}
	schema := &fakeFieldSchema{}
	svc := NewFieldService(schema, meta)

	in := EditFieldInput{
		FieldID:   "fld-u1",
		FieldType: "user", // unchanged
		Options:   map[string]any{"indexed": true},
	}
	if _, err := svc.EditField(context.Background(), "tenant-1", in); err != nil {
		t.Fatalf("EditField(user, indexed=true) = %v; want nil (DDL silently skipped, not attempted)", err)
	}
	if len(schema.idxAdded) != 0 {
		t.Errorf("indexed toggle on a user field must NOT call AddIndexedColumn; idxAdded=%v", schema.idxAdded)
	}
	if len(meta.updated) != 1 {
		t.Errorf("the metadata flip must still persist exactly once; updated=%d", len(meta.updated))
	}
}
