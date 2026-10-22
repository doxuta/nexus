package services

// Issue #23: a role's audit rows name the role. An update diff keeps only the changed keys, so a
// grants-only change (the AI's access card) used to leave a __role__ row with record_id NULL and no
// name: the audit log alone could not say which role changed.

import (
	"context"
	"testing"

	"nexus/platform/internal/audit"
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
