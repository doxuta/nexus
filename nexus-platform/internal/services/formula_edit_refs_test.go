package services

import (
	"context"
	"testing"

	"nexus/platform/internal/metadata"
)

// Queue #107 (formula-edit-path-ref-check): create and add-field check every identifier of a
// formula against the entity's sibling fields, but the EDIT path only ran the structural check
// (syntax + types). IF(amout > 1, 1, 0) — a typo for amount — was saved and read as 0 on every
// record write. These pin the edit path (PUT /admin/fields/:id → FieldService.EditField) to the
// same ref codes; the AI propose_edit_field twin lives in internal/ai.

// formulaEditSiblings is one entity's active field list; the last entry is the formula being
// edited, so the self-reference guard sees it among its siblings exactly like production does.
func formulaEditSiblings() []metadata.FieldDefinition {
	opts := `{"formula":"amount * 0.05","resultType":"number"}`
	return []metadata.FieldDefinition{
		{ID: "f-amount", EntityID: "ent-1", Name: "amount", FieldType: "currency"},
		{ID: "f-qty", EntityID: "ent-1", Name: "qty", FieldType: "number"},
		{ID: "f-note", EntityID: "ent-1", Name: "note", FieldType: "text"},
		{ID: "f-total", EntityID: "ent-1", Name: "total", FieldType: "formula"},
		{ID: "f-comm", EntityID: "ent-1", Name: "commission", FieldType: "formula", Options: &opts},
	}
}

func TestEditField_FormulaRefs(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		wantCode string // "" = accepted
	}{
		{name: "typo inside the IF condition", expr: "IF(amout > 1, 1, 0)", wantCode: "FIELD_FORMULA_UNKNOWN_REF"},
		{name: "text sibling", expr: "note * 2", wantCode: "FIELD_FORMULA_NON_NUMERIC_REF"},
		{name: "another formula", expr: "total + 1", wantCode: "FIELD_FORMULA_NESTED"},
		{name: "itself", expr: "commission + 1", wantCode: "FIELD_FORMULA_UNKNOWN_REF"},
		{name: "valid tiered rule", expr: "IF(amount >= 100, amount * 0.05, qty)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sibs := formulaEditSiblings()
			stored := sibs[len(sibs)-1]
			meta := &fakeEditMeta{field: &stored, siblings: sibs}
			svc, _ := editService(meta)
			_, err := svc.EditField(context.Background(), "tenant-1", EditFieldInput{
				FieldID: "f-comm",
				Options: map[string]any{"formula": tt.expr, "resultType": "number"},
			})
			if got := codeOf(err); got != tt.wantCode {
				t.Fatalf("EditField(%q) code = %q (err=%v); want %q", tt.expr, got, err, tt.wantCode)
			}
			if tt.wantCode != "" && len(meta.updated) != 0 {
				t.Fatalf("a refused edit must not persist; updated=%v", meta.updated)
			}
		})
	}
}

// TestEditField_FormulaUnchangedNotRejudged: the check judges the expression an edit WRITES,
// not one it merely carries back. Deleting a field does not guard its formula dependants, so a
// stored formula can already name a gone sibling; relabelling that field (Studio sends the
// stored options back on every save) must not be refused for it — the queue #62 rule for
// defaults, applied to formulas.
func TestEditField_FormulaUnchangedNotRejudged(t *testing.T) {
	opts := `{"formula":"amount + gone","resultType":"number"}`
	stored := metadata.FieldDefinition{ID: "f-comm", EntityID: "ent-1", Name: "commission", FieldType: "formula", Options: &opts}
	meta := &fakeEditMeta{field: &stored, siblings: []metadata.FieldDefinition{
		{ID: "f-amount", EntityID: "ent-1", Name: "amount", FieldType: "number"},
		stored,
	}}
	svc, _ := editService(meta)
	_, err := svc.EditField(context.Background(), "tenant-1", EditFieldInput{
		FieldID: "f-comm",
		Label:   strPtr("Hoa hồng"),
		Options: map[string]any{"formula": "amount + gone", "resultType": "number"},
	})
	if err != nil {
		t.Fatalf("relabel carrying the stored formula: %v", err)
	}
}
