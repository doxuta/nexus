package services

import (
	"testing"
)

// queue #235: IF(stage = "won", amount, 0) is computed from the stored picklist code.
func TestApplyComputedFields_TextCompare(t *testing.T) {
	specs := []FieldSpec{
		{Name: "stage", FieldType: "picklist", Options: map[string]any{"values": []any{"won", "lost"}}},
		makeNumberSpec("amount"),
		makeFormulaSpec("won_amount", `IF(stage = "won", amount, 0)`, "number", ""),
	}
	for _, tc := range []struct {
		stage any
		want  float64
	}{{"won", 500}, {"lost", 0}, {nil, 0}} {
		data := map[string]any{"stage": tc.stage, "amount": float64(500)}
		applyComputedFields(specs, data)
		if got, _ := data["won_amount"].(float64); got != tc.want {
			t.Errorf("stage=%v: won_amount = %v; want %v", tc.stage, data["won_amount"], tc.want)
		}
	}
}

func TestValidateFormulaConfig_TextCompareShape(t *testing.T) {
	tests := []struct {
		name, expr, resultType, wantCode string
	}{
		{"text compare ok", `IF(stage = "won", amount, 0)`, "number", ""},
		{"ordering refused", `IF(stage > "a", amount, 0)`, "number", "FIELD_FORMULA_TYPE_MISMATCH"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFormulaConfig("f", map[string]any{"formula": tt.expr, "resultType": tt.resultType})
			if got := codeOf(err); got != tt.wantCode {
				t.Fatalf("code = %q (err=%v); want %q", got, err, tt.wantCode)
			}
		})
	}
}
