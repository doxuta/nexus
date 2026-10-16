//go:build integration

// ENG-AUTH2 #1 forgot/reset-password — integration suite against a REAL MySQL 8 brought up with
// golang-migrate (see a4_rbac_harness_integration_test.go). Run with:
//
//	go test -tags=integration -race ./internal/handlers/
//
// This suite specifically exercises what only a real database can prove:
//   - the atomic single-use conditional UPDATE (ConsumeByHash) under genuine concurrency
//     (exactly one winner among concurrent double-submits, MUST-FIX 4);
//   - an expired token row is rejected (RESET_TOKEN_INVALID), not silently accepted;
//   - the happy path end to end through the real HTTP surface: request → email captured by a
//     spy sender → reset → password actually updated in MySQL → all sessions revoked → the
//     token row is truly marked used_at in the migrated password_reset_tokens table.
//
// The DB/migration harness (a4SetupMigratedDB) migrates 000001..latest (>= 000027, which creates
// password_reset_tokens with UNIQUE(token_hash)), so a regression in that migration (missing FK,
// missing unique index) fails this suite rather than being papered over by AutoMigrate.
package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/config"
	"nexus/platform/internal/notify"
	"nexus/platform/internal/services"
)

const (
	resetITTenantID   = "a4rp0000-0000-0000-0000-0000000000e7"
	resetITTenantSlug = "a4rp-acme"
)

// resetSpySender is a concurrency-safe notify.EmailSender recorder used to capture the
// reset-link email without a real SMTP server (mirrors the pattern in a7_security_alerts).
type resetSpySender struct {
	mu    sync.Mutex
	mails []notify.Mail
}

func (s *resetSpySender) Send(_ context.Context, m notify.Mail) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mails = append(s.mails, m)
	return nil
}

func (s *resetSpySender) last() notify.Mail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mails[len(s.mails)-1]
}

func (s *resetSpySender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.mails)
}

// resetITRouter bundles the production /api/v1 auth surface wired over the REAL migrated DB,
// with the reset-password repo + a spy mailer injected via WithResetRepo/WithResetMailer —
// exactly the main.go wiring shape, minus the real SMTPSender. It ALSO wires a real
// notify.AsyncDispatcher (over the migrated security_alerts table, a DIFFERENT spy sender from
// the reset-mailer) so the happy-path test can assert the post-reset confirmation alert was
// genuinely enqueued and audited — the same production wiring a7_security_alerts_integration_test
// uses for ChangePassword.
type resetITRouter struct {
	engine      *gin.Engine
	gdb         *gorm.DB
	sx          *sqlx.DB
	sender      *resetSpySender // captures the token-bearing reset-link email
	alertSender *resetSpySender // captures the token-free confirmation email (via the dispatcher)
	dispatcher  *notify.AsyncDispatcher
}

func newResetITRouter(t *testing.T, gdb *gorm.DB, sx *sqlx.DB) *resetITRouter {
	t.Helper()

	t.Setenv("JWT_SECRET", itJWTSecret)
	t.Setenv("JWT_ACCESS_TTL", "15m")
	t.Setenv("JWT_REFRESH_TTL", "168h")
	jwtCfg, err := config.LoadJWTConfig()
	if err != nil {
		t.Fatalf("config.LoadJWTConfig: %v", err)
	}
	tokenSvc, err := auth.NewTokenService(jwtCfg.JWTSecret, jwtCfg.AccessTTL, jwtCfg.RefreshTTL)
	if err != nil {
		t.Fatalf("auth.NewTokenService: %v", err)
	}

	userRepo := auth.NewUserRepository(gdb)
	refreshRepo := auth.NewRefreshTokenRepository(gdb)
	resetRepo := auth.NewPasswordResetTokenRepository(gdb)
	sender := &resetSpySender{}

	// Real audit repository over the migrated security_alerts table + real async dispatcher —
	// the EXACT production wiring (cmd/server/main.go: dispatcher := notify.NewAsyncDispatcher(...)).
	alertSender := &resetSpySender{}
	alertRepo := notify.NewSQLAlertRepository(sx)
	dispatcher := notify.NewAsyncDispatcher(
		notify.DispatcherConfig{Buffer: 16, Workers: 1},
		alertSender, alertRepo, zap.NewNop(),
	)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = dispatcher.Shutdown(ctx)
	})

	authSvc := services.NewAuthService(userRepo, refreshRepo, tokenSvc).
		WithNotifier(dispatcher).
		WithSessionGuard(services.NewCachedSessionGuard(refreshRepo, services.DefaultSessionGuardTTL)). // #310: /auth/me asks the same guard the reset invalidates
		WithResetRepo(resetRepo).
		WithResetMailer(sender, "reset@nexus.test", "https://app.nexus.test/reset").
		WithMailDispatcher(func(fn func()) { fn() }) // synchronous so the reset-mail assertions are deterministic

	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	NewAuthHandler(authSvc, zap.NewNop()).RegisterRoutes(api)

	return &resetITRouter{engine: r, gdb: gdb, sx: sx, sender: sender, alertSender: alertSender, dispatcher: dispatcher}
}

// drain shuts the dispatcher down, blocking until every buffered alert has been processed (the
// async send + audit write completed), so the security_alerts table + alertSender reflect the
// final state race-free. Mirrors a7Router.drain.
func (rt *resetITRouter) drain(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.dispatcher.Shutdown(ctx); err != nil {
		t.Fatalf("dispatcher drain did not complete: %v", err)
	}
}

func postForgotPassword(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/forgot-password", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func postResetPassword(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/reset-password", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// resetITExtractToken pulls the raw token out of the last captured mail's plain-text body.
func resetITExtractToken(t *testing.T, mail notify.Mail) string {
	t.Helper()
	idx := strings.Index(mail.Body, "?token=")
	if idx < 0 {
		t.Fatalf("no ?token= in mail body: %q", mail.Body)
	}
	rest := mail.Body[idx+len("?token="):]
	return strings.Fields(rest)[0]
}

// TestResetPasswordIT_HappyPath drives the FULL loop through the real HTTP surface + MySQL:
// forgot-password (spy captures the mail) → reset-password → login with the new password
// succeeds, the old one is rejected, every pre-existing refresh token is revoked, and the
// password_reset_tokens row is truly marked used_at in the migrated table.
func TestResetPasswordIT_HappyPath(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newResetITRouter(t, gdb, sx)

	a4SeedTenant(t, gdb, resetITTenantID, resetITTenantSlug, "Acme Reset")
	a4SeedUser(t, gdb, resetITTenantID, "alice@"+resetITTenantSlug, false, nil)

	// Establish a live session so we can assert it gets revoked by the reset.
	loginBefore := itLogin(t, rt.engine, resetITTenantSlug, "alice@"+resetITTenantSlug, authPassword)

	wf := postForgotPassword(t, rt.engine, `{"tenant":"`+resetITTenantSlug+`","email":"alice@`+resetITTenantSlug+`"}`)
	if wf.Code != http.StatusOK {
		t.Fatalf("forgot-password: got %d, want 200; body=%s", wf.Code, wf.Body.String())
	}
	if rt.sender.count() != 1 {
		t.Fatalf("expected exactly 1 reset email captured, got %d", rt.sender.count())
	}
	raw := resetITExtractToken(t, rt.sender.last())

	const newPassword = "N3w-Str0ng-Pw!"
	wr := postResetPassword(t, rt.engine, `{"token":"`+raw+`","newPassword":"`+newPassword+`"}`)
	if wr.Code != http.StatusOK {
		t.Fatalf("reset-password: got %d, want 200; body=%s", wr.Code, wr.Body.String())
	}

	// New password authenticates; old one is rejected.
	wOK := postLogin(t, rt.engine, `{"tenant":"`+resetITTenantSlug+`","email":"alice@`+resetITTenantSlug+`","password":"`+newPassword+`"}`)
	if wOK.Code != http.StatusOK {
		t.Fatalf("login with new password: got %d, want 200; body=%s", wOK.Code, wOK.Body.String())
	}
	wOld := postLogin(t, rt.engine, `{"tenant":"`+resetITTenantSlug+`","email":"alice@`+resetITTenantSlug+`","password":"`+authPassword+`"}`)
	if wOld.Code != http.StatusUnauthorized {
		t.Fatalf("login with OLD password: got %d, want 401; body=%s", wOld.Code, wOld.Body.String())
	}

	// #310: the pre-reset session is ENDED, not just stripped of its refresh row — its still-unexpired
	// ACCESS token is refused on the very next request (/auth/me asks the guard the reset invalidated).
	reqMe := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	reqMe.Header.Set("Authorization", "Bearer "+loginBefore.Data.AccessToken)
	wMe := httptest.NewRecorder()
	rt.engine.ServeHTTP(wMe, reqMe)
	if wMe.Code != http.StatusUnauthorized {
		t.Fatalf("pre-reset ACCESS token after the reset: got %d, want 401 at once; body=%s", wMe.Code, wMe.Body.String())
	}

	// The pre-existing session's refresh token must be revoked.
	wRefresh := postRefresh(t, rt.engine, `{"refreshToken":"`+loginBefore.Data.RefreshToken+`"}`)
	if wRefresh.Code != http.StatusUnauthorized {
		t.Fatalf("pre-reset refresh token should be revoked: got %d, want 401; body=%s", wRefresh.Code, wRefresh.Body.String())
	}

	// The token row in the REAL migrated table must be marked used.
	var usedAt *time.Time
	if err := sx.Get(&usedAt, "SELECT used_at FROM password_reset_tokens WHERE tenant_id = ?", resetITTenantID); err != nil {
		t.Fatalf("read password_reset_tokens: %v", err)
	}
	if usedAt == nil {
		t.Error("expected used_at to be set on the consumed token row")
	}

	// The post-reset confirmation alert must have been genuinely enqueued, delivered (via the
	// dispatcher's OWN sender — never the token-bearing resetSender), and audited in the REAL
	// migrated security_alerts table — the same production path ChangePassword uses. Filtered by
	// alert_type=password_changed (rather than asserting the table's total row count) because the
	// refresh-probe above deliberately replays an already-revoked refresh token, which correctly
	// triggers its OWN, unrelated E5 refresh_reuse alert — that is genuine product behavior, not
	// something this test is asserting about the reset confirmation.
	rt.drain(t)
	var alertRows []struct {
		AlertType string `db:"alert_type"`
		TenantID  string `db:"tenant_id"`
		UserID    string `db:"user_id"`
		Status    string `db:"status"`
	}
	if err := sx.Select(&alertRows,
		`SELECT alert_type, tenant_id, user_id, status FROM security_alerts WHERE tenant_id = ? AND alert_type = ?`,
		resetITTenantID, notify.AlertPasswordChanged); err != nil {
		t.Fatalf("query security_alerts: %v", err)
	}
	if len(alertRows) != 1 {
		t.Fatalf("expected exactly 1 security_alerts row of type %q, got %d: %+v", notify.AlertPasswordChanged, len(alertRows), alertRows)
	}
	if alertRows[0].Status != "sent" {
		t.Errorf("security_alerts status = %q, want sent", alertRows[0].Status)
	}
	if rt.sender.count() != 1 {
		t.Errorf("expected the reset-mailer to have sent only the original link (no confirmation reuse), got %d sends", rt.sender.count())
	}
}

// TestResetPasswordIT_ExpiredToken_Rejected proves an expired (but otherwise valid, unused) row
// is rejected — the atomic UPDATE's `expires_at > ?` predicate does real work, not just used_at.
func TestResetPasswordIT_ExpiredToken_Rejected(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newResetITRouter(t, gdb, sx)

	a4SeedTenant(t, gdb, resetITTenantID, resetITTenantSlug, "Acme Reset")
	a4SeedUser(t, gdb, resetITTenantID, "bob@"+resetITTenantSlug, false, nil)

	wf := postForgotPassword(t, rt.engine, `{"tenant":"`+resetITTenantSlug+`","email":"bob@`+resetITTenantSlug+`"}`)
	if wf.Code != http.StatusOK {
		t.Fatalf("forgot-password: got %d, want 200; body=%s", wf.Code, wf.Body.String())
	}
	raw := resetITExtractToken(t, rt.sender.last())

	// Force the row's expires_at into the past directly in MySQL (bypassing the 30m TTL).
	tokenHash := auth.HashRefreshToken(raw)
	if err := gdb.Exec(
		"UPDATE password_reset_tokens SET expires_at = ? WHERE token_hash = ?",
		time.Now().Add(-time.Hour), tokenHash,
	).Error; err != nil {
		t.Fatalf("force-expire token: %v", err)
	}

	wr := postResetPassword(t, rt.engine, `{"token":"`+raw+`","newPassword":"Wh4tever-Str0ng!"}`)
	if wr.Code != http.StatusBadRequest {
		t.Fatalf("expired token: got %d, want 400; body=%s", wr.Code, wr.Body.String())
	}
	if !strings.Contains(wr.Body.String(), "RESET_TOKEN_INVALID") {
		t.Errorf("expected RESET_TOKEN_INVALID, got body=%s", wr.Body.String())
	}
}

// TestResetPasswordIT_SingleUse_SecondAttemptRejected proves sequential single-use: once a
// token has been consumed, a second (correctly formed) attempt with the SAME raw token fails.
func TestResetPasswordIT_SingleUse_SecondAttemptRejected(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newResetITRouter(t, gdb, sx)

	a4SeedTenant(t, gdb, resetITTenantID, resetITTenantSlug, "Acme Reset")
	a4SeedUser(t, gdb, resetITTenantID, "carol@"+resetITTenantSlug, false, nil)

	wf := postForgotPassword(t, rt.engine, `{"tenant":"`+resetITTenantSlug+`","email":"carol@`+resetITTenantSlug+`"}`)
	if wf.Code != http.StatusOK {
		t.Fatalf("forgot-password: got %d, want 200; body=%s", wf.Code, wf.Body.String())
	}
	raw := resetITExtractToken(t, rt.sender.last())

	w1 := postResetPassword(t, rt.engine, `{"token":"`+raw+`","newPassword":"First-Str0ng-Pw!"}`)
	if w1.Code != http.StatusOK {
		t.Fatalf("first reset: got %d, want 200; body=%s", w1.Code, w1.Body.String())
	}
	w2 := postResetPassword(t, rt.engine, `{"token":"`+raw+`","newPassword":"Second-Str0ng-Pw!"}`)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("second reset (replay): got %d, want 400; body=%s", w2.Code, w2.Body.String())
	}
}

// TestResetPasswordIT_ConcurrentDoubleSubmit_ExactlyOneWins drives many concurrent HTTP requests
// presenting the SAME raw token against the REAL MySQL atomic conditional UPDATE, proving the
// single-use guarantee holds under genuine concurrency (not just the in-memory mutex fake).
// Run with -race.
func TestResetPasswordIT_ConcurrentDoubleSubmit_ExactlyOneWins(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newResetITRouter(t, gdb, sx)

	a4SeedTenant(t, gdb, resetITTenantID, resetITTenantSlug, "Acme Reset")
	a4SeedUser(t, gdb, resetITTenantID, "dave@"+resetITTenantSlug, false, nil)

	wf := postForgotPassword(t, rt.engine, `{"tenant":"`+resetITTenantSlug+`","email":"dave@`+resetITTenantSlug+`"}`)
	if wf.Code != http.StatusOK {
		t.Fatalf("forgot-password: got %d, want 200; body=%s", wf.Code, wf.Body.String())
	}
	raw := resetITExtractToken(t, rt.sender.last())

	const goroutines = 10
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		successes int
		rejects   int
		otherCode []int
	)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			w := postResetPassword(t, rt.engine, `{"token":"`+raw+`","newPassword":"Concurrent-Str0ng-Pw!"}`)
			mu.Lock()
			defer mu.Unlock()
			switch w.Code {
			case http.StatusOK:
				successes++
			case http.StatusBadRequest:
				rejects++
			default:
				otherCode = append(otherCode, w.Code)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(otherCode) > 0 {
		t.Fatalf("unexpected status codes from concurrent reset attempts: %v", otherCode)
	}
	if successes != 1 {
		t.Fatalf("expected exactly 1 successful reset under concurrent double-submit, got %d (rejects=%d)", successes, rejects)
	}
	if rejects != goroutines-1 {
		t.Fatalf("expected %d rejected attempts, got %d", goroutines-1, rejects)
	}
}

// TestResetPasswordIT_AntiEnumeration_UnknownEmailAndTenant proves the anti-enumeration
// contract end to end: identical 200, and NO row lands in the real password_reset_tokens table.
func TestResetPasswordIT_AntiEnumeration_UnknownEmailAndTenant(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newResetITRouter(t, gdb, sx)

	a4SeedTenant(t, gdb, resetITTenantID, resetITTenantSlug, "Acme Reset")
	a4SeedUser(t, gdb, resetITTenantID, "erin@"+resetITTenantSlug, false, nil)

	wUnknownEmail := postForgotPassword(t, rt.engine, `{"tenant":"`+resetITTenantSlug+`","email":"ghost@`+resetITTenantSlug+`"}`)
	if wUnknownEmail.Code != http.StatusOK {
		t.Fatalf("unknown email: got %d, want 200; body=%s", wUnknownEmail.Code, wUnknownEmail.Body.String())
	}
	wUnknownTenant := postForgotPassword(t, rt.engine, `{"tenant":"ghost-tenant-`+uuid.NewString()[:8]+`","email":"erin@`+resetITTenantSlug+`"}`)
	if wUnknownTenant.Code != http.StatusOK {
		t.Fatalf("unknown tenant: got %d, want 200; body=%s", wUnknownTenant.Code, wUnknownTenant.Body.String())
	}
	if rt.sender.count() != 0 {
		t.Fatalf("expected 0 emails sent for unknown email/tenant, got %d", rt.sender.count())
	}

	var count int
	if err := sx.Get(&count, "SELECT COUNT(*) FROM password_reset_tokens WHERE tenant_id = ?", resetITTenantID); err != nil {
		t.Fatalf("count password_reset_tokens: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 token rows for unknown email/tenant, got %d", count)
	}
}
