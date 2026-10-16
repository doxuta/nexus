//go:build integration

// Text comparison in formulas (queue #235) and the delete-field dependants guard (queue #233)
// against a real throwaway MySQL 8:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestFormulaTextCompareInt -v
package services

import (
	"context"
	"testing"
)

func TestFormulaTextCompareInt_PicklistAndDeleteGuard(t *testing.T) {
	st := frWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "cccc1111-0000-0000-0000-000000000052"

	st.createEntity(t, ctx, tenant, "fx_deals", []FieldInput{
		{Name: "title", Label: "Tên", FieldType: "text"},
		{Name: "stage", Label: "Giai đoạn", FieldType: "picklist", Options: map[string]any{"values": []any{"won", "lost"}}},
		{Name: "amount", Label: "Giá trị", FieldType: "number"},
		{Name: "won_amount", Label: "Doanh thu thắng", FieldType: "formula",
			Options: map[string]any{"formula": `IF(stage = "won", amount, 0)`, "resultType": "number"}},
	})

	for stage, want := range map[string]float64{"won": 500, "lost": 0} {
		id, err := st.recordSvc.Create(ctx, wfAdmin(user), tenant, user, "fx_deals",
			map[string]any{"title": stage, "stage": stage, "amount": 500.0})
		if err != nil {
			t.Fatalf("Create %s: %v", stage, err)
		}
		rec, err := st.recordSvc.Get(ctx, wfAdmin(user), tenant, "fx_deals", id, nil, nil)
		if err != nil {
			t.Fatalf("Get %s: %v", stage, err)
		}
		if got, _ := rec.Data["won_amount"].(float64); got != want {
			t.Fatalf("stage %s: won_amount = %v; want %v", stage, rec.Data["won_amount"], want)
		}
	}

	// A number field compared with text is refused on add-field.
	_, err := st.fieldSvc.AddField(ctx, tenant, AddFieldInput{EntityName: "fx_deals", Field: FieldInput{
		Name: "bad", Label: "Sai", FieldType: "formula",
		Options: map[string]any{"formula": `IF(amount = "won", 1, 0)`, "resultType": "number"},
	}})
	if got := w5IntCode(t, err); got != "FIELD_FORMULA_TYPE_MISMATCH" {
		t.Fatalf("number vs text: code = %q; want FIELD_FORMULA_TYPE_MISMATCH", got)
	}

	// #233: the picklist and amount are read by won_amount, so neither can be deleted.
	for _, name := range []string{"stage", "amount"} {
		var id string
		if err := st.sdb.GetContext(ctx, &id,
			"SELECT f.id FROM sys_fields f JOIN sys_entities e ON e.id = f.entity_id "+
				"WHERE e.tenant_id = ? AND e.name = ? AND f.name = ? AND f.deleted_at IS NULL",
			tenant, "fx_deals", name); err != nil {
			t.Fatalf("read %s id: %v", name, err)
		}
		err := st.fieldSvc.DeleteField(ctx, tenant, DeleteFieldInput{FieldID: id})
		if got := w5IntCode(t, err); got != "FIELD_HAS_FORMULA_DEPENDANTS" {
			t.Fatalf("delete %s: code = %q; want FIELD_HAS_FORMULA_DEPENDANTS", name, got)
		}
	}
}
