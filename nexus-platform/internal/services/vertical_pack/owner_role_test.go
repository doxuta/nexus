package vertical_pack

// specs/065 lát D (T074b, D-15): the customer-owner role belongs to the workspace, never to an app.
// Export leaves it (and its wildcard grants) out with a warning; Install neither creates it nor
// grants to it, and no other pack item can resolve to the tenant's real owner role.

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

type ownerRoleReader struct{}

func (ownerRoleReader) ListRoles(_ context.Context, tenantID string) ([]auth.Role, error) {
	return []auth.Role{
		{ID: "r-owner", TenantID: tenantID, Name: auth.OwnerRoleName, Label: "Chủ khách"},
		{ID: "r-sales", TenantID: tenantID, Name: "sales", Label: "Sales"},
	}, nil
}

func (ownerRoleReader) ListActionsForRole(_ context.Context, _, roleID string) ([]auth.RoleAction, error) {
	if roleID == "r-owner" {
		return []auth.RoleAction{{RoleID: roleID, EntityName: "*", Action: "view", AccessLevel: "all"}}, nil
	}
	return []auth.RoleAction{{RoleID: roleID, EntityName: "own_orders", Action: "view", AccessLevel: "all"}}, nil
}

func TestExport_LeavesTheOwnerRoleOut(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMintMeta{}
	if _, _, err := meta.CreateEntity(ctx, "tenant-1", services.CreateEntityInput{Name: "own_orders", Label: "Orders"}); err != nil {
		t.Fatal(err)
	}
	exporter := NewService(Deps{EntityReader: meta, LayoutReader: fakeApLayouts{}, RoleReader: ownerRoleReader{}})
	pack, warnings, err := exporter.Export(ctx, "tenant-1", []string{"own_orders"}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Roles) != 1 || pack.Roles[0].Name != "sales" {
		t.Fatalf("exported roles = %+v, want only sales", pack.Roles)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "Bỏ vai hệ thống") {
		t.Fatalf("warnings = %q, want the owner-role drop to be said out loud", joined)
	}
	if strings.Contains(joined, "WILDCARD") {
		t.Fatalf("the owner role's wildcard must not even reach the wildcard warning: %q", joined)
	}
}

func TestInstall_IgnoresAPackedOwnerRole(t *testing.T) {
	roleWr := &fakeApprovalRoleWriter{}
	meta := &fakeMintMeta{}
	svc := NewService(Deps{EntityReader: meta, EntityWriter: meta, RoleReader: ownerRoleReader{}, RoleWriter: roleWr})
	pack := &Pack{
		Entities: []PackEntity{{Name: "own_orders", Label: "Orders", Fields: []PackField{{Name: "ten", Label: "Tên", FieldType: "text"}}}},
		Roles: []PackRole{
			{Name: auth.OwnerRoleName, Label: "Hand-written", Actions: []PackRoleAction{{EntityName: "own_orders", Action: "delete", AccessLevel: "all"}}},
			{Name: "Tenant_Owner", Label: "Case variant"},
			{Name: "support", Label: "Support"},
		},
		FieldACLs: []PackFieldACL{{RoleName: auth.OwnerRoleName, EntityName: "own_orders", FieldName: "ten", CanView: false}},
	}
	res, err := svc.Install(context.Background(), "tenant-1", "u1", pack)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(res.RolesCreated) != 1 || res.RolesCreated[0] != "support" {
		t.Fatalf("RolesCreated = %v, want only support", res.RolesCreated)
	}
	for _, r := range roleWr.existing {
		if auth.IsOwnerRoleName(r.Name) {
			t.Fatalf("install wrote the owner role %+v", r)
		}
	}
	warned := 0
	for _, w := range res.Warnings {
		if strings.Contains(w, "vai hệ thống") {
			warned++
		}
	}
	if warned != 2 {
		t.Fatalf("warnings = %v, want one per ignored owner-role entry (2)", res.Warnings)
	}
}
