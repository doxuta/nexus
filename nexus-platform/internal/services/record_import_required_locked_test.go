package services

// queue #144 (import-required-locked-deadend) — CSV import is a create path too, so it must ask the
// question Create asks and say what Create says:
//   - a required field the engine fills from its default does not need a CSV column (Create omits
//     it and the default fills it; buildMapping used to demand a mapping anyway);
//   - a required field locked for the importer's role cannot be mapped (FIELD_FORBIDDEN) nor left
//     out (IMPORT_REQUIRED_FIELD_NOT_MAPPED) — the dead end must be named RECORD_REQUIRED_LOCKED
//     with the admin's way out, like the form and "Tạo nhiều dòng".

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metadata"
)

func newRequiredImportSvc(acl fieldACLEnforcer) (*ImportService, *fakeImportStore) {
	cat := &fakeImportCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-1", Name: "deals"},
		fields: []metadata.FieldDefinition{
			{Name: "name", FieldType: "text", IsRequired: true},
			{Name: "amount", Label: "Giá trị", FieldType: "number", IsRequired: true},
			{Name: "stage", FieldType: "text", IsRequired: true, DefaultVal: "new"},
		},
	}
	store := &fakeImportStore{}
	svc := NewImportService(cat, store, nil)
	if acl != nil {
		svc.WithFieldACL(acl)
	}
	return svc, store
}

func importCSV(csv string, mapping map[string]string, admin bool) ImportInput {
	return ImportInput{
		TenantID: "t1", UserID: "u1", RoleID: "role-1", IsAdmin: admin, EntityName: "deals",
		Mapping: mapping, Reader: strings.NewReader(csv), FileSize: int64(len(csv)),
	}
}

func TestImport_RequiredFieldWithDefault_NeedNotBeMapped(t *testing.T) {
	svc, store := newRequiredImportSvc(nil)
	res, err := svc.Import(context.Background(), importCSV("n,a\nAcme,5\n", map[string]string{"n": "name", "a": "amount"}, false))
	if err != nil {
		t.Fatalf("Import: %v; want success (stage has a default the engine fills)", err)
	}
	if res.Inserted != 1 || len(store.insertedRows) != 1 || store.insertedRows[0]["stage"] != "new" {
		t.Fatalf("result %+v rows %+v; want 1 row with stage filled from its default", res, store.insertedRows)
	}
}

func TestImport_RequiredFieldLockedForRole_SaysLocked(t *testing.T) {
	tests := []struct {
		name      string
		rows      []auth.FieldACL
		admin     bool
		wantCode  string
		wantField string
	}{
		{name: "read-only for the importer's role → RECORD_REQUIRED_LOCKED naming the exit",
			rows: []auth.FieldACL{lock("amount")}, wantCode: "RECORD_REQUIRED_LOCKED", wantField: "amount"},
		// queue #143: a HIDDEN required field must not be named to the importer (the error used to
		// say `field bắt buộc "amount" chưa có mapping` to a role that may not see amount).
		{name: "hidden for the importer's role → RECORD_REQUIRED_LOCKED without the name",
			rows: []auth.FieldACL{hide("amount")}, wantCode: "RECORD_REQUIRED_LOCKED", wantField: ""},
		{name: "not locked → the importer simply forgot the column",
			rows: nil, wantCode: "IMPORT_REQUIRED_FIELD_NOT_MAPPED", wantField: "amount"},
		{name: "admin bypasses locks → a real omission",
			rows: []auth.FieldACL{lock("amount")}, admin: true, wantCode: "IMPORT_REQUIRED_FIELD_NOT_MAPPED", wantField: "amount"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, store := newRequiredImportSvc(NewFieldACLService(&countingFLSLoader{rows: tt.rows}))
			_, err := svc.Import(context.Background(), importCSV("n\nAcme\n", map[string]string{"n": "name"}, tt.admin))
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != tt.wantCode {
				t.Fatalf("err = %v; want ValidationError %s", err, tt.wantCode)
			}
			if ve.Field != tt.wantField {
				t.Fatalf("field = %q; want %q", ve.Field, tt.wantField)
			}
			if tt.wantField == "" {
				assertNamesNoField(t, ve, "amount")
				if strings.Contains(ve.Msg, "Giá trị") {
					t.Fatalf("msg %q names the hidden field by its label", ve.Msg)
				}
			} else if tt.wantCode == "RECORD_REQUIRED_LOCKED" && (!strings.Contains(ve.Msg, "Giá trị") || !strings.Contains(ve.Msg, "bắt buộc")) {
				t.Fatalf("msg %q must name the field by label and the admin's exit", ve.Msg)
			}
			if store.insertCalled {
				t.Fatal("nothing may be inserted")
			}
		})
	}
}
