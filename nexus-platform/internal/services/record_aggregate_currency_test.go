package services

import "testing"

// DASH-CURRENCY: an aggregate over a currency field must carry the field's configured ISO code so
// the client formats it in that currency (US$) instead of a hardcoded VNĐ; an aggregate over a
// plain number/rating field must carry an EMPTY code so the client renders a bare number (not a
// bogus currency). resolveAggregatePlan is the one body shared by the run path and the M8 report
// validator, so pinning it here covers both dashboards and reports.
func TestResolveAggregatePlan_CurrencyCode(t *testing.T) {
	specs := []FieldSpec{
		{Name: "amount", FieldType: "currency", Options: map[string]any{"currencyCode": "USD"}},
		{Name: "score", FieldType: "number"},
		{Name: "stars", FieldType: "rating", Options: map[string]any{"max": 5}},
		{Name: "stage", FieldType: "picklist", IsIndexed: true},
	}
	byName := make(map[string]FieldSpec, len(specs))
	for _, f := range specs {
		byName[f.Name] = f
	}

	tests := []struct {
		name     string
		metric   AggregateMetric
		field    string
		wantCode string
	}{
		{"sum currency → carries USD", "sum", "amount", "USD"},
		{"avg currency → carries USD", "avg", "amount", "USD"},
		{"sum number → empty (plain number, no currency)", "sum", "score", ""},
		{"max rating → empty", "max", "stars", ""},
		{"count → empty (no field)", "count", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := resolveAggregatePlan(tt.metric, tt.field, "", "", specs, byName)
			if err != nil {
				t.Fatalf("resolveAggregatePlan(%s,%s): unexpected error %v", tt.metric, tt.field, err)
			}
			if plan.currencyCode != tt.wantCode {
				t.Fatalf("currencyCode = %q; want %q", plan.currencyCode, tt.wantCode)
			}
			// A currency field must also route to its .amount numeric sub-path (unchanged behavior).
			if tt.wantCode != "" && plan.fieldSubPath != "amount" {
				t.Fatalf("currency field should route to .amount sub-path, got %q", plan.fieldSubPath)
			}
		})
	}

	// Guard: a currency field with NO configured currencyCode yields empty (client falls back to a
	// bare number rather than inventing a currency).
	specs2 := []FieldSpec{{Name: "amount", FieldType: "currency"}}
	byName2 := map[string]FieldSpec{"amount": specs2[0]}
	plan, err := resolveAggregatePlan("sum", "amount", "", "", specs2, byName2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.currencyCode != "" {
		t.Fatalf("currency field w/o code should yield empty currencyCode, got %q", plan.currencyCode)
	}
}
