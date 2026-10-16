package services

import (
	"sort"
	"strings"
	"testing"
)

// queue #362: a row is judged through EVERY check, so the preview names all of its bad cells at once —
// a wrong-type cell no longer hides a missing required field or a malformed email in the same row.
func TestProcessRows_ReportsEveryErrorOfARow(t *testing.T) {
	specs := []FieldSpec{
		{Name: "name", FieldType: "text", IsRequired: true},
		{Name: "email", FieldType: "email"},
		{Name: "score", FieldType: "number"},
	}
	cm := columnMapping{"Tên": specs[0], "Email": specs[1], "Điểm": specs[2]}
	headers := []string{"Tên", "Email", "Điểm"}
	tests := []struct {
		name       string
		row        []string
		wantFields []string
		wantFailed int // rows kept aside for the later relation / user checks
	}{
		{"type error + required + email", []string{"", "khong-phai-email", "mười"}, []string{"email", "name", "score"}, 1},
		{"required + email, no type error", []string{"", "khong-phai-email", "5"}, []string{"email", "name"}, 1},
		{"type error alone", []string{"An", "an@x.vn", "mười"}, []string{"score"}, 1},
		{"clean row", []string{"An", "an@x.vn", "5"}, nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			valid, failed, errs := processRows(headers, [][]string{tt.row}, cm, specs)
			var got []string
			for _, e := range errs {
				if e.Row != 1 {
					t.Fatalf("error row = %d; want 1", e.Row)
				}
				got = append(got, e.Field)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tt.wantFields, ",") {
				t.Fatalf("error fields = %v; want %v (errs %+v)", got, tt.wantFields, errs)
			}
			if len(failed) != tt.wantFailed || (tt.wantFailed == 0) != (len(valid) == 1) {
				t.Fatalf("valid = %d, failed = %d; want failed %d", len(valid), len(failed), tt.wantFailed)
			}
		})
	}
}

// queue #362 through Import: a row failing validation still has its relation cells looked up, and is
// never written; the clean row beside it is.
func TestImport_NamesRelationCellOfAnAlreadyFailedRow(t *testing.T) {
	store := dealsStore()
	svc := NewImportService(relDealsCatalog(), store, nil).WithRelationChecker(store)
	res := importDeals(t, svc, "Tên,Dự án\n,Không Có\nCăn 2,Phú Gia Riverside\n", ImportInput{
		IsAdmin: true, DryRun: true, Mapping: map[string]string{"Tên": "name", "Dự án": "project"},
	})
	if res.Valid != 1 || res.Failed != 1 {
		t.Fatalf("result = %+v; want 1 valid, 1 failed", res)
	}
	got := map[string]string{}
	for _, e := range res.Errors {
		if e.Row == 1 {
			got[e.Field] = e.Code
		}
	}
	if got["name"] != "RECORD_REQUIRED_MISSING" || got["project"] != "RECORD_RELATION_INVALID" {
		t.Fatalf("row 1 errors = %v; want the missing name AND the unknown project", got)
	}
}

// ValidateRecord keeps answering with the FIRST error (the record routes' contract); the import gets all.
func TestValidateRecordAll_FirstErrorStaysTheContract(t *testing.T) {
	specs := []FieldSpec{
		{Name: "name", FieldType: "text", IsRequired: true},
		{Name: "email", FieldType: "email"},
	}
	payload := map[string]any{"email": "khong-phai-email"}
	if _, err := ValidateRecord(specs, payload); err == nil {
		t.Fatal("ValidateRecord accepted a row missing a required field and with a bad email")
	}
	_, errs := validateRecordAll(specs, payload, nil)
	if len(errs) != 2 {
		t.Fatalf("validateRecordAll = %v; want both errors", errs)
	}
}
