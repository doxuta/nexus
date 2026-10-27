package ai

// list_roles — the read that stops the model naming a role that does not exist.
//
// propose_workqueue's roleName and propose_approval's role approver are resolved to ids at APPLY.
// With no way to see the tenant's roles the model produced a plausible name and the Apply failed —
// the worst place to learn it, because the proposal already looked right and someone already
// pressed the button.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"nexus/platform/internal/services"
)

func TestListRoles(t *testing.T) {
	r := twoTenantReader()
	r.roles = map[string][]string{
		"t1": {"crm_sales_manager", "crm_sales_rep"},
		"t2": {"other_tenant_role"},
	}
	e := &toolExecutor{reader: r, tenantID: "t1"}

	out := e.execute(context.Background(), "list_roles", "{}")
	var got struct {
		OK     bool `json:"ok"`
		Result struct {
			Roles []string `json:"roles"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if !got.OK {
		t.Fatalf("ok=false: %s", out)
	}
	if strings.Join(got.Result.Roles, ",") != "crm_sales_manager,crm_sales_rep" {
		t.Errorf("roles = %v, want this tenant's two roles", got.Result.Roles)
	}
	// Tenant isolation is the property that must never regress: the model must not be able to
	// name — or even learn of — another tenant's roles.
	if strings.Contains(out, "other_tenant_role") {
		t.Fatalf("another tenant's role leaked into the tool result: %s", out)
	}
}

func TestListRoles_EmptyTenantReturnsAnEmptyList(t *testing.T) {
	e := &toolExecutor{reader: twoTenantReader(), tenantID: "t1"}
	out := e.execute(context.Background(), "list_roles", "{}")
	// Not null: the model reads this JSON, and "roles": null invites a hallucinated name far more
	// than an explicit empty array does.
	if !strings.Contains(out, `"roles":[]`) {
		t.Errorf("out = %s, want an explicit empty array", out)
	}
}

// Issue #23: list_roles {grants: true} answers "who can edit contacts?" whole — every role of THIS
// tenant with its current grants, and nothing buffered (a read, so MCP may offer it too).
func TestListRoles_WithGrants(t *testing.T) {
	e := accessExecutor(nil)
	out := e.execute(context.Background(), "list_roles", `{"grants":true}`)
	var got struct {
		OK     bool `json:"ok"`
		Result struct {
			Roles []struct {
				Name   string                    `json:"name"`
				Label  string                    `json:"label"`
				Grants []services.RoleActionView `json:"grants"`
			} `json:"roles"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || !got.OK {
		t.Fatalf("out = %s (%v); want ok", out, err)
	}
	byName := map[string][]services.RoleActionView{}
	for _, r := range got.Result.Roles {
		byName[r.Name] = r.Grants
	}
	if len(got.Result.Roles) != 5 || got.Result.Roles[0].Label != "Nhân viên kinh doanh" {
		t.Fatalf("roles = %+v; want tenant A's five roles with their labels", got.Result.Roles)
	}
	rep := []services.RoleActionView{{EntityName: "crm_accounts", Action: "view", AccessLevel: "all"}, {EntityName: "crm_accounts", Action: "create", AccessLevel: "all"}, {EntityName: "crm_accounts", Action: "edit", AccessLevel: "owner"}}
	if !slices.Equal(byName["crm_sales_rep"], rep) || !slices.Equal(byName["viewer"], []services.RoleActionView{{EntityName: "*", Action: "view", AccessLevel: "all"}}) {
		t.Fatalf("grants = %+v; want the rep's three crm_accounts grants and viewer's \"*\" view", byName)
	}
	if g, ok := byName["ops"]; !ok || g == nil || len(g) != 0 {
		t.Errorf("a role with no grant = %v; want an explicit empty list", g)
	}
	if strings.Contains(out, "b_only") {
		t.Fatalf("another tenant's role leaked: %s", out)
	}
	if len(e.draft) != 0 {
		t.Fatalf("draft = %+v; a read buffers nothing", e.draft)
	}
}

func TestListRoles_GrantsNeedTheRoleReader(t *testing.T) {
	e := &toolExecutor{reader: twoTenantReader(), tenantID: "t1"}
	if out := e.execute(context.Background(), "list_roles", `{"grants":true}`); !strings.Contains(out, "ROLES_GRANTS_UNAVAILABLE") {
		t.Fatalf("out = %s; want ROLES_GRANTS_UNAVAILABLE", out)
	}
	if out := e.execute(context.Background(), "list_roles", `{"grants":"yes"}`); !strings.Contains(out, "INVALID_ARGS") {
		t.Fatalf("out = %s; want INVALID_ARGS", out)
	}
}
