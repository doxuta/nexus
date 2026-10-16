package services

import "testing"

// TestValidateRecord_RequiredEmptyStructured locks the bug #3 fix: a required multi_select sent
// as an empty array [] and a required composite/currency sent as an empty object {} must NOT
// satisfy the required check — they are "empty" just like nil / blank string. Non-required empty
// structured values stay accepted (omitted from the cleaned data).
func TestValidateRecord_RequiredEmptyStructured(t *testing.T) {
	multiReq := []FieldSpec{
		{Name: "tags", FieldType: "multi_select", IsRequired: true,
			Options: map[string]any{"values": []any{"go", "rust"}}},
	}
	compReq := []FieldSpec{
		{Name: "fullName", FieldType: "composite", IsRequired: true, Options: map[string]any{
			"subFields": []any{map[string]any{"name": "firstName", "fieldType": "text"}},
		}},
	}
	currReq := []FieldSpec{
		{Name: "price", FieldType: "currency", IsRequired: true,
			Options: map[string]any{"currencyCode": "VND"}},
	}

	cases := []struct {
		name     string
		fields   []FieldSpec
		payload  map[string]any
		wantCode string // "" = success
	}{
		{"required multi_select empty array rejected", multiReq,
			map[string]any{"tags": []any{}}, "RECORD_REQUIRED_MISSING"},
		{"required multi_select present ok", multiReq,
			map[string]any{"tags": []any{"go"}}, ""},
		{"required composite empty object rejected", compReq,
			map[string]any{"fullName": map[string]any{}}, "RECORD_REQUIRED_MISSING"},
		{"required composite present ok", compReq,
			map[string]any{"fullName": map[string]any{"firstName": "Tai"}}, ""},
		{"required currency empty object rejected", currReq,
			map[string]any{"price": map[string]any{}}, "RECORD_REQUIRED_MISSING"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateRecord(tc.fields, tc.payload)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("ValidateRecord code = %q (err=%v); want %q", got, err, tc.wantCode)
			}
		})
	}
}

// TestIsEmptyStructured directly exercises isEmpty for the array/object cases the bug #3 fix added.
func TestIsEmptyStructured(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want bool
	}{
		{"nil", nil, true},
		{"blank string", "  ", true},
		{"non-blank string", "x", false},
		{"empty array", []any{}, true},
		{"non-empty array", []any{"a"}, false},
		{"empty object", map[string]any{}, true},
		{"non-empty object", map[string]any{"k": 1}, false},
		{"number not empty", float64(0), false},
		{"bool not empty", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isEmpty(tc.val); got != tc.want {
				t.Errorf("isEmpty(%v) = %v; want %v", tc.val, got, tc.want)
			}
		})
	}
}

// TestValidateRecord_DefaultApplied locks the bug #4 fix: when a record payload omits a field
// that declares a default, ValidateRecord fills the default (coerced to the field's type) into
// the cleaned data BEFORE the required check — so a defaulted required field passes, and the
// stored value carries the typed default.
func TestValidateRecord_DefaultApplied(t *testing.T) {
	t.Run("string default applied on omit", func(t *testing.T) {
		fields := []FieldSpec{{Name: "status", FieldType: "text", DefaultVal: "active"}}
		clean, err := ValidateRecord(fields, map[string]any{})
		if err != nil {
			t.Fatalf("ValidateRecord: %v", err)
		}
		if clean["status"] != "active" {
			t.Errorf("status = %v; want \"active\"", clean["status"])
		}
	})

	t.Run("default satisfies required", func(t *testing.T) {
		fields := []FieldSpec{{Name: "status", FieldType: "text", IsRequired: true, DefaultVal: "active"}}
		clean, err := ValidateRecord(fields, map[string]any{})
		if got := codeOf(err); got != "" {
			t.Fatalf("ValidateRecord code = %q (err=%v); want success", got, err)
		}
		if clean["status"] != "active" {
			t.Errorf("status = %v; want \"active\"", clean["status"])
		}
	})

	t.Run("number default coerced to float64", func(t *testing.T) {
		fields := []FieldSpec{{Name: "seats", FieldType: "number", DefaultVal: "30"}}
		clean, err := ValidateRecord(fields, map[string]any{})
		if err != nil {
			t.Fatalf("ValidateRecord: %v", err)
		}
		if n, ok := clean["seats"].(float64); !ok || n != 30 {
			t.Errorf("seats = %v (%T); want float64(30)", clean["seats"], clean["seats"])
		}
	})

	t.Run("boolean default coerced to bool", func(t *testing.T) {
		fields := []FieldSpec{{Name: "isActive", FieldType: "boolean", DefaultVal: "true"}}
		clean, err := ValidateRecord(fields, map[string]any{})
		if err != nil {
			t.Fatalf("ValidateRecord: %v", err)
		}
		if b, ok := clean["isActive"].(bool); !ok || !b {
			t.Errorf("isActive = %v (%T); want bool(true)", clean["isActive"], clean["isActive"])
		}
	})

	t.Run("explicit value overrides default", func(t *testing.T) {
		fields := []FieldSpec{{Name: "status", FieldType: "text", DefaultVal: "active"}}
		clean, err := ValidateRecord(fields, map[string]any{"status": "archived"})
		if err != nil {
			t.Fatalf("ValidateRecord: %v", err)
		}
		if clean["status"] != "archived" {
			t.Errorf("status = %v; want \"archived\" (explicit overrides default)", clean["status"])
		}
	})

	t.Run("empty value triggers default", func(t *testing.T) {
		fields := []FieldSpec{{Name: "status", FieldType: "text", DefaultVal: "active"}}
		clean, err := ValidateRecord(fields, map[string]any{"status": "   "})
		if err != nil {
			t.Fatalf("ValidateRecord: %v", err)
		}
		if clean["status"] != "active" {
			t.Errorf("status = %v; want \"active\" (blank falls back to default)", clean["status"])
		}
	})

	t.Run("no default and not required is omitted", func(t *testing.T) {
		fields := []FieldSpec{{Name: "status", FieldType: "text"}}
		clean, err := ValidateRecord(fields, map[string]any{})
		if err != nil {
			t.Fatalf("ValidateRecord: %v", err)
		}
		if _, present := clean["status"]; present {
			t.Errorf("status should be absent when no default and not required")
		}
	})

	t.Run("default that fails coercion is RECORD_INVALID_TYPE", func(t *testing.T) {
		fields := []FieldSpec{{Name: "seats", FieldType: "number", DefaultVal: "thirty"}}
		_, err := ValidateRecord(fields, map[string]any{})
		if got := codeOf(err); got != "RECORD_INVALID_TYPE" {
			t.Fatalf("ValidateRecord code = %q (err=%v); want RECORD_INVALID_TYPE", got, err)
		}
	})

	t.Run("picklist default must be a member", func(t *testing.T) {
		fields := []FieldSpec{{Name: "level", FieldType: "picklist", DefaultVal: "expert",
			Options: map[string]any{"values": []any{"beginner", "advanced"}}}}
		_, err := ValidateRecord(fields, map[string]any{})
		if got := codeOf(err); got != "RECORD_INVALID_PICKLIST" {
			t.Fatalf("ValidateRecord code = %q (err=%v); want RECORD_INVALID_PICKLIST", got, err)
		}
	})
}
