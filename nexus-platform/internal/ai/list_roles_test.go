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
	"strings"
	"testing"
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
