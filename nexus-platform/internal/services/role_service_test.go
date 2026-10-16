package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/auth"
)

// fakeRoleRepo is an in-memory stand-in for the role repository. Roles are keyed by
// (tenantID, id) and grants by roleID, so tenant-scoping (R2) and the grant lifecycle can be
// exercised without a database. It mirrors the real repo's typed errors.
type fakeRoleRepo struct {
	roles   map[string]*auth.Role        // key: tenantID\x00id
	actions map[string][]auth.RoleAction // key: roleID
}

func newFakeRoleRepo() *fakeRoleRepo {
	return &fakeRoleRepo{
		roles:   map[string]*auth.Role{},
		actions: map[string][]auth.RoleAction{},
	}
}

func (f *fakeRoleRepo) key(tenantID, id string) string { return tenantID + "\x00" + id }

func (f *fakeRoleRepo) ListRoles(_ context.Context, tenantID string) ([]auth.Role, error) {
	var out []auth.Role
	for _, r := range f.roles {
		if r.TenantID == tenantID {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *fakeRoleRepo) GetRoleByID(_ context.Context, tenantID, id string) (*auth.Role, error) {
	if r, ok := f.roles[f.key(tenantID, id)]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, auth.ErrRoleNotFound
}

func (f *fakeRoleRepo) CreateRole(_ context.Context, role *auth.Role, actions []auth.RoleAction) error {
	for _, ex := range f.roles {
		if ex.TenantID == role.TenantID && ex.Name == role.Name {
			return auth.ErrRoleAlreadyExists
		}
	}
	cp := *role
	f.roles[f.key(role.TenantID, role.ID)] = &cp
	for i := range actions {
		actions[i].RoleID = role.ID
	}
	f.actions[role.ID] = append([]auth.RoleAction(nil), actions...)
	return nil
}

func (f *fakeRoleRepo) UpdateRole(_ context.Context, tenantID, id string, fields map[string]any, actions []auth.RoleAction) (*auth.Role, error) {
	r, ok := f.roles[f.key(tenantID, id)]
	if !ok {
		return nil, auth.ErrRoleNotFound
	}
	if v, ok := fields["name"].(string); ok {
		// enforce (tenant,name) uniqueness on rename
		for _, ex := range f.roles {
			if ex.TenantID == tenantID && ex.Name == v && ex.ID != id {
				return nil, auth.ErrRoleAlreadyExists
			}
		}
		r.Name = v
	}
	if v, ok := fields["label"].(string); ok {
		r.Label = v
	}
	if actions != nil {
		for i := range actions {
			actions[i].RoleID = id
		}
		f.actions[id] = append([]auth.RoleAction(nil), actions...)
	}
	cp := *r
	return &cp, nil
}

func (f *fakeRoleRepo) DeleteRole(_ context.Context, tenantID, id string) error {
	k := f.key(tenantID, id)
	if _, ok := f.roles[k]; !ok {
		return auth.ErrRoleNotFound
	}
	delete(f.roles, k)
	delete(f.actions, id) // mirror the FK cascade on acl_role_actions
	return nil
}

func (f *fakeRoleRepo) ListActionsForRole(_ context.Context, tenantID, roleID string) ([]auth.RoleAction, error) {
	if _, ok := f.roles[f.key(tenantID, roleID)]; !ok {
		return nil, auth.ErrRoleNotFound
	}
	return append([]auth.RoleAction(nil), f.actions[roleID]...), nil
}

func newRoleSvc() (*RoleService, *fakeRoleRepo) {
	repo := newFakeRoleRepo()
	svc := NewRoleService(repo)
	n := 0
	svc.newID = func() string {
		n++
		return "rid-" + string(rune('0'+n))
	}
	return svc, repo
}

// R1: create a role with grants, then it is gettable with its actions.
func TestRole_CreateAndGet(t *testing.T) {
	svc, _ := newRoleSvc()
	view, err := svc.CreateRole(context.Background(), "t1", CreateRoleInput{
		Name:  "sales",
		Label: "Sales",
		Actions: []RoleActionInput{
			{EntityName: "contacts", Action: "view"},
			{EntityName: "contacts", Action: "edit", AccessLevel: "owner"},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if view.Name != "sales" || len(view.Actions) != 2 {
		t.Fatalf("unexpected view: %+v", view)
	}
	got, err := svc.GetRole(context.Background(), "t1", view.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != view.ID || len(got.Actions) != 2 {
		t.Fatalf("get returned %+v", got)
	}
	// access_level default is applied where omitted.
	for _, a := range got.Actions {
		if a.AccessLevel == "" {
			t.Fatalf("access_level should default, got empty for %+v", a)
		}
	}
}

// R1: an invalid action verb is rejected (only view/create/edit/delete are grantable).
func TestRole_CreateRejectsInvalidAction(t *testing.T) {
	svc, _ := newRoleSvc()
	_, err := svc.CreateRole(context.Background(), "t1", CreateRoleInput{
		Name:    "bad",
		Label:   "Bad",
		Actions: []RoleActionInput{{EntityName: "contacts", Action: "drop"}},
	})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "ROLE_ACTION_INVALID" {
		t.Fatalf("err = %v, want ROLE_ACTION_INVALID", err)
	}
}

// R1: an invalid access_level is rejected (none/owner/team/all only).
func TestRole_CreateRejectsInvalidAccessLevel(t *testing.T) {
	svc, _ := newRoleSvc()
	_, err := svc.CreateRole(context.Background(), "t1", CreateRoleInput{
		Name:    "bad",
		Label:   "Bad",
		Actions: []RoleActionInput{{EntityName: "contacts", Action: "view", AccessLevel: "galaxy"}},
	})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "ROLE_ACCESS_LEVEL_INVALID" {
		t.Fatalf("err = %v, want ROLE_ACCESS_LEVEL_INVALID", err)
	}
}

// R1: missing name is a validation error.
func TestRole_CreateRequiresName(t *testing.T) {
	svc, _ := newRoleSvc()
	_, err := svc.CreateRole(context.Background(), "t1", CreateRoleInput{Label: "x"})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "ROLE_NAME_REQUIRED" {
		t.Fatalf("err = %v, want ROLE_NAME_REQUIRED", err)
	}
}

// R1: duplicate (tenant, name) is a 409.
func TestRole_CreateDuplicateConflict(t *testing.T) {
	svc, _ := newRoleSvc()
	in := CreateRoleInput{Name: "dup", Label: "Dup"}
	if _, err := svc.CreateRole(context.Background(), "t1", in); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := svc.CreateRole(context.Background(), "t1", in)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "ROLE_ALREADY_EXISTS" {
		t.Fatalf("err = %v, want ROLE_ALREADY_EXISTS", err)
	}
}

// R1: update replaces label and grants; passing nil Actions leaves grants untouched.
func TestRole_UpdateReplacesGrants(t *testing.T) {
	svc, _ := newRoleSvc()
	created, _ := svc.CreateRole(context.Background(), "t1", CreateRoleInput{
		Name: "r", Label: "R", Actions: []RoleActionInput{{EntityName: "contacts", Action: "view"}},
	})
	newLabel := "Renamed"
	view, err := svc.UpdateRole(context.Background(), "t1", created.ID, UpdateRoleInput{
		Label:   &newLabel,
		Actions: []RoleActionInput{{EntityName: "*", Action: "view"}, {EntityName: "*", Action: "edit"}},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if view.Label != "Renamed" || len(view.Actions) != 2 {
		t.Fatalf("unexpected view after update: %+v", view)
	}
}

// R1: delete removes the role; a subsequent get is not-found.
func TestRole_Delete(t *testing.T) {
	svc, _ := newRoleSvc()
	created, _ := svc.CreateRole(context.Background(), "t1", CreateRoleInput{Name: "r", Label: "R"})
	if err := svc.DeleteRole(context.Background(), "t1", created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err := svc.GetRole(context.Background(), "t1", created.ID)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "ROLE_NOT_FOUND" {
		t.Fatalf("get after delete: err = %v, want ROLE_NOT_FOUND", err)
	}
}

// R2: a role created in tenant t1 is invisible / not-editable / not-deletable from tenant t2.
func TestRole_TenantScoped(t *testing.T) {
	svc, _ := newRoleSvc()
	created, _ := svc.CreateRole(context.Background(), "t1", CreateRoleInput{Name: "r", Label: "R"})

	// Get from another tenant → not-found.
	if _, err := svc.GetRole(context.Background(), "t2", created.ID); err == nil {
		t.Fatalf("cross-tenant get should fail")
	}
	// Update from another tenant → not-found.
	lbl := "x"
	if _, err := svc.UpdateRole(context.Background(), "t2", created.ID, UpdateRoleInput{Label: &lbl}); err == nil {
		t.Fatalf("cross-tenant update should fail")
	}
	// Delete from another tenant → not-found.
	if err := svc.DeleteRole(context.Background(), "t2", created.ID); err == nil {
		t.Fatalf("cross-tenant delete should fail")
	}
	// List from another tenant → empty.
	roles, err := svc.ListRoles(context.Background(), "t2")
	if err != nil {
		t.Fatalf("list t2: %v", err)
	}
	if len(roles) != 0 {
		t.Fatalf("tenant t2 sees %d roles; want 0", len(roles))
	}
}

// R3: wildcard entity is allowed in a grant ('*' = every entity).
func TestRole_WildcardGrantAccepted(t *testing.T) {
	svc, _ := newRoleSvc()
	view, err := svc.CreateRole(context.Background(), "t1", CreateRoleInput{
		Name: "super", Label: "Super",
		Actions: []RoleActionInput{{EntityName: "*", Action: "view"}},
	})
	if err != nil {
		t.Fatalf("create with wildcard: %v", err)
	}
	if len(view.Actions) != 1 || view.Actions[0].EntityName != "*" {
		t.Fatalf("wildcard grant not stored: %+v", view.Actions)
	}
}

// R1: an empty entity_name in a grant is rejected.
func TestRole_CreateRejectsEmptyEntity(t *testing.T) {
	svc, _ := newRoleSvc()
	_, err := svc.CreateRole(context.Background(), "t1", CreateRoleInput{
		Name: "r", Label: "R", Actions: []RoleActionInput{{EntityName: "", Action: "view"}},
	})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "ROLE_ENTITY_REQUIRED" {
		t.Fatalf("err = %v, want ROLE_ENTITY_REQUIRED", err)
	}
}
