package services

import (
	"errors"
	"testing"
)

// specs/070 FR-001 (#264): a formula field is computed AND stored at write time
// (applyComputedFields), in exactly the shape of a number (resultType number/empty) or a currency
// (resultType currency → {amount, currencyCode}). SUM/AVG/MIN/MAX must therefore run on it
// through the same JSON path as the plain type it stores as. Text formulas and read-time computed
// types (rollup/lookup) stay refused with AGGREGATE_FIELD_NOT_NUMERIC.
func TestResolveAggregatePlan_FormulaFields(t *testing.T) {
	specs := []FieldSpec{
		{Name: "balance", FieldType: "formula", Options: map[string]any{"formula": "total - paid"}},
		{Name: "margin", FieldType: "formula", Options: map[string]any{"formula": "price - cost", "resultType": "number"}},
		{Name: "line_total", FieldType: "formula", Options: map[string]any{"formula": "qty * price", "resultType": "currency", "currencyCode": "VND"}},
		{Name: "fee_usd", FieldType: "formula", Options: map[string]any{"formula": "qty * 2", "resultType": "currency"}},
		{Name: "tier", FieldType: "formula", Options: map[string]any{"formula": `IF(total > 100, "Lớn", "Nhỏ")`, "resultType": "text"}},
		{Name: "enrolled", FieldType: "rollup", Options: map[string]any{"function": "count"}},
		{Name: "account_name", FieldType: "lookup"},
		// specs/080: reads a rollup => computed at read, nothing stored to SUM.
		{Name: "thanh_tien", FieldType: "formula", Options: map[string]any{"formula": "enrolled * 2"}},
	}
	byName := make(map[string]FieldSpec, len(specs))
	for _, f := range specs {
		byName[f.Name] = f
	}

	ok := []struct {
		name        string
		metric      AggregateMetric
		field       string
		wantSubPath string
		wantCode    string
	}{
		{"sum formula, default resultType → number path", "sum", "balance", "", ""},
		{"avg formula resultType number → number path", "avg", "margin", "", ""},
		{"sum formula resultType currency → .amount + its code", "sum", "line_total", "amount", "VND"},
		// computed_fields.go stores "USD" when the formula has no currencyCode — the aggregate must
		// report the code the rows actually carry, not an empty/invented one.
		{"max currency formula without code → USD as stored", "max", "fee_usd", "amount", "USD"},
	}
	for _, tt := range ok {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := resolveAggregatePlan(tt.metric, tt.field, "", "", specs, byName)
			if err != nil {
				t.Fatalf("resolveAggregatePlan(%s,%s): unexpected error %v", tt.metric, tt.field, err)
			}
			if plan.fieldColumn != tt.field || plan.fieldSubPath != tt.wantSubPath || plan.currencyCode != tt.wantCode {
				t.Fatalf("plan = {col:%q sub:%q ccy:%q}; want {col:%q sub:%q ccy:%q}",
					plan.fieldColumn, plan.fieldSubPath, plan.currencyCode, tt.field, tt.wantSubPath, tt.wantCode)
			}
		})
	}

	for _, field := range []string{"tier", "enrolled", "account_name", "thanh_tien"} {
		t.Run("refused: "+field, func(t *testing.T) {
			_, err := resolveAggregatePlan("sum", field, "", "", specs, byName)
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != "AGGREGATE_FIELD_NOT_NUMERIC" {
				t.Fatalf("sum on %s: err = %v; want AGGREGATE_FIELD_NOT_NUMERIC", field, err)
			}
		})
	}
}

// The code the aggregate reports and the code applyComputedFields writes come from ONE helper, so
// they cannot drift: pin that the stored value carries the same code the plan reports.
func TestFormulaCurrencyCode_MatchesStoredValue(t *testing.T) {
	for _, opts := range []map[string]any{
		{"formula": "qty * 2", "resultType": "currency"},
		{"formula": "qty * 2", "resultType": "currency", "currencyCode": "VND"},
	} {
		specs := []FieldSpec{
			{Name: "qty", FieldType: "number"},
			{Name: "fee", FieldType: "formula", Options: opts},
		}
		data := map[string]any{"qty": 3.0}
		applyComputedFields(specs, data)
		stored, _ := data["fee"].(map[string]any)
		plan, err := resolveAggregatePlan("sum", "fee", "", "", specs, map[string]FieldSpec{"qty": specs[0], "fee": specs[1]})
		if err != nil {
			t.Fatalf("resolveAggregatePlan: %v", err)
		}
		if stored["currencyCode"] != plan.currencyCode {
			t.Fatalf("stored currencyCode %v != aggregate currencyCode %q (options %v)", stored["currencyCode"], plan.currencyCode, opts)
		}
	}
}
