package vertical_pack

// PACK-FIELD-ACL: field-level security must survive the tenant boundary.
//
// It was the STRICTEST configuration in the app and the only one handover dropped. A field hidden
// from a role in the factory tenant became fully visible to the same-named role in the customer's —
// silently: nothing failed, nothing warned, and the person checking is is_admin, who bypasses field
// ACL entirely and therefore cannot see the difference.
//
// Keyed by role NAME, like approvals and work queues, because ids do not cross tenants.

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

type fakeFieldACL struct {
	put  []services.FieldACLGrantInput
	list map[string][]services.FieldACLGrantView // "roleID|entity" → grants
	err  error
}

func (f *fakeFieldACL) PutGrants(_ context.Context, _ string, g []services.FieldACLGrantInput) (*services.FieldACLPutResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.put = append(f.put, g...)
	return &services.FieldACLPutResult{}, nil
}

func (f *fakeFieldACL) ListGrants(_ context.Context, _, roleID, entity string) ([]services.FieldACLGrantView, error) {
	return f.list[roleID+"|"+entity], nil
}

func fieldACLPack() *Pack {
	return &Pack{
		Entities: []PackEntity{{
			Name: "crm_leads", Label: "Lead",
			Fields: []PackField{{Name: "sdt", Label: "SĐT", FieldType: "text"}},
		}},
		Roles: []PackRole{{Name: "sales", Label: "Sales", Actions: []PackRoleAction{
			{EntityName: "crm_leads", Action: "view", AccessLevel: "owner"},
		}}},
		FieldACLs: []PackFieldACL{
			{RoleName: "sales", EntityName: "crm_leads", FieldName: "sdt", CanView: false, CanEdit: false},
		},
	}
}

func TestInstall_FieldACL_TravelsWithThePack(t *testing.T) {
	acl := &fakeFieldACL{}
	meta := &fakeMintMeta{}
	svc := NewService(Deps{
		EntityReader: meta, EntityWriter: meta,
		RoleReader: fakeMintRoles{}, RoleWriter: &fakeApprovalRoleWriter{},
		FieldACLWriter: acl,
	})

	res, err := svc.Install(context.Background(), "t-customer", "u1", fieldACLPack())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(acl.put) != 1 {
		t.Fatalf("field restrictions applied = %d; want 1 — the hidden field arrived fully visible", len(acl.put))
	}
	g := acl.put[0]
	if g.EntityName != "crm_leads" || g.FieldName != "sdt" || g.CanView || g.CanEdit {
		t.Errorf("restriction mangled in transit: %+v", g)
	}
	if g.RoleID == "" {
		t.Error("role name did not resolve to an id in the target tenant")
	}
	if res.FieldACLsApplied != 1 {
		t.Errorf("FieldACLsApplied = %d; the operator needs to see that security travelled", res.FieldACLsApplied)
	}
}

func TestInstall_FieldACL_UnwiredWarnsInsteadOfDroppingSilently(t *testing.T) {
	meta := &fakeMintMeta{}
	svc := NewService(Deps{ // no FieldACLWriter
		EntityReader: meta, EntityWriter: meta,
		RoleReader: fakeMintRoles{}, RoleWriter: &fakeApprovalRoleWriter{},
	})

	res, err := svc.Install(context.Background(), "t-customer", "u1", fieldACLPack())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	joined := strings.Join(res.Warnings, " | ")
	if !strings.Contains(joined, "ẩn/khoá field") {
		t.Fatalf("field-level security was dropped with NO warning; warnings = %v", res.Warnings)
	}
}

func TestInstall_FieldACL_UnknownRoleWarnsAndContinues(t *testing.T) {
	acl := &fakeFieldACL{}
	meta := &fakeMintMeta{}
	svc := NewService(Deps{
		EntityReader: meta, EntityWriter: meta,
		RoleReader: fakeMintRoles{}, RoleWriter: &fakeApprovalRoleWriter{},
		FieldACLWriter: acl,
	})

	p := fieldACLPack()
	p.Roles = nil // the rule names a role this pack no longer ships and the target does not have
	res, err := svc.Install(context.Background(), "t-customer", "u1", p)
	if err != nil {
		t.Fatalf("install must not fail over one unapplicable restriction: %v", err)
	}
	if len(acl.put) != 0 {
		t.Errorf("a restriction was applied to an unresolved role: %+v", acl.put)
	}
	if !strings.Contains(strings.Join(res.Warnings, " | "), "không tìm thấy vai") {
		t.Errorf("the skipped restriction was not reported; warnings = %v", res.Warnings)
	}
	var _ = auth.Role{}
}
