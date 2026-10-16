package services

import "testing"

// USER-FIELD slice: checkType's DB-free shape check for a `user` field — mirrors `relation`'s
// (a bare string), same table shape as TestValidateRecord (record_service_test.go). Existence
// (must be an ACTIVE user of the SAME tenant) is a SEPARATE probe tested in
// record_user_target_test.go, exactly as relation splits checkType from checkRelationTargets.
func TestValidateRecord_UserField_Shape(t *testing.T) {
	fields := []FieldSpec{
		{Name: "subject", FieldType: "text", IsRequired: true},
		{Name: "assigned_to", FieldType: "user"},
	}
	cases := []struct {
		name     string
		payload  map[string]any
		wantCode string // "" = success
	}{
		{"valid string id", map[string]any{"subject": "x", "assigned_to": "user-1"}, ""},
		{"omitted (optional) ok", map[string]any{"subject": "x"}, ""},
		{"wrong type: number", map[string]any{"subject": "x", "assigned_to": float64(1)}, "RECORD_INVALID_TYPE"},
		{"wrong type: object", map[string]any{"subject": "x", "assigned_to": map[string]any{"id": "user-1"}}, "RECORD_INVALID_TYPE"},
		{"wrong type: array", map[string]any{"subject": "x", "assigned_to": []any{"user-1"}}, "RECORD_INVALID_TYPE"},
		{"empty string treated as omitted (not required)", map[string]any{"subject": "x", "assigned_to": ""}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clean, err := ValidateRecord(fields, tc.payload)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("ValidateRecord code = %q (err=%v); want %q", got, err, tc.wantCode)
			}
			if tc.wantCode == "" {
				if v, ok := tc.payload["assigned_to"]; ok && v != "" {
					if clean["assigned_to"] != v {
						t.Errorf("clean[assigned_to] = %v; want %v", clean["assigned_to"], v)
					}
				}
			}
		})
	}
}

// TestValidateRecord_UserField_RequiredEmptyRejected proves an empty assigned_to on a REQUIRED
// user field is rejected RECORD_REQUIRED_MISSING (isEmpty gate runs before checkType, same as
// every other scalar type).
func TestValidateRecord_UserField_RequiredEmptyRejected(t *testing.T) {
	fields := []FieldSpec{{Name: "assigned_to", FieldType: "user", IsRequired: true}}
	_, err := ValidateRecord(fields, map[string]any{})
	if got := codeOf(err); got != "RECORD_REQUIRED_MISSING" {
		t.Fatalf("code = %q; want RECORD_REQUIRED_MISSING", got)
	}
}
