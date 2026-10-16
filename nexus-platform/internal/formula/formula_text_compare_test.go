package formula

import "testing"

// queue #235 / specs/053 §3.6: a formula may compare a picklist/text field with a text literal.
var textCompareFields = []FieldInfo{
	{Name: "amount", FieldType: "number"},
	{Name: "stage", FieldType: "picklist"},
	{Name: "note", FieldType: "text"},
	{Name: "due", FieldType: "date"},
}

func TestValidate_TextCompare(t *testing.T) {
	tests := []struct {
		name string
		expr string
		code string // "" = valid
	}{
		{"picklist = literal", `IF(stage = "won", amount, 0)`, ""},
		{"literal on the left", `IF("won" = stage, amount, 0)`, ""},
		{"not equal", `IF(stage != "lost", amount, 0)`, ""},
		{"text field", `IF(note = "x", 1, 0)`, ""},
		{"joined with AND", `IF(stage = "won" AND amount > 10, amount, 0)`, ""},
		{"number field vs text", `IF(amount = "won", 1, 0)`, "FIELD_FORMULA_TYPE_MISMATCH"},
		{"date field vs text", `IF(due = "2026", 1, 0)`, "FIELD_FORMULA_TYPE_MISMATCH"},
		{"ordering on text", `IF(stage > "a", 1, 0)`, "FIELD_FORMULA_TYPE_MISMATCH"},
		{"literal vs number", `IF("won" = 1, 1, 0)`, "FIELD_FORMULA_TYPE_MISMATCH"},
		{"same field as text and as number", `IF(stage = "won", stage, 0)`, "FIELD_FORMULA_TYPE_MISMATCH"},
		{"two fixed strings", `IF("a" = "a", 1, 0)`, "FIELD_FORMULA_TYPE_MISMATCH"},
		{"unknown field", `IF(nope = "won", 1, 0)`, "FIELD_FORMULA_UNKNOWN_REF"},
		{"text field in arithmetic stays refused", `stage * 2`, "FIELD_FORMULA_NON_NUMERIC_REF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Validate(tt.expr, "out", textCompareFields)
			if tt.code == "" {
				if err != nil {
					t.Fatalf("Validate(%q): %v", tt.expr, err)
				}
				return
			}
			if err == nil || errCode(err) != tt.code {
				t.Fatalf("Validate(%q) err = %v; want %s", tt.expr, err, tt.code)
			}
		})
	}
}

func TestEvaluateWith_TextCompare(t *testing.T) {
	num := func(n string) float64 {
		if n == "amount" {
			return 500
		}
		return 0
	}
	stage := "won"
	txt := func(n string) string {
		if n == "stage" {
			return stage
		}
		return ""
	}
	got, dz, err := EvaluateWith(`IF(stage = "won", amount, 0)`, num, txt)
	if err != nil || dz || got != 500 {
		t.Fatalf("won: (%v, %v, %v); want 500", got, dz, err)
	}
	stage = "lost"
	got, _, _ = EvaluateWith(`IF(stage = "won", amount, 0)`, num, txt)
	if got != 0 {
		t.Fatalf("lost: %v; want 0", got)
	}
	got, _, _ = EvaluateWith(`IF(stage != "won", amount, 0)`, num, txt)
	if got != 500 {
		t.Fatalf("!=: %v; want 500", got)
	}
	// text result with a text comparison; empty value never equals a literal.
	s, _, err := EvaluateTextWith(`IF(stage = "won", "Thắng", "Chưa")`, num, txt)
	if err != nil || s != "Chưa" {
		t.Fatalf("text result: (%q, %v); want Chưa", s, err)
	}
	stage = ""
	got, _, _ = EvaluateWith(`IF(stage = "won", amount, 0)`, num, nil)
	if got != 0 {
		t.Fatalf("nil resolver: %v; want 0", got)
	}
}
