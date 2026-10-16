package services

import "testing"

func courseFields() []FieldSpec {
	return []FieldSpec{
		{Name: "title", FieldType: "text", IsRequired: true},
		{Name: "seats", FieldType: "number"},
		{Name: "isActive", FieldType: "boolean"},
		{Name: "level", FieldType: "picklist", Options: map[string]any{"values": []any{"beginner", "advanced"}}},
		{Name: "contact", FieldType: "email"},
	}
}

func TestValidateRecord(t *testing.T) {
	cases := []struct {
		name     string
		payload  map[string]any
		wantCode string // "" = success
	}{
		{"valid full", map[string]any{
			"title": "Go 101", "seats": float64(30), "isActive": true,
			"level": "beginner", "contact": "a@b.com",
		}, ""},
		{"valid minimal (only required)", map[string]any{"title": "Go 101"}, ""},
		{"unknown field rejected (strict)", map[string]any{"title": "x", "hacker": 1}, "RECORD_UNKNOWN_FIELD"},
		{"missing required", map[string]any{"seats": float64(10)}, "RECORD_REQUIRED_MISSING"},
		{"empty required string", map[string]any{"title": "   "}, "RECORD_REQUIRED_MISSING"},
		{"number wrong type", map[string]any{"title": "x", "seats": "thirty"}, "RECORD_INVALID_TYPE"},
		{"boolean wrong type", map[string]any{"title": "x", "isActive": "yes"}, "RECORD_INVALID_TYPE"},
		{"picklist out of range", map[string]any{"title": "x", "level": "expert"}, "RECORD_INVALID_PICKLIST"},
		{"email malformed", map[string]any{"title": "x", "contact": "not-an-email"}, "RECORD_INVALID_EMAIL"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clean, err := ValidateRecord(courseFields(), tc.payload)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("ValidateRecord code = %q (err=%v); want %q", got, err, tc.wantCode)
			}
			// On success, cleaned data must not carry undeclared keys.
			if tc.wantCode == "" {
				for k := range clean {
					if k != "title" && k != "seats" && k != "isActive" && k != "level" && k != "contact" {
						t.Errorf("cleaned data has undeclared key %q", k)
					}
				}
			}
		})
	}
}

// TestValidatePartialRecord covers the apply-when-present (PUT partial-merge) contract:
// only keys present in the body are validated/returned, absent fields are neither defaulted nor
// reported missing, but present keys stay strictly type-checked and a present-but-empty required
// field is still rejected.
func TestValidatePartialRecord(t *testing.T) {
	fields := []FieldSpec{
		{Name: "title", FieldType: "text", IsRequired: true},
		{Name: "seats", FieldType: "number", DefaultVal: "10"},
		{Name: "level", FieldType: "picklist", Options: map[string]any{"values": []any{"beginner", "advanced"}}},
		{Name: "contact", FieldType: "email"},
	}

	cases := []struct {
		name     string
		payload  map[string]any
		wantCode string // "" = success
		wantKeys []string
	}{
		{"partial: only one optional field", map[string]any{"seats": float64(5)}, "", []string{"seats"}},
		{"partial: only required field", map[string]any{"title": "X"}, "", []string{"title"}},
		{"absent required is NOT required (no body key)", map[string]any{"seats": float64(7)}, "", []string{"seats"}},
		{"absent field with default is NOT materialized", map[string]any{"title": "X"}, "", []string{"title"}},
		{"present empty optional is dropped (no-op merge)", map[string]any{"title": "X", "contact": "  "}, "", []string{"title"}},
		{"present empty required rejected", map[string]any{"title": "   "}, "RECORD_REQUIRED_MISSING", nil},
		{"unknown key rejected (strict)", map[string]any{"hacker": 1}, "RECORD_UNKNOWN_FIELD", nil},
		{"present wrong type rejected", map[string]any{"seats": "thirty"}, "RECORD_INVALID_TYPE", nil},
		{"present out-of-range picklist rejected", map[string]any{"level": "expert"}, "RECORD_INVALID_PICKLIST", nil},
		{"empty body is a valid no-op", map[string]any{}, "", []string{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clean, err := ValidatePartialRecord(fields, tc.payload)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("ValidatePartialRecord code = %q (err=%v); want %q", got, err, tc.wantCode)
			}
			if tc.wantCode != "" {
				return
			}
			if len(clean) != len(tc.wantKeys) {
				t.Fatalf("cleaned keys = %v; want exactly %v", clean, tc.wantKeys)
			}
			for _, k := range tc.wantKeys {
				if _, ok := clean[k]; !ok {
					t.Errorf("cleaned data missing expected key %q (got %v)", k, clean)
				}
			}
		})
	}
}

// nestedFields declares one field of each Đ2 nested-JSON type, used to value-validate records
// against multi_select / currency / rating / rich_text / composite.
func nestedFields() []FieldSpec {
	return []FieldSpec{
		{Name: "tags", FieldType: "multi_select", Options: map[string]any{"values": []any{"go", "rust", "python"}}},
		{Name: "price", FieldType: "currency", Options: map[string]any{"currencyCode": "VND"}},
		{Name: "stars", FieldType: "rating", Options: map[string]any{"max": float64(5)}},
		{Name: "bio", FieldType: "rich_text"},
		{Name: "fullName", FieldType: "composite", Options: map[string]any{
			"subFields": []any{
				map[string]any{"name": "firstName", "fieldType": "text"},
				map[string]any{"name": "age", "fieldType": "number"},
			},
		}},
	}
}

// TestValidateRecordNestedTypes table-drives value-validation for the five Đ2 nested-JSON
// field types: each type gets at least one valid case and the failure modes the reviewer
// called out (element outside values, currency missing key, rating out of range, composite
// sub-field type mismatch / unknown sub-key).
func TestValidateRecordNestedTypes(t *testing.T) {
	cases := []struct {
		name     string
		payload  map[string]any
		wantCode string // "" = success
	}{
		// multi_select
		{"multi_select valid subset", map[string]any{"tags": []any{"go", "rust"}}, ""},
		{"multi_select empty array ok", map[string]any{"tags": []any{}}, ""},
		{"multi_select element out of values", map[string]any{"tags": []any{"go", "java"}}, "RECORD_INVALID_PICKLIST"},
		{"multi_select not an array", map[string]any{"tags": "go"}, "RECORD_INVALID_TYPE"},
		{"multi_select non-string element", map[string]any{"tags": []any{float64(1)}}, "RECORD_INVALID_TYPE"},

		// currency
		{"currency valid", map[string]any{"price": map[string]any{"amount": float64(1000), "currencyCode": "VND"}}, ""},
		{"currency not an object", map[string]any{"price": float64(1000)}, "RECORD_INVALID_TYPE"},
		{"currency missing amount", map[string]any{"price": map[string]any{"currencyCode": "VND"}}, "RECORD_INVALID_TYPE"},
		{"currency amount not number", map[string]any{"price": map[string]any{"amount": "1000", "currencyCode": "VND"}}, "RECORD_INVALID_TYPE"},
		{"currency missing currencyCode", map[string]any{"price": map[string]any{"amount": float64(1000)}}, "RECORD_INVALID_TYPE"},
		{"currency empty currencyCode", map[string]any{"price": map[string]any{"amount": float64(1000), "currencyCode": "  "}}, "RECORD_INVALID_TYPE"},
		// Queue #7 (Q7): the field is configured VND — a USD row is REJECTED, never relabelled.
		{"currency code mismatches configured code", map[string]any{"price": map[string]any{"amount": float64(1000), "currencyCode": "USD"}}, "RECORD_CURRENCY_MISMATCH"},

		// rating
		{"rating valid in range", map[string]any{"stars": float64(4)}, ""},
		{"rating valid lower bound", map[string]any{"stars": float64(1)}, ""},
		{"rating above max", map[string]any{"stars": float64(6)}, "RECORD_INVALID_RANGE"},
		{"rating below min", map[string]any{"stars": float64(0)}, "RECORD_INVALID_RANGE"},
		{"rating not integer", map[string]any{"stars": float64(3.5)}, "RECORD_INVALID_TYPE"},
		{"rating wrong type", map[string]any{"stars": "five"}, "RECORD_INVALID_TYPE"},

		// rich_text
		{"rich_text valid", map[string]any{"bio": "<b>hi</b>"}, ""},
		{"rich_text wrong type", map[string]any{"bio": float64(1)}, "RECORD_INVALID_TYPE"},

		// composite
		{"composite valid full", map[string]any{"fullName": map[string]any{"firstName": "Tai", "age": float64(30)}}, ""},
		{"composite valid partial", map[string]any{"fullName": map[string]any{"firstName": "Tai"}}, ""},
		{"composite not an object", map[string]any{"fullName": "Tai"}, "RECORD_INVALID_TYPE"},
		{"composite sub-field type mismatch", map[string]any{"fullName": map[string]any{"age": "thirty"}}, "RECORD_INVALID_TYPE"},
		{"composite unknown sub-key", map[string]any{"fullName": map[string]any{"firstName": "Tai", "hacker": 1}}, "RECORD_UNKNOWN_FIELD"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateRecord(nestedFields(), tc.payload)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("ValidateRecord code = %q (err=%v); want %q", got, err, tc.wantCode)
			}
		})
	}
}

// TestValidateRecordDateTimeFormat covers the date/datetime format validation: a value (or a
// schema default) that is a string but not a parseable date / datetime is rejected with
// RECORD_INVALID_DATE instead of leaking junk into the data JSON (bug #12).
func TestValidateRecordDateTimeFormat(t *testing.T) {
	fields := []FieldSpec{
		{Name: "startDate", FieldType: "date"},
		{Name: "startAt", FieldType: "datetime"},
	}
	cases := []struct {
		name     string
		payload  map[string]any
		wantCode string // "" = success
	}{
		// date
		{"date valid", map[string]any{"startDate": "2026-06-24"}, ""},
		{"date trims whitespace", map[string]any{"startDate": "  2026-06-24  "}, ""},
		{"date impossible calendar value", map[string]any{"startDate": "2026-13-99"}, "RECORD_INVALID_DATE"},
		{"date free text", map[string]any{"startDate": "hello"}, "RECORD_INVALID_DATE"},
		{"date with time rejected", map[string]any{"startDate": "2026-06-24T10:00:00Z"}, "RECORD_INVALID_DATE"},
		{"date wrong type", map[string]any{"startDate": float64(20260624)}, "RECORD_INVALID_TYPE"},
		// datetime
		{"datetime RFC3339", map[string]any{"startAt": "2026-06-24T10:30:00Z"}, ""},
		{"datetime RFC3339 offset", map[string]any{"startAt": "2026-06-24T10:30:00+07:00"}, ""},
		{"datetime local (no tz)", map[string]any{"startAt": "2026-06-24T10:30:00"}, ""},
		{"datetime date-only rejected", map[string]any{"startAt": "2026-06-24"}, "RECORD_INVALID_DATE"},
		{"datetime free text", map[string]any{"startAt": "hello"}, "RECORD_INVALID_DATE"},
		{"datetime wrong type", map[string]any{"startAt": float64(1)}, "RECORD_INVALID_TYPE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateRecord(fields, tc.payload)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("ValidateRecord code = %q (err=%v); want %q", got, err, tc.wantCode)
			}
		})
	}

	// A malformed date/datetime DEFAULT must surface the same way (default path runs through
	// checkType), so a bad schema default never silently materializes into a record.
	t.Run("bad date default rejected", func(t *testing.T) {
		f := []FieldSpec{{Name: "startDate", FieldType: "date", DefaultVal: "2026-13-99"}}
		_, err := ValidateRecord(f, map[string]any{})
		if got := codeOf(err); got != "RECORD_INVALID_DATE" {
			t.Fatalf("code = %q (err=%v); want RECORD_INVALID_DATE", got, err)
		}
	})
	t.Run("good date default materializes", func(t *testing.T) {
		f := []FieldSpec{{Name: "startDate", FieldType: "date", DefaultVal: "2026-06-24"}}
		clean, err := ValidateRecord(f, map[string]any{})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if clean["startDate"] != "2026-06-24" {
			t.Fatalf("default not applied: got %v", clean["startDate"])
		}
	})
}

// TestRatingDefaultRunsCheckRating closes bug #7: a rating field's DEFAULT must clear the same
// bar as an explicitly-sent rating value — it goes through checkRating (integer + [min,max]),
// so an out-of-range or non-integer default is rejected at validate time instead of persisting
// junk. A valid in-range default still materializes.
func TestRatingDefaultRunsCheckRating(t *testing.T) {
	mk := func(def string) []FieldSpec {
		return []FieldSpec{{Name: "stars", FieldType: "rating", DefaultVal: def, Options: map[string]any{"max": float64(5)}}}
	}
	cases := []struct {
		name     string
		def      string
		wantCode string // "" = success
		wantVal  float64
	}{
		{"in-range integer default applied", "3", "", 3},
		{"upper-bound default applied", "5", "", 5},
		{"above-max default rejected", "100", "RECORD_INVALID_RANGE", 0},
		{"below-min default rejected", "0", "RECORD_INVALID_RANGE", 0},
		{"non-integer default rejected", "3.5", "RECORD_INVALID_TYPE", 0},
		{"non-numeric default rejected", "abc", "RECORD_INVALID_TYPE", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clean, err := ValidateRecord(mk(tc.def), map[string]any{})
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("code = %q (err=%v); want %q", got, err, tc.wantCode)
			}
			if tc.wantCode == "" && clean["stars"] != tc.wantVal {
				t.Fatalf("default value = %v; want %v", clean["stars"], tc.wantVal)
			}
		})
	}
}
