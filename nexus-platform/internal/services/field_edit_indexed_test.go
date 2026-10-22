package services

// #56: PUT /admin/fields/:id {"indexed": …} sets options.indexed and nothing else of the options.

import (
	"context"
	"testing"
)

func TestEditField_IndexedSetsOnlyTheFlag(t *testing.T) {
	for _, tc := range []struct {
		name, stored, want string
		on                 bool
	}{
		{"on keeps the other options and adds the column", `{"maxLength":200}`, `{"indexed":true,"maxLength":200}`, true},
		{"off keeps the other options", `{"indexed":true,"maxLength":200}`, `{"maxLength":200}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := storedField()
			f.Options = &tc.stored
			meta := &fakeEditMeta{field: f}
			schema := &fakeFieldSchema{}
			on := tc.on
			if _, err := NewFieldService(schema, meta).EditField(context.Background(), "tenant-1", EditFieldInput{FieldID: "fld-1", Indexed: &on}); err != nil {
				t.Fatalf("EditField: %v", err)
			}
			if len(meta.updated) != 1 || meta.updated[0].Options == nil || *meta.updated[0].Options != tc.want {
				t.Fatalf("persisted options = %+v, want %s", meta.updated, tc.want)
			}
			if tc.on && len(schema.idxAdded) != 1 {
				t.Errorf("indexed on: AddIndexedColumn calls = %v, want one", schema.idxAdded)
			}
		})
	}
}

func TestEditField_IndexedWithOptionsIs422(t *testing.T) {
	meta := &fakeEditMeta{field: storedField()}
	on := true
	_, err := NewFieldService(&fakeFieldSchema{}, meta).EditField(context.Background(), "tenant-1",
		EditFieldInput{FieldID: "fld-1", Indexed: &on, Options: map[string]any{"maxLength": 10}})
	wantCode(t, err, "FIELD_INDEXED_WITH_OPTIONS")
	if len(meta.updated) != 0 {
		t.Errorf("a refused edit wrote %d rows", len(meta.updated))
	}
}
