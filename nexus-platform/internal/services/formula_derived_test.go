package services

// queue #284 (formula-reads-lookup-rollup): "thành tiền = số lượng × đơn giá (tra cứu)" and
// "còn lại = tổng tiền (roll-up) − giảm giá" were refused with FIELD_FORMULA_NON_NUMERIC_REF
// because lookup/rollup are computed at read time. They are now accepted when the value is
// numeric, stored NOT at save but computed at read, after applyLookups/applyRollups.

import (
	"context"
	"math"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// invoiceLines is a "dòng hoá đơn" definition: a service relation, a lookup of the service price
// (price's type is the parameter), qty, and a formula over them.
func invoiceLines(expr string) CreateEntityInput {
	return CreateEntityInput{
		Name: "invoice_lines", Label: "Dòng hoá đơn",
		Fields: []FieldInput{
			{Name: "service", Label: "Dịch vụ", FieldType: "relation", Options: map[string]any{"relatedEntity": "services"}},
			{Name: "don_gia", Label: "Đơn giá", FieldType: "lookup", Options: map[string]any{"via": "service", "sourceField": "price"}},
			{Name: "ten_dv", Label: "Tên DV", FieldType: "lookup", Options: map[string]any{"via": "service", "sourceField": "title"}},
			{Name: "so_luong", Label: "Số lượng", FieldType: "number"},
			{Name: "thanh_tien", Label: "Thành tiền", FieldType: "formula", Options: map[string]any{"formula": expr, "resultType": "currency"}},
		},
	}
}

func servicesMeta() *fakeMeta {
	return &fakeMeta{
		knownEntities: map[string]bool{"services": true},
		fieldsByEntityName: map[string][]metadata.FieldDefinition{
			"services": {{Name: "price", FieldType: "currency"}, {Name: "title", FieldType: "text"}},
		},
	}
}

func TestCreateEntity_FormulaReadsLookup(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		wantCode string
	}{
		{"qty × looked-up price", "so_luong * don_gia", ""},
		{"a looked-up TEXT is not a number", "so_luong * ten_dv", "FIELD_FORMULA_NON_NUMERIC_REF"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewEntityService(&fakeSchema{}, servicesMeta())
			_, _, err := svc.CreateEntity(context.Background(), "tenant-1", invoiceLines(tc.expr))
			if got := codeOf(err); got != tc.wantCode {
				t.Fatalf("code = %q; want %q (err: %v)", got, tc.wantCode, err)
			}
		})
	}
}

// A rollup cannot SUM a formula that is computed at read: its column is never written.
func TestCreateEntity_Rollup_AggFieldDerivedFormula_Rejected(t *testing.T) {
	meta := servicesMeta()
	meta.knownEntities["accounts"], meta.knownEntities["deals"] = true, true
	meta.fieldsByEntityName["deals"] = []metadata.FieldDefinition{
		{Name: "account", FieldType: "relation", Options: jsonOptsPtr(t, map[string]any{"relatedEntity": "accounts"})},
		{Name: "tax", FieldType: "rollup", Options: jsonOptsPtr(t, map[string]any{"childEntity": "x", "viaRelation": "y", "metric": "count"})},
		{Name: "qty", FieldType: "number"},
		{Name: "total", FieldType: "formula", Options: jsonOptsPtr(t, map[string]any{"formula": "qty * tax"})},
	}
	svc := NewEntityService(&fakeSchema{}, meta)
	in := accountsWithRollupFixture(map[string]any{"childEntity": "deals", "viaRelation": "account", "metric": "sum", "aggField": "total"})
	_, _, err := svc.CreateEntity(context.Background(), "tenant-1", in)
	if got := codeOf(err); got != "FIELD_ROLLUP_AGG_FIELD_FORMULA_DERIVED" {
		t.Fatalf("code = %q; want FIELD_ROLLUP_AGG_FIELD_FORMULA_DERIVED (err: %v)", got, err)
	}
}

func TestLookupValueTypes(t *testing.T) {
	meta := servicesMeta()
	fields := invoiceLines("so_luong").Fields
	got, err := lookupValueTypes(context.Background(), meta, "tenant-1", "invoice_lines", fields)
	if err != nil {
		t.Fatalf("lookupValueTypes: %v", err)
	}
	if got["don_gia"] != "currency" || got["ten_dv"] != "text" || len(got) != 2 {
		t.Fatalf("got %v; want don_gia=currency ten_dv=text", got)
	}
}

func derivedSpecs() []FieldSpec {
	return []FieldSpec{
		{Name: "so_luong", FieldType: "number"},
		{Name: "don_gia", FieldType: "lookup"},
		{Name: "tong", FieldType: "rollup"},
		{Name: "giam", FieldType: "number"},
		{Name: "thanh_tien", FieldType: "formula", Options: map[string]any{"formula": "so_luong * don_gia", "resultType": "currency", "currencyCode": "VND"}},
		{Name: "con_lai", FieldType: "formula", Options: map[string]any{"formula": "tong - giam"}},
		{Name: "chia", FieldType: "formula", Options: map[string]any{"formula": "tong / giam"}},
		{Name: "stored", FieldType: "formula", Options: map[string]any{"formula": "so_luong * 2"}},
	}
}

// Save time: a formula over a lookup/rollup is NOT written (a stored 0 would be a wrong number);
// a plain formula still is.
func TestApplyComputedFields_SkipsDerived(t *testing.T) {
	data := map[string]any{"so_luong": float64(3), "giam": float64(0)}
	applyComputedFields(derivedSpecs(), data)
	for _, name := range []string{"thanh_tien", "con_lai", "chia"} {
		if _, has := data[name]; has {
			t.Errorf("derived formula %q was written at save: %v", name, data[name])
		}
	}
	if data["stored"] != float64(6) {
		t.Errorf("stored formula = %v; want 6", data["stored"])
	}
}

func TestApplyDerivedFormulas(t *testing.T) {
	recs := []dynrepo.Record{
		{ID: "1", Data: map[string]any{"so_luong": float64(3), "don_gia": map[string]any{"amount": float64(250000), "currencyCode": "VND"}, "tong": "1000000.5", "giam": float64(100000)}},
		{ID: "2", Data: map[string]any{"so_luong": float64(2), "don_gia": nil, "tong": nil, "giam": float64(0)}},
		{ID: "3", Data: map[string]any{"so_luong": float64(1), "don_gia": float64(10), "tong": int64(5), "giam": float64(0)}},
	}
	applyDerivedFormulas(derivedSpecs(), recs, nil, nil)

	tt := recs[0].Data["thanh_tien"].(map[string]any)
	if tt["amount"] != float64(750000) || tt["currencyCode"] != "VND" {
		t.Errorf("thanh_tien = %v; want {750000 VND}", tt)
	}
	if v := recs[0].Data["con_lai"].(float64); math.Abs(v-900000.5) > 1e-9 {
		t.Errorf("con_lai = %v; want 900000.5 (rollup decimal string minus discount)", v)
	}
	if recs[0].Data["chia"].(float64) <= 0 {
		t.Errorf("chia = %v; want tong/giam > 0", recs[0].Data["chia"])
	}
	// unresolved lookup/rollup read as 0, like a stored formula's missing operand
	if recs[1].Data["thanh_tien"].(map[string]any)["amount"] != float64(0) || recs[1].Data["con_lai"] != float64(0) {
		t.Errorf("null operands: thanh_tien=%v con_lai=%v; want 0", recs[1].Data["thanh_tien"], recs[1].Data["con_lai"])
	}
	// division by zero → null, never an error
	if recs[1].Data["chia"] != nil {
		t.Errorf("chia = %v; want nil on division by zero", recs[1].Data["chia"])
	}
	if recs[2].Data["thanh_tien"].(map[string]any)["amount"] != float64(10) || recs[2].Data["con_lai"] != float64(5) {
		t.Errorf("row 3: %v / %v", recs[2].Data["thanh_tien"], recs[2].Data["con_lai"])
	}
	if _, has := recs[0].Data["stored"]; has {
		t.Errorf("a stored formula must not be touched at read: %v", recs[0].Data["stored"])
	}
}

// A formula the caller may not see (it, or an operand, is field-ACL hidden) is never computed.
func TestApplyDerivedFormulas_HiddenSkipped(t *testing.T) {
	recs := []dynrepo.Record{{ID: "1", Data: map[string]any{"so_luong": float64(3), "don_gia": float64(2)}}}
	applyDerivedFormulas(derivedSpecs(), recs, map[string]struct{}{"thanh_tien": {}}, nil)
	if _, has := recs[0].Data["thanh_tien"]; has {
		t.Fatalf("hidden formula was computed: %v", recs[0].Data["thanh_tien"])
	}
	if _, has := recs[0].Data["con_lai"]; !has {
		t.Fatalf("a visible derived formula must still be computed")
	}
}

// Rollups the derived formulas need are added to what ?rollup= asked for, hidden ones are not.
func TestWithDerivedRollups(t *testing.T) {
	specs := derivedSpecs()
	if got := withDerivedRollups([]string{"x"}, specs, nil); len(got) != 2 || got[0] != "x" || got[1] != "tong" {
		t.Fatalf("got %v; want [x tong]", got)
	}
	hidden := map[string]struct{}{"con_lai": {}, "chia": {}}
	if got := withDerivedRollups(nil, specs, hidden); len(got) != 0 {
		t.Fatalf("got %v; hidden formulas must not pull their rollups", got)
	}
}

// An operand the caller may not see resolves to null/0 — a number computed from it would be
// silently wrong, so the formula reads null (row stays; other derived formulas still compute).
func TestApplyDerivedFormulas_DeniedOperandReadsNull(t *testing.T) {
	recs := []dynrepo.Record{{ID: "1", Data: map[string]any{"so_luong": float64(3), "don_gia": nil, "tong": "10", "giam": float64(1)}}}
	applyDerivedFormulas(derivedSpecs(), recs, nil, map[string]struct{}{"don_gia": {}})
	if v, has := recs[0].Data["thanh_tien"]; !has || v != nil {
		t.Fatalf("thanh_tien = %v (present=%v); want nil: its lookup operand is ACL-denied", v, has)
	}
	if recs[0].Data["con_lai"] != float64(9) {
		t.Fatalf("con_lai = %v; want 9 (its operands are visible)", recs[0].Data["con_lai"])
	}
}
