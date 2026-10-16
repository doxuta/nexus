package services

// ENG-COMPUTED #2/#3 readonly-from-client enforcement — mirrors
// formula_service_test.go's ValidateRecord/ValidatePartialRecord formula tests exactly, for the
// two new server-computed field types (lookup, rollup). Also covers the CSV-import parity fix
// (record_import.go buildMapping previously only checked FieldType=="formula").

import "testing"

func lookupRollupFieldSpecs() []FieldSpec {
	return []FieldSpec{
		{Name: "name", FieldType: "text", IsRequired: true},
		{Name: "account", FieldType: "relation", Options: map[string]any{"relatedEntity": "accounts"}},
		{Name: "account_industry", FieldType: "lookup", Options: map[string]any{"via": "account", "sourceField": "industry"}},
		{Name: "deal_total", FieldType: "rollup", Options: map[string]any{"childEntity": "deals", "viaRelation": "account", "metric": "sum", "aggField": "amount"}},
	}
}

// --- POST (ValidateRecord) ---

func TestValidateRecord_LookupFieldInPayloadIsRejected(t *testing.T) {
	specs := lookupRollupFieldSpecs()
	payload := map[string]any{
		"name":             "Acme",
		"account_industry": "Manufacturing", // client trying to set a computed lookup field
	}
	_, err := ValidateRecord(specs, payload)
	if codeOf(err) != "RECORD_LOOKUP_READONLY" {
		t.Fatalf("code = %q; want RECORD_LOOKUP_READONLY (err=%v)", codeOf(err), err)
	}
}

func TestValidateRecord_RollupFieldInPayloadIsRejected(t *testing.T) {
	specs := lookupRollupFieldSpecs()
	payload := map[string]any{
		"name":       "Acme",
		"deal_total": float64(1000), // client trying to set a computed rollup field
	}
	_, err := ValidateRecord(specs, payload)
	if codeOf(err) != "RECORD_ROLLUP_READONLY" {
		t.Fatalf("code = %q; want RECORD_ROLLUP_READONLY (err=%v)", codeOf(err), err)
	}
}

func TestValidateRecord_LookupRollupAbsentIsOK(t *testing.T) {
	specs := lookupRollupFieldSpecs()
	payload := map[string]any{"name": "Acme"}
	clean, err := ValidateRecord(specs, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := clean["account_industry"]; present {
		t.Errorf("lookup field must not be in clean map from ValidateRecord")
	}
	if _, present := clean["deal_total"]; present {
		t.Errorf("rollup field must not be in clean map from ValidateRecord")
	}
}

// --- PUT (ValidatePartialRecord) ---

func TestValidatePartialRecord_LookupFieldInPayloadIsRejected(t *testing.T) {
	specs := lookupRollupFieldSpecs()
	payload := map[string]any{"account_industry": "Retail"}
	_, err := ValidatePartialRecord(specs, payload)
	if codeOf(err) != "RECORD_LOOKUP_READONLY" {
		t.Fatalf("code = %q; want RECORD_LOOKUP_READONLY (err=%v)", codeOf(err), err)
	}
}

func TestValidatePartialRecord_RollupFieldInPayloadIsRejected(t *testing.T) {
	specs := lookupRollupFieldSpecs()
	payload := map[string]any{"deal_total": float64(500)}
	_, err := ValidatePartialRecord(specs, payload)
	if codeOf(err) != "RECORD_ROLLUP_READONLY" {
		t.Fatalf("code = %q; want RECORD_ROLLUP_READONLY (err=%v)", codeOf(err), err)
	}
}

// --- CSV import parity (record_import.go buildMapping) ---

func TestBuildMapping_LookupFieldRejected(t *testing.T) {
	specs := lookupRollupFieldSpecs()
	_, err := buildMapping([]string{"Industry"}, map[string]string{"Industry": "account_industry"}, specs, nil)
	if codeOf(err) != "RECORD_LOOKUP_READONLY" {
		t.Fatalf("code = %q; want RECORD_LOOKUP_READONLY (err=%v)", codeOf(err), err)
	}
}

func TestBuildMapping_RollupFieldRejected(t *testing.T) {
	specs := lookupRollupFieldSpecs()
	_, err := buildMapping([]string{"Total"}, map[string]string{"Total": "deal_total"}, specs, nil)
	if codeOf(err) != "RECORD_ROLLUP_READONLY" {
		t.Fatalf("code = %q; want RECORD_ROLLUP_READONLY (err=%v)", codeOf(err), err)
	}
}

func TestBuildMapping_FormulaFieldStillRejected(t *testing.T) {
	// Regression guard: the computedReadonlyCode refactor must not change formula's behaviour.
	specs := []FieldSpec{
		{Name: "price", FieldType: "number"},
		{Name: "total", FieldType: "formula", Options: map[string]any{"formula": "price * 2"}},
	}
	_, err := buildMapping([]string{"Total"}, map[string]string{"Total": "total"}, specs, nil)
	if codeOf(err) != "RECORD_FORMULA_READONLY" {
		t.Fatalf("code = %q; want RECORD_FORMULA_READONLY (err=%v)", codeOf(err), err)
	}
}

func TestBuildMapping_OrdinaryFieldAccepted(t *testing.T) {
	specs := lookupRollupFieldSpecs()
	cm, err := buildMapping([]string{"Name"}, map[string]string{"Name": "name"}, specs, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := cm["Name"]; !ok {
		t.Errorf("ordinary field mapping missing from columnMapping: %v", cm)
	}
}

// --- required-coverage parity (loop 2 of buildMapping) ---
//
// No config-time guard currently prevents a lookup/rollup field from being marked IsRequired
// (unlike formula, which the doc-comments claim is guarded but no code actually enforces
// either — see entity_service.go's validateLookupConfig/validateRollupConfig, neither rejects
// IsRequired). So this path IS reachable: a required lookup/rollup field must be excluded from
// the "every required field needs a CSV mapping" check, exactly like formula — demanding a
// mapping for a field CSV import can never accept (buildMapping's mapping loop always rejects
// it) would be an unreachable, un-satisfiable requirement.

func TestBuildMapping_RequiredLookupField_UnmappedNotFlagged(t *testing.T) {
	specs := []FieldSpec{
		{Name: "name", FieldType: "text", IsRequired: true},
		{Name: "account", FieldType: "relation", Options: map[string]any{"relatedEntity": "accounts"}},
		{Name: "account_industry", FieldType: "lookup", IsRequired: true,
			Options: map[string]any{"via": "account", "sourceField": "industry"}},
	}
	// Map only "name" — "account_industry" (required, but computed) is deliberately left
	// unmapped and must NOT trip IMPORT_REQUIRED_FIELD_NOT_MAPPED.
	_, err := buildMapping([]string{"Name"}, map[string]string{"Name": "name"}, specs, nil)
	if err != nil {
		t.Fatalf("required-but-computed lookup field wrongly flagged as unmapped: %v", err)
	}
}

func TestBuildMapping_RequiredRollupField_UnmappedNotFlagged(t *testing.T) {
	specs := []FieldSpec{
		{Name: "name", FieldType: "text", IsRequired: true},
		{Name: "deal_total", FieldType: "rollup", IsRequired: true,
			Options: map[string]any{"childEntity": "deals", "viaRelation": "account", "metric": "sum", "aggField": "amount"}},
	}
	_, err := buildMapping([]string{"Name"}, map[string]string{"Name": "name"}, specs, nil)
	if err != nil {
		t.Fatalf("required-but-computed rollup field wrongly flagged as unmapped: %v", err)
	}
}

func TestBuildMapping_RequiredFormulaField_UnmappedNotFlagged_Regression(t *testing.T) {
	// Regression guard: the computedReadonlyCode refactor of loop 2 must not change formula's
	// pre-existing behaviour (it already special-cased FieldType=="formula").
	specs := []FieldSpec{
		{Name: "price", FieldType: "number", IsRequired: true},
		{Name: "total", FieldType: "formula", IsRequired: true, Options: map[string]any{"formula": "price * 2"}},
	}
	_, err := buildMapping([]string{"Price"}, map[string]string{"Price": "price"}, specs, nil)
	if err != nil {
		t.Fatalf("required-but-computed formula field wrongly flagged as unmapped: %v", err)
	}
}

// TestBuildMapping_RequiredOrdinaryField_UnmappedStillFlagged is the control: a genuinely
// required, non-computed field left unmapped must STILL be rejected — the computedReadonlyCode
// change must not have widened the skip to non-computed types.
func TestBuildMapping_RequiredOrdinaryField_UnmappedStillFlagged(t *testing.T) {
	specs := []FieldSpec{
		{Name: "name", FieldType: "text", IsRequired: true},
		{Name: "code", FieldType: "text", IsRequired: true},
	}
	_, err := buildMapping([]string{"Name"}, map[string]string{"Name": "name"}, specs, nil)
	if codeOf(err) != "IMPORT_REQUIRED_FIELD_NOT_MAPPED" {
		t.Fatalf("code = %q; want IMPORT_REQUIRED_FIELD_NOT_MAPPED (a real required field was left unmapped)", codeOf(err))
	}
}
