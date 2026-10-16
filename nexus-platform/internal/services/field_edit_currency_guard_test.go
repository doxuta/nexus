package services

// queue #157 (currency-config-change-guard) — the vector that kept producing "legacy mismatched"
// currency rows after queue #63. Changing a currency field's configured options.currencyCode used
// to be a plain metadata write: every record already stored in the old code instantly read as a
// different amount of money (USD 100 became "VND 100" in every total), and nobody was told. This
// pins that EditField refuses exactly that edit — only while live records still carry the old
// code — and names how many records it would relabel.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
)

func TestEditField_CurrencyCodeChange_Guard(t *testing.T) {
	usd := `{"currencyCode":"USD","precision":2}`
	noCode := `{"precision":2}`

	tests := []struct {
		name       string
		fieldType  string
		stored     *string
		in         EditFieldInput
		rows       map[string]int64 // stored code → live rows of this tenant
		trashed    map[string]int64 // stored code → recycle-bin rows of this tenant
		countErr   error
		wantCode   string // "" = edit succeeds
		wantErr    bool   // non-validation error (infra) — nothing persisted
		wantCounts int    // CountCurrencyCodeRows calls
		wantRecs   int64  // details.records on refusal (live + recycle bin)
		wantBin    int64  // details.recycleBin on refusal
	}{
		{name: "USD→VND while 3 records are stored in USD → 422 naming the count",
			fieldType: "currency", stored: &usd,
			in:   EditFieldInput{Options: map[string]any{"currencyCode": "VND", "precision": float64(2)}},
			rows: map[string]int64{"USD": 3}, wantCode: "FIELD_CURRENCY_CODE_IN_USE", wantCounts: 1, wantRecs: 3},
		{name: "USD→VND while the only USD records are in the recycle bin → 422 (a restore would bring them back relabelled)",
			fieldType: "currency", stored: &usd,
			in:   EditFieldInput{Options: map[string]any{"currencyCode": "VND", "precision": float64(2)}},
			rows: map[string]int64{"VND": 4}, trashed: map[string]int64{"USD": 3},
			wantCode: "FIELD_CURRENCY_CODE_IN_USE", wantCounts: 1, wantRecs: 3, wantBin: 3},
		{name: "USD→VND with USD records live and in the recycle bin → 422 naming both counts",
			fieldType: "currency", stored: &usd,
			in:   EditFieldInput{Options: map[string]any{"currencyCode": "VND", "precision": float64(2)}},
			rows: map[string]int64{"USD": 1}, trashed: map[string]int64{"USD": 2},
			wantCode: "FIELD_CURRENCY_CODE_IN_USE", wantCounts: 1, wantRecs: 3, wantBin: 2},
		{name: "USD→VND with no record in USD (every row already VND or empty) → allowed",
			fieldType: "currency", stored: &usd,
			in:   EditFieldInput{Options: map[string]any{"currencyCode": "VND", "precision": float64(2)}},
			rows: map[string]int64{"VND": 4}, trashed: map[string]int64{"VND": 2}, wantCounts: 1},
		{name: "options resent with the SAME code (precision edit) → not judged",
			fieldType: "currency", stored: &usd,
			in:   EditFieldInput{Options: map[string]any{"currencyCode": "USD", "precision": float64(0)}},
			rows: map[string]int64{"USD": 3}},
		{name: "relabel only (no options) → not judged",
			fieldType: "currency", stored: &usd, in: EditFieldInput{Label: strPtr("Doanh thu")},
			rows: map[string]int64{"USD": 3}},
		{name: "legacy field without a configured code gains one → nothing was being compared, not judged",
			fieldType: "currency", stored: &noCode,
			in:   EditFieldInput{Options: map[string]any{"currencyCode": "VND"}},
			rows: map[string]int64{"USD": 3}},
		{name: "count read fails → refuse (fail closed), nothing persisted",
			fieldType: "currency", stored: &usd,
			in:       EditFieldInput{Options: map[string]any{"currencyCode": "VND"}},
			countErr: errors.New("db down"), wantErr: true, wantCounts: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			field := &metadata.FieldDefinition{ID: "fld-1", EntityID: "ent-1", Name: "amount", Label: "Giá trị",
				FieldType: tc.fieldType, Options: tc.stored}
			meta := &fakeEditMeta{field: field}
			svc, schema := editService(meta)
			schema.currencyRows, schema.currencyTrashed, schema.currencyErr = tc.rows, tc.trashed, tc.countErr

			in := tc.in
			in.FieldID = "fld-1"
			_, err := svc.EditField(context.Background(), "tenant-1", in)

			if len(schema.currencyCalls) != tc.wantCounts {
				t.Fatalf("CountCurrencyCodeRows calls = %v; want %d", schema.currencyCalls, tc.wantCounts)
			}
			if tc.wantCounts > 0 && schema.currencyCalls[0] != "courses.amount=USD@tenant-1" {
				t.Errorf("count asked %q; want the OLD code of this tenant's field (courses.amount=USD@tenant-1)", schema.currencyCalls[0])
			}
			switch {
			case tc.wantErr:
				var ve *ValidationError
				if err == nil || errors.As(err, &ve) {
					t.Fatalf("err = %v; want a non-validation (infra) error", err)
				}
				if len(meta.updated) != 0 {
					t.Fatalf("persisted %d update(s) on a failed guard read; want 0", len(meta.updated))
				}
			case tc.wantCode != "":
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Code != tc.wantCode {
					t.Fatalf("err = %v; want %s", err, tc.wantCode)
				}
				if len(meta.updated) != 0 {
					t.Fatalf("persisted %d update(s) on a refused edit; want 0", len(meta.updated))
				}
				if ve.Details["records"] != tc.wantRecs || ve.Details["recycleBin"] != tc.wantBin || ve.Details["from"] != "USD" || ve.Details["to"] != "VND" || ve.Details["field"] != "amount" {
					t.Errorf("details = %v; want field=amount from=USD to=VND records=%d recycleBin=%d", ve.Details, tc.wantRecs, tc.wantBin)
				}
				if !strings.Contains(ve.Msg, "3 bản ghi") || !strings.Contains(ve.Msg, "Giá trị") || !strings.Contains(ve.Msg, "xoá vĩnh viễn") {
					t.Errorf("message %q must name the field label, the record count and the permanent-delete exit", ve.Msg)
				}
				if inBin := strings.Contains(ve.Msg, "thùng rác —"); inBin != (tc.wantBin > 0) {
					t.Errorf("message %q: names the recycle-bin count = %v; want %v", ve.Msg, inBin, tc.wantBin > 0)
				}
			default:
				if err != nil {
					t.Fatalf("EditField: %v", err)
				}
				if len(meta.updated) != 1 {
					t.Fatalf("persisted %d update(s); want 1", len(meta.updated))
				}
			}
		})
	}
}
