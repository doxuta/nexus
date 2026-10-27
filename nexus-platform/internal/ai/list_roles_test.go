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
	"fmt"
	"slices"
	"strings"
	"testing"

	"nexus/platform/internal/auth"
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

// Issue #67: a role deleted between the role list and its grants is skipped, not a failed read. The
// grants come from one snapshot of every role, so a role gone meanwhile is simply not in it.
func TestListRoles_GrantsSkipARoleDeletedMeanwhile(t *testing.T) {
	e := accessExecutor(nil)
	e.reader.(*readerWithAccessProbes).probes.Roles.(*fakeAccessRoles).gone = map[string]bool{"r-ops": true}
	out := e.execute(context.Background(), "list_roles", `{"grants":true}`)
	var got struct {
		OK     bool `json:"ok"`
		Result struct {
			Roles []struct {
				Name string `json:"name"`
			} `json:"roles"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || !got.OK {
		t.Fatalf("out = %s (%v); want ok with the roles that still exist", out, err)
	}
	var names []string
	for _, r := range got.Result.Roles {
		names = append(names, r.Name)
	}
	if strings.Join(names, ",") != "crm_sales_rep,crm_sales_manager,tenant_owner,viewer" {
		t.Fatalf("roles = %v; want the four roles left, without the deleted ops", names)
	}
}

// Issue #67: list_roles {grants: true} is bounded like snapshot_model and says what it cut. The issue's
// tenant (20 roles × 100 tables × 4 actions) was ≈ 550 KB in one tool result.
func TestListRoles_GrantsAreBoundedAndSayWhatWasCut(t *testing.T) {
	type role struct {
		Name            string                    `json:"name"`
		Grants          []services.RoleActionView `json:"grants"`
		GrantsTruncated bool                      `json:"grantsTruncated"`
		GrantsTotal     int                       `json:"grantsTotal"`
	}
	type result struct {
		Roles          []role `json:"roles"`
		RolesTruncated bool   `json:"rolesTruncated"`
		RolesTotal     int    `json:"rolesTotal"`
		Note           string `json:"note"`
	}
	run := func(t *testing.T, roles, tables int, smallFrom int) (result, string) {
		t.Helper()
		fake := &fakeAccessRoles{byTenant: map[string][]auth.Role{}, actions: map[string][]auth.RoleAction{}}
		for i := range roles {
			id := fmt.Sprintf("r%03d", i)
			fake.byTenant["t1"] = append(fake.byTenant["t1"], auth.Role{ID: id, TenantID: "t1", Name: fmt.Sprintf("role_%03d", i), Label: "Vai"})
			n := tables
			if i >= smallFrom {
				n = 1
			}
			for tb := range n {
				for _, a := range []string{"create", "delete", "edit", "view"} {
					fake.actions[id] = append(fake.actions[id], auth.RoleAction{EntityName: fmt.Sprintf("crm_table_%03d", tb), Action: a, AccessLevel: "all"})
				}
			}
		}
		e := &toolExecutor{reader: ReaderWithAccessProbes(&fakeMetaReader{}, &AccessProbes{Roles: fake}), tenantID: "t1"}
		out := e.execute(context.Background(), "list_roles", `{"grants":true}`)
		var got struct {
			OK     bool   `json:"ok"`
			Result result `json:"result"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil || !got.OK {
			t.Fatalf("out = %.300s (%v); want ok", out, err)
		}
		return got.Result, out
	}

	t.Run("the issue's tenant stays small", func(t *testing.T) {
		_, out := run(t, 20, 100, 20)
		if len(out) > 100_000 {
			t.Fatalf("20 roles × 100 tables × 4 actions = %d bytes; want at most 100 KB", len(out))
		}
	})

	t.Run("roles and grants past the caps are cut and counted", func(t *testing.T) {
		res, _ := run(t, maxListedRoles+5, 100, 1)
		if len(res.Roles) != maxListedRoles || !res.RolesTruncated || res.RolesTotal != maxListedRoles+5 {
			t.Fatalf("roles = %d, truncated %v, total %d; want %d shown of %d", len(res.Roles), res.RolesTruncated, res.RolesTotal, maxListedRoles, maxListedRoles+5)
		}
		big, small := res.Roles[0], res.Roles[1]
		if len(big.Grants) != maxListedGrantsPerRole || !big.GrantsTruncated || big.GrantsTotal != 400 {
			t.Fatalf("big role: %d grants, truncated %v, total %d; want %d shown of 400", len(big.Grants), big.GrantsTruncated, big.GrantsTotal, maxListedGrantsPerRole)
		}
		// The first grants in the stored order are the ones kept (a "*" grant sorts first).
		if big.Grants[0].EntityName != "crm_table_000" || big.Grants[0].Action != "create" {
			t.Errorf("first kept grant = %+v; want the first stored one", big.Grants[0])
		}
		if len(small.Grants) != 4 || small.GrantsTruncated || small.GrantsTotal != 0 {
			t.Errorf("small role = %+v; want its 4 grants and no cut flag", small)
		}
		if !strings.Contains(res.Note, "explain_access") {
			t.Errorf("note = %q; want what was cut and where to ask instead", res.Note)
		}
	})

	t.Run("nothing cut, no flag", func(t *testing.T) {
		res, out := run(t, 3, 2, 3)
		if res.RolesTruncated || res.Note != "" || strings.Contains(out, "Truncated") {
			t.Fatalf("out = %s; want no cut flag or note when everything fits", out)
		}
	})
}
