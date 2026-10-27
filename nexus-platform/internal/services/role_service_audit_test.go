package services

// Issue #23: a role's audit rows name the role. An update diff keeps only the changed keys, so a
// grants-only change (the AI's access card) used to leave a __role__ row with record_id NULL and no
// name: the audit log alone could not say which role changed.

import (
	"context"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
)

func TestRole_AuditRowsNameTheRole(t *testing.T) {
	ctx := context.Background()
	svc, _ := newRoleSvc()
	cap := &captureAuditor{}
	svc.WithAuditor(cap)
	created, err := svc.CreateRole(ctx, "t1", CreateRoleInput{Name: "sales", Label: "Sales", Actions: []RoleActionInput{{EntityName: "contacts", Action: "view"}}})
	if err != nil {
		t.Fatal(err)
	}
	// Grants only, as the access card's apply writes it: the name is resent unchanged.
	name, label := "sales", "Sales"
	if _, err := svc.UpdateRole(ctx, "t1", created.ID, UpdateRoleInput{Name: &name, Label: &label,
		Actions: []RoleActionInput{{EntityName: "contacts", Action: "view"}, {EntityName: "tickets", Action: "view"}}}); err != nil {
		t.Fatal(err)
	}
	// Nothing changes at all: the row still says which role was saved.
	if _, err := svc.UpdateRole(ctx, "t1", created.ID, UpdateRoleInput{Label: &label}); err != nil {
		t.Fatal(err)
	}
	renamed := "sales_team"
	if _, err := svc.UpdateRole(ctx, "t1", created.ID, UpdateRoleInput{Name: &renamed}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteRole(ctx, "t1", created.ID); err != nil {
		t.Fatal(err)
	}

	if len(cap.events) != 5 {
		t.Fatalf("events = %d; want create, three updates, delete", len(cap.events))
	}
	for i, e := range cap.events {
		if e.EntityName != audit.RoleMeta || e.RecordID != created.ID {
			t.Errorf("event %d (%s) = %s/%q; want __role__ with record_id = the role's id %q", i, e.Action, e.EntityName, e.RecordID, created.ID)
		}
	}
	for i, want := range [][2]string{{"sales", "sales"}, {"sales", "sales"}, {"sales", "sales_team"}} {
		d := cap.events[1+i].Diff
		if d == nil || d.Before["name"] != want[0] || d.After["name"] != want[1] {
			t.Errorf("update %d diff = %+v; want name %q -> %q", i+1, d, want[0], want[1])
		}
	}
	if d := cap.events[1].Diff; d.After["actions"] == nil {
		t.Errorf("grants-only update diff = %+v; want the changed actions kept", d)
	}
}

// RecreateRole makes the in-memory repo a roleRecreator: the role and its grants come back under their id.
func (f *fakeRoleRepo) RecreateRole(ctx context.Context, role *auth.Role, actions []auth.RoleAction, _ []auth.FieldACL, _ []string) (int64, error) {
	return 0, f.CreateRole(ctx, role, actions)
}

// Issue #67: Recreate (the undo of a role delete, publish revert) writes its create row on the role's
// own id too, so GET /audit-log?entityName=__role__&recordId=<id> keeps the whole life of the role.
func TestRole_RecreateAuditRowCarriesTheRoleID(t *testing.T) {
	ctx := context.Background()
	svc, repo := newRoleSvc()
	svc.WithRecreator(repo)
	cap := &captureAuditor{}
	svc.WithAuditor(cap)
	before := RoleView{ID: "rid-gone", TenantID: "t1", Name: "sales", Label: "Sales",
		Actions: []RoleActionView{{EntityName: "contacts", Action: "view", AccessLevel: "all"}}}
	if _, err := svc.Recreate(ctx, "t1", "rid-gone", before, nil, nil); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if len(cap.events) != 1 {
		t.Fatalf("events = %d; want the one create row", len(cap.events))
	}
	e := cap.events[0]
	if e.TenantID != "t1" || e.EntityName != audit.RoleMeta || e.Action != audit.ActionCreate || e.RecordID != "rid-gone" {
		t.Fatalf("event = %s %s/%q in %q; want create __role__ with record_id = the role's id \"rid-gone\" in t1", e.Action, e.EntityName, e.RecordID, e.TenantID)
	}
	if e.Diff == nil || e.Diff.After["name"] != "sales" {
		t.Errorf("diff = %+v; want the recreated role's name", e.Diff)
	}
}
