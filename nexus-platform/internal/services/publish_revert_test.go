package services

// publish_revert_test.go - specs/063-draft-publish S7-7b (T112): the stale check and the undo step of
// each direct-entry type, over an in-memory revertTargets.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metaversion"
)

type fakeRevert struct {
	later    bool
	role     *RoleView
	rows     []auth.FieldACL
	app      *appAuditDoc
	calls    []string
	recUsers []string
	recFACL  int
}

func (f *fakeRevert) HasLaterDoc(context.Context, string, string, string, uint) (bool, error) {
	return f.later, nil
}
func (f *fakeRevert) RoleView(context.Context, string, string) (*RoleView, bool, error) {
	return f.role, f.role != nil, nil
}
func (f *fakeRevert) UpdateRole(_ context.Context, _, _ string, b RoleView) error {
	f.calls = append(f.calls, "update:"+b.Name)
	return nil
}
func (f *fakeRevert) DeleteRole(_ context.Context, _, id string) error {
	f.calls = append(f.calls, "delete:"+id)
	return nil
}
func (f *fakeRevert) RecreateRole(_ context.Context, _, id string, _ RoleView, facl []auth.FieldACL, users []string) (int, error) {
	f.calls, f.recUsers, f.recFACL = append(f.calls, "recreate:"+id), users, len(facl)
	return len(users), nil
}
func (f *fakeRevert) FieldRows(context.Context, string, string) ([]auth.FieldACL, error) {
	return f.rows, nil
}
func (f *fakeRevert) RestoreFieldRows(_ context.Context, _, id string, _ []auth.FieldACL) error {
	f.calls = append(f.calls, "facl:"+id)
	return nil
}
func (f *fakeRevert) AppSnap(context.Context, string, string) (*appAuditDoc, error) {
	return f.app, nil
}
func (f *fakeRevert) DeleteApp(_ context.Context, _, id string) error {
	f.calls = append(f.calls, "delapp:"+id)
	return nil
}
func (f *fakeRevert) RestoreApp(_ context.Context, _, _ string, s appAuditDoc) error {
	f.calls = append(f.calls, "restoreapp:"+s.Name)
	return nil
}

func js(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func TestPlanRevert(t *testing.T) {
	v1 := RoleView{ID: "r1", Name: "sales", Label: "Sales", Actions: []RoleActionView{{EntityName: "deals", Action: "view", AccessLevel: "all"}}}
	v2 := RoleView{ID: "r1", Name: "sales", Label: "Sales 2", Actions: v1.Actions}
	frow := []auth.FieldACL{{EntityName: "deals", FieldName: "salary", CanView: false}}
	app := appAuditDoc{Name: "crm", Label: "CRM"}
	tests := []struct {
		name      string
		live      fakeRevert
		doc       metaversion.VersionDoc
		wantCode  string // "" = ok
		wantCalls string
	}{
		{"role update ok", fakeRevert{role: &v2}, metaversion.VersionDoc{DocType: "role", DocKey: "r1", Op: "update", BeforeJSON: js(v1), AfterJSON: js(v2)}, "", "update:sales"},
		{"role update stale: edited again", fakeRevert{role: &RoleView{Name: "sales", Label: "Sales 3"}}, metaversion.VersionDoc{DocType: "role", DocKey: "r1", Op: "update", BeforeJSON: js(v1), AfterJSON: js(v2)}, "REVERT_STALE", ""},
		{"role update stale: deleted since", fakeRevert{}, metaversion.VersionDoc{DocType: "role", DocKey: "r1", Op: "update", BeforeJSON: js(v1), AfterJSON: js(v2)}, "REVERT_STALE", ""},
		{"role create undone by delete", fakeRevert{role: &v1}, metaversion.VersionDoc{DocType: "role", DocKey: "r1", Op: "create", AfterJSON: js(v1)}, "", "delete:r1"},
		{"role delete recreates", fakeRevert{}, metaversion.VersionDoc{DocType: "role", DocKey: "r1", Op: "delete", BeforeJSON: js(v1),
			Meta: js(map[string]any{"assignedUserIds": []string{"u1", "u2"}, "fieldAcl": frow})}, "", "recreate:r1"},
		{"role delete stale: deleted again later", fakeRevert{later: true}, metaversion.VersionDoc{DocType: "role", DocKey: "r1", Op: "delete", BeforeJSON: js(v1),
			Meta: js(map[string]any{"assignedUserIds": []string{"u1"}, "fieldAcl": frow})}, "REVERT_STALE", ""},
		{"role delete without recorded meta refused", fakeRevert{}, metaversion.VersionDoc{DocType: "role", DocKey: "r1", Op: "delete", BeforeJSON: js(v1)}, "REVERT_UNSUPPORTED", ""},
		{"app delete stale: deleted again later", fakeRevert{later: true}, metaversion.VersionDoc{DocType: "app", DocKey: "a1", Op: "delete", BeforeJSON: js(app)}, "REVERT_STALE", ""},
		{"role delete stale: id is live again", fakeRevert{role: &v1}, metaversion.VersionDoc{DocType: "role", DocKey: "r1", Op: "delete", BeforeJSON: js(v1)}, "REVERT_STALE", ""},
		{"field acl ok", fakeRevert{rows: frow}, metaversion.VersionDoc{DocType: "field_acl", DocKey: "r1", Op: "update", BeforeJSON: js([]auth.FieldACL{}), AfterJSON: js(frow)}, "", "facl:r1"},
		{"field acl stale", fakeRevert{rows: nil}, metaversion.VersionDoc{DocType: "field_acl", DocKey: "r1", Op: "update", AfterJSON: js(frow)}, "REVERT_STALE", ""},
		{"app create undone by delete", fakeRevert{app: &app}, metaversion.VersionDoc{DocType: "app", DocKey: "a1", Op: "create", AfterJSON: js(app)}, "", "delapp:a1"},
		{"app create stale: renamed", fakeRevert{app: &appAuditDoc{Name: "crm", Label: "Other"}}, metaversion.VersionDoc{DocType: "app", DocKey: "a1", Op: "create", AfterJSON: js(app)}, "REVERT_STALE", ""},
		{"app delete restores", fakeRevert{}, metaversion.VersionDoc{DocType: "app", DocKey: "a1", Op: "delete", BeforeJSON: js(app)}, "", "restoreapp:crm"},
		{"app delete stale: back already", fakeRevert{app: &app}, metaversion.VersionDoc{DocType: "app", DocKey: "a1", Op: "delete", BeforeJSON: js(app)}, "REVERT_STALE", ""},
		{"unknown type", fakeRevert{}, metaversion.VersionDoc{DocType: "workflow", DocKey: "w", Op: "update"}, "REVERT_UNSUPPORTED", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.live
			step, err := planRevert(context.Background(), &f, "t1", 5, tc.doc)
			if tc.wantCode != "" {
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Code != tc.wantCode {
					t.Fatalf("err = %v, want %s", err, tc.wantCode)
				}
				if len(f.calls) != 0 {
					t.Fatalf("planning must write nothing, got %v", f.calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(f.calls) != 0 {
				t.Fatalf("planning wrote before apply: %v", f.calls)
			}
			n, err := step(context.Background())
			if err != nil || len(f.calls) != 1 || f.calls[0] != tc.wantCalls {
				t.Fatalf("calls = %v err = %v, want %s", f.calls, err, tc.wantCalls)
			}
			if tc.doc.Op == "delete" && tc.doc.DocType == "role" && (n != 2 || len(f.recUsers) != 2 || f.recFACL != 1) {
				t.Fatalf("recreate got %d users %v facl %d", n, f.recUsers, f.recFACL)
			}
		})
	}
}

// A role's undo compares grants as a set: the same grants in another order are NOT stale.
func TestRoleSigIgnoresGrantOrder(t *testing.T) {
	a := RoleView{Name: "r", Actions: []RoleActionView{{"x", "view", "all"}, {"y", "edit", "own"}}}
	b := RoleView{Name: "r", Actions: []RoleActionView{{"y", "edit", "own"}, {"x", "view", "all"}}}
	if roleSig(a) != roleSig(b) {
		t.Fatal("grant order changed the signature")
	}
	b.Actions[0].AccessLevel = "all"
	if roleSig(a) == roleSig(b) {
		t.Fatal("a changed grant kept the signature")
	}
}
