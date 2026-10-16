package ai

// propose_access (issue #2): every refusal of the contract, the additive merge (shared with the FE),
// and what a buffered proposal carries for the card, the matrix and the apply.

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

type fakeCutoff struct{ at *time.Time }

func (f fakeCutoff) HeadCutoff(context.Context, string) (*time.Time, error) { return f.at, nil }

var accCutoff = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func ptrStr(s string) *string { return &s }

// accessExecutor is tenant A of a delivered workspace (last published at accCutoff) with:
//   - crm_accounts (published), edge_table (born on the cutoff second: published), visits (born after:
//     not yet published); tenant B has invoices;
//   - roles crm_sales_rep (held by the rep, grants on crm_accounts), crm_sales_manager (held by an
//     admin), ops (held by a platform operator), tenant_owner (reserved), viewer ("*" view all);
//     tenant B has b_only.
func accessExecutor(published publishCutoff) *toolExecutor {
	meta := &fakeMetaReader{entities: map[string][]metadata.EntityDefinition{
		"tenant-A": {
			{ID: "e1", TenantID: "tenant-A", Name: "crm_accounts", Label: "Khách hàng", CreatedAt: accCutoff.Add(-time.Hour)},
			{ID: "e2", TenantID: "tenant-A", Name: "edge_table", Label: "Biên", CreatedAt: accCutoff},
			{ID: "e3", TenantID: "tenant-A", Name: "visits", Label: "Lượt khám", CreatedAt: accCutoff.Add(time.Second)},
		},
		"tenant-B": {{ID: "eb", TenantID: "tenant-B", Name: "invoices", Label: "Hóa đơn", CreatedAt: accCutoff.Add(time.Hour)}},
	}}
	roles := &fakeAccessRoles{
		byTenant: map[string][]auth.Role{
			"tenant-A": {
				{ID: "r-rep", TenantID: "tenant-A", Name: "crm_sales_rep", Label: "Nhân viên kinh doanh"},
				{ID: "r-mgr", TenantID: "tenant-A", Name: "crm_sales_manager", Label: "Quản lý kinh doanh"},
				{ID: "r-ops", TenantID: "tenant-A", Name: "ops", Label: "Vận hành"},
				{ID: "r-own", TenantID: "tenant-A", Name: auth.OwnerRoleName, Label: "Chủ khách"},
				{ID: "r-star", TenantID: "tenant-A", Name: "viewer", Label: "Xem tất cả"},
			},
			"tenant-B": {{ID: "r-b", TenantID: "tenant-B", Name: "b_only", Label: "B"}},
		},
		actions: map[string][]auth.RoleAction{
			"r-rep": {
				{EntityName: "crm_accounts", Action: "view", AccessLevel: "all"},
				{EntityName: "crm_accounts", Action: "create", AccessLevel: "all"},
				{EntityName: "crm_accounts", Action: "edit", AccessLevel: "owner"},
			},
			"r-star": {{EntityName: "*", Action: "view", AccessLevel: "all"}},
		},
	}
	users := &fakeAccessUsers{byTenant: map[string][]auth.User{"tenant-A": {
		{ID: "u-rep", TenantID: "tenant-A", RoleID: ptrStr("r-rep"), Status: "active"},
		{ID: "u-adm", TenantID: "tenant-A", RoleID: ptrStr("r-mgr"), IsAdmin: true, Status: "active"},
		{ID: "u-op", TenantID: "tenant-A", RoleID: ptrStr("r-ops"), IsPlatformOperator: true, Status: "active"}, // the flag alone
	}}}
	return &toolExecutor{reader: ReaderWithAccessProbes(meta, &AccessProbes{Roles: roles, Users: users, Published: published}), tenantID: "tenant-A"}
}

// withTickets proposes the tickets table first, as the turn that builds it does.
func withTickets(t *testing.T, e *toolExecutor) *toolExecutor {
	t.Helper()
	mustBuffer(t, e, "propose_entity", `{"name":"tickets","label":"Phiếu hỗ trợ","fields":[{"name":"title","label":"Tiêu đề","fieldType":"text","required":true}]}`)
	return e
}

func grantArgs(grants ...string) string { return `{"grants":[` + strings.Join(grants, ",") + `]}` }

func grant(role, entity, scope string, actions ...string) string {
	b, _ := json.Marshal(map[string]any{"role": role, "entity": entity, "actions": actions, "scope": scope})
	return string(b)
}

func TestProposeAccess_Refusals(t *testing.T) {
	ok := grant("crm_sales_rep", "tickets", "all", "view")
	tests := []struct{ name, args, code string }{
		{"unknown role", grantArgs(grant("nobody", "tickets", "all", "view")), "ACCESS_ROLE_NOT_FOUND"},
		{"a role of another tenant", grantArgs(grant("b_only", "tickets", "all", "view")), "ACCESS_ROLE_NOT_FOUND"},
		{"no role", grantArgs(grant(" ", "tickets", "all", "view")), "ACCESS_ROLE_REQUIRED"},
		{"the reserved owner role", grantArgs(grant(auth.OwnerRoleName, "tickets", "all", "view")), "ACCESS_ROLE_RESERVED"},
		{"the reserved owner role, other casing", grantArgs(grant("Tenant_Owner", "tickets", "all", "view")), "ACCESS_ROLE_RESERVED"},
		{"a role an admin holds", grantArgs(grant("crm_sales_manager", "tickets", "all", "view")), "ACCESS_ROLE_ADMIN"},
		{"a role a platform operator holds", grantArgs(grant("ops", "tickets", "all", "view")), "ACCESS_ROLE_ADMIN"},
		{"a published table not proposed here", grantArgs(grant("crm_sales_rep", "crm_accounts", "all", "view")), "ACCESS_ENTITY_PUBLISHED"},
		{"a table born on the publish second", grantArgs(grant("crm_sales_rep", "edge_table", "all", "view")), "ACCESS_ENTITY_PUBLISHED"},
		{"another tenant's table", grantArgs(grant("crm_sales_rep", "invoices", "all", "view")), "ENTITY_NOT_FOUND"},
		{"every table", grantArgs(grant("crm_sales_rep", "*", "all", "view")), "ENTITY_NOT_FOUND"},
		{"delete", grantArgs(grant("crm_sales_rep", "tickets", "all", "view", "delete")), "ACCESS_ACTION_UNSUPPORTED"},
		{"an unknown action", grantArgs(grant("crm_sales_rep", "tickets", "all", "share")), "ACCESS_ACTION_UNSUPPORTED"},
		{"no action", grantArgs(grant("crm_sales_rep", "tickets", "all")), "ACCESS_ACTIONS_REQUIRED"},
		{"field-level access", grantArgs(`{"role":"crm_sales_rep","entity":"tickets","actions":["view"],"scope":"all","fields":[{"field":"title","canEdit":false}]}`), "ACCESS_GRANT_KEY_UNSUPPORTED"},
		{"a record share", grantArgs(`{"role":"crm_sales_rep","entity":"tickets","actions":["view"],"scope":"all","shares":["rec-1"]}`), "ACCESS_GRANT_KEY_UNSUPPORTED"},
		{"an unknown top-level key", `{"grants":[` + ok + `],"admin":true}`, "ACCESS_GRANT_KEY_UNSUPPORTED"},
		{"scope team", grantArgs(grant("crm_sales_rep", "tickets", "team", "view")), "ACCESS_SCOPE_UNSUPPORTED"},
		{"scope none", grantArgs(grant("crm_sales_rep", "tickets", "none", "view")), "ACCESS_SCOPE_UNSUPPORTED"},
		{"a user-field scope", grantArgs(grant("crm_sales_rep", "tickets", "field:owner_user", "view")), "ACCESS_SCOPE_UNSUPPORTED"},
		{"no scope", grantArgs(grant("crm_sales_rep", "tickets", "", "view")), "ACCESS_SCOPE_UNSUPPORTED"},
		{"no grants", `{"grants":[]}`, "ACCESS_GRANTS_REQUIRED"},
		{"too many grants", grantArgs(slices.Repeat([]string{ok}, maxAccessGrants+1)...), "ACCESS_TOO_MANY_GRANTS"},
		{"one bad grant refuses the whole call", grantArgs(ok, grant("crm_sales_rep", "tickets", "all", "delete")), "ACCESS_ACTION_UNSUPPORTED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := withTickets(t, accessExecutor(fakeCutoff{at: &accCutoff}))
			mustReject(t, e, "propose_access", tt.args, tt.code)
		})
	}
}

func TestProposeAccess_BuffersOneCardPerRoleWithTheRoleChange(t *testing.T) {
	e := withTickets(t, accessExecutor(fakeCutoff{at: &accCutoff}))
	out := mustBuffer(t, e, "propose_access", grantArgs(
		grant("crm_sales_rep", "tickets", "all", "view", "create"),
		grant("crm_sales_rep", "tickets", "owner", "edit"),
	))
	if res, _ := out["result"].(map[string]any); res["op"] != "update" {
		t.Errorf("tool result %v: want op update (the role exists)", out)
	}
	if len(e.draft) != 2 || e.draft[1].Kind != "access" || e.draft[1].Access == nil {
		t.Fatalf("draft = %+v; want the entity then one access card", e.draft)
	}
	a := e.draft[1].Access
	want := []services.RoleActionView{{EntityName: "tickets", Action: "view", AccessLevel: "all"}, {EntityName: "tickets", Action: "create", AccessLevel: "all"}, {EntityName: "tickets", Action: "edit", AccessLevel: "owner"}}
	if a.Role != "crm_sales_rep" || a.RoleLabel != "Nhân viên kinh doanh" || !slices.Equal(a.Grants, want) || a.Labels["tickets"] != "Phiếu hỗ trợ" {
		t.Fatalf("access = %+v; want crm_sales_rep / Nhân viên kinh doanh, %v, label Phiếu hỗ trợ", a, want)
	}
	c := e.draft[1].Change
	if c == nil || c.DocType != "role" || c.DocKey != "r-rep" || c.Op != "update" || c.Target.Name != "crm_sales_rep" || c.Target.Entity != "tickets" || len(c.ChangedPaths) != 0 {
		t.Fatalf("change = %+v; want the role document r-rep, update, target crm_sales_rep/tickets, no changed paths", c)
	}
	var before, after struct {
		Grants []services.RoleActionView `json:"grants"`
		Labels map[string]string         `json:"labels"`
	}
	if json.Unmarshal(c.Before, &before) != nil || json.Unmarshal(c.After, &after) != nil {
		t.Fatalf("before/after are not {grants, labels}: %s / %s", c.Before, c.After)
	}
	// The projection is the touched table only: crm_accounts is not the card's business.
	if len(before.Grants) != 0 || !slices.Equal(after.Grants, want) || after.Labels["tickets"] != "Phiếu hỗ trợ" {
		t.Fatalf("before = %+v, after = %+v; want nothing on tickets before, the three grants after", before, after)
	}
}

func TestProposeAccess_RoleByLabelAndTablesNotYetPublished(t *testing.T) {
	// The builder says the label; the table was born after the last publish (still hidden).
	e := accessExecutor(fakeCutoff{at: &accCutoff})
	mustBuffer(t, e, "propose_access", grantArgs(grant("Nhân viên kinh doanh", "visits", "all", "view")))
	if a := e.draft[0].Access; a.Role != "crm_sales_rep" || a.Labels["visits"] != "Lượt khám" {
		t.Fatalf("access = %+v; want the role resolved by its label to crm_sales_rep, visits labelled", a)
	}

	// Never delivered: nothing is the customer's yet, so a live table is fine.
	e = accessExecutor(fakeCutoff{})
	mustBuffer(t, e, "propose_access", grantArgs(grant("crm_sales_rep", "edge_table", "all", "view")))

	// No cutoff reader wired: every live table counts as published, only this conversation's tables pass.
	e = withTickets(t, accessExecutor(nil))
	mustReject(t, e, "propose_access", grantArgs(grant("crm_sales_rep", "visits", "all", "view")), "ACCESS_ENTITY_PUBLISHED")
	mustBuffer(t, e, "propose_access", grantArgs(grant("crm_sales_rep", "tickets", "all", "view")))
}

func TestProposeAccess_IsAdditive(t *testing.T) {
	// The rep's grants on other tables are in the after-image untouched; viewer's "*" view all already
	// covers a view of tickets, so only its edit is new — and a proposal that adds nothing is refused.
	e := withTickets(t, accessExecutor(fakeCutoff{at: &accCutoff}))
	mustBuffer(t, e, "propose_access", grantArgs(grant("viewer", "tickets", "all", "view"), grant("viewer", "tickets", "owner", "edit")))
	var after struct {
		Grants []services.RoleActionView `json:"grants"`
	}
	if err := json.Unmarshal(e.draft[1].Change.After, &after); err != nil {
		t.Fatal(err)
	}
	want := []services.RoleActionView{{EntityName: "*", Action: "view", AccessLevel: "all"}, {EntityName: "tickets", Action: "edit", AccessLevel: "owner"}}
	if !slices.Equal(after.Grants, want) {
		t.Fatalf("viewer after = %v; want %v (the \"*\" row covers the view)", after.Grants, want)
	}
	mustReject(t, e, "propose_access", grantArgs(grant("viewer", "tickets", "owner", "view")), "ACCESS_NOTHING_TO_ADD")

	// Two roles in one call: one card each.
	e = withTickets(t, accessExecutor(fakeCutoff{at: &accCutoff}))
	mustBuffer(t, e, "propose_access", grantArgs(grant("crm_sales_rep", "tickets", "all", "view"), grant("viewer", "tickets", "owner", "edit")))
	if len(e.draft) != 3 || e.draft[1].Access.Role != "crm_sales_rep" || e.draft[2].Access.Role != "viewer" {
		t.Fatalf("draft = %+v; want one access card per role, in call order", e.draft)
	}
}

func TestProposeAccess_UnavailableWithoutProbes(t *testing.T) {
	e := withTickets(t, &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"})
	mustReject(t, e, "propose_access", grantArgs(grant("crm_sales_rep", "tickets", "all", "view")), "ACCESS_PROPOSE_UNAVAILABLE")
}

// The same JSON is read by nexus-web/lib/accessLevels.test.ts: one additive rule, two implementations.
func TestMergeAccessGrants_SharedCases(t *testing.T) {
	raw, err := os.ReadFile("testdata/access_merge_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name    string                    `json:"name"`
		Current []services.RoleActionView `json:"current"`
		Add     []services.RoleActionView `json:"add"`
		Want    []services.RoleActionView `json:"want"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil || len(cases) < 10 {
		t.Fatalf("bad cases file: %v (%d)", err, len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			before := slices.Clone(c.Current)
			got := mergeAccessGrants(c.Current, c.Add)
			if !slices.Equal(got, c.Want) {
				t.Fatalf("merge = %v; want %v", got, c.Want)
			}
			if !slices.Equal(c.Current, before) {
				t.Fatalf("the role's grants were changed in place: %v", c.Current)
			}
		})
	}
}
