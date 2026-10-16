package services

// W5-A1: EditField toggling a field's `indexed` flag OFF must NOT physically DROP the
// generated column from the shared obj_* table — that would remove the sort/filter column for
// every co-tenant that still indexes the same-named field (a cross-tenant schema mutation).
// Toggle-off is metadata-only (mirror DeleteField's soft semantics); toggle-on is idempotent
// via ColumnExists so a re-index after a never-dropped un-index does not re-ADD the column.

import (
	"context"
	"testing"

	"nexus/platform/internal/metadata"
)

// indexedTextField returns a stored text field whose options already declare indexed=true,
// so EditField sees wasIndexed=true and a toggle to indexed=false exercises the drop path.
func indexedTextField() *metadata.FieldDefinition {
	f := storedField() // text field "status"
	opts := `{"indexed":true}`
	f.Options = &opts
	return f
}

func TestEditField_ToggleIndexedOff_NeverDropsSharedColumn_W5A1(t *testing.T) {
	meta := &fakeEditMeta{field: indexedTextField()}
	schema := &fakeFieldSchema{}
	svc := NewFieldService(schema, meta)

	in := EditFieldInput{
		FieldID: "fld-1",
		Options: map[string]any{"indexed": false}, // toggle OFF
	}
	if _, err := svc.EditField(context.Background(), "tenant-1", in); err != nil {
		t.Fatalf("EditField toggle indexed off: unexpected error %v", err)
	}
	// The physical column must NOT be dropped — co-tenant safety (W5-A1).
	if len(schema.idxDropped) != 0 {
		t.Errorf("toggle-off must NOT DropIndexedColumn on the shared table; idxDropped=%v", schema.idxDropped)
	}
	// The metadata flag must still be persisted (the field is now un-indexed for this tenant).
	if len(meta.updated) != 1 {
		t.Errorf("toggle-off must persist the metadata flip exactly once; updated=%d", len(meta.updated))
	}
}

func TestEditField_ToggleIndexedOn_IdempotentWhenColumnExists_W5A1(t *testing.T) {
	// Stored field is currently un-indexed, but the physical column survived a prior un-index
	// (colExists=true). Re-indexing must SKIP AddIndexedColumn rather than ADD onto an existing
	// column (which would 1060), while still flipping the metadata flag back on.
	meta := &fakeEditMeta{field: storedField()} // text "status", no indexed option (wasIndexed=false)
	schema := &fakeFieldSchema{colExists: true}
	svc := NewFieldService(schema, meta)

	in := EditFieldInput{
		FieldID: "fld-1",
		Options: map[string]any{"indexed": true}, // toggle ON
	}
	if _, err := svc.EditField(context.Background(), "tenant-1", in); err != nil {
		t.Fatalf("EditField toggle indexed on (column exists): unexpected error %v", err)
	}
	if len(schema.idxAdded) != 0 {
		t.Errorf("re-index with an existing column must SKIP AddIndexedColumn; idxAdded=%v", schema.idxAdded)
	}
	if len(meta.updated) != 1 {
		t.Errorf("toggle-on must persist the metadata flip exactly once; updated=%d", len(meta.updated))
	}
}

func TestEditField_ToggleIndexedOn_AddsWhenColumnAbsent_W5A1(t *testing.T) {
	// Control: a genuinely-new index (column absent) must still ADD the column.
	meta := &fakeEditMeta{field: storedField()}
	schema := &fakeFieldSchema{colExists: false}
	svc := NewFieldService(schema, meta)

	in := EditFieldInput{
		FieldID: "fld-1",
		Options: map[string]any{"indexed": true},
	}
	if _, err := svc.EditField(context.Background(), "tenant-1", in); err != nil {
		t.Fatalf("EditField toggle indexed on (column absent): unexpected error %v", err)
	}
	if len(schema.idxAdded) != 1 {
		t.Errorf("a new index (column absent) must ADD the indexed column once; idxAdded=%v", schema.idxAdded)
	}
}
