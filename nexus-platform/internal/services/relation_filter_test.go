package services

// Relation picker dependent filter (queue #304): options.filterBy on a `relation` field narrows
// the picker by a sibling relation on the same form (Ticket.device lists only the chosen customer's
// devices). Config-time only; the picker is a UX filter, the write path is unchanged.

import (
	"errors"
	"testing"
)

const relFilterCode = "FIELD_RELATION_FILTER_INVALID"

func relFilterOpts(filterBy any) map[string]any {
	o := map[string]any{"relatedEntity": "device"}
	if filterBy != nil {
		o["filterBy"] = filterBy
	}
	return o
}

func wantRelFilterCode(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("want no error, got %v", err)
		}
		return
	}
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != want {
		t.Fatalf("want code %s, got %v", want, err)
	}
}

func TestRelationFilter_FieldConfig(t *testing.T) {
	good := map[string]any{"field": "customer", "parentField": "customer"}
	tests := []struct {
		name      string
		fieldType string
		opts      map[string]any
		wantCode  string
	}{
		{"no filter passes", "relation", relFilterOpts(nil), ""},
		{"valid", "relation", relFilterOpts(good), ""},
		{"not an object", "relation", relFilterOpts("customer"), relFilterCode},
		{"missing parentField", "relation", relFilterOpts(map[string]any{"field": "customer"}), relFilterCode},
		{"blank field", "relation", relFilterOpts(map[string]any{"field": " ", "parentField": "customer"}), relFilterCode},
		{"non-string parentField", "relation", relFilterOpts(map[string]any{"field": "customer", "parentField": 3}), relFilterCode},
		{"unknown key", "relation", relFilterOpts(map[string]any{"field": "c", "parentField": "p", "x": 1}), relFilterCode},
		{"on a non-relation field", "text", map[string]any{"filterBy": good}, relFilterCode},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wantRelFilterCode(t, validateFieldConfig("device", tc.fieldType, tc.opts), tc.wantCode)
		})
	}
}

func TestRelationFilter_Refs(t *testing.T) {
	siblings := []FieldInput{
		{Name: "customer", FieldType: "relation", Options: map[string]any{"relatedEntity": "customer"}},
		{Name: "title", FieldType: "text"},
		{Name: "device", FieldType: "relation", Options: relFilterOpts(map[string]any{"field": "customer", "parentField": "customer"})},
	}
	dev := func(parent string) FieldInput {
		return FieldInput{Name: "device", FieldType: "relation", Options: relFilterOpts(map[string]any{"field": "customer", "parentField": parent})}
	}
	tests := []struct {
		name     string
		f        FieldInput
		wantCode string
	}{
		{"parent is a sibling relation", dev("customer"), ""},
		{"parent missing", dev("nope"), relFilterCode},
		{"parent not a relation", dev("title"), relFilterCode},
		{"parent is itself", dev("device"), relFilterCode},
		{"no filter", FieldInput{Name: "device", FieldType: "relation", Options: relFilterOpts(nil)}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wantRelFilterCode(t, validateRelationFilterRefs(tc.f, siblings, nil), tc.wantCode)
		})
	}
	t.Run("cycle", func(t *testing.T) {
		sib := []FieldInput{
			{Name: "a", FieldType: "relation", Options: relFilterOpts(map[string]any{"field": "x", "parentField": "b"})},
			{Name: "b", FieldType: "relation", Options: relFilterOpts(map[string]any{"field": "x", "parentField": "a"})},
		}
		wantRelFilterCode(t, validateRelationFilterRefs(sib[0], sib, nil), relFilterCode)
	})
	t.Run("carried stored parent that vanished is tolerated", func(t *testing.T) {
		f := dev("gone")
		wantRelFilterCode(t, validateRelationFilterRefs(f, siblings, f.Options), "")
	})
}
