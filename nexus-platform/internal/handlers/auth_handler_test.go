package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// stubUserRepo satisfies the (unexported) services userRepo interface structurally so the
// handler test can drive the real AuthService without a database.
type stubUserRepo struct {
	tenant *auth.Tenant
	user   *auth.User
}

func (s *stubUserRepo) ResolveTenantBySlug(_ context.Context, slug string) (*auth.Tenant, error) {
	if s.tenant != nil && s.tenant.Slug == slug && (s.tenant.Status == "" || s.tenant.Status == "active") {
		return s.tenant, nil
	}
	return nil, auth.ErrTenantNotFound
}

func (s *stubUserRepo) GetTenantByID(_ context.Context, id string) (*auth.Tenant, error) {
	if s.tenant != nil && s.tenant.ID == id && (s.tenant.Status == "" || s.tenant.Status == "active") {
		return s.tenant, nil
	}
	return nil, auth.ErrTenantNotFound
}

func (s *stubUserRepo) GetUserByEmail(_ context.Context, tenantID, email string) (*auth.User, error) {
	if s.user != nil && s.user.TenantID == tenantID && s.user.Email == email {
		return s.user, nil
	}
	return nil, auth.ErrUserNotFound
}

func (s *stubUserRepo) GetUserByID(_ context.Context, tenantID, id string) (*auth.User, error) {
	if s.user != nil && s.user.TenantID == tenantID && s.user.ID == id {
		return s.user, nil
	}
	return nil, auth.ErrUserNotFound
}

func (s *stubUserRepo) UpdatePasswordHash(_ context.Context, tenantID, id, newHash string) error {
	if s.user != nil && s.user.TenantID == tenantID && s.user.ID == id {
		s.user.PasswordHash = newHash
		return nil
	}
	return auth.ErrUserNotFound
}

// RecordFailedLogin / ResetLoginFailures mirror the persistent-lockout bookkeeping
// (mig 000043) on the single in-memory user so handler tests exercising Login behave
// like the real repo. A missing row is a silent no-op, like the real 0-rows UPDATE.
func (s *stubUserRepo) RecordFailedLogin(_ context.Context, tenantID, id string, threshold int, lockUntil time.Time) error {
	if s.user != nil && s.user.TenantID == tenantID && s.user.ID == id {
		s.user.FailedLoginCount++
		if s.user.FailedLoginCount >= threshold {
			lu := lockUntil
			s.user.LockedUntil = &lu
		}
	}
	return nil
}

func (s *stubUserRepo) ResetLoginFailures(_ context.Context, tenantID, id string) error {
	if s.user != nil && s.user.TenantID == tenantID && s.user.ID == id {
		s.user.FailedLoginCount = 0
		s.user.LockedUntil = nil
	}
	return nil
}

// stubRefreshRepo is an in-memory refresh-token store for the handler tests: it models
// store / find-live / revoke with a revoked flag and expiry so refresh/logout paths work
// without a database. All operations are tenant-scoped via the (tenantID,hash) key.
type stubRefreshRepo struct {
	rows map[string]*auth.RefreshToken
}

func newStubRefreshRepo() *stubRefreshRepo {
	return &stubRefreshRepo{rows: map[string]*auth.RefreshToken{}}
}

func (s *stubRefreshRepo) key(tenantID, hash string) string { return tenantID + "\x00" + hash }

func (s *stubRefreshRepo) Store(_ context.Context, rt *auth.RefreshToken) error {
	cp := *rt
	s.rows[s.key(rt.TenantID, rt.TokenHash)] = &cp
	return nil
}

func (s *stubRefreshRepo) FindLiveByHash(_ context.Context, tenantID, tokenHash string) (*auth.RefreshToken, error) {
	rt, ok := s.rows[s.key(tenantID, tokenHash)]
	if !ok || rt.RevokedAt != nil || !rt.ExpiresAt.After(time.Now()) {
		return nil, auth.ErrRefreshTokenNotFound
	}
	return rt, nil
}

func (s *stubRefreshRepo) RevokeByHash(_ context.Context, tenantID, tokenHash string) (bool, error) {
	rt, ok := s.rows[s.key(tenantID, tokenHash)]
	if !ok || rt.RevokedAt != nil {
		return false, nil
	}
	now := time.Now()
	rt.RevokedAt = &now
	return true, nil
}

func (s *stubRefreshRepo) Rotate(_ context.Context, tenantID, oldHash string, next *auth.RefreshToken) (bool, error) {
	old, ok := s.rows[s.key(tenantID, oldHash)]
	if !ok || old.RevokedAt != nil {
		return false, nil
	}
	now := time.Now()
	old.RevokedAt = &now
	cp := *next
	s.rows[s.key(next.TenantID, next.TokenHash)] = &cp
	return true, nil
}

func (s *stubRefreshRepo) FindByHashIncludingRevoked(_ context.Context, tenantID, tokenHash string) (*auth.RefreshToken, error) {
	rt, ok := s.rows[s.key(tenantID, tokenHash)]
	if !ok || rt.RevokedAt == nil || (rt.RevokeReason != nil && *rt.RevokeReason == auth.RevokeReasonEnded) { // ended session: not a theft signal (specs/055 FR-009)
		return nil, auth.ErrRefreshTokenNotFound
	}
	cp := *rt
	return &cp, nil
}

func (s *stubRefreshRepo) RevokeAllForUser(_ context.Context, tenantID, userID string) error {
	now := time.Now()
	for _, rt := range s.rows {
		if rt.TenantID == tenantID && rt.UserID == userID && rt.RevokedAt == nil {
			rt.RevokedAt = &now
		}
	}
	return nil
}

// newAuthTestRouter builds a gin engine with the auth routes mounted over a real
// AuthService backed by an in-memory stub repo + real JWT issuer. Returns the engine,
// the seeded plaintext password, and the token service + refresh store so refresh/logout/me
// tests can mint tokens and inspect revocation directly.
func newAuthTestRouter(t *testing.T) (*gin.Engine, string, *auth.TokenService, *stubRefreshRepo) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	const password = "s3cret-pw"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	repo := &stubUserRepo{
		tenant: &auth.Tenant{ID: "t-1", Slug: "acme", Status: "active"},
		user: &auth.User{
			ID:           "u-1",
			TenantID:     "t-1",
			Email:        "alice@acme.test",
			PasswordHash: string(hash),
			Status:       "active",
			IsAdmin:      true,
		},
	}
	tokens, err := auth.NewTokenService("super-secret-test-key-0123456789", 15*60*time.Second, 7*24*60*60*time.Second)
	if err != nil {
		t.Fatalf("token service: %v", err)
	}
	refresh := newStubRefreshRepo()
	svc := services.NewAuthService(repo, refresh, tokens)

	r := gin.New()
	api := r.Group("/api/v1")
	NewAuthHandler(svc, zap.NewNop()).RegisterRoutes(api)
	return r, password, tokens, refresh
}

func postJSON(r *gin.Engine, path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestLoginHandler_Success(t *testing.T) {
	r, password, _, _ := newAuthTestRouter(t)
	w := postJSON(r, "/api/v1/auth/login", gin.H{
		"tenant": "acme", "email": "alice@acme.test", "password": password,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Data services.LoginOutput `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.AccessToken == "" || resp.Data.RefreshToken == "" {
		t.Errorf("expected token pair, got %+v", resp.Data)
	}
	if resp.Data.User == nil || resp.Data.User.ID != "u-1" {
		t.Errorf("expected user u-1, got %+v", resp.Data.User)
	}
	// password hash must never appear anywhere in the response body
	if bytes.Contains(w.Body.Bytes(), []byte("passwordHash")) || bytes.Contains(w.Body.Bytes(), []byte("$2a$")) {
		t.Error("response body leaked the password hash")
	}
}

func TestLoginHandler_WrongPassword_401(t *testing.T) {
	r, _, _, _ := newAuthTestRouter(t)
	w := postJSON(r, "/api/v1/auth/login", gin.H{
		"tenant": "acme", "email": "alice@acme.test", "password": "nope",
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestLoginHandler_UnknownUser_401(t *testing.T) {
	r, _, _, _ := newAuthTestRouter(t)
	w := postJSON(r, "/api/v1/auth/login", gin.H{
		"tenant": "acme", "email": "ghost@acme.test", "password": "whatever",
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestLoginHandler_BadBody_400(t *testing.T) {
	r, _, _, _ := newAuthTestRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (%s)", w.Code, w.Body.String())
	}
}

// TestLoginHandler_MissingField_422 verifies that a well-formed JSON body missing a
// required field (password) returns 422 Unprocessable Entity, not 400 (RM-06 fix).
// The body is valid JSON — gin can bind it — but the service rejects it as incomplete,
// which is a semantic (422) rather than a syntactic (400) error per api-conventions.
func TestLoginHandler_MissingField_422(t *testing.T) {
	r, _, _, _ := newAuthTestRouter(t)
	w := postJSON(r, "/api/v1/auth/login", gin.H{"tenant": "acme", "email": "alice@acme.test"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for missing password, got %d (%s)", w.Code, w.Body.String())
	}
}
