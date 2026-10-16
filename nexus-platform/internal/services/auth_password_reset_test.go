package services

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/notify"
)

// --- test doubles -------------------------------------------------------------------------

// fakeResetRepo is an in-memory stand-in for auth.PasswordResetTokenRepository. FindByHash is
// tenant-agnostic (scans all rows by hash, like the real UNIQUE(token_hash) lookup);
// ConsumeByHash is tenant-pinned and mutex-guarded so the concurrency (single-use) test
// exercises the same one-winner contract as the real conditional UPDATE.
type fakeResetRepo struct {
	mu   sync.Mutex
	rows map[string]*auth.PasswordResetToken // keyed by token_hash (globally unique, mirrors UNIQUE index)
}

func newFakeResetRepo() *fakeResetRepo {
	return &fakeResetRepo{rows: map[string]*auth.PasswordResetToken{}}
}

func (f *fakeResetRepo) Store(_ context.Context, prt *auth.PasswordResetToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.rows[prt.TokenHash]; exists {
		return errors.New("fakeResetRepo: duplicate token_hash (UNIQUE violation)")
	}
	cp := *prt
	f.rows[prt.TokenHash] = &cp
	return nil
}

func (f *fakeResetRepo) FindByHash(_ context.Context, tokenHash string) (*auth.PasswordResetToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prt, ok := f.rows[tokenHash]
	if !ok {
		return nil, auth.ErrResetTokenInvalid
	}
	cp := *prt
	return &cp, nil
}

func (f *fakeResetRepo) ConsumeByHash(_ context.Context, tenantID, tokenHash string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prt, ok := f.rows[tokenHash]
	if !ok || prt.TenantID != tenantID || prt.UsedAt != nil {
		return false, nil
	}
	now := time.Now()
	if !prt.ExpiresAt.After(now) {
		return false, nil
	}
	prt.UsedAt = &now
	return true, nil
}

func (f *fakeResetRepo) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// spySender is an in-memory notify.EmailSender that records every Mail it is asked to send.
type spySender struct {
	mu    sync.Mutex
	mails []notify.Mail
	err   error // when non-nil, Send returns this error instead of recording
}

func (s *spySender) Send(_ context.Context, m notify.Mail) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.mails = append(s.mails, m)
	return nil
}

func (s *spySender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.mails)
}

func (s *spySender) last() notify.Mail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mails[len(s.mails)-1]
}

// spyNotifier is a synchronous, in-memory notify.Notifier that records every SecurityEvent
// enqueued. Unlike the real notify.AsyncDispatcher it does no async delivery — it just
// records, so a test can assert the exact confirmation event was enqueued without racing an
// async worker pool.
type spyNotifier struct {
	mu     sync.Mutex
	events []notify.SecurityEvent
}

func (n *spyNotifier) EnqueueSecurityAlert(_ context.Context, evt notify.SecurityEvent) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, evt)
}

func (n *spyNotifier) snapshot() []notify.SecurityEvent {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]notify.SecurityEvent, len(n.events))
	copy(out, n.events)
	return out
}

// newResetTestService builds an AuthService wired for the reset-password flow: fake user repo
// (seeded with tenant "acme"/user alice@acme.test, mirroring newTestService), fake refresh repo,
// a fresh fake reset repo, and a spy mailer with a fixed URL base.
func newResetTestService(t *testing.T) (*AuthService, *fakeResetRepo, *spySender) {
	t.Helper()
	svc, _ := newTestService(t)
	repo := newFakeResetRepo()
	sender := &spySender{}
	svc.WithResetRepo(repo).
		WithResetMailer(sender, "reset@nexus.test", "https://app.nexus.test/reset").
		WithMailDispatcher(func(fn func()) { fn() }) // synchronous in tests for deterministic spy assertions
	return svc, repo, sender
}

// --- RequestPasswordReset (anti-enumeration + happy path) ----------------------------------

func TestRequestPasswordReset_UnknownEmail_NoOpAndOK(t *testing.T) {
	svc, repo, sender := newResetTestService(t)

	err := svc.RequestPasswordReset(context.Background(), "acme", "nobody@acme.test")
	if err != nil {
		t.Fatalf("expected nil error for unknown email (anti-enumeration), got %v", err)
	}
	if repo.count() != 0 {
		t.Errorf("expected 0 token rows stored for unknown email, got %d", repo.count())
	}
	if sender.count() != 0 {
		t.Errorf("expected 0 emails sent for unknown email, got %d", sender.count())
	}
}

func TestRequestPasswordReset_UnknownTenant_NoOpAndOK(t *testing.T) {
	svc, repo, sender := newResetTestService(t)

	err := svc.RequestPasswordReset(context.Background(), "ghost-tenant", "alice@acme.test")
	if err != nil {
		t.Fatalf("expected nil error for unknown tenant (anti-enumeration), got %v", err)
	}
	if repo.count() != 0 {
		t.Errorf("expected 0 token rows stored for unknown tenant, got %d", repo.count())
	}
	if sender.count() != 0 {
		t.Errorf("expected 0 emails sent for unknown tenant, got %d", sender.count())
	}
}

func TestRequestPasswordReset_UnknownTenantAndUnknownEmail_IdenticalOutcome(t *testing.T) {
	// Both anti-enumeration branches must be indistinguishable from the caller's perspective:
	// same nil error, same zero side effects.
	svcA, repoA, senderA := newResetTestService(t)
	errA := svcA.RequestPasswordReset(context.Background(), "ghost-tenant", "whoever@acme.test")

	svcB, repoB, senderB := newResetTestService(t)
	errB := svcB.RequestPasswordReset(context.Background(), "acme", "nobody@acme.test")

	if errA != nil || errB != nil {
		t.Fatalf("expected both branches nil, got errA=%v errB=%v", errA, errB)
	}
	if repoA.count() != repoB.count() || repoA.count() != 0 {
		t.Errorf("expected identical zero-row outcome, got repoA=%d repoB=%d", repoA.count(), repoB.count())
	}
	if senderA.count() != senderB.count() || senderA.count() != 0 {
		t.Errorf("expected identical zero-send outcome, got senderA=%d senderB=%d", senderA.count(), senderB.count())
	}
}

func TestRequestPasswordReset_KnownUser_StoresHashedTokenAndSendsRawLink(t *testing.T) {
	svc, repo, sender := newResetTestService(t)

	if err := svc.RequestPasswordReset(context.Background(), "acme", "alice@acme.test"); err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	if repo.count() != 1 {
		t.Fatalf("expected exactly 1 token row stored, got %d", repo.count())
	}
	if sender.count() != 1 {
		t.Fatalf("expected exactly 1 email sent, got %d", sender.count())
	}

	var stored *auth.PasswordResetToken
	for _, row := range repo.rows {
		stored = row
	}
	mail := sender.last()

	if mail.To != "alice@acme.test" {
		t.Errorf("mail.To = %q, want alice@acme.test", mail.To)
	}
	if mail.From != "reset@nexus.test" {
		t.Errorf("mail.From = %q, want reset@nexus.test", mail.From)
	}
	if !strings.Contains(mail.Body, "https://app.nexus.test/reset?token=") {
		t.Fatalf("mail body does not contain the expected reset URL prefix: %q", mail.Body)
	}

	// Extract the raw token from the link and assert it is NOT the stored hash (the hash must
	// never be the value mailed to the user, and vice versa).
	idx := strings.Index(mail.Body, "?token=")
	if idx < 0 {
		t.Fatalf("no ?token= found in mail body")
	}
	rest := mail.Body[idx+len("?token="):]
	rawFromLink := strings.Fields(rest)[0] // token ends at first whitespace/newline
	if rawFromLink == "" {
		t.Fatal("empty raw token extracted from mail body")
	}
	if rawFromLink == stored.TokenHash {
		t.Error("raw token in the mailed link must not equal the stored hash")
	}
	if got := auth.HashRefreshToken(rawFromLink); got != stored.TokenHash {
		t.Errorf("HashRefreshToken(rawFromLink) = %q, want stored.TokenHash %q", got, stored.TokenHash)
	}
	if stored.TenantID != "t-1" || stored.UserID != "u-1" {
		t.Errorf("unexpected stored token identity: tenant=%q user=%q", stored.TenantID, stored.UserID)
	}
}

func TestRequestPasswordReset_DefaultAsyncDispatch_SendsEventually(t *testing.T) {
	// Verify the PRODUCTION default (WithMailDispatcher NOT called → real `go fn()`): the token is
	// stored synchronously but the reset email is dispatched OFF the request path (MED-1), so it
	// arrives after RequestPasswordReset has already returned.
	svc, _ := newTestService(t)
	repo := newFakeResetRepo()
	sender := &spySender{}
	svc.WithResetRepo(repo).WithResetMailer(sender, "reset@nexus.test", "https://app.nexus.test/reset")
	// NOTE: no WithMailDispatcher → fire-and-forget goroutine.

	if err := svc.RequestPasswordReset(context.Background(), "acme", "alice@acme.test"); err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	if repo.count() != 1 {
		t.Fatalf("expected the token row written synchronously, got %d", repo.count())
	}
	// The email is async; poll (bounded) for it.
	deadline := time.Now().Add(2 * time.Second)
	for sender.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if sender.count() != 1 {
		t.Fatalf("expected exactly 1 async reset email within the deadline, got %d", sender.count())
	}
}

func TestResetPassword_UserDisabledAfterIssue_Invalid(t *testing.T) {
	// LOW-2: a token issued while active must not complete a reset if the account is disabled
	// between issuance and consumption — and the token must NOT be consumed by the rejected attempt.
	svc, repo, sender := newResetTestService(t)
	raw := requestAndExtractToken(t, svc, sender, "acme", "alice@acme.test")

	svc.users.(*fakeUserRepo).users["t-1\x00alice@acme.test"].Status = "disabled"

	if err := svc.ResetPassword(context.Background(), raw, "Str0ng-Enough-Pw!"); !errors.Is(err, auth.ErrResetTokenInvalid) {
		t.Fatalf("expected ErrResetTokenInvalid for a token whose account was disabled after issuance, got %v", err)
	}
	for _, row := range repo.rows {
		if row.UsedAt != nil {
			t.Error("token must NOT be consumed when the account is disabled (reject before ConsumeByHash)")
		}
	}
}

func TestRequestPasswordReset_DisabledUser_NoOp(t *testing.T) {
	svc, repo, sender := newResetTestService(t)
	svc.users.(*fakeUserRepo).users["t-1\x00alice@acme.test"].Status = "disabled"

	if err := svc.RequestPasswordReset(context.Background(), "acme", "alice@acme.test"); err != nil {
		t.Fatalf("expected nil error for disabled user, got %v", err)
	}
	if repo.count() != 0 || sender.count() != 0 {
		t.Errorf("expected zero side effects for disabled user, got rows=%d sends=%d", repo.count(), sender.count())
	}
}

func TestRequestPasswordReset_MissingFields_ValidationError(t *testing.T) {
	svc, _, _ := newResetTestService(t)

	cases := []struct{ tenant, email string }{
		{"", "alice@acme.test"},
		{"acme", ""},
		{"", ""},
	}
	for _, c := range cases {
		err := svc.RequestPasswordReset(context.Background(), c.tenant, c.email)
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("tenant=%q email=%q: expected *ValidationError, got %v", c.tenant, c.email, err)
			continue
		}
		if ve.Code != "RESET_FIELD_REQUIRED" {
			t.Errorf("tenant=%q email=%q: code = %q, want RESET_FIELD_REQUIRED", c.tenant, c.email, ve.Code)
		}
	}
}

// --- ResetPassword -----------------------------------------------------------------------

// requestAndExtractToken performs a RequestPasswordReset and pulls the raw token out of the
// mailed link, so ResetPassword tests can drive the full loop without reaching into internals.
func requestAndExtractToken(t *testing.T, svc *AuthService, sender *spySender, tenant, email string) string {
	t.Helper()
	if err := svc.RequestPasswordReset(context.Background(), tenant, email); err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	mail := sender.last()
	idx := strings.Index(mail.Body, "?token=")
	if idx < 0 {
		t.Fatalf("no ?token= in mail body")
	}
	rest := mail.Body[idx+len("?token="):]
	return strings.Fields(rest)[0]
}

// TestResetPassword_ClearsLockout pins security MED-1: a valid reset token proves account
// control, so ResetPassword MUST clear a durable lockout — otherwise a locked-out victim's
// forgot-password flow succeeds yet login still 401s against a stale locked_until (permanent
// lockout DoS with no self-rescue).
func TestResetPassword_ClearsLockout(t *testing.T) {
	svc, _, sender := newResetTestService(t)
	raw := requestAndExtractToken(t, svc, sender, "acme", "alice@acme.test")

	// Lock the account out (as ~10 failed logins would).
	locked := time.Now().Add(15 * time.Minute).UTC()
	u := svc.users.(*fakeUserRepo).findUser("t-1", "u-1")
	if u == nil {
		t.Fatal("seed user not found")
	}
	u.FailedLoginCount = 10
	u.LockedUntil = &locked

	if err := svc.ResetPassword(context.Background(), raw, "Br4nd-New-Pw!"); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}

	u = svc.users.(*fakeUserRepo).findUser("t-1", "u-1")
	if u.FailedLoginCount != 0 || u.LockedUntil != nil {
		t.Fatalf("lockout not cleared after reset: count=%d lockedUntil=%v", u.FailedLoginCount, u.LockedUntil)
	}
	// And the new password must now log in (lock gone).
	if _, err := svc.Login(context.Background(), LoginInput{Tenant: "acme", Email: "alice@acme.test", Password: "Br4nd-New-Pw!"}); err != nil {
		t.Fatalf("login after reset+unlock failed: %v", err)
	}
}

func TestResetPassword_HappyPath_UpdatesHashRevokesSessionsAndConfirms(t *testing.T) {
	svc, repo, sender := newResetTestService(t)
	notifier := &spyNotifier{}
	svc.WithNotifier(notifier)
	raw := requestAndExtractToken(t, svc, sender, "acme", "alice@acme.test")

	// Establish a live session so we can assert it gets revoked.
	_, refresh := loginAndGetRefresh(t, svc, "s3cret-pw")

	const newPassword = "Br4nd-New-Pw!"
	if err := svc.ResetPassword(context.Background(), raw, newPassword); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}

	// Snapshot the notifier events RIGHT AFTER ResetPassword, before the revoked-refresh probe
	// below (which deliberately replays a now-dead token and legitimately triggers a SEPARATE
	// E5 refresh_reuse alert — that is correct product behavior, not part of what this test
	// asserts about the reset confirmation).
	eventsAfterReset := notifier.snapshot()

	// New password must authenticate; old one must not.
	if _, err := svc.Login(context.Background(), LoginInput{Tenant: "acme", Email: "alice@acme.test", Password: newPassword}); err != nil {
		t.Fatalf("login with new password failed: %v", err)
	}
	if _, err := svc.Login(context.Background(), LoginInput{Tenant: "acme", Email: "alice@acme.test", Password: "s3cret-pw"}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected old password rejected, got %v", err)
	}

	// The pre-existing session must be revoked.
	if _, err := svc.Refresh(context.Background(), refresh); !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("expected pre-existing refresh token revoked, got %v", err)
	}

	// Token row must be marked used (single-use).
	if repo.count() != 1 {
		t.Fatalf("expected 1 token row, got %d", repo.count())
	}
	for _, row := range repo.rows {
		if row.UsedAt == nil {
			t.Error("expected token row UsedAt to be set after consumption")
		}
	}

	// A confirmation alert must have been enqueued via the notifier (not resetSender) — the
	// SAME token-free AlertPasswordChanged path ChangePassword uses. Asserted on the
	// pre-Refresh-probe snapshot (see comment above) so the unrelated E5 refresh_reuse alert
	// triggered by the later revoked-token replay does not pollute this count.
	if len(eventsAfterReset) != 1 {
		t.Fatalf("expected exactly 1 security alert enqueued by ResetPassword, got %d: %+v", len(eventsAfterReset), eventsAfterReset)
	}
	evt := eventsAfterReset[0]
	if evt.Type != notify.AlertPasswordChanged {
		t.Errorf("event.Type = %q, want %q", evt.Type, notify.AlertPasswordChanged)
	}
	if evt.TenantID != "t-1" || evt.UserID != "u-1" || evt.Email != "alice@acme.test" {
		t.Errorf("unexpected event identity: %+v", evt)
	}

	// The reset-mail sender must NOT be reused for the confirmation (exactly 1 send — the
	// original token-bearing reset link — proves the confirmation went through the notifier,
	// never through the token-capable resetSender).
	if sender.count() != 1 {
		t.Errorf("expected exactly 1 reset-link email (no second token-bearing send), got %d", sender.count())
	}
}

func TestResetPassword_WeakPassword_RejectedAndTokenNotConsumed(t *testing.T) {
	svc, repo, sender := newResetTestService(t)
	raw := requestAndExtractToken(t, svc, sender, "acme", "alice@acme.test")

	err := svc.ResetPassword(context.Background(), raw, "short")
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError for weak password, got %v", err)
	}
	if ve.Code != "PASSWORD_TOO_WEAK" {
		t.Errorf("code = %q, want PASSWORD_TOO_WEAK", ve.Code)
	}

	// The token must NOT be consumed — retry with a strong password on the SAME link must work.
	for _, row := range repo.rows {
		if row.UsedAt != nil {
			t.Fatal("token was consumed despite the weak-password rejection")
		}
	}
	if err := svc.ResetPassword(context.Background(), raw, "Str0ng-Enough-Pw!"); err != nil {
		t.Fatalf("retry with a strong password on the same (unconsumed) token should succeed: %v", err)
	}
}

func TestResetPassword_UnknownToken_Invalid(t *testing.T) {
	svc, _, _ := newResetTestService(t)

	err := svc.ResetPassword(context.Background(), "totally-unknown-token", "Str0ng-Enough-Pw!")
	if !errors.Is(err, auth.ErrResetTokenInvalid) {
		t.Fatalf("expected ErrResetTokenInvalid, got %v", err)
	}
}

func TestResetPassword_EmptyToken_Invalid(t *testing.T) {
	svc, _, _ := newResetTestService(t)

	for _, tok := range []string{"", "   "} {
		if err := svc.ResetPassword(context.Background(), tok, "Str0ng-Enough-Pw!"); !errors.Is(err, auth.ErrResetTokenInvalid) {
			t.Errorf("token %q: expected ErrResetTokenInvalid, got %v", tok, err)
		}
	}
}

func TestResetPassword_AlreadyUsedToken_Invalid(t *testing.T) {
	svc, _, sender := newResetTestService(t)
	raw := requestAndExtractToken(t, svc, sender, "acme", "alice@acme.test")

	if err := svc.ResetPassword(context.Background(), raw, "Str0ng-Enough-Pw!"); err != nil {
		t.Fatalf("first ResetPassword: %v", err)
	}
	// Replaying the same (now-used) token must fail — single-use.
	if err := svc.ResetPassword(context.Background(), raw, "Another-Str0ng-Pw!"); !errors.Is(err, auth.ErrResetTokenInvalid) {
		t.Fatalf("expected ErrResetTokenInvalid on reused token, got %v", err)
	}
}

func TestResetPassword_ConcurrentDoubleSubmit_ExactlyOneWins(t *testing.T) {
	svc, _, sender := newResetTestService(t)
	raw := requestAndExtractToken(t, svc, sender, "acme", "alice@acme.test")

	const goroutines = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		successes int
		rejects   int
		otherErr  error
	)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			err := svc.ResetPassword(context.Background(), raw, "Str0ng-Enough-Pw!")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, auth.ErrResetTokenInvalid):
				rejects++
			default:
				otherErr = err
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if otherErr != nil {
		t.Fatalf("unexpected non-ErrResetTokenInvalid error: %v", otherErr)
	}
	if successes != 1 {
		t.Fatalf("expected exactly 1 successful ResetPassword under concurrent double-submit, got %d (rejects=%d)", successes, rejects)
	}
	if rejects != goroutines-1 {
		t.Fatalf("expected %d rejected calls, got %d", goroutines-1, rejects)
	}
}

func TestResetPassword_NoResetRepoConfigured_ReturnsError(t *testing.T) {
	svc, _ := newTestService(t) // NOT wired with WithResetRepo

	if err := svc.ResetPassword(context.Background(), "whatever", "Str0ng-Enough-Pw!"); err == nil {
		t.Fatal("expected an error when reset repo is not configured")
	}
	if err := svc.RequestPasswordReset(context.Background(), "acme", "alice@acme.test"); err == nil {
		t.Fatal("expected an error when reset repo is not configured")
	}
}
