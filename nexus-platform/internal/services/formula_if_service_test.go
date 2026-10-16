package services

import (
	"context"
	"math"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
)

// Service-level tests for formula comparisons + IF (DAILY-QUEUE #25). The engine itself is
// tested in internal/formula; these pin the SEAMS: the config-time validator every write path
// shares (create, add-field, edit-field, AI propose, the live-editor dry-run) reports a type
// error under its own code, and applyComputedFields persists the tiered result.

const commissionExpr = "IF(amount >= 100000000, amount * 0.05, amount * 0.03)"

// TestValidateFormulaConfig_TypeMismatchCode: validateFormulaConfig is the structural check
// that runs FIRST on every path (validateFieldConfig on create/add AND on edit via
// ValidateEditFieldChanges), so the type check must surface here with its own code — not be
// swallowed into the generic "invalid expression".
func TestValidateFormulaConfig_TypeMismatchCode(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		wantCode string // "" = valid
	}{
		{name: "tiered commission is valid", expr: commissionExpr},
		{name: "bare comparison cannot be stored in a number field", expr: "amount >= 100", wantCode: "FIELD_FORMULA_TYPE_MISMATCH"},
		{name: "number as IF condition", expr: "IF(amount, 1, 2)", wantCode: "FIELD_FORMULA_TYPE_MISMATCH"},
		{name: "boolean in arithmetic", expr: "(amount > 1) * 2", wantCode: "FIELD_FORMULA_TYPE_MISMATCH"},
		{name: "number THEN, boolean ELSE", expr: "IF(amount > 0, 5, amount > 1)", wantCode: "FIELD_FORMULA_TYPE_MISMATCH"},
		{name: "IF arity is a syntax error", expr: "IF(amount > 1, 2)", wantCode: "FIELD_FORMULA_INVALID"},
		{name: "unknown function is a syntax error", expr: "ROUND(amount)", wantCode: "FIELD_FORMULA_INVALID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFormulaConfig("commission", map[string]any{"formula": tt.expr, "resultType": "currency"})
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("validateFormulaConfig(%q): %v", tt.expr, err)
				}
				return
			}
			if got := w5Code(t, err); got != tt.wantCode {
				t.Fatalf("validateFormulaConfig(%q) code = %q; want %q (err: %v)", tt.expr, got, tt.wantCode, err)
			}
		})
	}
}

// TestValidateEditFieldChanges_FormulaTypeMismatch: the edit path (PUT /admin/fields/:id and
// the AI propose_edit_field) re-runs validateFieldConfig, so turning a working formula into a
// mistyped one is rejected at save, not discovered as a silent null on the next record write.
func TestValidateEditFieldChanges_FormulaTypeMismatch(t *testing.T) {
	opts := `{"formula":"amount * 0.05","resultType":"currency"}`
	stored := metadata.FieldDefinition{Name: "commission", FieldType: "formula", Options: &opts}
	_, _, err := ValidateEditFieldChanges(stored, EditFieldInput{
		FieldID: "f-1",
		Options: map[string]any{"formula": "IF(amount, 1, 2)", "resultType": "currency"},
	}, nil, nil)
	if got := w5Code(t, err); got != "FIELD_FORMULA_TYPE_MISMATCH" {
		t.Fatalf("code = %q; want FIELD_FORMULA_TYPE_MISMATCH (err: %v)", got, err)
	}
}

// TestValidateFormula_DryRunIF: the live Studio editor (POST /admin/formula/validate) must say
// "valid" for the acceptance formula and name the type error for a mistyped one — the SAME
// code a real save returns.
func TestValidateFormula_DryRunIF(t *testing.T) {
	svc := NewFieldService(&fakeFieldSchema{}, formulaFixtureMeta())
	res, err := svc.ValidateFormula(context.Background(), "tenant-1", ValidateFormulaInput{
		EntityName: "courses", Formula: "IF(qty >= 10, price * qty * 0.95, price * qty)",
	})
	if err != nil {
		t.Fatalf("ValidateFormula: %v", err)
	}
	if !res.Valid || strings.Join(res.Fields, ",") != "qty,price" {
		t.Errorf("result = %+v; want valid with fields [qty price]", res)
	}

	_, err = svc.ValidateFormula(context.Background(), "tenant-1", ValidateFormulaInput{
		EntityName: "courses", Formula: "qty >= 10",
	})
	if got := w5Code(t, err); got != "FIELD_FORMULA_TYPE_MISMATCH" {
		t.Errorf("code = %q; want FIELD_FORMULA_TYPE_MISMATCH", got)
	}
	// Ref checks still reach inside IF.
	_, err = svc.ValidateFormula(context.Background(), "tenant-1", ValidateFormulaInput{
		EntityName: "courses", Formula: "IF(note > 1, 1, 0)",
	})
	if got := w5Code(t, err); got != "FIELD_FORMULA_NON_NUMERIC_REF" {
		t.Errorf("code = %q; want FIELD_FORMULA_NON_NUMERIC_REF", got)
	}
}

// TestApplyComputedFields_TieredCommission: the record-write hook stores the tier the rule
// picks, as a currency object, for both sides of the threshold.
func TestApplyComputedFields_TieredCommission(t *testing.T) {
	specs := []FieldSpec{
		makeCurrencySpec("amount"),
		makeFormulaSpec("commission", commissionExpr, "currency", "VND"),
	}
	for _, tc := range []struct{ amount, want float64 }{
		{amount: 120_000_000, want: 6_000_000},
		{amount: 40_000_000, want: 1_200_000},
	} {
		data := map[string]any{"amount": map[string]any{"amount": tc.amount, "currencyCode": "VND"}}
		applyComputedFields(specs, data)
		res, ok := data["commission"].(map[string]any)
		if !ok {
			t.Fatalf("amount=%v: commission = %T %v; want currency object", tc.amount, data["commission"], data["commission"])
		}
		if got, _ := res["amount"].(float64); math.Abs(got-tc.want) > 1e-6 {
			t.Errorf("amount=%v: commission = %v; want %v", tc.amount, got, tc.want)
		}
	}
}

// TestApplyComputedFields_GuardedDivisionIsNotNull: IF must short-circuit — a guard written
// precisely to avoid dividing by zero must yield the else value, not a null.
func TestApplyComputedFields_GuardedDivisionIsNotNull(t *testing.T) {
	specs := []FieldSpec{
		makeNumberSpec("revenue"),
		makeNumberSpec("units"),
		makeFormulaSpec("unit_price", "IF(units > 0, revenue / units, 0)", "number", ""),
	}
	data := map[string]any{"revenue": float64(500), "units": float64(0)}
	applyComputedFields(specs, data)
	got, ok := data["unit_price"].(float64)
	if !ok || got != 0 {
		t.Fatalf("unit_price = %T %v; want float64 0 (else branch), not null", data["unit_price"], data["unit_price"])
	}
}

// BenchmarkApplyComputedFields_Import models an import (queue #108): the same two formula
// fields evaluated for every row. With the AST cache the per-row cost is evaluation only; run
// with -benchmem to see the parse allocations disappear.
func BenchmarkApplyComputedFields_Import(b *testing.B) {
	specs := []FieldSpec{
		makeCurrencySpec("amount"),
		makeNumberSpec("qty"),
		makeFormulaSpec("commission", commissionExpr, "currency", "VND"),
		makeFormulaSpec("discount", "IF(qty >= 100, 0.15, IF(qty >= 10, 0.05, 0)) * amount", "number", ""),
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		data := map[string]any{"amount": map[string]any{"amount": float64(120_000_000 + i), "currencyCode": "VND"}, "qty": float64(i % 200)}
		applyComputedFields(specs, data)
	}
}
