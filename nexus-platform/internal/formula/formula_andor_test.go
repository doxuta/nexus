package formula

import (
	"errors"
	"testing"
)

// AND / OR (specs/053, queue #109). Kept out of formula_test.go and formula_if_test.go: both
// must stay green without a line changed (FR-004).

func TestEvaluate_AndOr(t *testing.T) {
	vals := func(a, b, c float64) Resolver {
		return staticResolver(map[string]float64{"a": a, "b": b, "c": c})
	}
	tests := []struct {
		name string
		expr string
		r    Resolver
		want float64
	}{
		{name: "AND both true", expr: "IF(a > 0 AND b > 10, 1, 0)", r: vals(5, 11, 0), want: 1},
		{name: "AND one false", expr: "IF(a > 0 AND b > 10, 1, 0)", r: vals(5, 10, 0), want: 0},
		{name: "OR one true", expr: "IF(a > 1 OR b > 1, 1, 0)", r: vals(0, 2, 0), want: 1},
		{name: "OR both false", expr: "IF(a > 1 OR b > 1, 1, 0)", r: vals(0, 0, 0), want: 0},
		{name: "lower case keywords", expr: "IF(a > 1 and b > 1 or c > 1, 1, 0)", r: vals(0, 0, 2), want: 1},
		// AND binds tighter than OR: a > 1 OR (b > 1 AND c > 1).
		{name: "AND before OR (left true)", expr: "IF(a > 1 OR b > 1 AND c > 1, 1, 0)", r: vals(2, 0, 0), want: 1},
		{name: "AND before OR (right half false)", expr: "IF(a > 1 OR b > 1 AND c > 1, 1, 0)", r: vals(0, 2, 0), want: 0},
		{name: "parentheses regroup", expr: "IF((a > 1 OR b > 1) AND c > 1, 1, 0)", r: vals(2, 0, 0), want: 0},
		{name: "arithmetic inside operands", expr: "IF(a * 2 > 5 AND b + c = 3, 7, 8)", r: vals(3, 1, 2), want: 7},
		{name: "Excel function form AND", expr: "IF(AND(a > 1, b > 1, c > 1), 1, 0)", r: vals(2, 2, 2), want: 1},
		{name: "Excel function form AND false", expr: "IF(AND(a > 1, b > 1, c > 1), 1, 0)", r: vals(2, 2, 0), want: 0},
		{name: "Excel function form OR", expr: "IF(or(a > 1, b > 1), 1, 0)", r: vals(0, 2, 0), want: 1},
		{name: "acceptance: amount AND qty", expr: "IF(amount > 0 AND qty > 10, amount * 0.1, 0)",
			r: staticResolver(map[string]float64{"amount": 500, "qty": 11}), want: 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, dz, err := Evaluate(tt.expr, tt.r)
			if err != nil || dz || got != tt.want {
				t.Fatalf("Evaluate(%q) = (%v, %v, %v); want %v", tt.expr, got, dz, err, tt.want)
			}
		})
	}
}

// TestEvaluate_AndOr_FieldsNamedAndOr: AND/OR are operators only AFTER an operand, where an
// identifier used to be a syntax error — so a field named "and" or "or" keeps working.
func TestEvaluate_AndOr_FieldsNamedAndOr(t *testing.T) {
	r := staticResolver(map[string]float64{"and": 3, "or": 4})
	for expr, want := range map[string]float64{
		"and * 2":                      6,
		"or + and":                     7,
		"IF(and > 1 AND or > 1, 1, 0)": 1,
	} {
		got, _, err := Evaluate(expr, r)
		if err != nil || got != want {
			t.Errorf("Evaluate(%q) = (%v, %v); want %v", expr, got, err, want)
		}
	}
}

// TestEvaluate_AndOr_ShortCircuit: the guard a user writes to avoid dividing by zero must hold,
// exactly like IF (specs/042 §3.3).
func TestEvaluate_AndOr_ShortCircuit(t *testing.T) {
	r := staticResolver(map[string]float64{"a": 10, "b": 0})
	for expr, want := range map[string]float64{
		"IF(b = 0 OR a / b > 2, 1, 0)":   1,
		"IF(b != 0 AND a / b > 2, 1, 0)": 0,
	} {
		got, dz, err := Evaluate(expr, r)
		if err != nil || dz || got != want {
			t.Errorf("Evaluate(%q) = (%v, divByZero=%v, %v); want %v, not null", expr, got, dz, err, want)
		}
	}
	// A left side that divides by zero is undecided: null, as for an IF condition.
	if _, dz, err := Evaluate("IF(a / b > 1 OR a > 1, 1, 0)", r); err != nil || !dz {
		t.Errorf("undecidable left side: divByZero = %v (err %v); want true", dz, err)
	}
}

func TestEvaluate_AndOr_Errors(t *testing.T) {
	typeErrs := []string{
		"IF(a AND b > 1, 1, 0)",         // number operand
		"IF(a > 1 OR 2, 1, 0)",          // number operand on the right
		"a > 1 AND b > 1",               // a boolean is never a stored result
		"(a > 1 AND b > 1) * 2",         // boolean in arithmetic
		"IF(AND(a > 1, b), 1, 0)",       // function form checks every argument
		"IF(a > 1 AND b > 1, 1, a > 1)", // mixed branches still rejected
	}
	for _, expr := range typeErrs {
		if _, _, err := Evaluate(expr, staticResolver(nil)); !errors.Is(err, ErrTypeMismatch) {
			t.Errorf("Evaluate(%q) err = %v; want ErrTypeMismatch", expr, err)
		}
	}
	syntaxErrs := []string{
		"IF(AND(a > 1), 1, 0)", // function form needs at least two conditions
		"IF(a > 1 AND, 1, 0)",
		"IF(AND a > 1, 1, 0)",
	}
	for _, expr := range syntaxErrs {
		_, _, err := Evaluate(expr, staticResolver(nil))
		if err == nil || errors.Is(err, ErrTypeMismatch) {
			t.Errorf("Evaluate(%q) err = %v; want a syntax error", expr, err)
		}
	}
}

func TestValidate_RefChecksReachInsideAndOr(t *testing.T) {
	fields := []FieldInfo{{Name: "amount", FieldType: "currency"}, {Name: "qty", FieldType: "number"}, {Name: "note", FieldType: "text"}}
	refs, err := Validate("IF(amount > 0 AND qty > 10, amount * 0.1, 0)", "discount", fields)
	if err != nil || len(refs) != 2 {
		t.Fatalf("Validate = (%v, %v); want refs [amount qty]", refs, err)
	}
	for expr, code := range map[string]string{
		"IF(amount > 0 AND missing > 1, 1, 0)": "FIELD_FORMULA_UNKNOWN_REF",
		"IF(OR(amount > 0, note > 1), 1, 0)":   "FIELD_FORMULA_NON_NUMERIC_REF",
	} {
		if _, err := Validate(expr, "discount", fields); err == nil || errCode(err) != code {
			t.Errorf("Validate(%q) err = %v; want %s", expr, err, code)
		}
	}
}

// errCode returns the "CODE:" prefix Validate puts on its errors.
func errCode(err error) string {
	msg := err.Error()
	for i := 0; i < len(msg); i++ {
		if msg[i] == ':' {
			return msg[:i]
		}
	}
	return msg
}
