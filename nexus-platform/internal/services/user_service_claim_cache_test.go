package services

import (
	"context"
	"errors"
	"testing"
)

// claimFakeInvalidator records every Invalidate(tenant, user) the user service issues, so the
// tests can pin that EXACTLY the three committed __user__ mutations (create / update / delete —
// the 6825dd15 audit sites) drop the caller's cached row, and that a refused mutation does not.
type claimFakeInvalidator struct {
	calls [][2]string
}

func (f *claimFakeInvalidator) Invalidate(tenantID, userID string) {
	f.calls = append(f.calls, [2]string{tenantID, userID})
}

func newClaimUserSvc(inv *claimFakeInvalidator) (*UserService, *fakeUserCRUDRepo) {
	repo := newFakeUserCRUDRepo()
	svc := NewUserService(repo).WithClaimCache(inv)
	n := 0
	svc.newID = func() string {
		n++
		return "cc-id-" + string(rune('0'+n))
	}
	return svc, repo
}

func assertOneInvalidate(t *testing.T, inv *claimFakeInvalidator, tenant, user string) {
	t.Helper()
	if len(inv.calls) != 1 || inv.calls[0] != [2]string{tenant, user} {
		t.Fatalf("invalidate calls = %v; want exactly one for {%s %s}", inv.calls, tenant, user)
	}
}

// Role change → the row must be re-read on the user's very next request.
func TestClaimCache_UpdateUserRoleChangeInvalidates(t *testing.T) {
	inv := &claimFakeInvalidator{}
	svc, repo := newClaimUserSvc(inv)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	newRole := "role-viewer"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{RoleID: &newRole}); err != nil {
		t.Fatalf("update role: %v", err)
	}
	assertOneInvalidate(t, inv, "t1", "u1")
}

// Disable → same.
func TestClaimCache_UpdateUserDisableInvalidates(t *testing.T) {
	inv := &claimFakeInvalidator{}
	svc, repo := newClaimUserSvc(inv)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	disabled := "disabled"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{Status: &disabled}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	assertOneInvalidate(t, inv, "t1", "u1")
}

// Demote admin → same (sibling admin so the last-admin guard allows it).
func TestClaimCache_UpdateUserDemoteInvalidates(t *testing.T) {
	inv := &claimFakeInvalidator{}
	svc, repo := newClaimUserSvc(inv)
	w1Seed(repo, "t1", "admin-1", true, "active", nil)
	w1Seed(repo, "t1", "admin-2", true, "active", nil)

	no := false
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "admin-1", UpdateUserInput{IsAdmin: &no}); err != nil {
		t.Fatalf("demote: %v", err)
	}
	assertOneInvalidate(t, inv, "t1", "admin-1")
}

// Every COMMITTED update invalidates — including a benign full_name edit. Invalidation is a
// free in-memory drop; conditioning it on "which fields changed" would be a second copy of the
// W1 predicate that could drift (and a re-enable to "active" must refresh too).
func TestClaimCache_UpdateUserBenignEditInvalidates(t *testing.T) {
	inv := &claimFakeInvalidator{}
	svc, repo := newClaimUserSvc(inv)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	name := "Renamed"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{FullName: &name}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	assertOneInvalidate(t, inv, "t1", "u1")
}

// Delete → the soft-deleted row must vanish from the cache at once.
func TestClaimCache_DeleteUserInvalidates(t *testing.T) {
	inv := &claimFakeInvalidator{}
	svc, repo := newClaimUserSvc(inv)
	w1Seed(repo, "t1", "admin-1", true, "active", nil)
	w1Seed(repo, "t1", "member-1", false, "active", nil)

	if err := svc.DeleteUser(context.Background(), "t1", "admin-1", true, "member-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	assertOneInvalidate(t, inv, "t1", "member-1")
}

// Create → invalidated for the new id (defensive: nothing can be cached yet; keeps the rule
// "every committed __user__ mutation invalidates" uniform across the three sites).
func TestClaimCache_CreateUserInvalidates(t *testing.T) {
	inv := &claimFakeInvalidator{}
	svc, _ := newClaimUserSvc(inv)

	v, err := svc.CreateUser(context.Background(), "t1", true, CreateUserInput{Email: "new@acme.test", Password: "Str0ngNewP@ss!"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	assertOneInvalidate(t, inv, "t1", v.ID)
}

// A REFUSED mutation (non-admin actor, unknown target) commits nothing → invalidates nothing.
func TestClaimCache_RefusedMutationsDoNotInvalidate(t *testing.T) {
	inv := &claimFakeInvalidator{}
	svc, repo := newClaimUserSvc(inv)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	newRole := "role-viewer"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-x", false /* non-admin */, "u1", UpdateUserInput{RoleID: &newRole}); !errors.Is(err, ErrUserForbidden) {
		t.Fatalf("non-admin update err = %v; want ErrUserForbidden", err)
	}
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "ghost", UpdateUserInput{RoleID: &newRole}); err == nil {
		t.Fatal("update of unknown user succeeded; want error")
	}
	if err := svc.DeleteUser(context.Background(), "t1", "admin-x", false /* non-admin */, "u1"); !errors.Is(err, ErrUserForbidden) {
		t.Fatalf("non-admin delete err = %v; want ErrUserForbidden", err)
	}
	if len(inv.calls) != 0 {
		t.Fatalf("refused mutations invalidated %v; want none", inv.calls)
	}
}

// Default: a user service built WITHOUT a claim cache is safe (noop), like the W1 revoker.
func TestClaimCache_NoCacheWiredIsSafe(t *testing.T) {
	repo := newFakeUserCRUDRepo()
	svc := NewUserService(repo).WithClaimCache(nil)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	newRole := "role-viewer"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{RoleID: &newRole}); err != nil {
		t.Fatalf("update with no claim cache should be safe: %v", err)
	}
}
