//go:build integration

// Formula IF + comparisons (specs/042) against a real throwaway MySQL 8 (o3a harness via w5Wire).
// Proves on the REAL metadata repo what a unit fake cannot: a tiered-commission formula persists
// through entity-create and add-field, and a mistyped formula is refused with
// FIELD_FORMULA_TYPE_MISMATCH on create, add-field AND edit — leaving the stored options intact.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestFormulaIFInt -v
package services

import (
	"context"
	"strings"
	"testing"
)

func TestFormulaIFInt_SaveAndEditPaths(t *testing.T) {
	entitySvc, fieldSvc, metaRepo, _ := w5Wire(t)
	ctx := context.Background()

	const tiered = "IF(amount >= 100000000, amount * 0.05, amount * 0.03)"

	// Create path: a mistyped formula in the declared field list is refused.
	_, _, err := entitySvc.CreateEntity(ctx, w5TenantA, CreateEntityInput{
		Name: "fif_bad", Label: "Bad",
		Fields: []FieldInput{
			{Name: "amount", Label: "Amount", FieldType: "number"},
			{Name: "flag", Label: "Flag", FieldType: "formula", Options: map[string]any{"formula": "amount >= 100"}},
		},
	})
	if got := w5IntCode(t, err); got != "FIELD_FORMULA_TYPE_MISMATCH" {
		t.Fatalf("create with mistyped formula: code = %q; want FIELD_FORMULA_TYPE_MISMATCH", got)
	}

	// Create path: the tiered formula is accepted.
	ent, _, err := entitySvc.CreateEntity(ctx, w5TenantA, CreateEntityInput{
		Name: "fif_deals", Label: "Deals",
		Fields: []FieldInput{
			{Name: "amount", Label: "Amount", FieldType: "currency", Options: map[string]any{"currencyCode": "VND"}},
			{Name: "qty", Label: "Qty", FieldType: "number"},
			{Name: "commission", Label: "Commission", FieldType: "formula",
				Options: map[string]any{"formula": tiered, "resultType": "currency", "currencyCode": "VND"}},
		},
	})
	if err != nil {
		t.Fatalf("CreateEntity with IF formula: %v", err)
	}

	// Add-field path: refs inside IF are checked against the REAL sibling list.
	_, err = fieldSvc.AddField(ctx, w5TenantA, AddFieldInput{EntityName: "fif_deals", Field: FieldInput{
		Name: "discount", Label: "Discount", FieldType: "formula",
		Options: map[string]any{"formula": "IF(qty >= 10, missing * 0.1, 0)"},
	}})
	if got := w5IntCode(t, err); got != "FIELD_FORMULA_UNKNOWN_REF" {
		t.Fatalf("add-field with unknown ref inside IF: code = %q; want FIELD_FORMULA_UNKNOWN_REF", got)
	}
	if _, err := fieldSvc.AddField(ctx, w5TenantA, AddFieldInput{EntityName: "fif_deals", Field: FieldInput{
		Name: "discount", Label: "Discount", FieldType: "formula",
		Options: map[string]any{"formula": "IF(qty >= 100, 0.15, IF(qty >= 10, 0.05, 0)) * amount"},
	}}); err != nil {
		t.Fatalf("add-field nested IF: %v", err)
	}

	// Edit path: turning the working commission into a mistyped formula is refused, and the
	// stored expression is untouched.
	fields, err := metaRepo.ListFields(ctx, w5TenantA, ent.ID)
	if err != nil {
		t.Fatalf("ListFields: %v", err)
	}
	var commissionID string
	for _, f := range fields {
		if f.Name == "commission" {
			commissionID = f.ID
		}
	}
	if commissionID == "" {
		t.Fatal("commission field not persisted")
	}
	_, err = fieldSvc.EditField(ctx, w5TenantA, EditFieldInput{
		FieldID: commissionID,
		Options: map[string]any{"formula": "IF(amount, 1, 2)", "resultType": "currency", "currencyCode": "VND"},
	})
	if got := w5IntCode(t, err); got != "FIELD_FORMULA_TYPE_MISMATCH" {
		t.Fatalf("edit to mistyped formula: code = %q; want FIELD_FORMULA_TYPE_MISMATCH", got)
	}
	// Queue #107: the edit path checks refs against the REAL sibling list too — a typo'd name
	// inside IF is refused with add-field's code instead of computing 0 forever.
	_, err = fieldSvc.EditField(ctx, w5TenantA, EditFieldInput{
		FieldID: commissionID,
		Options: map[string]any{"formula": "IF(amout > 1, 1, 0)", "resultType": "currency", "currencyCode": "VND"},
	})
	if got := w5IntCode(t, err); got != "FIELD_FORMULA_UNKNOWN_REF" {
		t.Fatalf("edit to a typo'd ref: code = %q; want FIELD_FORMULA_UNKNOWN_REF", got)
	}
	fields, err = metaRepo.ListFields(ctx, w5TenantA, ent.ID)
	if err != nil {
		t.Fatalf("ListFields after refused edit: %v", err)
	}
	for _, f := range fields {
		if f.Name == "commission" && (f.Options == nil || !strings.Contains(*f.Options, "amount >= 100000000")) {
			t.Errorf("refused edit changed the stored formula: %v", f.Options)
		}
	}
}
