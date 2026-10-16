package handlers

// MFA-ENFORCE handler-layer tests: POST /auth/login must map the fail-closed sentinel
// (services.ErrMFAEnrolledButUnavailable — enrolled account, MFA key lost) to
// 503 MFA_UNAVAILABLE, and the SSO callback must map the SAME sentinel identically.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// stubMFAEnrollment satisfies the services enrollment-reader seam structurally (like
// stubUserRepo does for userRepo): Get reports one fixed (tenant,user) as MFA-enabled and
// everyone else as never-enrolled.
type stubMFAEnrollment struct {
	tenantID string
	userID   string
}

func (s *stubMFAEnrollment) Get(_ context.Context, tenantID, userID string) (*auth.UserMFA, error) {
	if tenantID == s.tenantID && userID == s.userID {
		return &auth.UserMFA{
			ID: "mfa-1", TenantID: tenantID, UserID: userID,
			SecretEnc: "sealed:unreadable-without-key", Enabled: true,
		}, nil
	}
	return nil, auth.ErrMFANotEnrolled
}

// newMFAEnforceTestRouter mirrors newAuthTestRouter but wires the "key absent" production
// shape: the standalone enrollment reader is injected (WithMFAEnrollmentReader), WithMFA is
// never called (no sealer). Returns the engine and the seeded password.
func newMFAEnforceTestRouter(t *testing.T, failOpen bool) (*gin.Engine, string) {
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
	svc := services.NewAuthService(repo, newStubRefreshRepo(), tokens).
		WithMFAEnrollmentReader(&stubMFAEnrollment{tenantID: "t-1", userID: "u-1"}, failOpen)

	r := gin.New()
	NewAuthHandler(svc, zap.NewNop()).RegisterRoutes(r.Group("/api/v1"))
	return r, password
}

// TestLoginHandler_EnrolledKeyAbsent_503MFAUnavailable: correct credentials + enrolled account
// + missing MFA key → 503 MFA_UNAVAILABLE with the honest message, and NO token material.
func TestLoginHandler_EnrolledKeyAbsent_503MFAUnavailable(t *testing.T) {
	r, password := newMFAEnforceTestRouter(t, false)
	w := postJSON(r, "/api/v1/auth/login", gin.H{
		"tenant": "acme", "email": "alice@acme.test", "password": password,
	})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Code != "MFA_UNAVAILABLE" {
		t.Errorf("code = %q; want MFA_UNAVAILABLE", resp.Code)
	}
	if !strings.Contains(resp.Error, "thiếu khóa MFA") {
		t.Errorf("error message %q does not carry the honest missing-key explanation", resp.Error)
	}
	for _, forbidden := range []string{"accessToken", "refreshToken", "pendingToken"} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Errorf("fail-closed 503 body leaked %q: %s", forbidden, w.Body.String())
		}
	}
}

// TestLoginHandler_EnrolledKeyAbsent_FailOpen_200: with the INFRA-EMERGENCY flag the same
// request completes as an ordinary one-step login.
func TestLoginHandler_EnrolledKeyAbsent_FailOpen_200(t *testing.T) {
	r, password := newMFAEnforceTestRouter(t, true)
	w := postJSON(r, "/api/v1/auth/login", gin.H{
		"tenant": "acme", "email": "alice@acme.test", "password": password,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 under MFA_FAIL_OPEN, got %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Data services.LoginOutput `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.AccessToken == "" || resp.Data.RefreshToken == "" || resp.Data.User == nil {
		t.Fatalf("fail-open login incomplete: %s", w.Body.String())
	}
	if resp.Data.MFAPending {
		t.Fatalf("fail-open login must not return the pending shape: %s", w.Body.String())
	}
}

// TestLoginHandler_WrongPassword_EnrolledKeyAbsent_Still401: the fail-closed 503 must never
// fire ahead of a real credential failure — a wrong password on the enrolled account stays the
// generic 401 (no "this account has MFA" signal for unauthenticated probers).
func TestLoginHandler_WrongPassword_EnrolledKeyAbsent_Still401(t *testing.T) {
	r, _ := newMFAEnforceTestRouter(t, false)
	w := postJSON(r, "/api/v1/auth/login", gin.H{
		"tenant": "acme", "email": "alice@acme.test", "password": "totally-wrong",
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong password, got %d (%s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "MFA") {
		t.Errorf("wrong-password response leaked MFA state: %s", w.Body.String())
	}
}

// TestSSOHandler_Callback_EnrolledKeyAbsent_503MFAUnavailable: the SSO callback maps the SAME
// sentinel to the SAME 503 MFA_UNAVAILABLE — parity with the password login path (otherwise a
// generic 500 would hide that the block is deliberate and actionable).
func TestSSOHandler_Callback_EnrolledKeyAbsent_503MFAUnavailable(t *testing.T) {
	code, body := ssoCallbackResponse(&fakeSSOSvc{callbackErr: services.ErrMFAEnrolledButUnavailable})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d (%s)", code, body)
	}
	if !strings.Contains(body, `"MFA_UNAVAILABLE"`) {
		t.Errorf("SSO callback body missing MFA_UNAVAILABLE code: %s", body)
	}
	if !strings.Contains(body, "thiếu khóa MFA") {
		t.Errorf("SSO callback message %q lacks the honest missing-key explanation", body)
	}
}
