package ai

// explain_access tests: the decision-chain probe composes EXISTING service seams (user lookup,
// ACLService.Authorize, sys_field_acl rows, RecordService.Get) — these fakes prove the chain is
// narrated from the ENGINE's answers (probe results), tenant-pinned everywhere, and degrades
// honestly (ACCESS_EXPLAIN_UNAVAILABLE) when the probes are not wired.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/services"
)

type fakeAccessUsers struct {
	byKey    map[string]*auth.User  // key: tenantID+"/"+email or +id
	byTenant map[string][]auth.User // PrivilegedHolders (propose_access)
	asked    [][]string             // the roleIDs of each PrivilegedHolders call
}

// PrivilegedHolders filters as the SQL does (tenant, role in roleIDs, admin or operator) and records
// which roles were asked about.
func (f *fakeAccessUsers) PrivilegedHolders(_ context.Context, tenantID string, roleIDs []string) ([]auth.User, error) {
	f.asked = append(f.asked, roleIDs)
	var out []auth.User
	for _, u := range f.byTenant[tenantID] {
		if u.RoleID != nil && slices.Contains(roleIDs, *u.RoleID) && (u.IsAdmin || u.IsPlatformOperator) {
			out = append(out, u)
		}
	}
	return out, nil
}

func (f *fakeAccessUsers) GetUserByEmail(_ context.Context, tenantID, email string) (*auth.User, error) {
	if u, ok := f.byKey[tenantID+"/"+email]; ok {
		return u, nil
	}
	return nil, auth.ErrUserNotFound
}

func (f *fakeAccessUsers) GetUserByID(_ context.Context, tenantID, id string) (*auth.User, error) {
	if u, ok := f.byKey[tenantID+"/"+id]; ok {
		return u, nil
	}
	return nil, auth.ErrUserNotFound
}

type fakeAccessRoles struct {
	byID     map[string]*auth.Role
	byTenant map[string][]auth.Role       // ListRoles (propose_access)
	actions  map[string][]auth.RoleAction // role id -> grants
}

func (f *fakeAccessRoles) GetRoleByID(_ context.Context, _, id string) (*auth.Role, error) {
	if r, ok := f.byID[id]; ok {
		return r, nil
	}
	return nil, auth.ErrRoleNotFound
}

func (f *fakeAccessRoles) ListRoles(_ context.Context, tenantID string) ([]auth.Role, error) {
	return f.byTenant[tenantID], nil
}

func (f *fakeAccessRoles) ListActionsForRole(_ context.Context, tenantID, roleID string) ([]auth.RoleAction, error) {
	for _, r := range f.byTenant[tenantID] {
		if r.ID == roleID {
			return f.actions[roleID], nil
		}
	}
	return nil, auth.ErrRoleNotFound
}

type fakeAccessACL struct {
	allowed map[string]bool   // action -> allowed
	level   map[string]string // action -> access level
}

func (f *fakeAccessACL) Authorize(_ context.Context, _, _, _, action string) (bool, string, error) {
	return f.allowed[action], f.level[action], nil
}

type fakeAccessFieldACL struct{ rows []auth.FieldACL }

func (f *fakeAccessFieldACL) LoadFieldACLForEntity(_ context.Context, _, _, _ string) ([]auth.FieldACL, error) {
	return f.rows, nil
}

// fakeAccessRecords scripts the two probes: userSees controls the target-user-scope Get;
// exists+owner control the admin-scope Get.
type fakeAccessRecords struct {
	userSees bool
	exists   bool
	owner    string
}

func (f *fakeAccessRecords) Get(_ context.Context, scope services.RecordScope, _, _, id string, _, _ []string) (*dynrepo.Record, error) {
	if scope.IsAdmin {
		if f.exists {
			return &dynrepo.Record{ID: id, CreatedBy: f.owner}, nil
		}
		return nil, services.ErrRecordNotFound
	}
	if f.userSees {
		return &dynrepo.Record{ID: id, CreatedBy: f.owner}, nil
	}
	return nil, services.ErrRecordNotFound
}

func rolePtr(s string) *string { return &s }

func explainProbes(users *fakeAccessUsers, acl *fakeAccessACL, facl *fakeAccessFieldACL, recs *fakeAccessRecords) *AccessProbes {
	return &AccessProbes{
		Users:    users,
		Roles:    &fakeAccessRoles{byID: map[string]*auth.Role{"r1": {ID: "r1", Name: "sales_rep", Label: "Nhân viên sales"}}},
		ACL:      acl,
		FieldACL: facl,
		Records:  recs,
	}
}

// explainExec builds a tenant-A executor whose reader carries the probes (the production
// ReaderWithAccessProbes composite — tested here on purpose).
func explainExec(p *AccessProbes) *toolExecutor {
	return &toolExecutor{reader: ReaderWithAccessProbes(twoTenantReader(), p), tenantID: "tenant-A"}
}

func runExplain(t *testing.T, e *toolExecutor, args string) map[string]any {
	t.Helper()
	res := e.execute(context.Background(), "explain_access", args)
	ok, out := parseToolResult(t, res)
	if !ok {
		t.Fatalf("explain_access expected ok, got %s", res)
	}
	result, _ := out["result"].(map[string]any)
	if result == nil {
		t.Fatalf("explain_access result missing: %s", res)
	}
	return result
}

func TestExplainAccess_UnwiredProbesDegradeHonestly(t *testing.T) {
	// A plain reader (no AccessProbeCarrier) must yield the honest unavailable code, never a
	// half-made-up explanation.
	e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
	res := e.execute(context.Background(), "explain_access", `{"userEmail": "a@x.vn", "entity": "patients"}`)
	if ok, out := parseToolResult(t, res); ok || !strings.Contains(out["error"].(string), "ACCESS_EXPLAIN_UNAVAILABLE") {
		t.Fatalf("expected ACCESS_EXPLAIN_UNAVAILABLE, got %s", res)
	}
}

func TestExplainAccess_ArgAndLookupErrors(t *testing.T) {
	users := &fakeAccessUsers{byKey: map[string]*auth.User{
		"tenant-A/rep@example.com": {ID: "u1", Email: "rep@example.com", Status: "active", RoleID: rolePtr("r1")},
		// Same email exists in tenant-B ONLY — tenant-A lookups must not see it.
		"tenant-B/b@example.com": {ID: "uB", Email: "b@example.com", Status: "active"},
	}}
	e := explainExec(explainProbes(users, &fakeAccessACL{allowed: map[string]bool{}, level: map[string]string{}}, &fakeAccessFieldACL{}, &fakeAccessRecords{}))

	for _, tc := range []struct{ name, args, wantCode string }{
		{"neither user key", `{"entity": "patients"}`, "ACCESS_USER_REQUIRED"},
		{"unknown entity", `{"userEmail": "rep@example.com", "entity": "ghosts"}`, "ENTITY_NOT_FOUND"},
		{"cross-tenant entity", `{"userEmail": "rep@example.com", "entity": "invoices"}`, "ENTITY_NOT_FOUND"},
		{"user of another tenant invisible", `{"userEmail": "b@example.com", "entity": "patients"}`, "ACCESS_USER_NOT_FOUND"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := e.execute(context.Background(), "explain_access", tc.args)
			if ok, out := parseToolResult(t, res); ok || !strings.Contains(out["error"].(string), tc.wantCode) {
				t.Fatalf("expected %s, got %s", tc.wantCode, res)
			}
		})
	}
}

func TestExplainAccess_AdminBypass(t *testing.T) {
	users := &fakeAccessUsers{byKey: map[string]*auth.User{
		"tenant-A/boss@example.com": {ID: "u9", Email: "boss@example.com", Status: "active", IsAdmin: true},
	}}
	// ACL/fieldACL fakes would deny everything — admin must never even consult them.
	e := explainExec(explainProbes(users, &fakeAccessACL{allowed: map[string]bool{}, level: map[string]string{}}, &fakeAccessFieldACL{rows: []auth.FieldACL{{FieldName: "salary", CanView: false}}}, &fakeAccessRecords{exists: true, owner: "someone"}))
	out := runExplain(t, e, `{"userEmail": "boss@example.com", "entity": "patients", "recordId": "rec-1"}`)

	if v, _ := out["verdict"].(string); !strings.Contains(v, "ADMIN") {
		t.Fatalf("expected admin verdict, got %v", out["verdict"])
	}
	facl, _ := out["fieldAcl"].(map[string]any)
	if bypassed, _ := facl["bypassed"].(bool); !bypassed {
		t.Fatalf("admin must report fieldAcl bypassed, got %v", out["fieldAcl"])
	}
	rec, _ := out["record"].(map[string]any)
	if visible, _ := rec["visible"].(bool); !visible {
		t.Fatalf("admin must see the record, got %v", out["record"])
	}
}

func TestExplainAccess_NoRoleIsDenyAll(t *testing.T) {
	users := &fakeAccessUsers{byKey: map[string]*auth.User{
		"tenant-A/new@example.com": {ID: "u2", Email: "new@example.com", Status: "active"}, // RoleID nil
	}}
	e := explainExec(explainProbes(users, &fakeAccessACL{allowed: map[string]bool{"view": true}, level: map[string]string{"view": "all"}}, &fakeAccessFieldACL{}, &fakeAccessRecords{}))
	out := runExplain(t, e, `{"userEmail": "new@example.com", "entity": "patients"}`)

	if v, _ := out["verdict"].(string); !strings.Contains(v, "chưa có role") {
		t.Fatalf("expected no-role verdict, got %v", out["verdict"])
	}
	actions, _ := out["actions"].(map[string]any)
	view, _ := actions["view"].(map[string]any)
	if allowed, _ := view["allowed"].(bool); allowed {
		t.Fatalf("no-role user must be deny-all, got %v", actions)
	}
}

func TestExplainAccess_ModuleACLBlocksBeforeRowLevel(t *testing.T) {
	users := &fakeAccessUsers{byKey: map[string]*auth.User{
		"tenant-A/u3": {ID: "u3", Email: "rep3@example.com", Status: "active", RoleID: rolePtr("r1")},
	}}
	e := explainExec(explainProbes(users, &fakeAccessACL{allowed: map[string]bool{"view": false, "edit": false, "delete": false}, level: map[string]string{}}, &fakeAccessFieldACL{}, &fakeAccessRecords{exists: true, owner: "x"}))
	out := runExplain(t, e, `{"userId": "u3", "entity": "patients", "recordId": "rec-9"}`)

	if v, _ := out["verdict"].(string); !strings.Contains(v, "MODULE-ACL") {
		t.Fatalf("expected module-ACL verdict, got %v", out["verdict"])
	}
	rec, _ := out["record"].(map[string]any)
	if reason, _ := rec["reason"].(string); !strings.Contains(reason, "module-ACL") {
		t.Fatalf("record layer must say module-ACL blocked first, got %v", rec)
	}
}

func TestExplainAccess_RowLevelBlockNamesOwner(t *testing.T) {
	users := &fakeAccessUsers{byKey: map[string]*auth.User{
		"tenant-A/u4": {ID: "u4", Email: "rep4@example.com", Status: "active", RoleID: rolePtr("r1")},
	}}
	acl := &fakeAccessACL{
		allowed: map[string]bool{"view": true, "edit": true, "delete": false},
		level:   map[string]string{"view": "owner", "edit": "owner"},
	}
	facl := &fakeAccessFieldACL{rows: []auth.FieldACL{
		{FieldName: "salary", CanView: false, CanEdit: false},
		{FieldName: "cost", CanView: true, CanEdit: false},
	}}
	// User-scope probe misses, admin-scope probe hits with another owner → row-level verdict.
	e := explainExec(explainProbes(users, acl, facl, &fakeAccessRecords{userSees: false, exists: true, owner: "u-other"}))
	out := runExplain(t, e, `{"userId": "u4", "entity": "patients", "recordId": "rec-7"}`)

	if v, _ := out["verdict"].(string); !strings.Contains(v, "ROW-LEVEL") {
		t.Fatalf("expected row-level verdict, got %v", out["verdict"])
	}
	rec, _ := out["record"].(map[string]any)
	if rec["ownerId"] != "u-other" || rec["ownedByUser"] != false {
		t.Fatalf("row-level block must name the real owner, got %v", rec)
	}
	if reason, _ := rec["reason"].(string); !strings.Contains(reason, "owner") {
		t.Fatalf("owner-level reason expected, got %v", rec)
	}
	// Field-ACL layer: hidden vs read-only split, sorted.
	faclOut, _ := out["fieldAcl"].(map[string]any)
	b, _ := json.Marshal(faclOut)
	if !strings.Contains(string(b), `"hiddenFields":["salary"]`) || !strings.Contains(string(b), `"readOnlyFields":["cost"]`) {
		t.Fatalf("field-ACL layer wrong: %s", b)
	}
	// Role context resolved for the narration.
	role, _ := out["role"].(map[string]any)
	if role["name"] != "sales_rep" {
		t.Fatalf("role name must resolve, got %v", role)
	}
}

func TestExplainAccess_VisibleRecordAndMissingRecord(t *testing.T) {
	users := &fakeAccessUsers{byKey: map[string]*auth.User{
		"tenant-A/u5": {ID: "u5", Email: "rep5@example.com", Status: "active", RoleID: rolePtr("r1")},
	}}
	acl := &fakeAccessACL{allowed: map[string]bool{"view": true, "edit": false, "delete": false}, level: map[string]string{"view": "team"}}

	// Visible under the user's own scope.
	e := explainExec(explainProbes(users, acl, &fakeAccessFieldACL{}, &fakeAccessRecords{userSees: true, exists: true, owner: "u5"}))
	out := runExplain(t, e, `{"userId": "u5", "entity": "patients", "recordId": "rec-1"}`)
	if v, _ := out["verdict"].(string); !strings.Contains(v, "THẤY được") {
		t.Fatalf("expected visible verdict, got %v", out["verdict"])
	}

	// Record does not exist at all → honest "not an ACL problem".
	e2 := explainExec(explainProbes(users, acl, &fakeAccessFieldACL{}, &fakeAccessRecords{userSees: false, exists: false}))
	out2 := runExplain(t, e2, `{"userId": "u5", "entity": "patients", "recordId": "rec-404"}`)
	if v, _ := out2["verdict"].(string); !strings.Contains(v, "không tồn tại") {
		t.Fatalf("expected not-found verdict, got %v", out2["verdict"])
	}
	rec, _ := out2["record"].(map[string]any)
	if exists, _ := rec["exists"].(bool); exists {
		t.Fatalf("missing record must report exists=false, got %v", rec)
	}
}
