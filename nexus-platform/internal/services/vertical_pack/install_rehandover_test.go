package vertical_pack

// RE-HANDOVER: a pack installed into a tenant that already has the role.
//
// Pass 6 skipped an existing role by NAME, which was right for the role and wrong for its GRANTS.
// On delivery 2+ the role is already there, so every entity shipped in THIS delivery got no grant
// on it, and the new tables were invisible to every non-admin in the customer tenant. It stayed
// hidden because the person checking is is_admin and is_admin bypasses all ACL — the wall only
// exists for the third person.
//
// The reconcile must be ADDITIVE. UpdateRole replaces the grant set, so a blind write during a
// re-handover would destroy grants the customer tuned by hand. A pack extends a live tenant; it
// never overwrites one.

import (
	"context"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// existingRoleReader is the PLAN's view of the tenant's roles (a different port from roleWriter —
// buildPlan decides create-vs-skip from roleRd, while the install writes through roleWr).
type existingRoleReader struct{ roles []auth.Role }

func (r existingRoleReader) ListRoles(context.Context, string) ([]auth.Role, error) {
	return r.roles, nil
}
func (r existingRoleReader) ListActionsForRole(context.Context, string, string) ([]auth.RoleAction, error) {
	return nil, nil
}

func packWithRole(entity string, actions ...PackRoleAction) *Pack {
	return &Pack{
		Entities: []PackEntity{{
			Name: entity, Label: entity,
			Fields: []PackField{{Name: "ten", Label: "Tên", FieldType: "text"}},
		}},
		Roles: []PackRole{{Name: "sales", Label: "Sales", Actions: actions}},
	}
}

func TestInstall_ReHandover_AddsGrantsToAnExistingRole(t *testing.T) {
	roleWr := &fakeApprovalRoleWriter{
		// Delivery 1 already happened: the role exists and covers only the first entity.
		existing: []services.RoleView{{
			ID: "role-1", TenantID: "t1", Name: "sales", Label: "Sales",
			Actions: []services.RoleActionView{
				{EntityName: "crm_accounts", Action: "view", AccessLevel: "owner"},
			},
		}},
	}
	meta := &fakeMintMeta{}
	svc := NewService(Deps{
		EntityReader: meta, EntityWriter: meta,
		RoleReader: existingRoleReader{roles: []auth.Role{{ID: "role-1", TenantID: "t1", Name: "sales"}}},
		RoleWriter: roleWr,
	})

	// Delivery 2 ships a NEW entity and asks for a grant on it.
	pack := packWithRole("crm_contracts",
		PackRoleAction{EntityName: "crm_accounts", Action: "view", AccessLevel: "all"}, // already held
		PackRoleAction{EntityName: "crm_contracts", Action: "view", AccessLevel: "owner"},
		PackRoleAction{EntityName: "crm_contracts", Action: "edit", AccessLevel: "owner"},
	)

	res, err := svc.Install(context.Background(), "t1", "u1", pack)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(res.RolesSkipped) != 1 {
		t.Fatalf("RolesSkipped = %v; the role should still be reported as pre-existing", res.RolesSkipped)
	}

	got := roleWr.existing[0].Actions
	index := map[string]string{}
	for _, a := range got {
		index[a.EntityName+"|"+a.Action] = a.AccessLevel
	}

	// The whole point: the entity shipped in THIS delivery is now reachable by a non-admin.
	if index["crm_contracts|view"] != "owner" || index["crm_contracts|edit"] != "owner" {
		t.Fatalf("new entity's grants missing after re-handover: %+v", got)
	}
	// And the tenant's own level was NOT narrowed or widened by the pack.
	if index["crm_accounts|view"] != "owner" {
		t.Errorf("crm_accounts|view = %q; want the tenant's existing \"owner\" — a pack must not rewrite a grant the customer already has",
			index["crm_accounts|view"])
	}
	if len(res.RoleGrantsAdded) != 1 {
		t.Errorf("RoleGrantsAdded = %v; the operator needs to be told the new tables became visible", res.RoleGrantsAdded)
	}
}

func TestInstall_ReHandover_NothingToAddIsSilent(t *testing.T) {
	roleWr := &fakeApprovalRoleWriter{
		existing: []services.RoleView{{
			ID: "role-1", TenantID: "t1", Name: "sales", Label: "Sales",
			Actions: []services.RoleActionView{
				{EntityName: "crm_contracts", Action: "view", AccessLevel: "team"},
			},
		}},
	}
	meta := &fakeMintMeta{}
	svc := NewService(Deps{
		EntityReader: meta, EntityWriter: meta,
		RoleReader: existingRoleReader{roles: []auth.Role{{ID: "role-1", TenantID: "t1", Name: "sales"}}},
		RoleWriter: roleWr,
	})

	res, err := svc.Install(context.Background(), "t1", "u1",
		packWithRole("crm_contracts", PackRoleAction{EntityName: "crm_contracts", Action: "view", AccessLevel: "owner"}))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(res.RoleGrantsAdded) != 0 {
		t.Errorf("RoleGrantsAdded = %v; a re-run that adds nothing must say nothing", res.RoleGrantsAdded)
	}
	// The customer's WIDER level survives untouched — this is the narrowing guard.
	if lvl := roleWr.existing[0].Actions[0].AccessLevel; lvl != "team" {
		t.Errorf("access level = %q; want \"team\" — the pack narrowed a grant the tenant already had", lvl)
	}
}
