package services

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/formula"
)

// Service seams for text-result formulas and AND/OR (specs/053, queue #109): the config-time
// validator every save path shares accepts resultType "text" and matches it against what the
// expression produces, and the record-write hook stores the chosen text.

const gradeExpr = `IF(score >= 8, "Giỏi", IF(score >= 6.5, "Khá", "TB"))`

func TestValidateFormulaConfig_FormulaTextAndOr(t *testing.T) {
	tests := []struct {
		name       string
		expr       string
		resultType any // nil = omitted
		wantCode   string
	}{
		{name: "grading with resultType text", expr: gradeExpr, resultType: "text"},
		{name: "AND inside IF", expr: "IF(amount > 0 AND qty > 10, amount * 0.1, 0)", resultType: "number"},
		{name: "Excel AND() form", expr: "IF(AND(amount > 0, qty > 10), amount * 0.1, 0)", resultType: "currency"},
		{name: "text formula stored as number", expr: gradeExpr, resultType: "number", wantCode: "FIELD_FORMULA_TYPE_MISMATCH"},
		{name: "text formula with resultType omitted (number)", expr: gradeExpr, wantCode: "FIELD_FORMULA_TYPE_MISMATCH"},
		{name: "number formula stored as text", expr: "amount * 2", resultType: "text", wantCode: "FIELD_FORMULA_TYPE_MISMATCH"},
		{name: "text in arithmetic", expr: `"Giỏi" + 1`, resultType: "text", wantCode: "FIELD_FORMULA_TYPE_MISMATCH"},
		{name: "mixed IF branches", expr: `IF(score > 1, "Giỏi", 2)`, resultType: "text", wantCode: "FIELD_FORMULA_TYPE_MISMATCH"},
		{name: "number operand of AND", expr: "IF(amount AND qty > 1, 1, 0)", resultType: "number", wantCode: "FIELD_FORMULA_TYPE_MISMATCH"},
		{name: "unknown resultType", expr: "amount * 2", resultType: "boolean", wantCode: "FIELD_FORMULA_INVALID"},
		{name: "unterminated string", expr: `IF(score > 1, "Giỏi, "Khá")`, resultType: "text", wantCode: "FIELD_FORMULA_INVALID"},
		// Length cap in RUNES (review 2026-09-30): "ơ" is 2 bytes, so the MaxLen-rune formula is
		// ~2×MaxLen bytes and must still save; one rune more is refused on every save path.
		{name: "at the length cap", expr: `"` + strings.Repeat("ơ", formula.MaxLen-2) + `"`, resultType: "text"},
		{name: "over the length cap", expr: `"` + strings.Repeat("ơ", formula.MaxLen-1) + `"`, resultType: "text", wantCode: "FIELD_FORMULA_INVALID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := map[string]any{"formula": tt.expr}
			if tt.resultType != nil {
				opts["resultType"] = tt.resultType
			}
			err := validateFormulaConfig("grade", opts)
			if got := codeOf(err); got != tt.wantCode {
				t.Fatalf("validateFormulaConfig(%q, %v) code = %q (err=%v); want %q", tt.expr, tt.resultType, got, err, tt.wantCode)
			}
		})
	}
}

func TestApplyComputedFields_FormulaTextGrade(t *testing.T) {
	specs := []FieldSpec{makeNumberSpec("score"), makeFormulaSpec("grade", gradeExpr, "text", "")}
	for score, want := range map[float64]string{9: "Giỏi", 8: "Giỏi", 7: "Khá", 6.5: "Khá", 5: "TB"} {
		data := map[string]any{"score": score}
		applyComputedFields(specs, data)
		if got, ok := data["grade"].(string); !ok || got != want {
			t.Errorf("score %v: grade = %T %v; want %q", score, data["grade"], data["grade"], want)
		}
	}
	// Division by zero in the condition of a text formula → null, like a number formula.
	specs = []FieldSpec{makeNumberSpec("a"), makeNumberSpec("b"), makeFormulaSpec("tag", `IF(a / b > 1, "cao", "thấp")`, "text", "")}
	data := map[string]any{"a": float64(1), "b": float64(0)}
	applyComputedFields(specs, data)
	if v, ok := data["tag"]; !ok || v != nil {
		t.Errorf("tag = %v (present=%v); want explicit null", v, ok)
	}
}

func TestApplyComputedFields_FormulaAndOr(t *testing.T) {
	specs := []FieldSpec{makeNumberSpec("amount"), makeNumberSpec("qty"),
		makeFormulaSpec("discount", "IF(amount > 0 AND qty > 10, amount * 0.1, 0)", "number", "")}
	for _, tc := range []struct{ amount, qty, want float64 }{{500, 11, 50}, {500, 10, 0}, {0, 20, 0}} {
		data := map[string]any{"amount": tc.amount, "qty": tc.qty}
		applyComputedFields(specs, data)
		if got, _ := data["discount"].(float64); got != tc.want {
			t.Errorf("amount=%v qty=%v: discount = %v; want %v", tc.amount, tc.qty, data["discount"], tc.want)
		}
	}
}

// TestValidateFormula_FormulaTextDryRun: the live editor's dry-run echoes resultType "text"
// and reports the refs inside the text formula, with the same codes a save returns.
func TestValidateFormula_FormulaTextDryRun(t *testing.T) {
	svc := NewFieldService(&fakeFieldSchema{}, formulaFixtureMeta())
	res, err := svc.ValidateFormula(context.Background(), "tenant-1", ValidateFormulaInput{
		EntityName: "courses", Formula: `IF(price >= 100 AND qty > 1, "Cao", "Thấp")`, ResultType: "text",
	})
	if err != nil {
		t.Fatalf("ValidateFormula: %v", err)
	}
	if !res.Valid || res.ResultType != "text" || len(res.Fields) != 2 {
		t.Errorf("result = %+v; want valid, resultType text, fields [price qty]", res)
	}
	_, err = svc.ValidateFormula(context.Background(), "tenant-1", ValidateFormulaInput{
		EntityName: "courses", Formula: `IF(price >= 100, "Cao", "Thấp")`,
	})
	if got := codeOf(err); got != "FIELD_FORMULA_TYPE_MISMATCH" {
		t.Errorf("text formula without resultType text: code = %q; want FIELD_FORMULA_TYPE_MISMATCH", got)
	}
}
