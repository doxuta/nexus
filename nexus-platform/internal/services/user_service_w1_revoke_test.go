package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/auth"
)

// w1FakeRevoker records the (tenantID, userID) of every RevokeAllForUser call so a test can
// assert WHETHER the W1 revoke fired and FOR WHOM, and can be programmed to fail to exercise
// the error path. It is the in-memory stand-in for the refresh-token repository. The type name
// is W1-prefixed to avoid colliding with any other fake the services test package may declare.
type w1FakeRevoker struct {
	calls [][2]string // each: {tenantID, userID}
	err   error
}

func (f *w1FakeRevoker) RevokeAllForUser(_ context.Context, tenantID, userID string) error {
	f.calls = append(f.calls, [2]string{tenantID, userID})
	return f.err
}

// newW1UserSvc builds a user service wired to a w1FakeRevoker (the W1 path active), returning
// all three so a test can seed rows AND inspect revoke calls. It uses its own deterministic id
// generator so it does not depend on helpers in the concurrently-edited user_service_test.go.
func newW1UserSvc(rev *w1FakeRevoker) (*UserService, *fakeUserCRUDRepo) {
	repo := newFakeUserCRUDRepo()
	svc := NewUserService(repo).WithTokenRevoker(rev)
	n := 0
	svc.newID = func() string {
		n++
		return "w1-id-" + string(rune('0'+n))
	}
	return svc, repo
}

// w1Seed writes a user row straight into the fake repo with an explicit id/admin/status/role so
// the W1 paths can be exercised deterministically.
func w1Seed(repo *fakeUserCRUDRepo, tenantID, id string, isAdmin bool, status string, roleID *string) {
	repo.rows[repo.key(tenantID, id)] = &auth.User{
		ID: id, TenantID: tenantID, Email: id + "@acme.test", IsAdmin: isAdmin, Status: status, RoleID: roleID,
	}
}

// W1: changing a user's role_id revokes that user's refresh tokens (so they cannot mint a fresh
// access token carrying the old role claim). ../specs/001-auth-security/spec.md §5 claim-freshness.
func TestW1_UpdateUserRoleChangeRevokes(t *testing.T) {
	rev := &w1FakeRevoker{}
	svc, repo := newW1UserSvc(rev)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	newRole := "role-xyz"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{RoleID: &newRole}); err != nil {
		t.Fatalf("update role: %v", err)
	}
	if len(rev.calls) != 1 {
		t.Fatalf("revoke called %d times; want exactly 1 on role change", len(rev.calls))
	}
	if rev.calls[0] != [2]string{"t1", "u1"} {
		t.Fatalf("revoke called for %v; want {t1 u1}", rev.calls[0])
	}
}

// W1: clearing a user's role_id (empty string → NULL) is still a privilege change → revoke.
func TestW1_UpdateUserRoleClearRevokes(t *testing.T) {
	rev := &w1FakeRevoker{}
	svc, repo := newW1UserSvc(rev)
	existing := "role-old"
	w1Seed(repo, "t1", "u1", false, "active", &existing)

	empty := ""
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{RoleID: &empty}); err != nil {
		t.Fatalf("clear role: %v", err)
	}
	if len(rev.calls) != 1 {
		t.Fatalf("revoke called %d times; want 1 on role clear", len(rev.calls))
	}
}

// W1: promoting/demoting is_admin revokes refresh tokens. Uses a sibling admin so the
// anti-lock-out guard does not block the demotion.
func TestW1_UpdateUserIsAdminChangeRevokes(t *testing.T) {
	rev := &w1FakeRevoker{}
	svc, repo := newW1UserSvc(rev)
	w1Seed(repo, "t1", "admin-1", true, "active", nil)
	w1Seed(repo, "t1", "admin-2", true, "active", nil) // sibling admin so demote is allowed

	no := false
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "admin-1", UpdateUserInput{IsAdmin: &no}); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if len(rev.calls) != 1 || rev.calls[0] != [2]string{"t1", "admin-1"} {
		t.Fatalf("revoke calls = %v; want one for {t1 admin-1}", rev.calls)
	}
}

// W1: an update that touches NO privilege claim (only full_name) must NOT revoke — a benign
// profile edit should not log the user out everywhere.
func TestW1_UpdateUserNonPrivilegeDoesNotRevoke(t *testing.T) {
	rev := &w1FakeRevoker{}
	svc, repo := newW1UserSvc(rev)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	name := "New Name"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{FullName: &name}); err != nil {
		t.Fatalf("update name: %v", err)
	}
	if len(rev.calls) != 0 {
		t.Fatalf("revoke fired on a non-privilege update (%v); want none", rev.calls)
	}
}

// W1: deleting a user revokes their refresh tokens, so a removed account cannot re-authenticate
// via a still-live refresh token.
func TestW1_DeleteUserRevokes(t *testing.T) {
	rev := &w1FakeRevoker{}
	svc, repo := newW1UserSvc(rev)
	w1Seed(repo, "t1", "admin-1", true, "active", nil) // sole admin must survive
	w1Seed(repo, "t1", "member-1", false, "active", nil)

	if err := svc.DeleteUser(context.Background(), "t1", "admin-1", true, "member-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(rev.calls) != 1 || rev.calls[0] != [2]string{"t1", "member-1"} {
		t.Fatalf("revoke calls = %v; want one for {t1 member-1}", rev.calls)
	}
}

// W1: a revoke failure on a privilege change FAILS the operation — a privilege change must never
// be silently left un-enforced (the caller sees an error, not a success).
func TestW1_UpdateUserRevokeFailureSurfacesError(t *testing.T) {
	rev := &w1FakeRevoker{err: errors.New("revoke backend down")}
	svc, repo := newW1UserSvc(rev)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	newRole := "role-xyz"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{RoleID: &newRole}); err == nil {
		t.Fatalf("update succeeded despite revoke failure; want error")
	}
}

// W1: a revoke failure on delete FAILS the operation for the same reason.
func TestW1_DeleteUserRevokeFailureSurfacesError(t *testing.T) {
	rev := &w1FakeRevoker{err: errors.New("revoke backend down")}
	svc, repo := newW1UserSvc(rev)
	w1Seed(repo, "t1", "admin-1", true, "active", nil)
	w1Seed(repo, "t1", "member-1", false, "active", nil)

	if err := svc.DeleteUser(context.Background(), "t1", "admin-1", true, "member-1"); err == nil {
		t.Fatalf("delete succeeded despite revoke failure; want error")
	}
}

// W1 default: a user service built WITHOUT a revoker (the no-op default) does not blow up on a
// privilege change — the W1 path is simply inert until WithTokenRevoker is wired.
func TestW1_NoRevokerWiredIsSafe(t *testing.T) {
	repo := newFakeUserCRUDRepo()
	svc := NewUserService(repo) // no WithTokenRevoker → noopRevoker
	w1Seed(repo, "t1", "u1", false, "active", nil)

	newRole := "role-xyz"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{RoleID: &newRole}); err != nil {
		t.Fatalf("update with no revoker should be safe: %v", err)
	}
}

// W1: disabling a user (status → non-active) revokes their refresh tokens. This is the §6 fix:
// disable is the strongest deprovisioning, so it must cut refresh just like a role/is_admin
// change — otherwise the disabled user keeps refreshing access tokens until RefreshTTL. A sibling
// admin is seeded so the anti-lock-out guard does not block disabling this (admin) user.
func TestW1_UpdateUserDisableRevokes(t *testing.T) {
	rev := &w1FakeRevoker{}
	svc, repo := newW1UserSvc(rev)
	w1Seed(repo, "t1", "admin-1", true, "active", nil)
	w1Seed(repo, "t1", "admin-2", true, "active", nil) // sibling admin so disable is allowed

	disabled := "disabled"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "admin-1", UpdateUserInput{Status: &disabled}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if len(rev.calls) != 1 || rev.calls[0] != [2]string{"t1", "admin-1"} {
		t.Fatalf("revoke calls = %v; want exactly one for {t1 admin-1} on disable", rev.calls)
	}
}

// W1 control: changing status back TO "active" (re-enabling a user) is NOT a deprovisioning and
// must NOT revoke — only status LEAVING active triggers W1.
func TestW1_UpdateUserEnableDoesNotRevoke(t *testing.T) {
	rev := &w1FakeRevoker{}
	svc, repo := newW1UserSvc(rev)
	w1Seed(repo, "t1", "u1", false, "disabled", nil)

	active := "active"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{Status: &active}); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if len(rev.calls) != 0 {
		t.Fatalf("revoke fired on re-enable (%v); want none — only status leaving active revokes", rev.calls)
	}
}

// W1 gate: a non-admin actor is refused (ErrUserForbidden) before any write OR revoke — the
// admin gate added in §17 fronts UpdateUser the same way it fronts create/delete.
func TestW1_UpdateUserNonAdminDeniedBeforeRevoke(t *testing.T) {
	rev := &w1FakeRevoker{}
	svc, repo := newW1UserSvc(rev)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	newRole := "role-xyz"
	_, err := svc.UpdateUser(context.Background(), "t1", "actor-x", false /* actorIsAdmin */, "u1", UpdateUserInput{RoleID: &newRole})
	if !errors.Is(err, ErrUserForbidden) {
		t.Fatalf("err = %v, want ErrUserForbidden for non-admin update", err)
	}
	if len(rev.calls) != 0 {
		t.Fatalf("revoke fired despite forbidden update (%v); want none", rev.calls)
	}
	if repo.rows[repo.key("t1", "u1")].RoleID != nil {
		t.Fatalf("non-admin update mutated role_id; want unchanged")
	}
}

// W1: changing a user's password (admin reset) revokes their refresh tokens. spec §5 requires
// "đổi mật khẩu thu hồi hết" — an admin password reset is the standard remediation for a
// compromised account, so any refresh token an attacker still holds MUST be cut, otherwise the
// attacker keeps minting access tokens for the full RefreshTTL despite the reset.
func TestW1_UpdateUserPasswordChangeRevokes(t *testing.T) {
	rev := &w1FakeRevoker{}
	svc, repo := newW1UserSvc(rev)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	newPass := "Str0ngNewP@ss!"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{Password: &newPass}); err != nil {
		t.Fatalf("password reset: %v", err)
	}
	if len(rev.calls) != 1 || rev.calls[0] != [2]string{"t1", "u1"} {
		t.Fatalf("revoke calls = %v; want exactly one for {t1 u1} on password change", rev.calls)
	}
}

// W1 control: an empty password (Password != nil but "") is a no-op on the persist guard (line
// 291) — no new hash is written — so it must NOT revoke, mirroring that guard exactly.
func TestW1_UpdateUserEmptyPasswordDoesNotRevoke(t *testing.T) {
	rev := &w1FakeRevoker{}
	svc, repo := newW1UserSvc(rev)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	empty := ""
	name := "Renamed"
	// Pair the empty password with a benign non-privilege field so the update has something to
	// write; the empty password alone must not trip the revoke gate.
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{Password: &empty, FullName: &name}); err != nil {
		t.Fatalf("update with empty password: %v", err)
	}
	if len(rev.calls) != 0 {
		t.Fatalf("revoke fired on empty-password update (%v); want none", rev.calls)
	}
}

// W1: when password and a privilege claim (role) change in the SAME update, the `||` chain must
// revoke EXACTLY ONCE — not twice — so a combined reset+reassign does not double-call the revoker.
func TestW1_UpdateUserPasswordAndRoleRevokesOnce(t *testing.T) {
	rev := &w1FakeRevoker{}
	svc, repo := newW1UserSvc(rev)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	newPass := "Str0ngNewP@ss!"
	newRole := "role-xyz"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{Password: &newPass, RoleID: &newRole}); err != nil {
		t.Fatalf("password+role update: %v", err)
	}
	if len(rev.calls) != 1 || rev.calls[0] != [2]string{"t1", "u1"} {
		t.Fatalf("revoke calls = %v; want exactly one for {t1 u1} on combined password+role change", rev.calls)
	}
}

// W1: a revoke failure on a password change FAILS the operation — a password reset must never be
// silently left with live refresh tokens (the caller sees an error, not a success).
func TestW1_UpdateUserPasswordRevokeFailureSurfacesError(t *testing.T) {
	rev := &w1FakeRevoker{err: errors.New("revoke backend down")}
	svc, repo := newW1UserSvc(rev)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	newPass := "Str0ngNewP@ss!"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{Password: &newPass}); err == nil {
		t.Fatalf("update succeeded despite revoke failure on password change; want error")
	}
}
