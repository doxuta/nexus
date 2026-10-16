package services

// Queue #63 — D2: the Doctor reports every live record whose stored currency code differs from the
// field's configured code, and offers NO fix (Q7: never relabel, never convert).

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
)

// seedCurrencyEntity registers an entity with one VND-configured currency field, one currency
// field without a configured code, and a text field; its obj_ table physically exists.
func seedCurrencyEntity(w *fakeDoctorWorld, name, id string) {
	vnd := `{"currencyCode":"VND"}`
	w.entities[name] = &metadata.EntityDefinition{ID: id, Name: name}
	w.fields[id] = []metadata.FieldDefinition{
		{Name: "title", FieldType: "text"},
		{Name: "amount", FieldType: "currency", Options: &vnd},
		{Name: "legacy_money", FieldType: "currency"}, // no configured code: nothing to compare against
	}
	w.tables["obj_"+name] = newFakeTableShape()
}

func TestDoctor_D2_CurrencyCodeMismatchReported(t *testing.T) {
	w, _, svc := newDoctorFixture()
	seedCurrencyEntity(w, "deals", "e-deals")
	w.currencyCodes = map[string]map[string]string{
		"deals.amount":       {"rec-usd": "USD", "rec-ok": "VND", "rec-lower": "vnd", "rec-blank": " "},
		"deals.legacy_money": {"rec-x": "EUR"},
	}

	got := scanByID(t, svc, "t-a")[DoctorCheckCurrencyCodeMismatch]
	if !got.ReportOnly || got.AutoFixSafe {
		t.Fatalf("D2 = %+v; want reportOnly and never auto-fix-safe (Q7: no relabel, no conversion)", got)
	}
	if got.Count != 2 {
		t.Fatalf("D2 count = %d (%+v); want 2 — USD and the lowercase vnd (exact match, same as the write guard); VND, blank and the unconfigured field are not findings", got.Count, got.Items)
	}
	keys := map[string]string{}
	for _, it := range got.Items {
		keys[it.Key] = it.Detail
	}
	d, ok := keys["deals.amount:rec-usd"]
	if !ok {
		t.Fatalf("items = %+v; want key deals.amount:rec-usd", got.Items)
	}
	for _, want := range []string{"rec-usd", "deals.amount", `"USD"`, `"VND"`, "Không sửa tự động"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail %q does not name %s", d, want)
		}
	}
	if _, ok := keys["deals.amount:rec-lower"]; !ok {
		t.Errorf("items = %+v; want the lowercase vnd reported too", got.Items)
	}
	for _, code := range w.gotWantCodes {
		if code != "VND" {
			t.Errorf("a D2 query compared against %q; only the configured VND field may be scanned", code)
		}
	}
}

func TestDoctor_D2_MissingTableIsNotD2sClass(t *testing.T) {
	w, _, svc := newDoctorFixture()
	seedCurrencyEntity(w, "deals", "e-deals")
	delete(w.tables, "obj_deals") // A6's class — querying it would fail the whole scan
	w.currencyCodes = map[string]map[string]string{"deals.amount": {"rec-usd": "USD"}}

	if got := scanByID(t, svc, "t-a")[DoctorCheckCurrencyCodeMismatch]; got.Count != 0 {
		t.Fatalf("D2 = %+v; want 0 on a missing table", got)
	}
	if len(w.gotWantCodes) != 0 {
		t.Fatalf("D2 queried a missing table %d time(s)", len(w.gotWantCodes))
	}
}

func TestDoctor_D2_FixRefused(t *testing.T) {
	w, _, svc := newDoctorFixture()
	seedCurrencyEntity(w, "deals", "e-deals")
	w.currencyCodes = map[string]map[string]string{"deals.amount": {"rec-usd": "USD"}}

	_, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckCurrencyCodeMismatch}, true)
	if codeOf(err) != "DOCTOR_REPORT_ONLY" {
		t.Fatalf("fix D2 err = %v; want 422 DOCTOR_REPORT_ONLY", err)
	}
}
