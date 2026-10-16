package services

// Queue #45 session-list-kill-b (../specs/055-session-list-kill/spec.md): every mint site stamps the
// session id into the tokens (`sid`), the list marks the caller's own session, one session can be
// ended without touching the others, an ended session's token is not a theft signal, and the
// session guard (the auth stage's "ended?" reader) is invalidated on every end.

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"nexus/platform/internal/auth"
)

// --- fakes ---------------------------------------------------------------------------------------

// EndSession / EndAllSessions / IsSessionEnded mirror the real repository on the in-memory store.
func (f *fakeRefreshRepo) EndSession(_ context.Context, tenantID, userID, sessionID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ended := false
	for _, rt := range f.rows {
		if rt.TenantID == tenantID && rt.UserID == userID && rt.SessionID == sessionID && rt.RevokedAt == nil {
			f.endLocked(rt)
			ended = true
		}
	}
	return ended, nil
}

func (f *fakeRefreshRepo) EndAllSessions(_ context.Context, tenantID, userID, except string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]bool{}
	var sids []string
	for _, rt := range f.rows {
		if rt.TenantID == tenantID && rt.UserID == userID && rt.RevokedAt == nil && (except == "" || rt.SessionID != except) {
			f.endLocked(rt)
			if !seen[rt.SessionID] {
				seen[rt.SessionID] = true
				sids = append(sids, rt.SessionID)
			}
		}
	}
	sort.Strings(sids)
	return sids, nil
}

func (f *fakeRefreshRepo) IsSessionEnded(_ context.Context, tenantID, userID, sessionID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rt := range f.rows {
		if rt.TenantID == tenantID && rt.UserID == userID && rt.SessionID == sessionID &&
			rt.RevokeReason != nil && *rt.RevokeReason == auth.RevokeReasonEnded {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeRefreshRepo) endLocked(rt *auth.RefreshToken) {
	t := f.now()
	reason := auth.RevokeReasonEnded
	rt.RevokedAt, rt.RevokeReason = &t, &reason
}

func (r *raceLoserRefreshRepo) EndSession(context.Context, string, string, string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endCalls++
	return false, nil
}
func (r *raceLoserRefreshRepo) EndAllSessions(context.Context, string, string, string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endAllCalls++
	return nil, nil
}

// recordingGuard is a session guard that answers from a set and records invalidations.
type recordingGuard struct {
	mu          sync.Mutex
	ended       map[string]bool
	invalidated []string
	err         error
}

func (g *recordingGuard) SessionEnded(_ context.Context, _, _, sessionID string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ended[sessionID], g.err
}

func (g *recordingGuard) Invalidate(_, _, sessionID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.invalidated = append(g.invalidated, sessionID)
}

// --- helpers -------------------------------------------------------------------------------------

func sidOfAccess(t *testing.T, svc *AuthService, access string) string {
	t.Helper()
	c, err := svc.tokens.VerifyAccessToken(access)
	if err != nil {
		t.Fatalf("verify access: %v", err)
	}
	return c.SessionID
}

func sidOfRefresh(t *testing.T, svc *AuthService, refresh string) string {
	t.Helper()
	c, err := svc.tokens.VerifyRefreshToken(refresh)
	if err != nil {
		t.Fatalf("verify refresh: %v", err)
	}
	return c.SessionID
}

// liveRowBySID returns the live row of a session, or nil.
func (f *fakeRefreshRepo) liveRowBySID(sid string) *auth.RefreshToken {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rt := range f.rows {
		if rt.SessionID == sid && rt.RevokedAt == nil {
			cp := *rt
			return &cp
		}
	}
	return nil
}

func loginAlice(t *testing.T, svc *AuthService, password string) *LoginOutput {
	t.Helper()
	out, err := svc.Login(context.Background(), LoginInput{Tenant: "acme", Email: "alice@acme.test", Password: password})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return out
}

// --- the sid claim at every mint site ------------------------------------------------------------

func TestLogin_TokensCarryTheSessionID(t *testing.T) {
	svc, password := newTestService(t)
	refresh := svc.refresh.(*fakeRefreshRepo)
	out := loginAlice(t, svc, password)

	sid := sidOfAccess(t, svc, out.AccessToken)
	if sid == "" || refresh.liveRowBySID(sid) == nil {
		t.Fatalf("access sid = %q; want the id of the session row just stored", sid)
	}
	if got := sidOfRefresh(t, svc, out.RefreshToken); got != sid {
		t.Errorf("refresh sid = %q, access sid = %q; want the same session", got, sid)
	}
}

func TestRefresh_KeepsTheSidClaimAcrossRotation(t *testing.T) {
	svc, password := newTestService(t)
	out := loginAlice(t, svc, password)
	sid := sidOfAccess(t, svc, out.AccessToken)

	rot, err := svc.Refresh(context.Background(), out.RefreshToken)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := sidOfAccess(t, svc, rot.AccessToken); got != sid {
		t.Errorf("sid after rotation = %q, want %q (the 'phiên này' label must not move every refresh)", got, sid)
	}
	if got := sidOfRefresh(t, svc, rot.RefreshToken); got != sid {
		t.Errorf("rotated refresh sid = %q, want %q", got, sid)
	}
}

func TestCompleteMFALogin_TokensCarryTheSessionID(t *testing.T) {
	f := newMFAFixture(t)
	secret, _, step := f.enrollAndActivate(t)
	ctx := context.Background()
	pending, err := f.svc.Login(ctx, LoginInput{Tenant: "acme", Email: f.email, Password: f.password})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	out, err := f.svc.CompleteMFALogin(ctx, pending.PendingToken, codeAfterStep(t, secret, step))
	if err != nil {
		t.Fatalf("CompleteMFALogin: %v", err)
	}
	sid := sidOfAccess(t, f.svc, out.AccessToken)
	if sid == "" || f.svc.refresh.(*fakeRefreshRepo).liveRowBySID(sid) == nil {
		t.Fatalf("MFA access sid = %q; want the stored session's id", sid)
	}
}

func TestSSOSession_TokensCarryTheSessionID(t *testing.T) {
	svc, _ := newTestService(t)
	u := &auth.User{ID: "u-1", TenantID: "t-1", Email: "alice@acme.test", Status: "active"}
	out, err := svc.issueSessionForSubject(context.Background(), u)
	if err != nil {
		t.Fatalf("issueSessionForSubject: %v", err)
	}
	sid := sidOfAccess(t, svc, out.AccessToken)
	if sid == "" || svc.refresh.(*fakeRefreshRepo).liveRowBySID(sid) == nil {
		t.Fatalf("SSO access sid = %q; want the stored session's id", sid)
	}
}

// --- current ------------------------------------------------------------------------------------

func TestListSessions_MarksOnlyTheCallersSessionCurrent(t *testing.T) {
	svc, password := newTestService(t)
	a := loginAlice(t, svc, password)
	loginAlice(t, svc, password)
	sidA := sidOfAccess(t, svc, a.AccessToken)

	got, err := svc.ListSessions(context.Background(), "t-1", "u-1", sidA)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	current := 0
	for _, s := range got {
		if s.Current {
			current++
			if s.ID != sidA {
				t.Errorf("row %q marked current; want only %q", s.ID, sidA)
			}
		}
	}
	if len(got) != 2 || current != 1 {
		t.Fatalf("rows=%d current=%d (%+v), want 2 rows with exactly one current", len(got), current, got)
	}
	// No caller session (the admin looking at someone else) ⇒ nothing is "phiên này".
	got, _ = svc.ListSessions(context.Background(), "t-1", "u-1", "")
	for _, s := range got {
		if s.Current {
			t.Errorf("row %q current with no caller session", s.ID)
		}
	}
}

// --- ending one session -------------------------------------------------------------------------

func TestEndSession_EndsThatSessionOnly(t *testing.T) {
	svc, password := newTestService(t)
	guard := &recordingGuard{}
	svc = svc.WithSessionGuard(guard)
	a := loginAlice(t, svc, password)
	b := loginAlice(t, svc, password)
	sidB := sidOfAccess(t, svc, b.AccessToken)

	if err := svc.EndSession(context.Background(), "t-1", "u-1", sidB); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if _, err := svc.Refresh(context.Background(), b.RefreshToken); !errors.Is(err, ErrInvalidRefresh) {
		t.Errorf("ended session still refreshes: err = %v", err)
	}
	if _, err := svc.Refresh(context.Background(), a.RefreshToken); err != nil {
		t.Errorf("the OTHER session was hurt: refresh A = %v", err)
	}
	if len(guard.invalidated) != 1 || guard.invalidated[0] != sidB {
		t.Errorf("guard invalidations = %v, want exactly [%s] so the auth stage sees it on the next request", guard.invalidated, sidB)
	}
	ended, _ := svc.refresh.(*fakeRefreshRepo).IsSessionEnded(context.Background(), "t-1", "u-1", sidB)
	if !ended {
		t.Error("session row not marked ended")
	}
}

func TestEndSession_NotTheCallersLiveSessionIsNotFound(t *testing.T) {
	svc, password := newTestService(t)
	refresh := svc.refresh.(*fakeRefreshRepo)
	a := loginAlice(t, svc, password)
	sidA := sidOfAccess(t, svc, a.AccessToken)
	if err := refresh.Store(context.Background(), &auth.RefreshToken{ID: "r-bob", SessionID: "s-bob", TenantID: "t-1", UserID: "u-2", TokenHash: "h-bob", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for _, tc := range []struct{ name, tenant, user, sid string }{
		{"another user's session", "t-1", "u-1", "s-bob"},
		{"my session under another tenant", "t-2", "u-1", sidA},
		{"unknown id", "t-1", "u-1", "nope"},
		{"blank id never means 'all'", "t-1", "u-1", "  "},
	} {
		err := svc.EndSession(context.Background(), tc.tenant, tc.user, tc.sid)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "SESSION_NOT_FOUND" {
			t.Errorf("%s: err = %v, want SESSION_NOT_FOUND", tc.name, err)
		}
	}
	if refresh.liveRowBySID(sidA) == nil || refresh.liveRowBySID("s-bob") == nil {
		t.Fatal("a refused EndSession still ended something")
	}
}

// FR-009: the device whose session was ended presents its (now dead) refresh token — that is the
// expected next step of that device, not a stolen token. No family kill, no alert.
func TestRefresh_EndedSessionTokenIsNotATheftSignal(t *testing.T) {
	svc, password := newTestService(t)
	notifier := &e5FakeNotifier{}
	svc = svc.WithNotifier(notifier)
	a := loginAlice(t, svc, password)
	b := loginAlice(t, svc, password)
	if err := svc.EndSession(context.Background(), "t-1", "u-1", sidOfAccess(t, svc, b.AccessToken)); err != nil {
		t.Fatalf("EndSession: %v", err)
	}

	if _, err := svc.Refresh(context.Background(), b.RefreshToken); !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("refresh of the ended session: err = %v, want ErrInvalidRefresh", err)
	}
	if svc.refresh.(*fakeRefreshRepo).liveRowBySID(sidOfAccess(t, svc, a.AccessToken)) == nil {
		t.Fatal("reuse detection fired: the session that did the ending was killed too")
	}
	if ev := notifier.recorded(); len(ev) != 0 {
		t.Errorf("security alert sent for an ended session: %+v", ev)
	}
}

// A ROTATED token replayed is still the theft signal (E5 unchanged).
func TestRefresh_RotatedTokenReplayStillKillsTheFamily(t *testing.T) {
	svc, password := newTestService(t)
	a := loginAlice(t, svc, password)
	b := loginAlice(t, svc, password)
	if _, err := svc.Refresh(context.Background(), a.RefreshToken); err != nil {
		t.Fatalf("rotate A: %v", err)
	}
	if _, err := svc.Refresh(context.Background(), a.RefreshToken); !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("replay: %v", err)
	}
	if svc.refresh.(*fakeRefreshRepo).liveRowBySID(sidOfAccess(t, svc, b.AccessToken)) != nil {
		t.Error("replaying a rotated token no longer kills the family (E5 regression)")
	}
}

// FR-012: /auth/logout ENDS the session of the presented token. Two tabs of one browser share the
// session; tab A logs out, tab B still holds the same refresh token in memory. B's access token must
// die at once (guard invalidated) and B replaying the logged-out token must NOT read as theft — an
// ordinary revoke used to make it one: E5 killed every device of the user and mailed an alert.
func TestLogout_EndsTheSessionSoTheOtherTabIsNotATheftSignal(t *testing.T) {
	svc, password := newTestService(t)
	notifier := &e5FakeNotifier{}
	guard := &recordingGuard{}
	svc = svc.WithNotifier(notifier).WithSessionGuard(guard)
	other := loginAlice(t, svc, password) // another device of the same user
	tab := loginAlice(t, svc, password)   // the browser whose two tabs share this session
	sid := sidOfAccess(t, svc, tab.AccessToken)

	if err := svc.Logout(context.Background(), tab.RefreshToken); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	repo := svc.refresh.(*fakeRefreshRepo)
	if ended, _ := repo.IsSessionEnded(context.Background(), "t-1", "u-1", sid); !ended {
		t.Error("logout did not END the session: the other tab's access token keeps working until it expires")
	}
	if len(guard.invalidated) != 1 || guard.invalidated[0] != sid {
		t.Errorf("guard invalidated %v, want [%s] (the other tab refused on its next request)", guard.invalidated, sid)
	}

	if _, err := svc.Refresh(context.Background(), tab.RefreshToken); !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("other tab refreshing the logged-out token: err = %v, want ErrInvalidRefresh", err)
	}
	if ev := notifier.recorded(); len(ev) != 0 {
		t.Errorf("security alert sent for a logged-out session: %+v", ev)
	}
	if repo.liveRowBySID(sidOfAccess(t, svc, other.AccessToken)) == nil {
		t.Error("reuse detection fired: the user's other device was logged out too")
	}
}

// A token minted before 055 carries no `sid`; its row was backfilled with session_id = id. Logout
// still ends that session (found through the row), not just revokes it.
func TestLogout_PreSessionTokenEndsItsRow(t *testing.T) {
	svc, password := newTestService(t)
	notifier := &e5FakeNotifier{}
	svc = svc.WithNotifier(notifier)
	loginAlice(t, svc, password)
	repo := svc.refresh.(*fakeRefreshRepo)
	legacy, err := svc.tokens.IssueRefreshToken(auth.Subject{UserID: "u-1", TenantID: "t-1"}) // no SessionID
	if err != nil {
		t.Fatalf("issue legacy token: %v", err)
	}
	repo.mu.Lock()
	for k, rt := range repo.rows { // the one row now answers to the legacy token
		delete(repo.rows, k)
		rt.TokenHash = auth.HashRefreshToken(legacy)
		rt.SessionID = rt.ID // the migration's backfill
		repo.rows[repo.key(rt.TenantID, rt.TokenHash)] = rt
	}
	repo.mu.Unlock()

	if err := svc.Logout(context.Background(), legacy); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if rows := repo.liveRowsFor("t-1", "u-1"); len(rows) != 0 {
		t.Fatalf("live rows after logout = %d, want 0", len(rows))
	}
	if _, err := svc.Refresh(context.Background(), legacy); !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("refresh after logout: %v", err)
	}
	if ev := notifier.recorded(); len(ev) != 0 {
		t.Errorf("security alert sent for a logged-out legacy session: %+v", ev)
	}
}

func TestLogoutEverywhere_EndsEverySessionForTheGuard(t *testing.T) {
	svc, password := newTestService(t)
	guard := &recordingGuard{}
	svc = svc.WithSessionGuard(guard)
	a := loginAlice(t, svc, password)
	b := loginAlice(t, svc, password)

	if err := svc.LogoutEverywhere(context.Background(), "t-1", "u-1"); err != nil {
		t.Fatalf("LogoutEverywhere: %v", err)
	}
	want := []string{sidOfAccess(t, svc, a.AccessToken), sidOfAccess(t, svc, b.AccessToken)}
	sort.Strings(want)
	got := append([]string(nil), guard.invalidated...)
	sort.Strings(got)
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("invalidated %v, want every session %v", got, want)
	}
	for _, sid := range want {
		if ended, _ := svc.refresh.(*fakeRefreshRepo).IsSessionEnded(context.Background(), "t-1", "u-1", sid); !ended {
			t.Errorf("session %s not ended by logout-all", sid)
		}
	}
}

func TestMe_EndedSessionIsUnauthorized(t *testing.T) {
	svc, password := newTestService(t)
	guard := &recordingGuard{ended: map[string]bool{}}
	svc = svc.WithSessionGuard(guard)
	out := loginAlice(t, svc, password)
	if _, err := svc.Me(context.Background(), out.AccessToken); err != nil {
		t.Fatalf("Me before: %v", err)
	}
	guard.ended[sidOfAccess(t, svc, out.AccessToken)] = true
	if _, err := svc.Me(context.Background(), out.AccessToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Me with an ended session: err = %v, want ErrUnauthorized", err)
	}
}

// --- the cached guard ---------------------------------------------------------------------------

type countingEndedReader struct {
	mu    sync.Mutex
	ended bool
	calls int
}

func (r *countingEndedReader) IsSessionEnded(context.Context, string, string, string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.ended, nil
}

func TestCachedSessionGuard_CachesUntilInvalidated(t *testing.T) {
	inner := &countingEndedReader{}
	g := NewCachedSessionGuard(inner, time.Minute)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if ended, err := g.SessionEnded(ctx, "t-1", "u-1", "s-1"); err != nil || ended {
			t.Fatalf("SessionEnded = %v, %v; want false", ended, err)
		}
	}
	if inner.calls != 1 {
		t.Fatalf("reader calls = %d, want 1 (cached)", inner.calls)
	}
	inner.ended = true
	g.Invalidate("t-1", "u-1", "s-1")
	if ended, _ := g.SessionEnded(ctx, "t-1", "u-1", "s-1"); !ended {
		t.Fatal("after Invalidate the ended state was not reloaded")
	}
	// Keys are tenant- and user-scoped: the same session id elsewhere is a different entry.
	if _, _ = g.SessionEnded(ctx, "t-2", "u-1", "s-1"); inner.calls != 3 {
		t.Errorf("reader calls = %d, want 3 (other tenant is its own key)", inner.calls)
	}
	var nilGuard *CachedSessionGuard
	nilGuard.Invalidate("t", "u", "s") // nil-safe like CachedUserLoader
}
