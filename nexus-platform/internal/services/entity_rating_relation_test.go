package services

import "testing"

// TestValidateRatingConfig_Min locks the bug #6 fix: validateRatingConfig now validates
// options.min (integer >= 1, and <= effective max), not just options.max. A min greater than max
// (or below 1, or non-integer) makes the rating field unsatisfiable and must be rejected at
// schema time with FIELD_RATING_MIN_INVALID. Max-only configs keep their existing behavior.
func TestValidateRatingConfig_Min(t *testing.T) {
	cases := []struct {
		name     string
		options  map[string]any
		wantCode string // "" = success
	}{
		{"min and max valid", map[string]any{"min": float64(1), "max": float64(5)}, ""},
		{"min equals max valid", map[string]any{"min": float64(5), "max": float64(5)}, ""},
		{"min only (within default max)", map[string]any{"min": float64(3)}, ""},
		{"min greater than max", map[string]any{"min": float64(6), "max": float64(5)}, "FIELD_RATING_MIN_INVALID"},
		{"min greater than default max", map[string]any{"min": float64(9)}, "FIELD_RATING_MIN_INVALID"},
		{"min below one", map[string]any{"min": float64(0), "max": float64(5)}, "FIELD_RATING_MIN_INVALID"},
		{"min not integer", map[string]any{"min": float64(2.5), "max": float64(5)}, "FIELD_RATING_MIN_INVALID"},
		{"min wrong type", map[string]any{"min": "1", "max": float64(5)}, "FIELD_RATING_MIN_INVALID"},
		{"max still validated", map[string]any{"max": float64(1)}, "FIELD_RATING_MAX_INVALID"},
		{"no options ok", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRatingConfig("score", tc.options)
			if got := codeOf(err); got != tc.wantCode {
				t.Errorf("validateRatingConfig code = %q (err=%v); want %q", got, err, tc.wantCode)
			}
		})
	}
}

// TestValidateCreateEntity_RelationColumnConflict locks the bug #5 part (c) fix: a relation field
// <rel> implicitly materializes a "<rel>_id" FK column, so entity-create must reject a payload
// where that derived column collides with a literal field of the same name, or where two
// relations derive the same column — FIELD_RELATION_COLUMN_CONFLICT (422) — instead of letting it
// blow up as a 500 at CREATE TABLE. Order-independent: the literal field may precede or follow.
func TestValidateCreateEntity_RelationColumnConflict(t *testing.T) {
	rel := func(name, target string) FieldInput {
		return FieldInput{Name: name, Label: name, FieldType: "relation",
			Options: map[string]any{"relatedEntity": target}}
	}
	text := func(name string) FieldInput {
		return FieldInput{Name: name, Label: name, FieldType: "text"}
	}
	base := func(fields ...FieldInput) CreateEntityInput {
		return CreateEntityInput{Name: "orders", Label: "Đơn hàng", Fields: fields}
	}

	cases := []struct {
		name     string
		in       CreateEntityInput
		wantCode string // "" = success
	}{
		{"relation then colliding text field",
			base(rel("manager", "users"), text("manager_id")), "FIELD_RELATION_COLUMN_CONFLICT"},
		{"colliding text field then relation",
			base(text("manager_id"), rel("manager", "users")), "FIELD_RELATION_COLUMN_CONFLICT"},
		{"two relations deriving same column",
			base(rel("manager", "users"), rel("manager", "users")), "FIELD_DUPLICATE"}, // same field name caught first
		{"relation plus distinct fields ok",
			base(rel("manager", "users"), text("note")), ""},
		{"relation alone ok",
			base(rel("manager", "users")), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCreateEntity(tc.in)
			if got := codeOf(err); got != tc.wantCode {
				t.Errorf("ValidateCreateEntity code = %q (err=%v); want %q", got, err, tc.wantCode)
			}
		})
	}
}
