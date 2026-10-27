//go:build integration

// UserRepository.PrivilegedHolders against a real MySQL (issue #23): propose_access asks only for the
// admins and platform operators holding the roles it names, never every user of the tenant.
//
//	bash scripts/run-integration.sh -run 'TestPrivilegedHolders_' ./internal/auth/
package auth

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"
)

func TestPrivilegedHolders_OnlyAdminsAndOperatorsOfTheNamedRolesInTheTenant(t *testing.T) {
	ctx := context.Background()
	f := manageSetup(t)
	role := func(tenant, name string) string {
		t.Helper()
		id := uuid.NewString()
		if err := f.db.Exec(`INSERT INTO acl_roles (id, tenant_id, name, label) VALUES (?, ?, ?, ?)`, id, tenant, name, name).Error; err != nil {
			t.Fatalf("seed role %s: %v", name, err)
		}
		return id
	}
	holder := func(tenant, roleID, label string, admin, operator bool) string {
		t.Helper()
		id := uuid.NewString()
		f.seed(t, tenant, id, label, "active", admin, false, operator)
		if err := f.db.Exec(`UPDATE users SET role_id = ? WHERE id = ?`, roleID, id).Error; err != nil {
			t.Fatalf("give %s the role: %v", label, err)
		}
		return id
	}
	asked, other, unnamed := role(f.tenant, "ph_asked"), role(f.tenant, "ph_other"), role(f.tenant, "ph_unnamed")
	admin := holder(f.tenant, asked, "admin", true, false)
	operator := holder(f.tenant, other, "operator", false, true)
	holder(f.tenant, asked, "staff", false, false)       // holds a named role, but is neither
	holder(f.tenant, unnamed, "admin2", true, false)     // an admin of a role nobody asked about
	holder(f.other, asked, "foreign", true, false)       // another tenant's admin carrying this role id
	gone := holder(f.tenant, asked, "gone", true, false) // soft-deleted
	if err := f.db.Exec(`UPDATE users SET deleted_at = NOW() WHERE id = ?`, gone).Error; err != nil {
		t.Fatal(err)
	}

	got, err := f.repo.PrivilegedHolders(ctx, f.tenant, []string{asked, other})
	if err != nil {
		t.Fatalf("PrivilegedHolders: %v", err)
	}
	ids := make([]string, len(got))
	for i, u := range got {
		ids[i] = u.ID
	}
	want := []string{admin, operator}
	slices.Sort(ids)
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Fatalf("holders = %v; want only the admin and the operator of the two named roles in this tenant %v", ids, want)
	}
	if none, err := f.repo.PrivilegedHolders(ctx, f.tenant, nil); err != nil || none != nil {
		t.Fatalf("no roles = %v, %v; want nil without a query", none, err)
	}
}
