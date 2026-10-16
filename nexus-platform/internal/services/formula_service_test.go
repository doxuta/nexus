package services

import (
	"math"
	"testing"
)

// --- applyComputedFields unit tests ---

// makeFormulaSpec builds a FieldSpec for a formula field with the given expression and resultType.
func makeFormulaSpec(name, expr, resultType, currencyCode string) FieldSpec {
	opts := map[string]any{
		"formula":    expr,
		"resultType": resultType,
	}
	if currencyCode != "" {
		opts["currencyCode"] = currencyCode
	}
	return FieldSpec{Name: name, FieldType: "formula", Options: opts}
}

func makeNumberSpec(name string) FieldSpec {
	return FieldSpec{Name: name, FieldType: "number"}
}

func makeCurrencySpec(name string) FieldSpec {
	return FieldSpec{Name: name, FieldType: "currency", Options: map[string]any{"currencyCode": "USD"}}
}

func TestApplyComputedFields_NumberResult(t *testing.T) {
	specs := []FieldSpec{
		makeNumberSpec("base"),
		makeNumberSpec("multiplier"),
		makeFormulaSpec("computed", "base * multiplier", "number", ""),
	}
	data := map[string]any{
		"base":       float64(10),
		"multiplier": float64(5),
	}
	applyComputedFields(specs, data)
	got, ok := data["computed"].(float64)
	if !ok {
		t.Fatalf("expected float64 for computed, got %T: %v", data["computed"], data["computed"])
	}
	if math.Abs(got-50) > 1e-9 {
		t.Errorf("want 50, got %v", got)
	}
}

func TestApplyComputedFields_CurrencyResult(t *testing.T) {
	specs := []FieldSpec{
		makeCurrencySpec("amount"),
		makeNumberSpec("probability"),
		makeFormulaSpec("weighted_value", "amount * probability / 100", "currency", "USD"),
	}
	data := map[string]any{
		"amount":      map[string]any{"amount": float64(75000), "currencyCode": "USD"},
		"probability": float64(60),
	}
	applyComputedFields(specs, data)
	res, ok := data["weighted_value"].(map[string]any)
	if !ok {
		t.Fatalf("expected map for weighted_value, got %T: %v", data["weighted_value"], data["weighted_value"])
	}
	amt, _ := res["amount"].(float64)
	if math.Abs(amt-45000) > 1e-6 {
		t.Errorf("want 45000, got %v", amt)
	}
	if res["currencyCode"] != "USD" {
		t.Errorf("want USD currencyCode, got %v", res["currencyCode"])
	}
}

func TestApplyComputedFields_DivByZeroWritesNull(t *testing.T) {
	specs := []FieldSpec{
		makeNumberSpec("a"),
		makeNumberSpec("b"),
		makeFormulaSpec("result", "a / b", "number", ""),
	}
	data := map[string]any{"a": float64(10), "b": float64(0)}
	applyComputedFields(specs, data)
	if val, exists := data["result"]; !exists || val != nil {
		t.Errorf("expected result=nil for div-by-zero, got %v (exists=%v)", val, exists)
	}
}

func TestApplyComputedFields_MissingRefTreatedAs0(t *testing.T) {
	specs := []FieldSpec{
		makeNumberSpec("present"),
		makeFormulaSpec("result", "present + missing", "number", ""),
	}
	data := map[string]any{"present": float64(42)}
	applyComputedFields(specs, data)
	got, ok := data["result"].(float64)
	if !ok {
		t.Fatalf("expected float64 for result, got %T: %v", data["result"], data["result"])
	}
	if math.Abs(got-42) > 1e-9 {
		t.Errorf("want 42 (missing=0), got %v", got)
	}
}

func TestApplyComputedFields_CurrencyRefUsesAmountSubValue(t *testing.T) {
	specs := []FieldSpec{
		makeCurrencySpec("price"),
		makeNumberSpec("qty"),
		makeFormulaSpec("total", "price * qty", "number", ""),
	}
	data := map[string]any{
		"price": map[string]any{"amount": float64(100), "currencyCode": "USD"},
		"qty":   float64(3),
	}
	applyComputedFields(specs, data)
	got, ok := data["total"].(float64)
	if !ok {
		t.Fatalf("expected float64 for total, got %T: %v", data["total"], data["total"])
	}
	if math.Abs(got-300) > 1e-9 {
		t.Errorf("want 300, got %v", got)
	}
}

func TestApplyComputedFields_NeverBlocksWrite(t *testing.T) {
	// Bad expression stored in options — applyComputedFields should skip silently.
	specs := []FieldSpec{
		{Name: "bad_formula", FieldType: "formula", Options: map[string]any{
			"formula": "1 @@@", // invalid syntax
		}},
		makeNumberSpec("x"),
	}
	data := map[string]any{"x": float64(5)}
	// Must not panic or return error.
	applyComputedFields(specs, data)
	// bad_formula should be nil (written as null sentinel).
	if val, exists := data["bad_formula"]; !exists || val != nil {
		t.Errorf("expected nil for bad formula field, got %v (exists=%v)", val, exists)
	}
}

func TestApplyComputedFields_EmptyFormulaSkipped(t *testing.T) {
	// A formula field with empty options.formula is a no-op.
	specs := []FieldSpec{
		{Name: "empty_formula", FieldType: "formula", Options: map[string]any{
			"formula": "",
		}},
	}
	data := map[string]any{}
	applyComputedFields(specs, data)
	// No entry written for a formula with empty expression.
	if _, exists := data["empty_formula"]; exists {
		t.Errorf("expected no entry for empty formula, but found one")
	}
}

func TestApplyComputedFields_PartialUpdateMergedData(t *testing.T) {
	// Simulates the partial-PUT scenario: before ⊕ clean is passed to applyComputedFields.
	// Before: amount=75000, probability=60 → weighted_value should have been 45000.
	// Client PUTs only probability=80. Merged: amount=75000, probability=80.
	// Expected weighted_value: 75000 * 80 / 100 = 60000.
	specs := []FieldSpec{
		makeCurrencySpec("amount"),
		makeNumberSpec("probability"),
		makeFormulaSpec("weighted_value", "amount * probability / 100", "currency", "USD"),
	}
	// This is the merged map (before ⊕ clean) — both fields present.
	merged := map[string]any{
		"amount":      map[string]any{"amount": float64(75000), "currencyCode": "USD"},
		"probability": float64(80),
	}
	applyComputedFields(specs, merged)
	res, ok := merged["weighted_value"].(map[string]any)
	if !ok {
		t.Fatalf("expected map for weighted_value, got %T: %v", merged["weighted_value"], merged["weighted_value"])
	}
	amt, _ := res["amount"].(float64)
	if math.Abs(amt-60000) > 1e-6 {
		t.Errorf("want 60000, got %v", amt)
	}
}

// --- ValidateRecord / ValidatePartialRecord formula read-only enforcement ---

func formulaFieldSpecs() []FieldSpec {
	return []FieldSpec{
		makeNumberSpec("price"),
		makeFormulaSpec("total", "price * 2", "number", ""),
	}
}

func TestValidateRecord_FormulaFieldInPayloadIsRejected(t *testing.T) {
	specs := formulaFieldSpecs()
	payload := map[string]any{
		"price": float64(10),
		"total": float64(20), // client trying to set formula field
	}
	_, err := ValidateRecord(specs, payload)
	if err == nil {
		t.Fatal("expected RECORD_FORMULA_READONLY error, got nil")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Code != "RECORD_FORMULA_READONLY" {
		t.Errorf("expected RECORD_FORMULA_READONLY, got: %v", err)
	}
}

func TestValidateRecord_FormulaFieldAbsentIsOK(t *testing.T) {
	specs := formulaFieldSpecs()
	payload := map[string]any{
		"price": float64(10),
		// "total" absent — correct, server will compute it
	}
	clean, err := ValidateRecord(specs, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Formula field must NOT be in clean (it's written by applyComputedFields, not here).
	if _, present := clean["total"]; present {
		t.Errorf("formula field 'total' should not be in clean map from ValidateRecord")
	}
}

func TestValidatePartialRecord_FormulaFieldInPayloadIsRejected(t *testing.T) {
	specs := formulaFieldSpecs()
	payload := map[string]any{
		"total": float64(99), // client trying to set formula field via PUT
	}
	_, err := ValidatePartialRecord(specs, payload)
	if err == nil {
		t.Fatal("expected RECORD_FORMULA_READONLY error, got nil")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Code != "RECORD_FORMULA_READONLY" {
		t.Errorf("expected RECORD_FORMULA_READONLY, got: %v", err)
	}
}

// --- validateFormulaRefs unit tests ---

func TestValidateFormulaRefs_HappyPath(t *testing.T) {
	fields := []FieldInput{
		{Name: "amount", FieldType: "currency"},
		{Name: "probability", FieldType: "number"},
		{Name: "weighted_value", FieldType: "formula", Options: map[string]any{
			"formula": "amount * probability / 100",
		}},
	}
	if err := validateFormulaRefs(fields, nil); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateFormulaRefs_UnknownRef(t *testing.T) {
	fields := []FieldInput{
		{Name: "amount", FieldType: "currency"},
		{Name: "bad_formula", FieldType: "formula", Options: map[string]any{
			"formula": "amount * nonexistent",
		}},
	}
	err := validateFormulaRefs(fields, nil)
	if err == nil {
		t.Fatal("expected FIELD_FORMULA_UNKNOWN_REF, got nil")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Code != "FIELD_FORMULA_UNKNOWN_REF" {
		t.Errorf("expected FIELD_FORMULA_UNKNOWN_REF, got: %v", err)
	}
}

func TestValidateFormulaRefs_NonNumericRef(t *testing.T) {
	fields := []FieldInput{
		{Name: "stage", FieldType: "picklist", Options: map[string]any{
			"values": []any{"a", "b"},
		}},
		{Name: "bad_formula", FieldType: "formula", Options: map[string]any{
			"formula": "stage + 1",
		}},
	}
	err := validateFormulaRefs(fields, nil)
	if err == nil {
		t.Fatal("expected FIELD_FORMULA_NON_NUMERIC_REF, got nil")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Code != "FIELD_FORMULA_NON_NUMERIC_REF" {
		t.Errorf("expected FIELD_FORMULA_NON_NUMERIC_REF, got: %v", err)
	}
}

func TestValidateFormulaRefs_SelfRef(t *testing.T) {
	fields := []FieldInput{
		{Name: "amount", FieldType: "currency"},
		{Name: "self_formula", FieldType: "formula", Options: map[string]any{
			"formula": "self_formula + amount",
		}},
	}
	err := validateFormulaRefs(fields, nil)
	if err == nil {
		t.Fatal("expected FIELD_FORMULA_UNKNOWN_REF for self-ref, got nil")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Code != "FIELD_FORMULA_UNKNOWN_REF" {
		t.Errorf("expected FIELD_FORMULA_UNKNOWN_REF, got: %v", err)
	}
}

func TestValidateFormulaRefs_NestedFormula(t *testing.T) {
	fields := []FieldInput{
		{Name: "amount", FieldType: "currency"},
		{Name: "wv", FieldType: "formula", Options: map[string]any{"formula": "amount * 2"}},
		{Name: "bad_nested", FieldType: "formula", Options: map[string]any{
			"formula": "wv + 1",
		}},
	}
	err := validateFormulaRefs(fields, nil)
	if err == nil {
		t.Fatal("expected FIELD_FORMULA_NESTED, got nil")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Code != "FIELD_FORMULA_NESTED" {
		t.Errorf("expected FIELD_FORMULA_NESTED, got: %v", err)
	}
}

func TestValidateFormulaRefs_NoFormulaFields(t *testing.T) {
	// No formula fields → no validation needed, should succeed.
	fields := []FieldInput{
		{Name: "amount", FieldType: "currency"},
		{Name: "probability", FieldType: "number"},
	}
	if err := validateFormulaRefs(fields, nil); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
