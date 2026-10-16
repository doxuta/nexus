package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/pquerna/otp/totp"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// The MFA handler surface has two shapes of protection:
//   - POST /auth/mfa/verify is PUBLIC (step 2 of the two-step login, no Bearer token yet) —
//     directly httptest-able on RegisterRoutes alone, exactly like forgot/reset-password.
//   - POST /auth/mfa/{enroll,activate,disable} are PROTECTED (need AuthMiddleware+RequireTenant
//     to populate the verified caller identity tenantOf/actorOf read). Like ChangePassword
//     (the existing protected auth route), their full DB-backed flows are exercised end-to-end
//     in the //go:build integration suite (eng_mfa_integration_test.go). The MFA-STEPUP tests
//     below additionally unit-test their request/response mapping through the REAL middleware
//     chain with a stubVerifier (the acl_wiring_test.go pattern), because the step-up password
//     contract (missing → 422, wrong → 401, lockout → 429) is pure handler+service wiring that
//     needs no database.

// --- local stub doubles (mirrors the unexported services-package fakes; kept package-local
// like stubResetRepo mirrors fakeResetRepo) ---------------------------------------------------

type stubMFARepo struct {
	mu   sync.Mutex
	rows map[string]*auth.UserMFA

	// backup, when set, is where ActivateWithStepAndBackupCodes persists the backup codes on a
	// successful call (reviewer LOW-1 activate+store merge) — mirrors services package's
	// fakeMFARepo.backup wiring.
	backup *stubMFABackupRepo
}

func newStubMFARepo() *stubMFARepo            { return &stubMFARepo{rows: map[string]*auth.UserMFA{}} }
func (s *stubMFARepo) key(t, u string) string { return t + "\x00" + u }

func (s *stubMFARepo) Get(_ context.Context, tenantID, userID string) (*auth.UserMFA, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[s.key(tenantID, userID)]
	if !ok {
		return nil, auth.ErrMFANotEnrolled
	}
	cp := *row
	return &cp, nil
}

func (s *stubMFARepo) Create(_ context.Context, m *auth.UserMFA) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *m
	s.rows[s.key(m.TenantID, m.UserID)] = &cp
	return nil
}

func (s *stubMFARepo) UpdateSecret(_ context.Context, tenantID, userID, secretEnc string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[s.key(tenantID, userID)]
	if !ok || row.Enabled {
		return fmt.Errorf("no matching row")
	}
	row.SecretEnc = secretEnc
	row.LastUsedStep = 0
	return nil
}

func (s *stubMFARepo) ActivateWithStepAndBackupCodes(ctx context.Context, tenantID, userID string, step int64, codes []*auth.MFABackupCode) (bool, error) {
	s.mu.Lock()
	row, ok := s.rows[s.key(tenantID, userID)]
	if !ok || row.Enabled {
		s.mu.Unlock()
		return false, nil
	}
	row.Enabled = true
	row.LastUsedStep = step
	s.mu.Unlock()

	if s.backup != nil {
		if err := s.backup.StoreBatch(ctx, codes); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (s *stubMFARepo) ConsumeStep(_ context.Context, tenantID, userID string, step int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[s.key(tenantID, userID)]
	if !ok || !row.Enabled || row.LastUsedStep >= step {
		return false, nil
	}
	row.LastUsedStep = step
	return true, nil
}

func (s *stubMFARepo) Delete(_ context.Context, tenantID, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, s.key(tenantID, userID))
	return nil
}

type stubMFABackupRepo struct {
	mu   sync.Mutex
	rows map[string][]*auth.MFABackupCode
}

func newStubMFABackupRepo() *stubMFABackupRepo {
	return &stubMFABackupRepo{rows: map[string][]*auth.MFABackupCode{}}
}

func (s *stubMFABackupRepo) StoreBatch(_ context.Context, codes []*auth.MFABackupCode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range codes {
		k := c.TenantID + "\x00" + c.UserID
		cp := *c
		s.rows[k] = append(s.rows[k], &cp)
	}
	return nil
}

func (s *stubMFABackupRepo) ConsumeByHash(_ context.Context, tenantID, userID, codeHash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.rows[tenantID+"\x00"+userID] {
		if c.CodeHash == codeHash && c.UsedAt == nil {
			now := time.Now()
			c.UsedAt = &now
			return true, nil
		}
	}
	return false, nil
}

func (s *stubMFABackupRepo) DeleteAllForUser(_ context.Context, tenantID, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, tenantID+"\x00"+userID)
	return nil
}

type stubMFAPendingRepo struct {
	mu     sync.Mutex
	byHash map[string]*auth.MFAPendingToken
}

func newStubMFAPendingRepo() *stubMFAPendingRepo {
	return &stubMFAPendingRepo{byHash: map[string]*auth.MFAPendingToken{}}
}

func (s *stubMFAPendingRepo) Store(_ context.Context, pt *auth.MFAPendingToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *pt
	s.byHash[pt.TokenHash] = &cp
	return nil
}

func (s *stubMFAPendingRepo) FindByHash(_ context.Context, tokenHash string) (*auth.MFAPendingToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pt, ok := s.byHash[tokenHash]
	if !ok {
		return nil, auth.ErrMFAPendingInvalid
	}
	cp := *pt
	return &cp, nil
}

func (s *stubMFAPendingRepo) ConsumeByHash(_ context.Context, tenantID, tokenHash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pt, ok := s.byHash[tokenHash]
	if !ok || pt.TenantID != tenantID || pt.UsedAt != nil || !pt.ExpiresAt.After(time.Now()) {
		return false, nil
	}
	now := time.Now()
	pt.UsedAt = &now
	return true, nil
}

// DeleteAllForUser purges every pending token for (tenantID, userID) — mirrors
// auth.MFAPendingTokenRepository.DeleteAllForUser (reviewer LOW-2 hygiene fix).
func (s *stubMFAPendingRepo) DeleteAllForUser(_ context.Context, tenantID, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for hash, pt := range s.byHash {
		if pt.TenantID == tenantID && pt.UserID == userID {
			delete(s.byHash, hash)
		}
	}
	return nil
}

type stubMFASealer struct{}

const stubSealPrefix = "sealed:"

func (stubMFASealer) Seal(pt []byte) (string, error) { return stubSealPrefix + string(pt), nil }
func (stubMFASealer) Open(sealed string) ([]byte, error) {
	if !strings.HasPrefix(sealed, stubSealPrefix) {
		return nil, fmt.Errorf("stubMFASealer: malformed envelope")
	}
	return []byte(strings.TrimPrefix(sealed, stubSealPrefix)), nil
}

// mfaHandlerFixture builds a gin engine with ONLY the public auth routes mounted (RegisterRoutes)
// over an MFA-wired AuthService, plus a helper to enroll+activate the seeded user directly
// through the service (bypassing the protected HTTP endpoints, which need the full
// AuthMiddleware chain — see file doc).
type mfaHandlerFixture struct {
	router *gin.Engine
	svc    *services.AuthService
}

func newMFAHandlerFixture(t *testing.T) *mfaHandlerFixture {
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
			ID: "u-1", TenantID: "t-1", Email: "alice@acme.test",
			PasswordHash: string(hash), Status: "active",
		},
	}
	tokens, err := auth.NewTokenService("super-secret-test-key-0123456789", 15*60*time.Second, 7*24*60*60*time.Second)
	if err != nil {
		t.Fatalf("token service: %v", err)
	}
	mfaRepo := newStubMFARepo()
	backup := newStubMFABackupRepo()
	mfaRepo.backup = backup // wires ActivateWithStepAndBackupCodes's insert (reviewer LOW-1)
	svc := services.NewAuthService(repo, newStubRefreshRepo(), tokens).
		WithMFA(mfaRepo, backup, newStubMFAPendingRepo(), stubMFASealer{})

	r := gin.New()
	api := r.Group("/api/v1")
	NewAuthHandler(svc, zap.NewNop()).RegisterRoutes(api)
	return &mfaHandlerFixture{router: r, svc: svc}
}

// enrollAndActivate drives the SERVICE (not HTTP) to a fully-active MFA account and returns
// the raw TOTP secret + a code valid one step after activation (see auth_mfa_test.go's
// codeAfterStep doc for why activation's own step cannot be reused).
func (f *mfaHandlerFixture) enrollAndActivate(t *testing.T) (secret string, nextCode string) {
	t.Helper()
	ctx := context.Background()
	secret, _, err := f.svc.MFAEnroll(ctx, "t-1", "u-1", "s3cret-pw")
	if err != nil {
		t.Fatalf("MFAEnroll: %v", err)
	}
	now := time.Now().UTC()
	step := now.Unix() / 30
	activateCode, err := totp.GenerateCodeCustom(secret, now, totp.ValidateOpts{Period: 30, Digits: 6})
	if err != nil {
		t.Fatalf("generate activate code: %v", err)
	}
	if _, err := f.svc.MFAActivate(ctx, "t-1", "u-1", "s3cret-pw", activateCode); err != nil {
		t.Fatalf("MFAActivate: %v", err)
	}
	next, err := totp.GenerateCodeCustom(secret, time.Unix((step+1)*30, 0).UTC(), totp.ValidateOpts{Period: 30, Digits: 6})
	if err != nil {
		t.Fatalf("generate next code: %v", err)
	}
	return secret, next
}

func (f *mfaHandlerFixture) login(t *testing.T) string {
	t.Helper()
	w := postJSON(f.router, "/api/v1/auth/login", gin.H{"tenant": "acme", "email": "alice@acme.test", "password": "s3cret-pw"})
	if w.Code != http.StatusOK {
		t.Fatalf("login: got %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			MFAPending   bool   `json:"mfaPending"`
			PendingToken string `json:"pendingToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	if !resp.Data.MFAPending || resp.Data.PendingToken == "" {
		t.Fatalf("login did not return an mfaPending response: %s", w.Body.String())
	}
	return resp.Data.PendingToken
}

func TestMFAVerifyHandler_Success_SameShapeAsLogin(t *testing.T) {
	f := newMFAHandlerFixture(t)
	_, code := f.enrollAndActivate(t)
	pending := f.login(t)

	w := postJSON(f.router, "/api/v1/auth/mfa/verify", gin.H{"pendingToken": pending, "code": code})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			User         struct {
				ID string `json:"id"`
			} `json:"user"`
			MFAPending bool `json:"mfaPending,omitempty"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.AccessToken == "" || resp.Data.RefreshToken == "" || resp.Data.User.ID != "u-1" {
		t.Errorf("incomplete token pair: %s", w.Body.String())
	}
	if resp.Data.MFAPending {
		t.Errorf("mfaPending must be absent/false on a completed verify: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "pendingToken") {
		t.Errorf("completed verify response must not echo pendingToken: %s", w.Body.String())
	}
}

func TestMFAVerifyHandler_WrongCode_401(t *testing.T) {
	f := newMFAHandlerFixture(t)
	f.enrollAndActivate(t)
	pending := f.login(t)

	w := postJSON(f.router, "/api/v1/auth/mfa/verify", gin.H{"pendingToken": pending, "code": "000000"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "MFA_CODE_INVALID") {
		t.Errorf("expected code MFA_CODE_INVALID, got %s", w.Body.String())
	}
}

func TestMFAVerifyHandler_UnknownPendingToken_401(t *testing.T) {
	f := newMFAHandlerFixture(t)
	w := postJSON(f.router, "/api/v1/auth/mfa/verify", gin.H{"pendingToken": "garbage", "code": "123456"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "MFA_PENDING_INVALID") {
		t.Errorf("expected code MFA_PENDING_INVALID, got %s", w.Body.String())
	}
}

func TestMFAVerifyHandler_BadBody_400(t *testing.T) {
	f := newMFAHandlerFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/verify", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestMFAVerifyHandler_MFAUnavailable_503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &stubUserRepo{
		tenant: &auth.Tenant{ID: "t-1", Slug: "acme", Status: "active"},
		user:   &auth.User{ID: "u-1", TenantID: "t-1", Email: "alice@acme.test", Status: "active"},
	}
	tokens, err := auth.NewTokenService("super-secret-test-key-0123456789", 15*60*time.Second, 7*24*60*60*time.Second)
	if err != nil {
		t.Fatalf("token service: %v", err)
	}
	svc := services.NewAuthService(repo, newStubRefreshRepo(), tokens) // NO WithMFA — MFA support inert
	r := gin.New()
	api := r.Group("/api/v1")
	NewAuthHandler(svc, zap.NewNop()).RegisterRoutes(api)

	w := postJSON(r, "/api/v1/auth/mfa/verify", gin.H{"pendingToken": "anything", "code": "123456"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "MFA_UNAVAILABLE") {
		t.Errorf("expected code MFA_UNAVAILABLE, got %s", w.Body.String())
	}
}

// --- MFA-STEPUP: protected enroll/activate/disable through the real middleware chain ---------

// stepUpToken is the fixed bearer string the stub verifier maps to the seeded caller (u-1/t-1).
const stepUpToken = "stepup-access-token"

// newMFAStepUpFixture builds a router with BOTH the public auth routes and the PROTECTED
// /auth/mfa/{enroll,activate,disable} routes mounted through the real production chain
// (AuthMiddleware → RequireTenant), using a stubVerifier so no real JWT minting is needed —
// exactly the acl_wiring_test.go pattern. The seeded user is u-1/t-1 with password "s3cret-pw".
func newMFAStepUpFixture(t *testing.T) *mfaHandlerFixture {
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
			ID: "u-1", TenantID: "t-1", Email: "alice@acme.test",
			PasswordHash: string(hash), Status: "active",
		},
	}
	tokens, err := auth.NewTokenService("super-secret-test-key-0123456789", 15*60*time.Second, 7*24*60*60*time.Second)
	if err != nil {
		t.Fatalf("token service: %v", err)
	}
	mfaRepo := newStubMFARepo()
	backup := newStubMFABackupRepo()
	mfaRepo.backup = backup
	svc := services.NewAuthService(repo, newStubRefreshRepo(), tokens).
		WithMFA(mfaRepo, backup, newStubMFAPendingRepo(), stubMFASealer{})

	r := gin.New()
	api := r.Group("/api/v1")
	h := NewAuthHandler(svc, zap.NewNop())
	h.RegisterRoutes(api)

	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		stepUpToken: {
			Subject:   auth.Subject{UserID: "u-1", TenantID: "t-1"},
			TokenType: auth.TokenTypeAccess,
		},
	}}
	protected := api.Group("")
	protected.Use(middleware.AuthMiddleware(ver, zap.NewNop()))
	protected.Use(middleware.RequireTenant(zap.NewNop()))
	h.RegisterProtectedRoutes(protected)

	return &mfaHandlerFixture{router: r, svc: svc}
}

// postStepUp issues an authenticated POST with the stub bearer token.
func postStepUp(r *gin.Engine, path string, body gin.H) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+stepUpToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// mfaStepUpEndpoints is the shared endpoint×body table: each protected MFA endpoint with a
// body that is complete EXCEPT for whatever the individual test mutates.
var mfaStepUpEndpoints = []struct {
	name string
	path string
}{
	{"enroll", "/api/v1/auth/mfa/enroll"},
	{"activate", "/api/v1/auth/mfa/activate"},
	{"disable", "/api/v1/auth/mfa/disable"},
}

// TestMFAStepUpHandler_MissingPassword_422 proves every protected MFA endpoint rejects a body
// without the step-up password as 422 MFA_PASSWORD_REQUIRED (mirrors LOGIN_FIELD_REQUIRED's
// well-formed-JSON-but-incomplete family) — never a 500, never a silent pass-through.
func TestMFAStepUpHandler_MissingPassword_422(t *testing.T) {
	for _, ep := range mfaStepUpEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			f := newMFAStepUpFixture(t)
			body := gin.H{}
			if ep.name != "enroll" {
				body["code"] = "123456"
			}
			w := postStepUp(f.router, ep.path, body)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422, got %d (%s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "MFA_PASSWORD_REQUIRED") {
				t.Errorf("expected code MFA_PASSWORD_REQUIRED, got %s", w.Body.String())
			}
		})
	}
}

// TestMFAStepUpHandler_WrongPassword_401InvalidCredentials proves a wrong step-up password maps
// to the SAME 401 INVALID_CREDENTIALS family as Login/ChangePassword on all three endpoints.
// No enrollment state is needed: the password gate runs before any MFA state is consulted.
func TestMFAStepUpHandler_WrongPassword_401InvalidCredentials(t *testing.T) {
	for _, ep := range mfaStepUpEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			f := newMFAStepUpFixture(t)
			body := gin.H{"password": "totally-wrong"}
			if ep.name != "enroll" {
				body["code"] = "123456"
			}
			w := postStepUp(f.router, ep.path, body)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d (%s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "INVALID_CREDENTIALS") {
				t.Errorf("expected code INVALID_CREDENTIALS, got %s", w.Body.String())
			}
		})
	}
}

// TestMFAStepUpHandler_WrongPasswordLockout_429 proves five wrong step-up passwords trip the
// shared per-account limiter on each endpoint: the 6th attempt — even with the CORRECT
// password — is 429 MFA_TOO_MANY_ATTEMPTS with a Retry-After header. Each subtest uses a fresh
// fixture (fresh limiter) so lockouts cannot bleed between endpoints.
func TestMFAStepUpHandler_WrongPasswordLockout_429(t *testing.T) {
	for _, ep := range mfaStepUpEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			f := newMFAStepUpFixture(t)
			wrong := gin.H{"password": "totally-wrong"}
			right := gin.H{"password": "s3cret-pw"}
			if ep.name != "enroll" {
				wrong["code"] = "123456"
				right["code"] = "123456"
			}
			for i := 0; i < 5; i++ {
				w := postStepUp(f.router, ep.path, wrong)
				if w.Code != http.StatusUnauthorized {
					t.Fatalf("attempt %d: expected 401, got %d (%s)", i+1, w.Code, w.Body.String())
				}
			}
			w := postStepUp(f.router, ep.path, right)
			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("6th attempt (correct password): expected 429, got %d (%s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "MFA_TOO_MANY_ATTEMPTS") {
				t.Errorf("expected code MFA_TOO_MANY_ATTEMPTS, got %s", w.Body.String())
			}
			if w.Header().Get("Retry-After") == "" {
				t.Error("expected a Retry-After header on the 429 response")
			}
		})
	}
}

// TestMFAStepUpHandler_Enroll_CorrectPassword_200 pins the happy path through the real
// protected chain: a correct step-up password yields the ordinary enroll response
// ({secret, otpauthUrl}) — proving the new gate does not break legitimate enrollment.
func TestMFAStepUpHandler_Enroll_CorrectPassword_200(t *testing.T) {
	f := newMFAStepUpFixture(t)
	w := postStepUp(f.router, "/api/v1/auth/mfa/enroll", gin.H{"password": "s3cret-pw"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Secret     string `json:"secret"`
			OtpauthURL string `json:"otpauthUrl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.Secret == "" || resp.Data.OtpauthURL == "" {
		t.Errorf("incomplete enroll response: %s", w.Body.String())
	}
}

// TestMFAStepUpHandler_NoBody_400InvalidBody is the dual-review LOW-2 regression test: a
// COMPLETELY ABSENT (or JSON-malformed) request body on any protected MFA endpoint is a 400
// INVALID_BODY from bindJSON — running BEFORE any field, including password, is ever read —
// distinct from a well-formed JSON body that simply omits password (422 MFA_PASSWORD_REQUIRED,
// covered by TestMFAStepUpHandler_MissingPassword_422 above). enroll is the endpoint whose body
// was entirely optional before MFA-STEPUP; it now requires a body just like activate/disable.
func TestMFAStepUpHandler_NoBody_400InvalidBody(t *testing.T) {
	for _, ep := range mfaStepUpEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			f := newMFAStepUpFixture(t)
			req := httptest.NewRequest(http.MethodPost, ep.path, nil)
			req.Header.Set("Authorization", "Bearer "+stepUpToken)
			w := httptest.NewRecorder()
			f.router.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d (%s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "INVALID_BODY") {
				t.Errorf("expected code INVALID_BODY, got %s", w.Body.String())
			}
		})
	}
}
