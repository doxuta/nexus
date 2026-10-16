package services

// queue #143 — C9: the admin-side half of the hidden-field anti-oracle. The create path no longer
// names a HIDDEN required field to the role that hits it, so the admin needs one place that does:
// every sys_field_acl row that leaves a role unable to create anything (hidden or read-only on a
// required field the engine cannot fill), with the role, the field and the exits. Report-only —
// lifting a hidden row exposes a field, which is the admin's call, never the Doctor's.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/doctor"
	"nexus/platform/internal/metadata"
)

type fakeDoctorRestrictions struct {
	rows   []auth.FieldACLRestriction
	err    error
	tenant string
}

func (f *fakeDoctorRestrictions) ListFieldRestrictions(_ context.Context, tenantID, entity, field string) ([]auth.FieldACLRestriction, error) {
	f.tenant = tenantID
	if entity != "" || field != "" {
		return nil, errors.New("C9 must scan the whole tenant")
	}
	return f.rows, f.err
}

func TestDoctor_C9_LockedRequiredFieldsReported(t *testing.T) {
	w, _, svc := newDoctorFixture()
	w.entities["deals"] = &metadata.EntityDefinition{ID: "ent-deals", Name: "deals"}
	w.fields["ent-deals"] = []metadata.FieldDefinition{
		{Name: "secret_score", Label: "Điểm nội bộ", FieldType: "number", IsRequired: true},
		{Name: "amount", Label: "Giá trị", FieldType: "currency", IsRequired: true},
		{Name: "stage", FieldType: "text", IsRequired: true, DefaultVal: "new"}, // engine fills it
		{Name: "note", FieldType: "text"},                                       // not required
		{Name: "seq", FieldType: "auto_number", IsRequired: true},               // server-assigned
	}
	restr := &fakeDoctorRestrictions{rows: []auth.FieldACLRestriction{
		{RoleID: "r1", RoleName: "crm_sales_rep", RoleLabel: "Nhân viên kinh doanh", EntityName: "deals", FieldName: "secret_score"},
		{RoleID: "r1", RoleName: "crm_sales_rep", RoleLabel: "Nhân viên kinh doanh", EntityName: "deals", FieldName: "amount", CanView: true},
		{RoleID: "r1", RoleName: "crm_sales_rep", EntityName: "deals", FieldName: "stage"},
		{RoleID: "r1", RoleName: "crm_sales_rep", EntityName: "deals", FieldName: "note"},
		{RoleID: "r1", RoleName: "crm_sales_rep", EntityName: "deals", FieldName: "seq"},
		{RoleID: "r1", RoleName: "crm_sales_rep", EntityName: "deals", FieldName: "gone_field"}, // no such field
		{RoleID: "r1", RoleName: "crm_sales_rep", EntityName: "ghost_entity", FieldName: "x"},   // no such entity
	}}
	svc.WithFieldRestrictions(restr)

	got := scanByID(t, svc, "t-a")[DoctorCheckLockedRequiredFields]
	if !got.ReportOnly || got.AutoFixSafe || got.Severity != doctor.SeverityWarn {
		t.Fatalf("C9 = %+v; want report-only warn, never auto-fix-safe", got)
	}
	if got.Count != 2 || restr.tenant != "t-a" {
		t.Fatalf("C9 count %d (tenant %q); want the 2 dead-end rows of t-a: %+v", got.Count, restr.tenant, got.Items)
	}
	details := map[string]string{}
	for _, it := range got.Items {
		details[it.Key] = it.Detail
	}
	for key, want := range map[string][]string{
		"deals.secret_score:r1": {"Nhân viên kinh doanh", "ẩn", "deals.secret_score", "Điểm nội bộ", "đặt giá trị mặc định"},
		"deals.amount:r1":       {"Nhân viên kinh doanh", "chỉ đọc", "deals.amount", "bắt buộc"},
	} {
		d, ok := details[key]
		if !ok {
			t.Fatalf("items %+v; want key %s", got.Items, key)
		}
		for _, s := range want {
			if !strings.Contains(d, s) {
				t.Errorf("detail %q does not say %q", d, s)
			}
		}
	}
	if strings.Contains(details["deals.amount:r1"], "đặt giá trị mặc định") {
		t.Errorf("currency detail %q advises a default Studio cannot set", details["deals.amount:r1"])
	}

	if _, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckLockedRequiredFields}, true); codeOf(err) != "DOCTOR_REPORT_ONLY" {
		t.Fatalf("fix C9 err = %v; want 422 DOCTOR_REPORT_ONLY", err)
	}
}

func TestDoctor_C9_ReadFailureFailsTheCheck(t *testing.T) {
	_, _, svc := newDoctorFixture()
	svc.WithFieldRestrictions(&fakeDoctorRestrictions{err: errors.New("db down")})
	// The runner is fail-loud: a check that cannot read must fail the scan, not report a clean 0.
	if _, err := svc.Scan(context.Background(), "t-a"); err == nil || !strings.Contains(err.Error(), DoctorCheckLockedRequiredFields) {
		t.Fatalf("Scan err = %v; want the C9 read failure", err)
	}
}

// Unwired (a server built without the field-ACL repository) the check is absent, not a false "0".
func TestDoctor_C9_AbsentWhenUnwired(t *testing.T) {
	_, _, svc := newDoctorFixture()
	if _, ok := scanByID(t, svc, "t-a")[DoctorCheckLockedRequiredFields]; ok {
		t.Fatalf("C9 reported without a restriction reader — a clean count it never measured")
	}
}
