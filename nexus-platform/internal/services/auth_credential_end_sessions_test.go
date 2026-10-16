package services

import (
	"context"
	"errors"
	"testing"
)

// #310 password-change-ends-sessions: the credential-level events — a self password change, the
// forgot-password reset, an admin-set password and the E5 reuse alarm — END sessions (refresh rows
// 'ended' + session guard invalidated) instead of the plain revoke whose access tokens lived ≤ 15 min.

// liveSessionOf reports whether the session still has a live refresh row.
func liveSessionOf(repo *fakeRefreshRepo, sid string) bool {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	for _, rt := range repo.rows {
		if rt.SessionID == sid && rt.RevokedAt == nil {
			return true
		}
	}
	return false
}

func invalidatedHas(g *recordingGuard, sid string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, s := range g.invalidated {
		if s == sid {
			return true
		}
	}
	return false
}

// A self change from device A ends device B at once and leaves A signed in.
func TestChangePassword_EndsOtherSessionsAndKeepsTheCaller(t *testing.T) {
	svc, password := newTestService(t)
	guard := &recordingGuard{}
	svc = svc.WithSessionGuard(guard)
	repo := svc.refresh.(*fakeRefreshRepo)
	a := loginAlice(t, svc, password)
	b := loginAlice(t, svc, password)
	sidA, sidB := sidOfAccess(t, svc, a.AccessToken), sidOfAccess(t, svc, b.AccessToken)

	if err := svc.ChangePassword(context.Background(), "t-1", "u-1", password, "Br4nd-New-Pw!", sidA); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	ended, _ := repo.IsSessionEnded(context.Background(), "t-1", "u-1", sidB)
	if !ended {
		t.Error("device B's session was not ENDED (a plain revoke leaves its access token alive ≤ 15 min)")
	}
	if !invalidatedHas(guard, sidB) {
		t.Errorf("guard invalidations = %v, want B (%s) so the auth stage refuses it on the next request", guard.invalidated, sidB)
	}
	if invalidatedHas(guard, sidA) || !liveSessionOf(repo, sidA) {
		t.Error("the calling device was signed out; it proved the old password and must stay")
	}
	if _, err := svc.Refresh(context.Background(), b.RefreshToken); !errors.Is(err, ErrInvalidRefresh) {
		t.Errorf("device B still refreshes: %v", err)
	}
	if _, err := svc.Refresh(context.Background(), a.RefreshToken); err != nil {
		t.Errorf("device A cannot refresh after its own password change: %v", err)
	}
}

// A token minted before specs/055 carries no sid: nothing to keep, every session ends.
func TestChangePassword_WithoutSessionIDEndsEverySession(t *testing.T) {
	svc, password := newTestService(t)
	guard := &recordingGuard{}
	svc = svc.WithSessionGuard(guard)
	repo := svc.refresh.(*fakeRefreshRepo)
	a := loginAlice(t, svc, password)
	sidA := sidOfAccess(t, svc, a.AccessToken)

	if err := svc.ChangePassword(context.Background(), "t-1", "u-1", password, "Br4nd-New-Pw!", ""); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if liveSessionOf(repo, sidA) || !invalidatedHas(guard, sidA) {
		t.Errorf("session A live=%v invalidated=%v, want ended and invalidated", liveSessionOf(repo, sidA), invalidatedHas(guard, sidA))
	}
}

// Forgot-password: the requester is not signed in, so every session ends.
func TestResetPassword_EndsEverySessionAndInvalidatesTheGuard(t *testing.T) {
	svc, _, sender := newResetTestService(t)
	guard := &recordingGuard{}
	svc.WithSessionGuard(guard)
	raw := requestAndExtractToken(t, svc, sender, "acme", "alice@acme.test")
	a := loginAlice(t, svc, "s3cret-pw")
	sidA := sidOfAccess(t, svc, a.AccessToken)

	if err := svc.ResetPassword(context.Background(), raw, "Br4nd-New-Pw!"); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	ended, _ := svc.refresh.(*fakeRefreshRepo).IsSessionEnded(context.Background(), "t-1", "u-1", sidA)
	if !ended || !invalidatedHas(guard, sidA) {
		t.Errorf("session A ended=%v invalidated=%v, want both: its access token must be refused on the next request", ended, invalidatedHas(guard, sidA))
	}
}

// E5: theft detected ⇒ the family's sessions are ENDED (an attacker's still-valid access token is
// refused at once), not just stripped of their refresh rows.
func TestE5_FamilyKillEndsTheOtherSessionsAndInvalidatesTheGuard(t *testing.T) {
	n := &e5FakeNotifier{}
	repo := newFakeRefreshRepo()
	svc := newE5AuthSvc(t, repo, n)
	guard := &recordingGuard{}
	svc = svc.WithSessionGuard(guard)

	raw := loginE5(t, svc) // device A
	other, err := svc.Login(context.Background(), LoginInput{Tenant: "acme", Email: "alice@acme.test", Password: e5TestPassword})
	if err != nil {
		t.Fatalf("login device B: %v", err)
	}
	sidB := sidOfAccess(t, svc, other.AccessToken)
	if _, err := svc.Refresh(context.Background(), raw); err != nil { // rotate A: its first token is now dead
		t.Fatalf("rotate A: %v", err)
	}
	if _, err := svc.Refresh(context.Background(), raw); !errors.Is(err, ErrInvalidRefresh) { // replay ⇒ E5
		t.Fatalf("replay = %v, want ErrInvalidRefresh", err)
	}

	ended, _ := repo.IsSessionEnded(context.Background(), "t-1", "u-1", sidB)
	if !ended || !invalidatedHas(guard, sidB) {
		t.Errorf("device B ended=%v invalidated=%v after the family kill, want both", ended, invalidatedHas(guard, sidB))
	}
}

// --- admin sets a password (UserService → session ender) -----------------------------------------

type fakeSessionEnder struct {
	calls [][2]string
	err   error
}

func (f *fakeSessionEnder) LogoutEverywhere(_ context.Context, tenantID, userID string) error {
	f.calls = append(f.calls, [2]string{tenantID, userID})
	return f.err
}

func TestUpdateUser_PasswordSetEndsSessionsInsteadOfPlainRevoke(t *testing.T) {
	rev, ender := &w1FakeRevoker{}, &fakeSessionEnder{}
	svc, repo := newW1UserSvc(rev)
	svc.WithSessionEnder(ender)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	pw := "Str0ng-Pass-2026!"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{Password: &pw}); err != nil {
		t.Fatalf("set password: %v", err)
	}
	if len(ender.calls) != 1 || ender.calls[0] != [2]string{"t1", "u1"} {
		t.Fatalf("session ender calls = %v, want one for {t1 u1}", ender.calls)
	}
	if len(rev.calls) != 0 {
		t.Errorf("plain revoke also fired (%v); the ender already covers every live row", rev.calls)
	}
}

// A role change keeps the plain revoke: it must land as a 403 on the next request (claim-freshness),
// not as a sign-out.
func TestUpdateUser_RoleChangeStillUsesPlainRevoke(t *testing.T) {
	rev, ender := &w1FakeRevoker{}, &fakeSessionEnder{}
	svc, repo := newW1UserSvc(rev)
	svc.WithSessionEnder(ender)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	role := "role-xyz"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{RoleID: &role}); err != nil {
		t.Fatalf("role change: %v", err)
	}
	if len(rev.calls) != 1 || len(ender.calls) != 0 {
		t.Fatalf("revoke calls = %v, ender calls = %v; want the plain revoke only", rev.calls, ender.calls)
	}
}

func TestUpdateUser_PasswordSetEndSessionsFailureSurfaces(t *testing.T) {
	svc, repo := newW1UserSvc(&w1FakeRevoker{})
	svc.WithSessionEnder(&fakeSessionEnder{err: errors.New("db down")})
	w1Seed(repo, "t1", "u1", false, "active", nil)

	pw := "Str0ng-Pass-2026!"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{Password: &pw}); err == nil {
		t.Fatal("password set succeeded although ending the sessions failed — the old credential would stay live")
	}
}

var _ sessionEnder = (*AuthService)(nil) // AuthService.LogoutEverywhere is the production ender
