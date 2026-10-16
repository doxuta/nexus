package services

// queue #440 — a currency SUB-field inside a composite carries its own options.currencyCode. The
// #157 edit guard and the #63 Doctor D2 sweep used to look at top-level currency fields only, so
// changing a sub-field's code (or a stale sub-field row) was invisible to both.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
)

func TestDoctor_D2_CompositeCurrencySubFieldReported(t *testing.T) {
	w, _, svc := newDoctorFixture()
	opts := `{"subFields":[` +
		`{"name":"unit","fieldType":"currency","options":{"currencyCode":"VND"}},` +
		`{"name":"legacy","fieldType":"currency"},` +
		`{"name":"note","fieldType":"text"}]}`
	w.entities["deals"] = &metadata.EntityDefinition{ID: "e-deals", Name: "deals"}
	w.fields["e-deals"] = []metadata.FieldDefinition{{Name: "price_box", FieldType: "composite", Options: &opts}}
	w.tables["obj_deals"] = newFakeTableShape()
	w.currencyCodes = map[string]map[string]string{
		"deals.price_box.unit":   {"rec-usd": "USD", "rec-ok": "VND"},
		"deals.price_box.legacy": {"rec-x": "EUR"}, // no configured code: nothing to compare
		"deals.price_box.note":   {"rec-y": "EUR"}, // not a currency sub-field
	}

	got := scanByID(t, svc, "t-a")[DoctorCheckCurrencyCodeMismatch]
	if got.Count != 1 || got.Items[0].Key != "deals.price_box.unit:rec-usd" {
		t.Fatalf("D2 = %+v; want exactly deals.price_box.unit:rec-usd", got.Items)
	}
}

func TestEditField_CompositeSubCurrencyCodeChange_Guard(t *testing.T) {
	mk := func(code string) map[string]any {
		return map[string]any{"subFields": []any{
			map[string]any{"name": "unit", "fieldType": "currency", "options": map[string]any{"currencyCode": code}},
			map[string]any{"name": "note", "fieldType": "text"},
		}}
	}
	stored := `{"subFields":[{"name":"unit","fieldType":"currency","options":{"currencyCode":"USD"}},{"name":"note","fieldType":"text"}]}`

	tests := []struct {
		name     string
		in       map[string]any
		rows     map[string]int64
		wantCode string
		wantCall string
	}{
		{name: "sub USD→VND while 2 records store USD → 422 on composite.sub", in: mk("VND"),
			rows: map[string]int64{"USD": 2}, wantCode: "FIELD_CURRENCY_CODE_IN_USE", wantCall: "courses.box.unit=USD@tenant-1"},
		{name: "sub USD→VND with no USD record → allowed", in: mk("VND"),
			rows: map[string]int64{"VND": 2}, wantCall: "courses.box.unit=USD@tenant-1"},
		{name: "sub code unchanged → not judged", in: mk("USD"), rows: map[string]int64{"USD": 2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			field := &metadata.FieldDefinition{ID: "fld-1", EntityID: "ent-1", Name: "box", Label: "Hộp giá",
				FieldType: "composite", Options: &stored}
			meta := &fakeEditMeta{field: field}
			svc, schema := editService(meta)
			schema.currencyRows = tc.rows
			_, err := svc.EditField(context.Background(), "tenant-1", EditFieldInput{FieldID: "fld-1", Options: tc.in})
			if calls := strings.Join(schema.currencyCalls, ","); calls != tc.wantCall {
				t.Fatalf("CountCurrencyCodeRows calls = %q; want %q", calls, tc.wantCall)
			}
			if tc.wantCode != "" {
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Code != tc.wantCode || ve.Details["field"] != "box.unit" || ve.Details["records"] != int64(2) {
					t.Fatalf("err = %v; want %s on box.unit with 2 records", err, tc.wantCode)
				}
				if len(meta.updated) != 0 {
					t.Fatalf("persisted %d update(s) on a refused edit", len(meta.updated))
				}
				return
			}
			if err != nil {
				t.Fatalf("EditField: %v", err)
			}
		})
	}
}
