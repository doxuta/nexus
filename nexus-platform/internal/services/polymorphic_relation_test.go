package services

import (
	"errors"
	"testing"
)

// fieldOf extracts the ValidationError.Field hint (details.field) or "" for a non-ValidationError.
func fieldOf(err error) string {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve.Field
	}
	return ""
}

// ---- config-time (validatePolymorphicConfig) --------------------------------------------------

func TestValidatePolymorphicConfig(t *testing.T) {
	tests := []struct {
		name     string
		options  map[string]any
		wantCode string // "" = valid
	}{
		{
			name:     "valid non-empty targetEntities",
			options:  map[string]any{"targetEntities": []any{"accounts", "contacts"}},
			wantCode: "",
		},
		{
			name:     "missing targetEntities",
			options:  map[string]any{},
			wantCode: "FIELD_POLYMORPHIC_NO_TARGETS",
		},
		{
			name:     "nil options",
			options:  nil,
			wantCode: "FIELD_POLYMORPHIC_NO_TARGETS",
		},
		{
			name:     "empty targetEntities array",
			options:  map[string]any{"targetEntities": []any{}},
			wantCode: "FIELD_POLYMORPHIC_NO_TARGETS",
		},
		{
			name:     "non-string element",
			options:  map[string]any{"targetEntities": []any{"accounts", 42}},
			wantCode: "FIELD_POLYMORPHIC_NO_TARGETS",
		},
		{
			name:     "blank string element",
			options:  map[string]any{"targetEntities": []any{"accounts", "  "}},
			wantCode: "FIELD_POLYMORPHIC_NO_TARGETS",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Exercise through the shared validateFieldConfig switch so the wiring is covered too.
			err := validateFieldConfig("related_to", "polymorphic_relation", tc.options)
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("validateFieldConfig code = %q (err=%v); want %q", got, err, tc.wantCode)
			}
		})
	}
}

// TestPolymorphicFieldFlags: polymorphic_relation can be neither unique nor indexed.
func TestPolymorphicFieldFlags(t *testing.T) {
	if got := codeOf(validatePolymorphicFieldFlags("related_to", "polymorphic_relation", true, nil)); got != "FIELD_POLYMORPHIC_NOT_UNIQUE" {
		t.Errorf("unique flag: code = %q; want FIELD_POLYMORPHIC_NOT_UNIQUE", got)
	}
	idxOpts := map[string]any{"targetEntities": []any{"accounts"}, "indexed": true}
	if got := codeOf(validatePolymorphicFieldFlags("related_to", "polymorphic_relation", false, idxOpts)); got != "FIELD_POLYMORPHIC_NOT_INDEXED" {
		t.Errorf("indexed flag: code = %q; want FIELD_POLYMORPHIC_NOT_INDEXED", got)
	}
	// A plain (non-unique, non-indexed) polymorphic field passes the flag guard.
	if err := validatePolymorphicFieldFlags("related_to", "polymorphic_relation", false, map[string]any{"targetEntities": []any{"accounts"}}); err != nil {
		t.Errorf("plain polymorphic field should pass flag guard, got %v", err)
	}
	// polymorphic_relation cannot express a scalar default value.
	if got := codeOf(ValidateFieldDefaultNotSupported("polymorphic_relation", "accounts:x")); got != "FIELD_DEFAULT_NOT_SUPPORTED" {
		t.Errorf("default: code = %q; want FIELD_DEFAULT_NOT_SUPPORTED", got)
	}
}

// TestValidateCreateEntity_PolymorphicColumnConflict: a literal field named after either derived
// physical column (<f>_type / <f>_id) collides and is rejected before any DDL.
func TestValidateCreateEntity_PolymorphicColumnConflict(t *testing.T) {
	in := CreateEntityInput{
		Name:  "activities",
		Label: "Activities",
		Fields: []FieldInput{
			{Name: "related_to", Label: "Related", FieldType: "polymorphic_relation",
				Options: map[string]any{"targetEntities": []any{"accounts"}}},
			{Name: "related_to_id", Label: "Clash", FieldType: "text"},
		},
	}
	if got := codeOf(ValidateCreateEntity(in)); got != "FIELD_RELATION_COLUMN_CONFLICT" {
		t.Fatalf("column conflict code = %q; want FIELD_RELATION_COLUMN_CONFLICT", got)
	}
}

// ---- record-time (checkPolymorphic via ValidateRecord) ----------------------------------------

func polyField(required bool) FieldSpec {
	return FieldSpec{
		Name:       "related_to",
		FieldType:  "polymorphic_relation",
		IsRequired: required,
		Options:    map[string]any{"targetEntities": []any{"accounts", "contacts"}},
	}
}

func TestCheckPolymorphic_Atomic(t *testing.T) {
	tests := []struct {
		name      string
		value     any
		wantCode  string // "" = valid
		wantField string // expected details.field, "" = don't assert
	}{
		{
			name:     "valid {type,id}",
			value:    map[string]any{"type": "accounts", "id": "abc-123"},
			wantCode: "",
		},
		{
			name:      "missing id (has type)",
			value:     map[string]any{"type": "accounts"},
			wantCode:  "RECORD_REQUIRED_MISSING",
			wantField: "related_to.id",
		},
		{
			name:      "missing type (has id)",
			value:     map[string]any{"id": "abc-123"},
			wantCode:  "RECORD_REQUIRED_MISSING",
			wantField: "related_to.type",
		},
		{
			name:     "not an object (bare string)",
			value:    "accounts:abc-123",
			wantCode: "RECORD_INVALID_TYPE",
		},
		{
			name:     "extra key beyond type/id",
			value:    map[string]any{"type": "accounts", "id": "abc-123", "extra": "x"},
			wantCode: "RECORD_INVALID_TYPE",
		},
		{
			name:      "id present but non-string",
			value:     map[string]any{"type": "accounts", "id": 42.0},
			wantCode:  "RECORD_INVALID_TYPE",
			wantField: "related_to.id",
		},
		{
			name:      "type outside targetEntities allowlist",
			value:     map[string]any{"type": "invoices", "id": "abc-123"},
			wantCode:  "RECORD_POLYMORPHIC_TARGET_INVALID",
			wantField: "related_to.type",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateRecord([]FieldSpec{polyField(false)}, map[string]any{"related_to": tc.value})
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("ValidateRecord code = %q (err=%v); want %q", got, err, tc.wantCode)
			}
			if tc.wantField != "" {
				if got := fieldOf(err); got != tc.wantField {
					t.Errorf("details.field = %q; want %q", got, tc.wantField)
				}
			}
		})
	}
}

// TestCheckPolymorphic_AbsentOptional: a polymorphic field omitted entirely from an optional
// record is fine (atomic contract: present-and-complete, or absent).
func TestCheckPolymorphic_AbsentOptional(t *testing.T) {
	clean, err := ValidateRecord([]FieldSpec{polyField(false)}, map[string]any{})
	if err != nil {
		t.Fatalf("absent optional polymorphic field should be valid, got %v", err)
	}
	if _, present := clean["related_to"]; present {
		t.Errorf("absent field should not appear in clean map, got %v", clean)
	}
}

// TestCheckPolymorphic_RequiredMissing: a required polymorphic field absent → RECORD_REQUIRED_MISSING.
func TestCheckPolymorphic_RequiredMissing(t *testing.T) {
	_, err := ValidateRecord([]FieldSpec{polyField(true)}, map[string]any{})
	if got := codeOf(err); got != "RECORD_REQUIRED_MISSING" {
		t.Fatalf("required-missing code = %q; want RECORD_REQUIRED_MISSING", got)
	}
}

// ---- filter / sort allowlist recognition of the sub-columns -----------------------------------

func TestPolymorphicSubColumns_FilterAndSort(t *testing.T) {
	specs := []FieldSpec{
		{Name: "subject", FieldType: "text"},
		polyField(false),
	}
	byName := map[string]FieldSpec{}
	for _, f := range specs {
		byName[f.Name] = f
	}

	// polymorphicSubField recognizes both physical sub-columns and maps them to the base field.
	for _, key := range []string{"related_to_type", "related_to_id"} {
		base, ok := polymorphicSubField(byName, key)
		if !ok || base != "related_to" {
			t.Errorf("polymorphicSubField(%q) = (%q, %v); want (\"related_to\", true)", key, base, ok)
		}
	}
	// A non-sub key is not recognized.
	if _, ok := polymorphicSubField(byName, "subject_type"); ok {
		t.Errorf("subject_type must NOT be recognized (subject is not polymorphic)")
	}

	// Sort allowlist contains the two sub-columns (routed to the physical gen-columns), NOT the
	// bare polymorphic field (a JSON object has no scalar projection).
	allow := buildSortAllowlist(specs)
	if allow["related_to_type"] != "related_to_type" {
		t.Errorf("sort allowlist[related_to_type] = %q; want related_to_type", allow["related_to_type"])
	}
	if allow["related_to_id"] != "related_to_id" {
		t.Errorf("sort allowlist[related_to_id] = %q; want related_to_id", allow["related_to_id"])
	}
	if _, present := allow["related_to"]; present {
		t.Errorf("bare polymorphic field must NOT be sortable, got %v", allow["related_to"])
	}

	// The aggregate/list filter resolver accepts a sub-column filter and routes it to the
	// physical column; an unknown key still fails LIST_UNKNOWN_FILTER.
	resolved, err := resolveFiltersForAggregate(byName, []FilterInput{
		{Field: "related_to_type", Operator: "", Values: []string{"accounts"}},
		{Field: "related_to_id", Operator: "", Values: []string{"abc-123"}},
	})
	if err != nil {
		t.Fatalf("resolveFiltersForAggregate on sub-columns errored: %v", err)
	}
	if len(resolved) != 2 {
		t.Fatalf("resolved %d filters; want 2", len(resolved))
	}
	if resolved[0].PhysicalColumn != "related_to_type" || resolved[1].PhysicalColumn != "related_to_id" {
		t.Errorf("sub-column filters not routed to physical columns: %+v", resolved)
	}

	// The BARE polymorphic field is unfilterable (JSON object, no scalar equality).
	if _, err := resolveFiltersForAggregate(byName, []FilterInput{
		{Field: "related_to", Operator: "", Values: []string{"x"}},
	}); codeOf(err) != "FILTER_OPERATOR_NOT_ALLOWED" {
		t.Errorf("bare polymorphic filter code = %q; want FILTER_OPERATOR_NOT_ALLOWED", codeOf(err))
	}

	// A genuinely unknown filter key still fails.
	if _, err := resolveFiltersForAggregate(byName, []FilterInput{
		{Field: "nope_type", Operator: "", Values: []string{"x"}},
	}); codeOf(err) != "LIST_UNKNOWN_FILTER" {
		t.Errorf("unknown filter code = %q; want LIST_UNKNOWN_FILTER", codeOf(err))
	}
}
