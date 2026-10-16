package services

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/notify"
)

// ----------------------------------------------------------------------------
// e5FakeNotifier records enqueued events for assertion; optionally panics to
// test the fail-open contract on the alert path.
// ----------------------------------------------------------------------------

type e5FakeNotifier struct {
	mu     sync.Mutex
	events []notify.SecurityEvent
	panics bool // when true, EnqueueSecurityAlert panics (fail-open probe)
}

func (f *e5FakeNotifier) EnqueueSecurityAlert(_ context.Context, evt notify.SecurityEvent) {
	if f.panics {
		panic("e5FakeNotifier: deliberate panic")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, evt)
}

func (f *e5FakeNotifier) recorded() []notify.SecurityEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]notify.SecurityEvent, len(f.events))
	copy(out, f.events)
	return out
}

// ----------------------------------------------------------------------------
// raceLoserRefreshRepo is a minimal refreshRepo implementation for the benign
// double-submit test. It models the exact step-3 race:
//   - FindLiveByHash: always returns a live row (simulates the goroutine passed
//     step 2a BEFORE the winner revoked the token).
//   - FindByHashIncludingRevoked: never called on the step-3 path (theft check
//     only runs when FindLiveByHash returns ErrRefreshTokenNotFound).
//   - Rotate: always returns (false, nil) — simulates losing the atomic claim
//     (the winner already stamped revoked_at).
//   - RevokeAllForUser / EndAllSessions / EndSession: record any call so the test can assert
//     NO family kill of either flavour was made (#310 moved the kill to EndAllSessions).
// ----------------------------------------------------------------------------

type raceLoserRefreshRepo struct {
	liveRow        *auth.RefreshToken
	mu             sync.Mutex
	revokeAllCalls int
	endAllCalls    int
	endCalls       int
	storedRows     map[string]*auth.RefreshToken
}

func newRaceLoserRepo(liveRow *auth.RefreshToken) *raceLoserRefreshRepo {
	return &raceLoserRefreshRepo{
		liveRow:    liveRow,
		storedRows: map[string]*auth.RefreshToken{},
	}
}

func (r *raceLoserRefreshRepo) key(tenantID, hash string) string { return tenantID + "\x00" + hash }

func (r *raceLoserRefreshRepo) Store(_ context.Context, rt *auth.RefreshToken) error {
	cp := *rt
	r.storedRows[r.key(rt.TenantID, rt.TokenHash)] = &cp
	return nil
}

// FindLiveByHash always returns the pre-seeded live row regardless of revocation state —
// this simulates the goroutine passing step 2a before the concurrent winner revokes the token.
func (r *raceLoserRefreshRepo) FindLiveByHash(_ context.Context, _, _ string) (*auth.RefreshToken, error) {
	if r.liveRow == nil {
		return nil, auth.ErrRefreshTokenNotFound
	}
	cp := *r.liveRow
	return &cp, nil
}

// FindByHashIncludingRevoked is the theft-detection probe in step 2a. It is only called when
// FindLiveByHash returns ErrRefreshTokenNotFound — on the race-loser path FindLiveByHash
// succeeds, so this method should never be reached. Return not-found defensively.
func (r *raceLoserRefreshRepo) FindByHashIncludingRevoked(_ context.Context, _, _ string) (*auth.RefreshToken, error) {
	return nil, auth.ErrRefreshTokenNotFound
}

// RevokeByHash returns (false, nil) — simulates the loser that lost the atomic UPDATE.
func (r *raceLoserRefreshRepo) RevokeByHash(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

// Rotate loses the claim the same way (false, nil): nothing is inserted.
func (r *raceLoserRefreshRepo) Rotate(_ context.Context, _, _ string, _ *auth.RefreshToken) (bool, error) {
	return false, nil
}

func (r *raceLoserRefreshRepo) RevokeAllForUser(_ context.Context, _, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.revokeAllCalls++
	return nil
}

// ----------------------------------------------------------------------------
// Helpers shared by E5 tests.
// ----------------------------------------------------------------------------

const e5TestPassword = "E5-test-pw-secure!"

// newE5AuthSvc returns an AuthService wired with the given refreshRepo implementation and
// notifier. It seeds tenant "acme" (id t-1) with one active admin alice@acme.test (id u-1).
func newE5AuthSvc(t *testing.T, repo refreshRepo, n notify.Notifier) *AuthService {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(e5TestPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt hash for E5 test: %v", err)
	}
	alice := &auth.User{
		ID:           "u-1",
		TenantID:     "t-1",
		Email:        "alice@acme.test",
		PasswordHash: string(hash),
		Status:       "active",
		IsAdmin:      true,
	}
	users := &fakeUserRepo{
		tenants: map[string]*auth.Tenant{
			"acme": {ID: "t-1", Slug: "acme", Name: "Acme", Status: "active"},
		},
		users: map[string]*auth.User{
			"t-1\x00alice@acme.test": alice,
		},
		byID: map[string]*auth.User{
			"t-1\x00u-1": alice,
		},
	}
	tokens, err := auth.NewTokenService("e5-secret-test-key-00000000000", 15*time.Second, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("token service: %v", err)
	}
	return NewAuthService(users, repo, tokens).WithNotifier(n)
}

// loginE5 logs in as alice and returns the raw refresh token.
func loginE5(t *testing.T, svc *AuthService) string {
	t.Helper()
	out, err := svc.Login(context.Background(), LoginInput{
		Tenant: "acme", Email: "alice@acme.test", Password: e5TestPassword,
	})
	if err != nil {
		t.Fatalf("E5 login: %v", err)
	}
	return out.RefreshToken
}

// ----------------------------------------------------------------------------
// E5 tests.
// ----------------------------------------------------------------------------

// TestE5_ReplayRevokedToken_KillsFamilyAndSendsAlert is the primary theft-signal test:
// a token that was previously rotated (revoked_at IS NOT NULL) is presented to Refresh.
// Refresh passes step 2a → finds no live row → theft check → finds a revoked row →
// kills the family + enqueues AlertRefreshReuse + returns ErrInvalidRefresh.
func TestE5_ReplayRevokedToken_KillsFamilyAndSendsAlert(t *testing.T) {
	n := &e5FakeNotifier{}
	// Use the standard fakeRefreshRepo (supports FindByHashIncludingRevoked).
	repo := newFakeRefreshRepo()
	svc := newE5AuthSvc(t, repo, n)

	// Login → rotate once → original token is now revoked.
	raw := loginE5(t, svc)
	if _, err := svc.Refresh(context.Background(), raw); err != nil {
		t.Fatalf("first Refresh (rotation): %v", err)
	}
	// Pre-condition: original token row must be revoked.
	hash := auth.HashRefreshToken(raw)
	rt, ok := repo.rows[repo.key("t-1", hash)]
	if !ok || rt.RevokedAt == nil {
		t.Fatal("pre-condition: original token should be revoked after rotation")
	}

	// Replay the now-dead token — theft signal.
	_, err := svc.Refresh(context.Background(), raw)
	if !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("replay of revoked token: err = %v; want ErrInvalidRefresh", err)
	}

	// The family (all tokens for u-1 in t-1) must now be revoked.
	// After RevokeAllForUser, every row for this user should have RevokedAt set.
	repo.mu.Lock()
	anyLive := false
	for _, row := range repo.rows {
		if row.TenantID == "t-1" && row.UserID == "u-1" && row.RevokedAt == nil {
			anyLive = true
		}
	}
	repo.mu.Unlock()
	if anyLive {
		t.Error("family kill: expected all tokens for u-1/t-1 to be revoked after theft detection")
	}

	// Exactly one alert enqueued with the correct fields.
	evts := n.recorded()
	if len(evts) != 1 {
		t.Fatalf("enqueued %d alerts; want exactly 1 refresh_reuse alert", len(evts))
	}
	e := evts[0]
	if e.Type != notify.AlertRefreshReuse {
		t.Errorf("alert type = %q; want %q", e.Type, notify.AlertRefreshReuse)
	}
	if e.TenantID != "t-1" {
		t.Errorf("alert tenant = %q; want t-1", e.TenantID)
	}
	if e.UserID != "u-1" {
		t.Errorf("alert user = %q; want u-1", e.UserID)
	}
	if e.Email != "alice@acme.test" {
		t.Errorf("alert email = %q; want alice@acme.test", e.Email)
	}
}

// TestE5_BenignRaceLoser_RejectOnlyNoAlert tests the step-3 double-submit race:
// FindLiveByHash returned a live row (the goroutine passed step 2a before rotation),
// but RevokeByHash returned false (another goroutine won the atomic UPDATE first).
// Expected behavior: reject with ErrInvalidRefresh, NO family kill, NO alert.
//
// The raceLoserRefreshRepo drives this path deterministically without relying on
// real concurrency: FindLiveByHash always returns a live-looking row, RevokeByHash
// always returns (false, nil), and FindByHashIncludingRevoked is never reached.
func TestE5_BenignRaceLoser_RejectOnlyNoAlert(t *testing.T) {
	n := &e5FakeNotifier{}

	// Build a live-looking row for the token we will "present". We set an ExpiresAt in
	// the future and UserID/TenantID matching our seeded user. TokenHash is unused by the
	// controllable methods but set for completeness.
	liveRow := &auth.RefreshToken{
		ID:        "rt-race-1",
		UserID:    "u-1",
		TenantID:  "t-1",
		TokenHash: "placeholder-hash",
		ExpiresAt: time.Now().Add(7 * 24 * time.Hour),
	}
	repo := newRaceLoserRepo(liveRow)
	svc := newE5AuthSvc(t, repo, n)

	// We need a real refresh JWT to pass step 1 (signature verification). Issue one via Login.
	// The raceLoserRepo.Store captures the issued token row; FindLiveByHash returns our
	// liveRow regardless. We only need the raw JWT to pass step 1.
	raw := loginE5(t, svc)

	// Make the liveRow's UserID/TenantID match what the JWT's claims will be so the rest of
	// Refresh (tenant gate, user gate) works. The row returned by FindLiveByHash drives the
	// subsequent GetUserByID / GetTenantByID calls via rt.TenantID / rt.UserID.
	repo.liveRow.TenantID = "t-1"
	repo.liveRow.UserID = "u-1"

	// Call Refresh — step 2a finds the live row (no theft check), step 3 loses the
	// atomic UPDATE (RevokeByHash returns false), step 3 returns ErrInvalidRefresh.
	_, err := svc.Refresh(context.Background(), raw)
	if !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("race-loser path: err = %v; want ErrInvalidRefresh", err)
	}

	// No family kill.
	repo.mu.Lock()
	calls, endAll, end := repo.revokeAllCalls, repo.endAllCalls, repo.endCalls
	repo.mu.Unlock()
	if calls != 0 || endAll != 0 || end != 0 {
		t.Fatalf("benign race killed sessions: RevokeAllForUser=%d EndAllSessions=%d EndSession=%d; want all 0", calls, endAll, end)
	}

	// No alert.
	if evts := n.recorded(); len(evts) != 0 {
		t.Fatalf("enqueued %d alerts on benign race; want 0", len(evts))
	}
}

// TestE5_NeverIssuedToken_NoAlert verifies that a token that was never stored (no DB row
// at all, not even a revoked one) does not trigger family revocation or an alert.
// A legitimately forgotten / externally minted token is not a theft signal.
func TestE5_NeverIssuedToken_NoAlert(t *testing.T) {
	n := &e5FakeNotifier{}
	repo := newFakeRefreshRepo()
	svc := newE5AuthSvc(t, repo, n)

	// Issue a token JWT with the right claims but do NOT store a DB row.
	sub := auth.Subject{UserID: "u-1", TenantID: "t-1"}
	stray, err := svc.tokens.IssueRefreshToken(sub)
	if err != nil {
		t.Fatalf("issue stray refresh token: %v", err)
	}

	_, err = svc.Refresh(context.Background(), stray)
	if !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("never-issued token: err = %v; want ErrInvalidRefresh", err)
	}

	// No family kill, no alert — the row simply never existed.
	if evts := n.recorded(); len(evts) != 0 {
		t.Fatalf("enqueued %d alerts for never-issued token; want 0", len(evts))
	}
}

// TestE5_NotifierPanic_FamilyKillAndRejectStillHappen verifies the fail-open contract:
// a panicking notifier must not prevent the family revoke or the ErrInvalidRefresh response
// when a theft signal is detected.
func TestE5_NotifierPanic_FamilyKillAndRejectStillHappen(t *testing.T) {
	n := &e5FakeNotifier{panics: true}
	core, logs := observer.New(zapcore.WarnLevel)
	obsLogger := zap.New(core)

	repo := newFakeRefreshRepo()
	svc := newE5AuthSvc(t, repo, n).WithLogger(obsLogger)

	// Login → rotate once → original token is revoked.
	raw := loginE5(t, svc)
	if _, err := svc.Refresh(context.Background(), raw); err != nil {
		t.Fatalf("first rotation: %v", err)
	}

	// Replay the dead token — notifier will panic during alert emission.
	_, err := svc.Refresh(context.Background(), raw)
	if !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("panic notifier: err = %v; want ErrInvalidRefresh", err)
	}

	// Family must still be dead (the revoke happened before the panic).
	repo.mu.Lock()
	anyLive := false
	for _, row := range repo.rows {
		if row.TenantID == "t-1" && row.UserID == "u-1" && row.RevokedAt == nil {
			anyLive = true
		}
	}
	repo.mu.Unlock()
	if anyLive {
		t.Error("family kill did not happen despite notifier panic — revoke must precede alert")
	}

	// The panic must have been logged (fail-open audit trail).
	found := false
	for _, entry := range logs.All() {
		if entry.Level == zapcore.WarnLevel {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected a Warn log for the recovered notifier panic; none emitted")
	}
}

// TestE5_UserService_NotifierPanicLogged verifies that UserService.enqueueAlert also logs
// recovered panics via zap (item 1 gap — the zap-log-on-recover contract). Uses UpdateUser
// with a password change to trigger E1 with a panicking notifier.
func TestE5_UserService_NotifierPanicLogged(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	obsLogger := zap.New(core)

	n := &notifyFakeNotifier{panicMsg: "simulated panic"}
	repo := newFakeUserCRUDRepo()
	svc := NewUserService(repo).WithNotifier(n).WithLogger(obsLogger)
	w1Seed(repo, "t1", "u1", false, "active", nil)

	pass := "Str0ngNewP@ss!"
	v, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{Password: &pass})
	if err != nil {
		t.Fatalf("UpdateUser failed due to notifier panic (must be fail-open): %v", err)
	}
	if v == nil {
		t.Fatal("UpdateUser returned nil view despite success")
	}

	// A Warn log must have been emitted for the recovered panic.
	found := false
	for _, entry := range logs.All() {
		if entry.Level == zapcore.WarnLevel {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected Warn log for notifier panic from UserService.enqueueAlert; none found")
	}
}
