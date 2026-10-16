package formula

import (
	"math"
	"strings"
	"testing"
	"time"
)

// staticResolver returns a Resolver that looks up values from a fixed map.
func staticResolver(vals map[string]float64) Resolver {
	return func(name string) float64 {
		return vals[name] // returns 0 for missing keys
	}
}

// --- Evaluate tests ---

func TestEvaluate_Arithmetic(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		vals     map[string]float64
		wantVal  float64
		wantDivZ bool
		wantErr  bool
	}{
		// Numeric literals only
		{name: "add", expr: "1 + 2", wantVal: 3},
		{name: "sub", expr: "10 - 4", wantVal: 6},
		{name: "mul", expr: "3 * 4", wantVal: 12},
		{name: "div", expr: "10 / 4", wantVal: 2.5},
		{name: "float literal", expr: "1.5 + 2.5", wantVal: 4},

		// Precedence: * before +
		{name: "precedence mul+add", expr: "2 + 3 * 4", wantVal: 14},
		{name: "precedence add+mul", expr: "3 * 4 + 2", wantVal: 14},
		{name: "precedence div+sub", expr: "10 - 6 / 2", wantVal: 7},

		// Parentheses override precedence
		{name: "parens override", expr: "(2 + 3) * 4", wantVal: 20},
		{name: "nested parens", expr: "((2 + 3) * (1 + 1))", wantVal: 10},

		// Unary minus
		{name: "unary minus literal", expr: "-5", wantVal: -5},
		{name: "unary minus expr", expr: "-(2 + 3)", wantVal: -5},
		{name: "double unary", expr: "--5", wantVal: 5},

		// Field references resolve to 0 when absent
		{name: "missing field = 0", expr: "amount + 10", vals: nil, wantVal: 10},

		// Field references present
		{name: "field ref", expr: "amount * 2",
			vals: map[string]float64{"amount": 50}, wantVal: 100},
		{name: "two refs", expr: "amount * probability / 100",
			vals: map[string]float64{"amount": 75000, "probability": 60}, wantVal: 45000},

		// Division by zero
		{name: "div by zero literal", expr: "1 / 0", wantDivZ: true},
		{name: "div by zero field", expr: "amount / prob",
			vals: map[string]float64{"amount": 100, "prob": 0}, wantDivZ: true},
		// div-by-zero in subexpr propagates
		{name: "div by zero propagates", expr: "(1/0) + 5", wantDivZ: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, dz, err := Evaluate(tt.expr, staticResolver(tt.vals))
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil (val=%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if dz != tt.wantDivZ {
				t.Errorf("divByZero=%v, want %v", dz, tt.wantDivZ)
			}
			if !dz && math.Abs(got-tt.wantVal) > 1e-9 {
				t.Errorf("Evaluate(%q) = %v, want %v", tt.expr, got, tt.wantVal)
			}
		})
	}
}

func TestEvaluate_ParseErrors(t *testing.T) {
	malformed := []struct {
		name string
		expr string
	}{
		{name: "empty", expr: ""},
		{name: "trailing junk", expr: "1 + 2 )"},
		{name: "missing operand", expr: "1 +"},
		{name: "missing close paren", expr: "(1 + 2"},
		{name: "bad char", expr: "1 @ 2"},
		{name: "double operator", expr: "1 ** 2"},
	}
	for _, tt := range malformed {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Evaluate(tt.expr, staticResolver(nil))
			if err == nil {
				t.Errorf("expected parse error for %q, got nil", tt.expr)
			}
		})
	}
}

// --- Validate tests ---

var numericFields = []FieldInfo{
	{Name: "amount", FieldType: "currency"},
	{Name: "probability", FieldType: "number"},
	{Name: "discount", FieldType: "number"},
	{Name: "weighted_value", FieldType: "formula"}, // another formula field
	{Name: "stage", FieldType: "picklist"},         // non-numeric
}

func TestValidate_HappyPath(t *testing.T) {
	refs, err := Validate("amount * probability / 100", "new_formula", numericFields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(refs) != 2 {
		t.Errorf("want 2 refs, got %d: %v", len(refs), refs)
	}
}

func TestValidate_SelfReference(t *testing.T) {
	_, err := Validate("self_field + 1", "self_field", numericFields)
	if err == nil {
		t.Fatal("expected error for self-reference")
	}
	if !strings.Contains(err.Error(), "FIELD_FORMULA_UNKNOWN_REF") {
		t.Errorf("expected FIELD_FORMULA_UNKNOWN_REF, got: %v", err)
	}
}

func TestValidate_UnknownRef(t *testing.T) {
	_, err := Validate("nonexistent + 1", "my_formula", numericFields)
	if err == nil {
		t.Fatal("expected error for unknown ref")
	}
	if !strings.Contains(err.Error(), "FIELD_FORMULA_UNKNOWN_REF") {
		t.Errorf("expected FIELD_FORMULA_UNKNOWN_REF, got: %v", err)
	}
}

func TestValidate_NonNumericRef(t *testing.T) {
	_, err := Validate("stage + 1", "my_formula", numericFields)
	if err == nil {
		t.Fatal("expected error for non-numeric ref")
	}
	if !strings.Contains(err.Error(), "FIELD_FORMULA_NON_NUMERIC_REF") {
		t.Errorf("expected FIELD_FORMULA_NON_NUMERIC_REF, got: %v", err)
	}
}

func TestValidate_NestedFormula(t *testing.T) {
	_, err := Validate("weighted_value * 2", "my_formula", numericFields)
	if err == nil {
		t.Fatal("expected error for nested formula ref")
	}
	if !strings.Contains(err.Error(), "FIELD_FORMULA_NESTED") {
		t.Errorf("expected FIELD_FORMULA_NESTED, got: %v", err)
	}
}

func TestValidate_MalformedExpr(t *testing.T) {
	_, err := Validate("amount *", "my_formula", numericFields)
	if err == nil {
		t.Fatal("expected error for malformed expr")
	}
	if !strings.Contains(err.Error(), "FIELD_FORMULA_INVALID") {
		t.Errorf("expected FIELD_FORMULA_INVALID, got: %v", err)
	}
}

func TestValidate_EmptyExpr(t *testing.T) {
	_, err := Validate("", "my_formula", numericFields)
	if err == nil {
		t.Fatal("expected error for empty expr")
	}
}

func TestValidate_LiteralOnlyExpr(t *testing.T) {
	// A literal-only formula is valid (no refs to validate).
	refs, err := Validate("100 * 2 + 50", "my_formula", numericFields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("want 0 refs, got %d: %v", len(refs), refs)
	}
}

func TestValidate_DeduplicatedRefs(t *testing.T) {
	// "amount" appears twice; only 1 unique ref expected.
	refs, err := Validate("amount + amount", "my_formula", numericFields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(refs) != 1 {
		t.Errorf("want 1 dedup ref, got %d: %v", len(refs), refs)
	}
}

// --- CRM weighted_value scenario ---

func TestEvaluate_CRMWeightedValue(t *testing.T) {
	cases := []struct {
		name        string
		amount      float64
		probability float64
		want        float64
		wantDivZ    bool
	}{
		{name: "acme_q1", amount: 75000, probability: 60, want: 45000},
		{name: "globex_ent", amount: 250000, probability: 80, want: 200000},
		{name: "initech_pilot (prob=0)", amount: 15000, probability: 0, want: 0},
		{name: "acme_upsell", amount: 40000, probability: 20, want: 8000},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			vals := map[string]float64{
				"amount":      tt.amount,
				"probability": tt.probability,
			}
			// probability=0 → 15000 * 0 / 100 = 0, NOT div-by-zero (100 is the divisor)
			got, dz, err := Evaluate("amount * probability / 100", staticResolver(vals))
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if dz != tt.wantDivZ {
				t.Errorf("divByZero=%v, want %v", dz, tt.wantDivZ)
			}
			if !dz && math.Abs(got-tt.want) > 1e-6 {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheck_DeepNestedComparisonIsFast(t *testing.T) {
	expr := "a"
	for i := 0; i < 40; i++ {
		expr = "IF(" + expr + " > 1,1,0)"
	}
	start := time.Now()
	_ = Check(expr, ResultNumber)
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("Check on 40-deep nested comparison took %v, want < 100ms", d)
	}
}
