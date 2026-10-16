package services

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"nexus/platform/internal/dynrepo"
)

// DAILY-QUEUE #336: "{{nguoi_phu_trach.email}}" — a `user` field names a row of the system `users`
// table, read through BatchFetchUserLabels (tenant-pinned), never through an obj_* record.

// tplUsers is tplRecords plus the user-label read; asked tenants/ids are recorded.
type tplUsers struct {
	tplRecords
	users map[string]dynrepo.UserLabelResult // "tenant|id"
	uerr  error
	reads []string // "tenant|id,id"
}

func (u *tplUsers) BatchFetchUserLabels(_ context.Context, tenantID string, ids []string) ([]dynrepo.UserLabelResult, error) {
	u.reads = append(u.reads, tenantID+"|"+ids[0])
	if u.uerr != nil {
		return nil, u.uerr
	}
	var out []dynrepo.UserLabelResult
	for _, id := range ids {
		if r, ok := u.users[tenantID+"|"+id]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func userFixture() *tplCatalog {
	c := tplFixture()
	c.fields["lich_hen"] = append(c.fields["lich_hen"], tplField("nguoi_phu_trach", "user", ""))
	return c
}

func TestWorkflowTemplateResolver_UserField(t *testing.T) {
	users := &tplUsers{users: map[string]dynrepo.UserLabelResult{
		"t1|u-1": {ID: "u-1", Name: "Lê Tân", Email: "tan@nhakhoa.vn"},
		"t2|u-9": {ID: "u-9", Name: "Người tenant khác", Email: "x@tenant2.vn"},
	}}
	r := NewWorkflowTemplateResolver(userFixture(), users)

	got, err := r.ResolveTemplateValues(context.Background(), "t1", "lich_hen",
		map[string]any{"nguoi_phu_trach": "u-1"}, []string{"nguoi_phu_trach.email", "nguoi_phu_trach.name"})
	if err != nil {
		t.Fatalf("ResolveTemplateValues: %v", err)
	}
	want := map[string]any{"nguoi_phu_trach.email": "tan@nhakhoa.vn", "nguoi_phu_trach.name": "Lê Tân"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v; want %v", got, want)
	}
	if !reflect.DeepEqual(users.reads, []string{"t1|u-1"}) {
		t.Errorf("user reads %v; want ONE tenant-pinned read", users.reads)
	}

	// Another tenant's user, an empty FK, a field users do not expose: nothing resolves, no error.
	for name, data := range map[string]map[string]any{
		"cross-tenant id": {"nguoi_phu_trach": "u-9"},
		"empty fk":        {"nguoi_phu_trach": ""},
		"no fk":           {},
	} {
		got, err := r.ResolveTemplateValues(context.Background(), "t1", "lich_hen", data, []string{"nguoi_phu_trach.email"})
		if err != nil || len(got) != 0 {
			t.Errorf("%s: got %v, %v; want nothing", name, got, err)
		}
	}
	if got, _ := r.ResolveTemplateValues(context.Background(), "t1", "lich_hen", map[string]any{"nguoi_phu_trach": "u-1"}, []string{"nguoi_phu_trach.password_hash"}); len(got) != 0 {
		t.Errorf("a column users do not expose resolved: %v", got)
	}
}

func TestWorkflowTemplateResolver_UserReadFailureSurfaces(t *testing.T) {
	cause := errors.New("db down")
	r := NewWorkflowTemplateResolver(userFixture(), &tplUsers{uerr: cause})
	_, err := r.ResolveTemplateValues(context.Background(), "t1", "lich_hen", map[string]any{"nguoi_phu_trach": "u-1"}, []string{"nguoi_phu_trach.email"})
	if !errors.Is(err, cause) {
		t.Errorf("err = %v; want the user read failure wrapped", err)
	}
}

func TestEmailTemplateRefs_UserFieldConfigTimeValidation(t *testing.T) {
	tests := []struct {
		name, to  string
		wantField string // "" = accepted
	}{
		{name: "owner email", to: "{{nguoi_phu_trach.email}}"},
		{name: "owner name", to: "{{nguoi_phu_trach.name}}"},
		{name: "a column users do not expose", to: "{{nguoi_phu_trach.password_hash}}", wantField: "nguoi_phu_trach.password_hash"},
		{name: "unknown user field", to: "{{nguoi_phu_trach.so_dt}}", wantField: "nguoi_phu_trach.so_dt"},
		{name: "two hops", to: "{{nguoi_phu_trach.email.x}}", wantField: "nguoi_phu_trach.email.x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeWFRepo()
			svc := NewWorkflowService(repo).WithEntityChecker(userFixture())
			_, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
				Name: "r", EntityName: "lich_hen", Trigger: "after_create", ActorID: "u1",
				Actions: []WorkflowActionInput{{Type: "email_alert", To: tt.to, Subject: "s", Body: "b"}},
			})
			if tt.wantField == "" {
				if err != nil || len(repo.created) != 1 {
					t.Fatalf("CreateWorkflow = %v (created %d); want the rule saved", err, len(repo.created))
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != "WORKFLOW_TEMPLATE_REF_INVALID" {
				t.Fatalf("err = %v; want WORKFLOW_TEMPLATE_REF_INVALID", err)
			}
			if got, _ := ve.Details["field"].(string); got != tt.wantField {
				t.Errorf("details.field = %q; want %q", got, tt.wantField)
			}
		})
	}
}
