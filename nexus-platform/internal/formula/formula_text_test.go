package formula

import (
	"errors"
	"testing"
)

// Text results (specs/053, queue #109): grading "Giỏi"/"Khá" is the reference case.

const gradeFormula = `IF(score >= 8, "Giỏi", IF(score >= 6.5, "Khá", "TB"))`

func TestEvaluateText_Grading(t *testing.T) {
	for score, want := range map[float64]string{9: "Giỏi", 8: "Giỏi", 7.9: "Khá", 6.5: "Khá", 6.4: "TB", 0: "TB"} {
		got, dz, err := EvaluateText(gradeFormula, staticResolver(map[string]float64{"score": score}))
		if err != nil || dz || got != want {
			t.Errorf("score %v: EvaluateText = (%q, %v, %v); want %q", score, got, dz, err, want)
		}
	}
}

func TestEvaluateText_Literals(t *testing.T) {
	r := staticResolver(map[string]float64{"a": 1, "b": 0})
	for expr, want := range map[string]string{
		`"cố định"`:                        "cố định", // a constant text formula
		`IF(a > 0, "", "rỗng")`:            "",        // empty string is a value
		`IF(a > 0 AND b = 0, "x, y", "z")`: "x, y",    // commas and spaces inside quotes are text
	} {
		got, dz, err := EvaluateText(expr, r)
		if err != nil || dz || got != want {
			t.Errorf("EvaluateText(%q) = (%q, %v, %v); want %q", expr, got, dz, err, want)
		}
	}
	// A condition that divides by zero leaves the value undecided → the caller writes null.
	if _, dz, err := EvaluateText(`IF(a / b > 1, "cao", "thấp")`, r); err != nil || !dz {
		t.Errorf("division by zero in the condition: divByZero = %v (err %v); want true", dz, err)
	}
}

// TestEvaluate_ResultKindMustMatch: Evaluate is the number entry point and EvaluateText the
// text one; handing either the other kind is a type error, never a coercion.
func TestEvaluate_ResultKindMustMatch(t *testing.T) {
	r := staticResolver(map[string]float64{"score": 9})
	if _, _, err := Evaluate(gradeFormula, r); !errors.Is(err, ErrTypeMismatch) {
		t.Errorf("Evaluate(text formula) err = %v; want ErrTypeMismatch", err)
	}
	if _, _, err := EvaluateText("score * 2", r); !errors.Is(err, ErrTypeMismatch) {
		t.Errorf("EvaluateText(number formula) err = %v; want ErrTypeMismatch", err)
	}
	if _, _, err := EvaluateText("score > 2", r); !errors.Is(err, ErrTypeMismatch) {
		t.Errorf("EvaluateText(boolean formula) err = %v; want ErrTypeMismatch", err)
	}
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		want     Result
		mismatch bool // ErrTypeMismatch expected
		syntax   bool // a non-type error expected
	}{
		{name: "grading as text", expr: gradeFormula, want: ResultText},
		{name: "tiered commission as number", expr: "IF(amount >= 100, amount * 0.05, amount * 0.03)", want: ResultNumber},
		{name: "grading as number", expr: gradeFormula, want: ResultNumber, mismatch: true},
		{name: "number as text", expr: "amount * 2", want: ResultText, mismatch: true},
		{name: "boolean as text", expr: "amount > 2", want: ResultText, mismatch: true},
		{name: "text in arithmetic", expr: `"a" + 1`, want: ResultText, mismatch: true},
		{name: "text compared", expr: `IF("a" = "a", "x", "y")`, want: ResultText, mismatch: true},
		{name: "text as condition", expr: `IF("a", 1, 2)`, want: ResultNumber, mismatch: true},
		{name: "text operand of AND", expr: `IF(amount > 1 AND "a", "x", "y")`, want: ResultText, mismatch: true},
		{name: "text and number branches", expr: `IF(amount > 1, "x", 2)`, want: ResultText, mismatch: true},
		{name: "unterminated string", expr: `IF(amount > 1, "x, "y")`, want: ResultText, syntax: true},
		{name: "single quotes are not strings", expr: `IF(amount > 1, 'x', 'y')`, want: ResultText, syntax: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Check(tt.expr, tt.want)
			switch {
			case tt.mismatch:
				if !errors.Is(err, ErrTypeMismatch) {
					t.Fatalf("Check(%q) err = %v; want ErrTypeMismatch", tt.expr, err)
				}
			case tt.syntax:
				if err == nil || errors.Is(err, ErrTypeMismatch) {
					t.Fatalf("Check(%q) err = %v; want a syntax error", tt.expr, err)
				}
			default:
				if err != nil {
					t.Fatalf("Check(%q): %v", tt.expr, err)
				}
			}
		})
	}
}

func TestValidate_TextFormula(t *testing.T) {
	fields := []FieldInfo{{Name: "score", FieldType: "number"}, {Name: "note", FieldType: "text"}}
	refs, err := Validate(gradeFormula, "grade", fields)
	if err != nil || len(refs) != 1 || refs[0] != "score" {
		t.Fatalf("Validate(grading) = (%v, %v); want refs [score]", refs, err)
	}
	// Identifiers are still numbers only — a text field cannot be compared (out of scope).
	if _, err := Validate(`IF(note = 1, "x", "y")`, "grade", fields); err == nil || errCode(err) != "FIELD_FORMULA_NON_NUMERIC_REF" {
		t.Errorf("text field in a condition: err = %v; want FIELD_FORMULA_NON_NUMERIC_REF", err)
	}
	// A boolean result is still refused on the ref-check path.
	if _, err := Validate("score > 1", "grade", fields); err == nil || errCode(err) != "FIELD_FORMULA_TYPE_MISMATCH" {
		t.Errorf("boolean result: err = %v; want FIELD_FORMULA_TYPE_MISMATCH", err)
	}
}
