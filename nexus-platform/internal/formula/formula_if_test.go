package formula

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// Tests for comparisons + IF (DAILY-QUEUE #25 formula-typed-values-if). Kept in a separate
// file on purpose: the acceptance rule is that formula_test.go stays green WITHOUT a single
// line changed — the arithmetic contract is the regression floor.

// commissionFormula is the acceptance formula: tiered commission, 5% from 100M up, else 3%.
const commissionFormula = "IF(amount >= 100000000, amount * 0.05, amount * 0.03)"

func TestEvaluate_Comparisons(t *testing.T) {
	tests := []struct {
		name string
		expr string
		want float64
	}{
		// A comparison is a boolean — only usable inside IF — so each case wraps it.
		{name: "gt true", expr: "IF(2 > 1, 1, 0)", want: 1},
		{name: "gt false", expr: "IF(1 > 1, 1, 0)", want: 0},
		{name: "gte equal", expr: "IF(1 >= 1, 1, 0)", want: 1},
		{name: "lt true", expr: "IF(1 < 2, 1, 0)", want: 1},
		{name: "lte equal", expr: "IF(2 <= 2, 1, 0)", want: 1},
		{name: "lte false", expr: "IF(3 <= 2, 1, 0)", want: 0},
		{name: "eq single =", expr: "IF(2 = 2, 1, 0)", want: 1},
		{name: "eq double ==", expr: "IF(2 == 3, 1, 0)", want: 0},
		{name: "neq !=", expr: "IF(2 != 3, 1, 0)", want: 1},
		{name: "neq <>", expr: "IF(2 <> 2, 1, 0)", want: 0},
		// Arithmetic binds tighter than comparison: (1 + 1) > (3 - 2).
		{name: "arith before compare", expr: "IF(1 + 1 > 3 - 2, 10, 20)", want: 10},
		{name: "unary minus operand", expr: "IF(-5 < 0, 1, 0)", want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, dz, err := Evaluate(tt.expr, staticResolver(nil))
			if err != nil {
				t.Fatalf("Evaluate(%q) error: %v", tt.expr, err)
			}
			if dz {
				t.Fatalf("Evaluate(%q) divByZero=true, want false", tt.expr)
			}
			if got != tt.want {
				t.Errorf("Evaluate(%q) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

func TestEvaluate_IF_TieredCommission(t *testing.T) {
	// The acceptance numbers: 120M → 6M (5%), 40M → 1.2M (3%), and the boundary itself
	// takes the upper tier (>=).
	tests := []struct {
		name   string
		amount float64
		want   float64
	}{
		{name: "120M upper tier", amount: 120_000_000, want: 6_000_000},
		{name: "40M lower tier", amount: 40_000_000, want: 1_200_000},
		{name: "exactly 100M boundary", amount: 100_000_000, want: 5_000_000},
		{name: "missing amount = 0", amount: 0, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, dz, err := Evaluate(commissionFormula, staticResolver(map[string]float64{"amount": tt.amount}))
			if err != nil || dz {
				t.Fatalf("Evaluate error=%v divByZero=%v", err, dz)
			}
			if math.Abs(got-tt.want) > 1e-6 {
				t.Errorf("commission(%v) = %v, want %v", tt.amount, got, tt.want)
			}
		})
	}
}

func TestEvaluate_IF_NestedTiers(t *testing.T) {
	// Quantity discount with three tiers via nested IF.
	expr := "IF(qty >= 100, 0.15, IF(qty >= 10, 0.05, 0)) * price * qty"
	tests := []struct {
		qty, want float64
	}{
		{qty: 5, want: 0},
		{qty: 10, want: 0.05 * 20 * 10},
		{qty: 150, want: 0.15 * 20 * 150},
	}
	for _, tt := range tests {
		got, dz, err := Evaluate(expr, staticResolver(map[string]float64{"qty": tt.qty, "price": 20}))
		if err != nil || dz {
			t.Fatalf("qty=%v: error=%v divByZero=%v", tt.qty, err, dz)
		}
		if math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("qty=%v: got %v, want %v", tt.qty, got, tt.want)
		}
	}
}

// TestEvaluate_IF_ShortCircuit is the mandatory case from the queue item: the binop branch
// propagates divByZero from BOTH sides, so a naive IF that evaluates both branches would
// null out IF(x > 0, a / x, 0) exactly at x = 0 — the one value the guard exists for.
func TestEvaluate_IF_ShortCircuit(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		vals     map[string]float64
		want     float64
		wantDivZ bool
	}{
		{name: "guarded div, x=0 takes else", expr: "IF(x > 0, a / x, 0)",
			vals: map[string]float64{"a": 10, "x": 0}, want: 0},
		{name: "guarded div, x=2 takes then", expr: "IF(x > 0, a / x, 0)",
			vals: map[string]float64{"a": 10, "x": 2}, want: 5},
		{name: "unchosen THEN branch divides by zero", expr: "IF(x = 0, 0, a / x)",
			vals: map[string]float64{"a": 10, "x": 0}, want: 0},
		// A div-by-zero on the branch that IS taken still means "no value".
		{name: "chosen branch divides by zero", expr: "IF(x = 0, a / x, 1)",
			vals: map[string]float64{"a": 10, "x": 0}, wantDivZ: true},
		// A condition that cannot be decided means no value either.
		{name: "div by zero inside condition", expr: "IF(a / x > 1, 1, 2)",
			vals: map[string]float64{"a": 10, "x": 0}, wantDivZ: true},
		// Short-circuit survives being embedded in arithmetic.
		{name: "guarded IF inside arithmetic", expr: "1 + IF(x > 0, a / x, 0)",
			vals: map[string]float64{"a": 10, "x": 0}, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, dz, err := Evaluate(tt.expr, staticResolver(tt.vals))
			if err != nil {
				t.Fatalf("Evaluate(%q) error: %v", tt.expr, err)
			}
			if dz != tt.wantDivZ {
				t.Fatalf("Evaluate(%q) divByZero=%v, want %v", tt.expr, dz, tt.wantDivZ)
			}
			if !dz && got != tt.want {
				t.Errorf("Evaluate(%q) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

func TestEvaluate_IF_CaseInsensitiveAndFieldNamedIf(t *testing.T) {
	// Function names are case-insensitive (Excel habit).
	for _, expr := range []string{"if(1 < 2, 7, 8)", "If(1 < 2, 7, 8)", "IF (1 < 2, 7, 8)"} {
		got, _, err := Evaluate(expr, staticResolver(nil))
		if err != nil || got != 7 {
			t.Errorf("Evaluate(%q) = %v, %v; want 7, nil", expr, got, err)
		}
	}
	// A bare identifier NOT followed by "(" is still a field reference, so an existing field
	// that happens to be called `if` keeps working — no reserved words were introduced.
	got, _, err := Evaluate("if * 2", staticResolver(map[string]float64{"if": 4}))
	if err != nil || got != 8 {
		t.Errorf(`Evaluate("if * 2") = %v, %v; want 8, nil`, got, err)
	}
}

// TestEvaluate_TypeErrors: expressions that parse but mix booleans and numbers. They must be
// rejected (never silently coerced) and the error must be recognisable as a type mismatch.
func TestEvaluate_TypeErrors(t *testing.T) {
	tests := []struct {
		name string
		expr string
	}{
		{name: "bare comparison as result", expr: "amount > 5"},
		{name: "number as IF condition", expr: "IF(amount, 1, 2)"},
		{name: "comparison in arithmetic", expr: "(amount > 5) * 2"},
		{name: "chained comparison", expr: "IF(1 < 2 < 3, 1, 0)"},
		{name: "unary minus on comparison", expr: "IF(-(1 < 2), 1, 0)"},
		{name: "branches of different types", expr: "IF(1 < 2, 1 < 2, 3)"},
		// Reversed order: THEN is a number, so the root check alone would pass — only the
		// branch-type check rejects it. Without it the boolean ELSE would be stored as 0/1.
		{name: "number THEN, boolean ELSE", expr: "IF(1 < 2, 3, 1 < 2)"},
		{name: "IF returning a boolean", expr: "IF(1 < 2, 1 < 2, 2 < 1)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Evaluate(tt.expr, staticResolver(nil))
			if err == nil {
				t.Fatalf("Evaluate(%q): expected a type error, got nil", tt.expr)
			}
			if !errors.Is(err, ErrTypeMismatch) {
				t.Errorf("Evaluate(%q): error %v is not ErrTypeMismatch", tt.expr, err)
			}
		})
	}
}

func TestEvaluate_FunctionSyntaxErrors(t *testing.T) {
	tests := []struct {
		name string
		expr string
	}{
		{name: "too few args", expr: "IF(1 < 2, 1)"},
		{name: "too many args", expr: "IF(1 < 2, 1, 2, 3)"},
		{name: "no args", expr: "IF()"},
		{name: "unknown function", expr: "ROUND(amount, 2)"},
		{name: "missing close paren", expr: "IF(1 < 2, 1, 2"},
		{name: "trailing comma", expr: "IF(1 < 2, 1, )"},
		{name: "comma outside call", expr: "1, 2"},
		{name: "dangling comparison", expr: "IF(1 <, 1, 2)"},
		{name: "lone bang", expr: "IF(1 ! 2, 1, 2)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Evaluate(tt.expr, staticResolver(nil))
			if err == nil {
				t.Fatalf("Evaluate(%q): expected a syntax error, got nil", tt.expr)
			}
			if errors.Is(err, ErrTypeMismatch) {
				t.Errorf("Evaluate(%q): syntax error misreported as type mismatch: %v", tt.expr, err)
			}
		})
	}
}

// --- Validate (config-time) ---

var commissionFields = []FieldInfo{
	{Name: "amount", FieldType: "currency"},
	{Name: "probability", FieldType: "number"},
	{Name: "stage", FieldType: "picklist"},
	{Name: "weighted_value", FieldType: "formula"},
}

func TestValidate_IF_HappyPath(t *testing.T) {
	refs, err := Validate("IF(probability >= 50, amount * 0.05, amount * 0.03)", "commission", commissionFields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Identifiers inside the condition and both branches are all collected (deduplicated).
	if strings.Join(refs, ",") != "probability,amount" {
		t.Errorf("refs = %v, want [probability amount]", refs)
	}
}

func TestValidate_TypeMismatchCode(t *testing.T) {
	for _, expr := range []string{"amount >= 100", "IF(amount, 1, 2)", "(amount > 1) + 1", "IF(amount > 0, 5, amount > 1)"} {
		_, err := Validate(expr, "commission", commissionFields)
		if err == nil {
			t.Fatalf("Validate(%q): expected error", expr)
		}
		if !strings.HasPrefix(err.Error(), "FIELD_FORMULA_TYPE_MISMATCH:") {
			t.Errorf("Validate(%q): want FIELD_FORMULA_TYPE_MISMATCH prefix, got %v", expr, err)
		}
	}
}

func TestValidate_RefChecksReachInsideIF(t *testing.T) {
	tests := []struct {
		name string
		expr string
		code string
	}{
		{name: "unknown ref in condition", expr: "IF(missing > 1, 1, 2)", code: "FIELD_FORMULA_UNKNOWN_REF"},
		{name: "non-numeric ref in else", expr: "IF(amount > 1, 1, stage)", code: "FIELD_FORMULA_NON_NUMERIC_REF"},
		{name: "nested formula in then", expr: "IF(amount > 1, weighted_value, 0)", code: "FIELD_FORMULA_NESTED"},
		{name: "unknown function", expr: "SUM(amount)", code: "FIELD_FORMULA_INVALID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Validate(tt.expr, "commission", commissionFields)
			if err == nil {
				t.Fatalf("Validate(%q): expected %s", tt.expr, tt.code)
			}
			if !strings.HasPrefix(err.Error(), tt.code+":") {
				t.Errorf("Validate(%q): want %s, got %v", tt.expr, tt.code, err)
			}
		})
	}
}
