package services

import "testing"

// relationField returns a valid 1-N relation AddFieldInput: type=relation with the required
// options.relatedEntity naming a target entity. Helper for the M3 validation tests.
func relationField() AddFieldInput {
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

// TestRelationFieldValidation pins the M3 contract for the `relation` field type (replacing
// B-4's pre-M3 fail-fast): `relation` is now a SUPPORTED type, but it is only valid when
// options.relatedEntity names a target entity. The two outcomes clients must be able to tell
// apart:
//   - relation WITHOUT options.relatedEntity -> 422 FIELD_RELATION_TARGET_REQUIRED.
//   - relation WITH a valid options.relatedEntity -> accepted (no validation error).
//
// This guards both entry points that validate a field type: entity-create and add-field.
func TestRelationFieldValidation(t *testing.T) {
	t.Run("relation is now in the allowlist", func(t *testing.T) {
		if _, ok := allowedFieldTypes["relation"]; !ok {
			t.Error("allowedFieldTypes must contain \"relation\" now that M3 lands")
		}
	})

	t.Run("ValidateAddField accepts relation with relatedEntity", func(t *testing.T) {
		if got := codeOf(ValidateAddField(relationField())); got != "" {
			t.Errorf("ValidateAddField(valid relation) code = %q; want success", got)
		}
	})

	t.Run("ValidateAddField rejects relation missing relatedEntity", func(t *testing.T) {
		in := relationField()
		in.Field.Options = nil
		if got := codeOf(ValidateAddField(in)); got != "FIELD_RELATION_TARGET_REQUIRED" {
			t.Errorf("ValidateAddField(relation no target) code = %q; want FIELD_RELATION_TARGET_REQUIRED", got)
		}
	})

	t.Run("ValidateAddField rejects relation with empty relatedEntity", func(t *testing.T) {
		in := relationField()
		in.Field.Options = map[string]any{"relatedEntity": "  "}
		if got := codeOf(ValidateAddField(in)); got != "FIELD_RELATION_TARGET_REQUIRED" {
			t.Errorf("ValidateAddField(relation empty target) code = %q; want FIELD_RELATION_TARGET_REQUIRED", got)
		}
	})

	t.Run("ValidateAddField rejects relation with invalid relatedEntity name", func(t *testing.T) {
		in := relationField()
		in.Field.Options = map[string]any{"relatedEntity": "x; drop"}
		if got := codeOf(ValidateAddField(in)); got != "FIELD_RELATION_TARGET_INVALID" {
			t.Errorf("ValidateAddField(relation bad target) code = %q; want FIELD_RELATION_TARGET_INVALID", got)
		}
	})

	t.Run("ValidateCreateEntity accepts relation with relatedEntity", func(t *testing.T) {
		in := validEntity()
		in.Fields[0].FieldType = "relation"
		in.Fields[0].Options = map[string]any{"relatedEntity": "accounts"}
		if got := codeOf(ValidateCreateEntity(in)); got != "" {
			t.Errorf("ValidateCreateEntity(valid relation) code = %q; want success", got)
		}
	})

	t.Run("ValidateCreateEntity rejects relation missing relatedEntity", func(t *testing.T) {
		in := validEntity()
		in.Fields[0].FieldType = "relation"
		in.Fields[0].Options = nil
		if got := codeOf(ValidateCreateEntity(in)); got != "FIELD_RELATION_TARGET_REQUIRED" {
			t.Errorf("ValidateCreateEntity(relation no target) code = %q; want FIELD_RELATION_TARGET_REQUIRED", got)
		}
	})
}
