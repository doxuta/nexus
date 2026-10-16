//go:build integration

// #281 — the recipient resolver's SQL against REAL MySQL: tenant pin + eligibility (active, not
// soft-deleted). A fake resolver keyed by tenant cannot catch a dropped `tenant_id = ?`.
package services

import (
	"context"
	"reflect"
	"testing"
)

func TestWorkflowRecipientResolver_TenantAndEligibility(t *testing.T) {
	_, sdb := o3aSetup(t)
	ctx := context.Background()
	r := NewWorkflowRecipientResolver(sdb)

	const (
		tenA  = "aaaaaaaa-2222-2222-2222-aaaaaaaaaaaa"
		tenB  = "bbbbbbbb-2222-2222-2222-bbbbbbbbbbbb"
		teamA = "cccccccc-2222-2222-2222-cccccccccccc"
		teamB = "dddddddd-2222-2222-2222-dddddddddddd"
		role  = "eeeeeeee-2222-2222-2222-eeeeeeeeeeee"
	)
	ringA := asgSeedTeam(t, sdb, tenA, teamA, []string{
		"11111111-2222-2222-2222-111111111111",
		"22222222-2222-2222-2222-222222222222",
	})
	asgSeedTeam(t, sdb, tenB, teamB, []string{"33333333-2222-2222-2222-333333333333"})

	asgExec(t, sdb, "INSERT INTO acl_roles (id, tenant_id, name, label) VALUES (?, ?, 'r', 'R')", role, tenA)
	// Same role id (FK allows a stray cross-tenant holder) held by users of BOTH tenants, plus a disabled and a soft-deleted holder in A.
	asgExec(t, sdb, "UPDATE users SET role_id = ? WHERE id IN (?, ?, ?, ?, ?)", role,
		ringA[0], "33333333-2222-2222-2222-333333333333",
		"dddddddd-0000-0000-0000-"+tenA[:12], "dddddddd-0001-0000-0000-"+tenA[:12], ringA[1])

	t.Run("role: own tenant, active only", func(t *testing.T) {
		got, err := r.RoleMembers(ctx, tenA, role)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{ringA[0], ringA[1]}; !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})
	t.Run("team: own tenant, active only", func(t *testing.T) {
		got, err := r.TeamMembers(ctx, tenA, teamA)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, ringA) {
			t.Fatalf("got %v want %v", got, ringA)
		}
	})
	t.Run("team of another tenant resolves to nobody", func(t *testing.T) {
		got, err := r.TeamMembers(ctx, tenA, teamB)
		if err != nil || len(got) != 0 {
			t.Fatalf("got %v err %v, want empty", got, err)
		}
	})
	t.Run("role asked under the other tenant sees only that tenant's holders", func(t *testing.T) {
		got, err := r.RoleMembers(ctx, tenB, role)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"33333333-2222-2222-2222-333333333333"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})
}
