package services

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
)

// specs/061 FR-001..FR-009 — a customer's CSV is written in words people read: picklist LABELS,
// amounts with Vietnamese thousand separators, dd/mm/yyyy dates. The journey of 2026-09-30 lost
// 20/20 rows in each of four apps on exactly these cells.

var stageOpts = map[string]any{
	"values":      []any{"Prospecting", "Proposal", "Closed Won"},
	"valueLabels": map[string]any{"Prospecting": "Tìm kiếm", "Proposal": "Báo giá", "Closed Won": "Thắng"},
}

func TestCoerceCell_HumanValues(t *testing.T) {
	vnd := map[string]any{"currencyCode": "VND"}
	tests := []struct {
		name     string
		spec     FieldSpec
		cell     string
		want     any
		wantCode string // non-empty: expect an error with this code, naming the field
	}{
		// FR-001 picklist: code, label, case/space-insensitive label.
		{"picklist code", FieldSpec{Name: "stage", FieldType: "picklist", Options: stageOpts}, "Proposal", "Proposal", ""},
		{"picklist label", FieldSpec{Name: "stage", FieldType: "picklist", Options: stageOpts}, "Báo giá", "Proposal", ""},
		{"picklist label loose", FieldSpec{Name: "stage", FieldType: "picklist", Options: stageOpts}, "  báo GIÁ ", "Proposal", ""},
		{"picklist code loose", FieldSpec{Name: "stage", FieldType: "picklist", Options: stageOpts}, "closed won", "Closed Won", ""},
		{"picklist unknown", FieldSpec{Name: "stage", FieldType: "picklist", Options: stageOpts}, "Đàm phán", nil, "RECORD_INVALID_PICKLIST"},
		{"picklist ambiguous label", FieldSpec{Name: "g", FieldType: "picklist", Options: map[string]any{
			"values": []any{"a", "b"}, "valueLabels": map[string]any{"a": "Khác", "b": "khác"}}}, "Khác", nil, "RECORD_INVALID_PICKLIST"},
		// FR-002 multi_select: delimited labels, JSON array of labels.
		{"multi_select labels ;", FieldSpec{Name: "tags", FieldType: "multi_select", Options: stageOpts}, "Báo giá; Thắng", []any{"Proposal", "Closed Won"}, ""},
		{"multi_select labels ,", FieldSpec{Name: "tags", FieldType: "multi_select", Options: stageOpts}, "Tìm kiếm, Proposal", []any{"Prospecting", "Proposal"}, ""},
		{"multi_select JSON labels", FieldSpec{Name: "tags", FieldType: "multi_select", Options: stageOpts}, `["Thắng"]`, []any{"Closed Won"}, ""},
		{"multi_select unknown item", FieldSpec{Name: "tags", FieldType: "multi_select", Options: stageOpts}, "Báo giá; Hủy", nil, "RECORD_INVALID_PICKLIST"},
		{"multi_select label with comma", FieldSpec{Name: "city", FieldType: "multi_select", Options: map[string]any{
			"values": []any{"hn"}, "valueLabels": map[string]any{"hn": "Hà Nội, Việt Nam"}}}, "Hà Nội, Việt Nam", []any{"hn"}, ""},
		// FR-003 numbers the way a Vietnamese spreadsheet writes them.
		{"number plain", FieldSpec{Name: "n", FieldType: "number"}, "3200000000", float64(3200000000), ""},
		{"number VN groups", FieldSpec{Name: "n", FieldType: "number"}, "3.200.000", float64(3200000), ""},
		{"number US groups", FieldSpec{Name: "n", FieldType: "number"}, "3,200,000", float64(3200000), ""},
		{"number one VN group", FieldSpec{Name: "n", FieldType: "number"}, "150.000", float64(150000), ""},
		{"number VN decimal comma", FieldSpec{Name: "n", FieldType: "number"}, "85,5", float64(85.5), ""},
		{"number machine decimal", FieldSpec{Name: "n", FieldType: "number"}, "85.5", float64(85.5), ""},
		{"number leading zero decimal", FieldSpec{Name: "n", FieldType: "number"}, "0.125", float64(0.125), ""},
		{"number VN mixed", FieldSpec{Name: "n", FieldType: "number"}, "1.500.000,5", float64(1500000.5), ""},
		{"number US mixed", FieldSpec{Name: "n", FieldType: "number"}, "1,500,000.5", float64(1500000.5), ""},
		{"number negative", FieldSpec{Name: "n", FieldType: "number"}, "-12.5", float64(-12.5), ""},
		{"number spaces", FieldSpec{Name: "n", FieldType: "number"}, "3 200 000", float64(3200000), ""},
		{"number exponent", FieldSpec{Name: "n", FieldType: "number"}, "3.2e+09", float64(3.2e9), ""},
		{"number broken groups", FieldSpec{Name: "n", FieldType: "number"}, "12.34.5", nil, "RECORD_INVALID_TYPE"},
		{"number NaN refused", FieldSpec{Name: "n", FieldType: "number"}, "NaN", nil, "RECORD_INVALID_TYPE"},
		{"number words", FieldSpec{Name: "n", FieldType: "number"}, "abc", nil, "RECORD_INVALID_TYPE"},
		// FR-004 currency: bare amount takes the field's code; a written code is kept (Q7 judges it).
		{"currency bare", FieldSpec{Name: "price", FieldType: "currency", Options: vnd}, "3200000000", map[string]any{"amount": float64(3200000000), "currencyCode": "VND"}, ""},
		{"currency VN groups", FieldSpec{Name: "price", FieldType: "currency", Options: vnd}, "3.200.000.000", map[string]any{"amount": float64(3200000000), "currencyCode": "VND"}, ""},
		{"currency đ suffix", FieldSpec{Name: "price", FieldType: "currency", Options: vnd}, "3.200.000.000 đ", map[string]any{"amount": float64(3200000000), "currencyCode": "VND"}, ""},
		{"currency ₫ suffix", FieldSpec{Name: "price", FieldType: "currency", Options: vnd}, "1.500.000₫", map[string]any{"amount": float64(1500000), "currencyCode": "VND"}, ""},
		{"currency code prefix", FieldSpec{Name: "price", FieldType: "currency", Options: vnd}, "VND 1.000", map[string]any{"amount": float64(1000), "currencyCode": "VND"}, ""},
		{"currency other code kept", FieldSpec{Name: "price", FieldType: "currency", Options: vnd}, "100 usd", map[string]any{"amount": float64(100), "currencyCode": "USD"}, ""},
		{"currency JSON still ok", FieldSpec{Name: "price", FieldType: "currency", Options: vnd}, `{"amount":100,"currencyCode":"VND"}`, map[string]any{"amount": float64(100), "currencyCode": "VND"}, ""},
		{"currency no code anywhere", FieldSpec{Name: "price", FieldType: "currency"}, "100", nil, "RECORD_INVALID_TYPE"},
		{"currency junk", FieldSpec{Name: "price", FieldType: "currency", Options: vnd}, "ba tỷ", nil, "RECORD_INVALID_TYPE"},
		// FR-005 boolean.
		{"boolean Có", FieldSpec{Name: "b", FieldType: "boolean"}, "Có", true, ""},
		{"boolean không", FieldSpec{Name: "b", FieldType: "boolean"}, "không", false, ""},
		{"boolean x", FieldSpec{Name: "b", FieldType: "boolean"}, "x", true, ""},
		{"boolean junk", FieldSpec{Name: "b", FieldType: "boolean"}, "maybe", nil, "RECORD_INVALID_TYPE"},
		// FR-006 date, day first.
		{"date dd/mm/yyyy", FieldSpec{Name: "d", FieldType: "date"}, "15/10/2026", "2026-10-15", ""},
		{"date d/m/yyyy", FieldSpec{Name: "d", FieldType: "date"}, "5/1/2026", "2026-01-05", ""},
		{"date dd-mm-yyyy", FieldSpec{Name: "d", FieldType: "date"}, "15-10-2026", "2026-10-15", ""},
		{"date dd.mm.yyyy", FieldSpec{Name: "d", FieldType: "date"}, "15.10.2026", "2026-10-15", ""},
		{"date iso kept", FieldSpec{Name: "d", FieldType: "date"}, "2026-10-15", "2026-10-15", ""},
		{"date impossible", FieldSpec{Name: "d", FieldType: "date"}, "31/02/2026", nil, "RECORD_INVALID_DATE"},
		// FR-007 datetime, Vietnam wall clock.
		{"datetime dd/mm/yyyy hh:mm", FieldSpec{Name: "at", FieldType: "datetime"}, "15/10/2026 09:30", "2026-10-15T09:30:00+07:00", ""},
		{"datetime iso kept", FieldSpec{Name: "at", FieldType: "datetime"}, "2026-10-15T09:30:00Z", "2026-10-15T09:30:00Z", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := coerceCell(tt.spec, tt.cell)
			if tt.wantCode != "" {
				var ve *ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("coerceCell(%q) = %v, %v; want %s", tt.cell, got, err, tt.wantCode)
				}
				if ve.Code != tt.wantCode || ve.Field != tt.spec.Name {
					t.Fatalf("error = {code %s, field %q}; want {code %s, field %q} (%s)", ve.Code, ve.Field, tt.wantCode, tt.spec.Name, ve.Msg)
				}
				return
			}
			if err != nil {
				t.Fatalf("coerceCell(%q): %v", tt.cell, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("coerceCell(%q) = %#v; want %#v", tt.cell, got, tt.want)
			}
		})
	}
}

// FR-001: an unknown label is refused with the choices a person can read, so the fix is obvious.
func TestCoerceCell_PicklistErrorListsLabels(t *testing.T) {
	_, err := coerceCell(FieldSpec{Name: "stage", FieldType: "picklist", Options: stageOpts}, "Đàm phán")
	if err == nil || !strings.Contains(err.Error(), "Báo giá") || !strings.Contains(err.Error(), "Đàm phán") {
		t.Fatalf("error = %v; want it to name the value and list the labels", err)
	}
}

// FR-008: Excel's "CSV UTF-8" opens with a BOM, and a Vietnamese-region Excel separates with ';'.
func TestParseCSV_ExcelFiles(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"BOM", "\ufeffHọ tên,Giới tính\nAn,Nam\n"},
		{"semicolon", "Họ tên;Giới tính\nAn;Nam\n"},
		{"BOM + semicolon", "\ufeffHọ tên;Giới tính\nAn;Nam\n"},
		// The screen trims every header cell before it sends the mapping (FR-008): a trailing
		// space kept here silently unmapped that column.
		{"padded headers", " Họ tên ,Giới tính \nAn,Nam\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers, rows, err := parseCSV(strings.NewReader(tt.input))
			if err != nil {
				t.Fatalf("parseCSV: %v", err)
			}
			if !reflect.DeepEqual(headers, []string{"Họ tên", "Giới tính"}) {
				t.Fatalf("headers = %q", headers)
			}
			if len(rows) != 1 || !reflect.DeepEqual(rows[0], []string{"An", "Nam"}) {
				t.Fatalf("rows = %q", rows)
			}
		})
	}
}

// FR-009: a type error names its field — for import rows and for the create/edit form alike.
func TestValidateRecord_TypeErrorNamesTheField(t *testing.T) {
	specs := []FieldSpec{{Name: "status", FieldType: "picklist", Options: map[string]any{"values": []any{"a"}}}}
	_, err := ValidateRecord(specs, map[string]any{"status": "z"})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != "status" || ve.Details["field"] != "status" {
		t.Fatalf("ValidateRecord err = %#v; want Field/details.field = status", err)
	}
	_, err = ValidatePartialRecord(specs, map[string]any{"status": "z"})
	if !errors.As(err, &ve) || ve.Field != "status" {
		t.Fatalf("ValidatePartialRecord err = %#v; want Field = status", err)
	}
}

// humanCatalog is an opportunities entity with the cells the journey's files carried.
func humanCatalog() *fakeImportCatalog {
	return &fakeImportCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-opp", Name: "opps"},
		fields: []metadata.FieldDefinition{
			{Name: "name", FieldType: "text", IsRequired: true},
			{Name: "stage", FieldType: "picklist", IsRequired: true, Options: strPtr(`{"values":["Prospecting","Proposal","Closed Won"],"valueLabels":{"Prospecting":"Tìm kiếm","Proposal":"Báo giá","Closed Won":"Thắng"}}`)},
			{Name: "amount", FieldType: "currency", Options: strPtr(`{"currencyCode":"VND"}`)},
			{Name: "close_date", FieldType: "date"},
			{Name: "code", FieldType: "auto_number", Options: strPtr(`{"prefix":"OPP-","padding":5}`)},
		},
	}
}

const humanCSV = "\ufeffTên;Giai đoạn;Giá trị;Ngày chốt\n" +
	"Căn A1;Báo giá;3.200.000.000;15/10/2026\n" +
	"Căn A2;thắng;2.750.000.000 đ;1/11/2026\n" +
	"Căn A3;Đàm phán;1.000.000;02/11/2026\n"

func humanMapping() map[string]string {
	return map[string]string{"Tên": "name", "Giai đoạn": "stage", "Giá trị": "amount", "Ngày chốt": "close_date"}
}

// SC-001/SC-002 at the service: human cells go in as codes/amounts/ISO dates; the one bad row is
// reported against its line AND its field.
func TestImport_HumanFile(t *testing.T) {
	store := &fakeImportStore{}
	svc := NewImportService(humanCatalog(), store, nil)
	res, err := svc.Import(context.Background(), ImportInput{
		TenantID: "t1", UserID: "u1", IsAdmin: true, EntityName: "opps",
		Mapping: humanMapping(), Reader: strings.NewReader(humanCSV),
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Inserted != 2 || res.Failed != 1 || res.Valid != 2 {
		t.Fatalf("result = %+v; want 2 inserted, 1 failed, 2 valid", res)
	}
	if len(res.Errors) != 1 || res.Errors[0].Row != 3 || res.Errors[0].Field != "stage" {
		t.Fatalf("errors = %+v; want row 3 on field stage", res.Errors)
	}
	first := store.insertedRows[0]
	if first["stage"] != "Proposal" || first["close_date"] != "2026-10-15" {
		t.Fatalf("stored row = %#v", first)
	}
	amount, _ := first["amount"].(map[string]any)
	if amount["amount"] != float64(3200000000) || amount["currencyCode"] != "VND" {
		t.Fatalf("stored amount = %#v", first["amount"])
	}
	if store.insertedRows[1]["stage"] != "Closed Won" {
		t.Fatalf("second row stage = %v; want Closed Won", store.insertedRows[1]["stage"])
	}
}

// FR-010/FR-011: the preview runs the same path and writes nothing — no insert, no number claimed.
func TestImport_DryRunWritesNothing(t *testing.T) {
	store := &fakeImportStore{}
	seq := &fakeSequenceClaimer{}
	svc := NewImportService(humanCatalog(), store, nil).WithSequenceClaimer(seq)
	res, err := svc.Import(context.Background(), ImportInput{
		TenantID: "t1", UserID: "u1", IsAdmin: true, EntityName: "opps", DryRun: true,
		Mapping: humanMapping(), Reader: strings.NewReader(humanCSV),
	})
	if err != nil {
		t.Fatalf("Import dry-run: %v", err)
	}
	if store.insertCalled || seq.calls != 0 {
		t.Fatalf("dry-run wrote: insert=%v claims=%d", store.insertCalled, seq.calls)
	}
	if !res.DryRun || res.Valid != 2 || res.Inserted != 0 || res.Failed != 1 || len(res.Errors) != 1 {
		t.Fatalf("dry-run result = %+v; want dryRun, 2 valid, 0 inserted, 1 failed", res)
	}
}

// SC-002: the preview marks EVERY wrong cell of a row, not just the first — a builder fixes the
// file once instead of once per cell.
func TestProcessRows_ReportsEveryBadCell(t *testing.T) {
	specs := []FieldSpec{
		{Name: "name", FieldType: "text", IsRequired: true},
		{Name: "stage", FieldType: "picklist", Options: stageOpts},
		{Name: "score", FieldType: "number"},
	}
	cm := columnMapping{"Tên": specs[0], "Giai đoạn": specs[1], "Điểm": specs[2]}
	headers := []string{"Tên", "Giai đoạn", "Điểm"}
	valid, _, errs := processRows(headers, [][]string{{"An", "Đàm phán", "mười"}}, cm, specs)
	if len(valid) != 0 || len(errs) != 2 {
		t.Fatalf("valid = %d, errs = %+v; want 0 valid and one error per bad cell", len(valid), errs)
	}
	got := map[string]bool{}
	for _, e := range errs {
		if e.Row != 1 {
			t.Fatalf("error row = %d; want 1", e.Row)
		}
		got[e.Field] = true
	}
	if !got["stage"] || !got["score"] {
		t.Fatalf("errors name fields %v; want stage and score", got)
	}
}
