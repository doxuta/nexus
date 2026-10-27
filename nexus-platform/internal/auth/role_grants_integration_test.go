//go:build integration

// RoleRepository.ListRoleGrants against a real MySQL (issue #67): list_roles {grants: true} reads every
// role of the tenant with its grants in one query, so a role deleted meanwhile is absent instead of
// failing the read.
//
//	bash scripts/run-integration.sh -run 'TestListRoleGrants_' ./internal/auth/
package auth

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func TestListRoleGrants_EveryRoleOfTheTenantWithItsGrantsInOrder(t *testing.T) {
	ctx := context.Background()
	f := manageSetup(t)
	repo := NewRoleRepository(f.db)
	role := func(tenant, name string, grants ...[3]string) string {
		t.Helper()
		id := uuid.NewString()
		if err := f.db.Exec(`INSERT INTO acl_roles (id, tenant_id, name, label) VALUES (?, ?, ?, ?)`, id, tenant, name, "L "+name).Error; err != nil {
			t.Fatalf("seed role %s: %v", name, err)
		}
		for _, g := range grants {
			if err := f.db.Exec(`INSERT INTO acl_role_actions (id, role_id, entity_name, action, access_level) VALUES (?, ?, ?, ?, ?)`,
				uuid.NewString(), id, g[0], g[1], g[2]).Error; err != nil {
				t.Fatalf("seed grant %v: %v", g, err)
			}
		}
		return id
	}
	// Inserted out of order: the read sorts roles by name and grants by entity, then action.
	rep := role(f.tenant, "rg_rep", [3]string{"tickets", "view", "owner"}, [3]string{"contacts", "view", "all"}, [3]string{"contacts", "edit", "owner"})
	empty := role(f.tenant, "rg_empty")
	star := role(f.tenant, "rg_all", [3]string{"*", "view", "all"})
	gone := role(f.tenant, "rg_gone", [3]string{"contacts", "delete", "all"})
	role(f.other, "rg_foreign", [3]string{"contacts", "delete", "all"}) // another tenant's role and grant
	if err := repo.DeleteRole(ctx, f.tenant, gone); err != nil {
		t.Fatalf("delete: %v", err)
	}

	got, err := repo.ListRoleGrants(ctx, f.tenant)
	if err != nil {
		t.Fatalf("ListRoleGrants: %v", err)
	}
	type view struct {
		ID, Name, Label string
		Grants          [][3]string
	}
	views := make([]view, len(got))
	for i, g := range got {
		if g.Role.TenantID != f.tenant {
			t.Fatalf("role %q carries tenant %q; want %q", g.Role.Name, g.Role.TenantID, f.tenant)
		}
		views[i] = view{ID: g.Role.ID, Name: g.Role.Name, Label: g.Role.Label, Grants: [][3]string{}}
		for _, a := range g.Actions {
			if a.RoleID != g.Role.ID || a.ID == "" {
				t.Fatalf("grant %+v of role %q: want its own id and the role's id", a, g.Role.Name)
			}
			views[i].Grants = append(views[i].Grants, [3]string{a.EntityName, a.Action, a.AccessLevel})
		}
	}
	want := []view{
		{ID: star, Name: "rg_all", Label: "L rg_all", Grants: [][3]string{{"*", "view", "all"}}},
		{ID: empty, Name: "rg_empty", Label: "L rg_empty", Grants: [][3]string{}},
		{ID: rep, Name: "rg_rep", Label: "L rg_rep", Grants: [][3]string{{"contacts", "edit", "owner"}, {"contacts", "view", "all"}, {"tickets", "view", "owner"}}},
	}
	if !reflect.DeepEqual(views, want) {
		t.Fatalf("roles = %+v\nwant  %+v (this tenant only, the deleted role absent, sorted)", views, want)
	}
	if got[1].Actions == nil {
		t.Fatal("a role without a grant has nil Actions; want an empty list")
	}

	none, err := repo.ListRoleGrants(ctx, uuid.NewString())
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("a tenant without roles = %#v, %v; want an empty, non-nil list", none, err)
	}
}
