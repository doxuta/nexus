package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/notify"
	"nexus/platform/internal/services"
)

// stubResetRepo is an in-memory stand-in for the reset-token repository, mirroring the
// services-package fakeResetRepo (kept package-local here since services' is unexported).
type stubResetRepo struct {
	mu   sync.Mutex
	rows map[string]*auth.PasswordResetToken
}

func newStubResetRepo() *stubResetRepo {
	return &stubResetRepo{rows: map[string]*auth.PasswordResetToken{}}
}

func (s *stubResetRepo) Store(_ context.Context, prt *auth.PasswordResetToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *prt
	s.rows[prt.TokenHash] = &cp
	return nil
}

func (s *stubResetRepo) FindByHash(_ context.Context, tokenHash string) (*auth.PasswordResetToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prt, ok := s.rows[tokenHash]
	if !ok {
		return nil, auth.ErrResetTokenInvalid
	}
	cp := *prt
	return &cp, nil
}

func (s *stubResetRepo) ConsumeByHash(_ context.Context, tenantID, tokenHash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prt, ok := s.rows[tokenHash]
	if !ok || prt.TenantID != tenantID || prt.UsedAt != nil || !prt.ExpiresAt.After(time.Now()) {
		return false, nil
	}
	now := time.Now()
	prt.UsedAt = &now
	return true, nil
}

// stubMailSender is a minimal notify.EmailSender recorder for the handler tests.
type stubMailSender struct {
	mu    sync.Mutex
	mails []notify.Mail
}

func (s *stubMailSender) Send(_ context.Context, m notify.Mail) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mails = append(s.mails, m)
	return nil
}

func (s *stubMailSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.mails)
}

func (s *stubMailSender) last() notify.Mail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mails[len(s.mails)-1]
}

// newResetTestRouter builds a gin engine with the auth routes (incl. forgot/reset-password)
// mounted over a real AuthService backed by in-memory stub repos + a real JWT issuer.
func newResetTestRouter(t *testing.T) (*gin.Engine, *stubResetRepo, *stubMailSender) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	const password = "s3cret-pw"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	userRepo := &stubUserRepo{
		tenant: &auth.Tenant{ID: "t-1", Slug: "acme", Status: "active"},
		user: &auth.User{
			ID:           "u-1",
			TenantID:     "t-1",
			Email:        "alice@acme.test",
			PasswordHash: string(hash),
			Status:       "active",
		},
	}
	tokens, err := auth.NewTokenService("super-secret-test-key-0123456789", 15*60*time.Second, 7*24*60*60*time.Second)
	if err != nil {
		t.Fatalf("token service: %v", err)
	}
	resetRepo := newStubResetRepo()
	sender := &stubMailSender{}

	svc := services.NewAuthService(userRepo, newStubRefreshRepo(), tokens).
		WithResetRepo(resetRepo).
		WithResetMailer(sender, "reset@nexus.test", "https://app.nexus.test/reset").
		WithMailDispatcher(func(fn func()) { fn() }) // synchronous so the spy sees the send within the request

	r := gin.New()
	api := r.Group("/api/v1")
	NewAuthHandler(svc, zap.NewNop()).RegisterRoutes(api)
	return r, resetRepo, sender
}

func TestForgotPasswordHandler_KnownEmail_200(t *testing.T) {
	r, repo, sender := newResetTestRouter(t)

	w := postJSON(r, "/api/v1/auth/forgot-password", gin.H{"tenant": "acme", "email": "alice@acme.test"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("expected {\"data\":{\"ok\":true}}, got %s", w.Body.String())
	}
	if len(repo.rows) != 1 {
		t.Errorf("expected 1 token row, got %d", len(repo.rows))
	}
	if sender.count() != 1 {
		t.Errorf("expected 1 email sent, got %d", sender.count())
	}
}

func TestForgotPasswordHandler_UnknownEmail_Identical200(t *testing.T) {
	r, repo, sender := newResetTestRouter(t)

	w := postJSON(r, "/api/v1/auth/forgot-password", gin.H{"tenant": "acme", "email": "ghost@acme.test"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (anti-enumeration), got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("expected {\"data\":{\"ok\":true}}, got %s", w.Body.String())
	}
	if len(repo.rows) != 0 {
		t.Errorf("expected 0 token rows for unknown email, got %d", len(repo.rows))
	}
	if sender.count() != 0 {
		t.Errorf("expected 0 emails for unknown email, got %d", sender.count())
	}
}

func TestForgotPasswordHandler_UnknownTenant_Identical200(t *testing.T) {
	r, repo, sender := newResetTestRouter(t)

	w := postJSON(r, "/api/v1/auth/forgot-password", gin.H{"tenant": "ghost-tenant", "email": "alice@acme.test"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (anti-enumeration), got %d (%s)", w.Code, w.Body.String())
	}
	if len(repo.rows) != 0 || sender.count() != 0 {
		t.Errorf("expected zero side effects for unknown tenant, got rows=%d sends=%d", len(repo.rows), sender.count())
	}
}

func TestForgotPasswordHandler_BadBody_400(t *testing.T) {
	r, _, _ := newResetTestRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/forgot-password", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (%s)", w.Code, w.Body.String())
	}
}

// extractRawToken pulls the raw reset token out of a recorded mail's plain-text body.
func extractRawToken(t *testing.T, mail notify.Mail) string {
	t.Helper()
	idx := strings.Index(mail.Body, "?token=")
	if idx < 0 {
		t.Fatalf("no ?token= in mail body: %q", mail.Body)
	}
	rest := mail.Body[idx+len("?token="):]
	return strings.Fields(rest)[0]
}

func TestResetPasswordHandler_HappyPath_200(t *testing.T) {
	r, _, sender := newResetTestRouter(t)

	w := postJSON(r, "/api/v1/auth/forgot-password", gin.H{"tenant": "acme", "email": "alice@acme.test"})
	if w.Code != http.StatusOK {
		t.Fatalf("forgot-password: expected 200, got %d", w.Code)
	}
	raw := extractRawToken(t, sender.last())

	w = postJSON(r, "/api/v1/auth/reset-password", gin.H{"token": raw, "newPassword": "Str0ng-Enough-Pw!"})
	if w.Code != http.StatusOK {
		t.Fatalf("reset-password: expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	// Login with the new password must now succeed.
	w = postJSON(r, "/api/v1/auth/login", gin.H{"tenant": "acme", "email": "alice@acme.test", "password": "Str0ng-Enough-Pw!"})
	if w.Code != http.StatusOK {
		t.Fatalf("login with new password: expected 200, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestResetPasswordHandler_InvalidToken_400(t *testing.T) {
	r, _, _ := newResetTestRouter(t)

	w := postJSON(r, "/api/v1/auth/reset-password", gin.H{"token": "totally-bogus", "newPassword": "Str0ng-Enough-Pw!"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "RESET_TOKEN_INVALID") {
		t.Errorf("expected RESET_TOKEN_INVALID code, got %s", w.Body.String())
	}
}

func TestResetPasswordHandler_WeakPassword_422(t *testing.T) {
	r, _, sender := newResetTestRouter(t)

	w := postJSON(r, "/api/v1/auth/forgot-password", gin.H{"tenant": "acme", "email": "alice@acme.test"})
	if w.Code != http.StatusOK {
		t.Fatalf("forgot-password: expected 200, got %d", w.Code)
	}
	raw := extractRawToken(t, sender.last())

	w = postJSON(r, "/api/v1/auth/reset-password", gin.H{"token": raw, "newPassword": "short"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d (%s)", w.Code, w.Body.String())
	}

	// Token must still be usable (not consumed by the weak-password rejection).
	w = postJSON(r, "/api/v1/auth/reset-password", gin.H{"token": raw, "newPassword": "Str0ng-Enough-Pw!"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected retry with strong password to succeed, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestResetPasswordHandler_BadBody_400(t *testing.T) {
	r, _, _ := newResetTestRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/reset-password", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (%s)", w.Code, w.Body.String())
	}
}
