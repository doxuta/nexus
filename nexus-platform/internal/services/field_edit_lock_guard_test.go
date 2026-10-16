package services

// queue #142 (field-edit-required-while-locked-guard) — the other door to the #62 dead end.
// PutGrants refuses to LOCK a required field the engine cannot fill; this pins that EditField
// refuses to make a LOCKED field required-without-fill (isRequired:true, or clearing the default
// that was filling it) and names the roles it would strand.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metadata"
)

type fakeRestrictionLister struct {
	rows         []auth.FieldACLRestriction
	err          error
	calls        int
	gotTenant    string
	gotEntity    string
	gotFieldName string
}

func (f *fakeRestrictionLister) ListFieldRestrictions(_ context.Context, tenantID, entity, field string) ([]auth.FieldACLRestriction, error) {
	f.calls++
	f.gotTenant, f.gotEntity, f.gotFieldName = tenantID, entity, field
	return f.rows, f.err
}

func TestEditField_RequiredWhileLocked_Guard(t *testing.T) {
	salesLock := auth.FieldACLRestriction{RoleID: "r-sales", RoleName: "crm_sales_rep", RoleLabel: "Nhân viên kinh doanh",
		EntityName: "courses", FieldName: "amount", CanView: true, CanEdit: false}
	supportHide := auth.FieldACLRestriction{RoleID: "r-sup", RoleName: "support", EntityName: "courses", FieldName: "amount"}
	yes, no, empty, fill := true, false, "", "100"

	tests := []struct {
		name        string
		stored      metadata.FieldDefinition
		in          EditFieldInput
		rows        []auth.FieldACLRestriction
		listErr     error
		wantCode    string // "" = edit succeeds
		wantErr     bool   // non-validation error (infra) — nothing persisted
		wantRoles   int
		wantDefault bool // details.defaultSupported
		wantListed  bool // the guard had to read sys_field_acl
	}{
		{name: "make a locked text field required without default → 422 naming both roles",
			stored: metadata.FieldDefinition{FieldType: "text"}, in: EditFieldInput{IsRequired: &yes},
			rows: []auth.FieldACLRestriction{salesLock, supportHide}, wantCode: "FIELD_REQUIRED_WHILE_LOCKED", wantRoles: 2, wantDefault: true, wantListed: true},
		{name: "clear the default that was filling a required locked field → 422",
			stored: metadata.FieldDefinition{FieldType: "number", IsRequired: true, DefaultVal: "5"}, in: EditFieldInput{DefaultVal: &empty},
			rows: []auth.FieldACLRestriction{salesLock}, wantCode: "FIELD_REQUIRED_WHILE_LOCKED", wantRoles: 1, wantDefault: true, wantListed: true},
		{name: "currency: the refusal does not advise a default Studio cannot set",
			stored: metadata.FieldDefinition{FieldType: "currency"}, in: EditFieldInput{IsRequired: &yes},
			rows: []auth.FieldACLRestriction{salesLock}, wantCode: "FIELD_REQUIRED_WHILE_LOCKED", wantRoles: 1, wantDefault: false, wantListed: true},
		{name: "required WITH a default in the same edit → engine fills it, allowed without reading locks",
			stored: metadata.FieldDefinition{FieldType: "number"}, in: EditFieldInput{IsRequired: &yes, DefaultVal: &fill},
			rows: []auth.FieldACLRestriction{salesLock}},
		{name: "no role restricts the field → allowed",
			stored: metadata.FieldDefinition{FieldType: "text"}, in: EditFieldInput{IsRequired: &yes}, wantListed: true},
		{name: "turning required OFF on a locked field is the exit — never judged",
			stored: metadata.FieldDefinition{FieldType: "text", IsRequired: true}, in: EditFieldInput{IsRequired: &no},
			rows: []auth.FieldACLRestriction{salesLock}},
		// queue #62 rule: judge what the edit CHANGES. A field already required-without-fill under a
		// lock (rows saved before the PutGrants guard) must stay editable, or the admin cannot even
		// relabel it while deciding which exit to take.
		{name: "already a dead end (legacy) + relabel → allowed",
			stored: metadata.FieldDefinition{FieldType: "text", IsRequired: true}, in: EditFieldInput{Label: strPtr("Mã")},
			rows: []auth.FieldACLRestriction{salesLock}},
		{name: "server-assigned type made required → engine fills it, not judged",
			stored: metadata.FieldDefinition{FieldType: "auto_number"}, in: EditFieldInput{IsRequired: &yes},
			rows: []auth.FieldACLRestriction{salesLock}},
		{name: "lock read fails → refuse (fail closed), nothing persisted",
			stored: metadata.FieldDefinition{FieldType: "text"}, in: EditFieldInput{IsRequired: &yes},
			listErr: errors.New("db down"), wantErr: true, wantListed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stored := tt.stored
			stored.ID, stored.EntityID, stored.Name, stored.Label = "fld-a", "ent-1", "amount", "Giá trị"
			meta := &fakeEditMeta{field: &stored}
			lister := &fakeRestrictionLister{rows: tt.rows, err: tt.listErr}
			svc, _ := editService(meta)
			svc.WithFieldRestrictions(lister)

			in := tt.in
			in.FieldID = "fld-a"
			_, err := svc.EditField(context.Background(), "tenant-1", in)

			if (lister.calls > 0) != tt.wantListed {
				t.Fatalf("sys_field_acl read = %d; want read=%v", lister.calls, tt.wantListed)
			}
			if lister.calls > 0 && (lister.gotTenant != "tenant-1" || lister.gotEntity != "courses" || lister.gotFieldName != "amount") {
				t.Fatalf("lister got (%q, %q, %q); want the edited field, tenant-pinned", lister.gotTenant, lister.gotEntity, lister.gotFieldName)
			}
			switch {
			case tt.wantErr:
				if err == nil || codeOf(err) != "" {
					t.Fatalf("err = %v; want an infra error (not a validation code)", err)
				}
			case tt.wantCode == "":
				if err != nil {
					t.Fatalf("EditField: %v; want success", err)
				}
				if len(meta.updated) != 1 {
					t.Fatalf("updated = %d; want the edit persisted once", len(meta.updated))
				}
				return
			default:
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Code != tt.wantCode {
					t.Fatalf("err = %v; want ValidationError %s", err, tt.wantCode)
				}
				roles, _ := ve.Details["roles"].([]map[string]any)
				if len(roles) != tt.wantRoles {
					t.Fatalf("details.roles = %v; want %d roles", ve.Details["roles"], tt.wantRoles)
				}
				if !strings.Contains(ve.Msg, "Nhân viên kinh doanh") {
					t.Fatalf("msg %q must name the stranded role by its label", ve.Msg)
				}
				if got, _ := ve.Details["defaultSupported"].(bool); got != tt.wantDefault {
					t.Fatalf("details.defaultSupported = %v; want %v", ve.Details["defaultSupported"], tt.wantDefault)
				}
				if offers := strings.Contains(ve.Msg, "đặt giá trị mặc định"); offers != tt.wantDefault {
					t.Fatalf("msg %q offers a default = %v; want %v", ve.Msg, offers, tt.wantDefault)
				}
				if ve.Field != "amount" {
					t.Fatalf("field = %q; want amount", ve.Field)
				}
			}
			if len(meta.updated) != 0 {
				t.Fatalf("a refused edit must not persist: %+v", meta.updated)
			}
		})
	}
}
