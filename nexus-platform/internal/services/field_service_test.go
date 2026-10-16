package services

import "testing"

// TestValidateAddField exercises the field-level rules reused from entity validation:
// identifier allowlist, supported field type, and picklist options presence.
func TestValidateAddField(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(in *AddFieldInput)
		wantCode string // "" = expect success
	}{
		{"valid ordinary", func(in *AddFieldInput) {}, ""},
		{"valid unique", func(in *AddFieldInput) { in.Field.IsUnique = true }, ""},
		{"empty field name", func(in *AddFieldInput) { in.Field.Name = "" }, "FIELD_INVALID_NAME"},
		{"reserved field name", func(in *AddFieldInput) { in.Field.Name = "select" }, "FIELD_INVALID_NAME"},
		{"injection field name", func(in *AddFieldInput) { in.Field.Name = "x; drop" }, "FIELD_INVALID_NAME"},
		{"unknown field type", func(in *AddFieldInput) { in.Field.FieldType = "foobar" }, "FIELD_INVALID_TYPE"},
		{"picklist without options", func(in *AddFieldInput) {
			in.Field.FieldType = "picklist"
			in.Field.Options = nil
		}, "FIELD_PICKLIST_NO_OPTIONS"},
		{"picklist with options ok", func(in *AddFieldInput) {
			in.Field.FieldType = "picklist"
			in.Field.Options = map[string]any{"values": []any{"a", "b"}}
		}, ""},
		// #7: every value must be a non-empty string — junk elements are malformed config.
		{"picklist value non-string", func(in *AddFieldInput) {
			in.Field.FieldType = "picklist"
			in.Field.Options = map[string]any{"values": []any{"a", 1}}
		}, "FIELD_PICKLIST_NO_OPTIONS"},
		{"picklist value empty string", func(in *AddFieldInput) {
			in.Field.FieldType = "picklist"
			in.Field.Options = map[string]any{"values": []any{"a", ""}}
		}, "FIELD_PICKLIST_NO_OPTIONS"},
		{"picklist value blank string", func(in *AddFieldInput) {
			in.Field.FieldType = "picklist"
			in.Field.Options = map[string]any{"values": []any{"   "}}
		}, "FIELD_PICKLIST_NO_OPTIONS"},
		{"picklist value null", func(in *AddFieldInput) {
			in.Field.FieldType = "picklist"
			in.Field.Options = map[string]any{"values": []any{"a", nil}}
		}, "FIELD_PICKLIST_NO_OPTIONS"},
		{"multi_select with valid string values ok", func(in *AddFieldInput) {
			in.Field.FieldType = "multi_select"
			in.Field.Options = map[string]any{"values": []any{"x", "y"}}
		}, ""},
		{"multi_select value non-string", func(in *AddFieldInput) {
			in.Field.FieldType = "multi_select"
			in.Field.Options = map[string]any{"values": []any{"x", false}}
		}, "FIELD_MULTI_SELECT_NO_OPTIONS"},
		{"empty entity name", func(in *AddFieldInput) { in.EntityName = "" }, "ENTITY_INVALID_NAME"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := ordinaryField()
			tc.mutate(&in)
			err := ValidateAddField(in)
			if got := codeOf(err); got != tc.wantCode {
				t.Errorf("ValidateAddField code = %q (err=%v); want %q", got, err, tc.wantCode)
			}
		})
	}
}
