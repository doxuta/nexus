package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// buildSSORateLimitedRouter wires the REAL production route structure (RegisterAPIRoutes +
// BOTH WithAuthRateLimiter and WithSSORateLimiter) over a stub-backed AuthService and a fake
// SSO service, so these tests verify the SSO-RATELIMIT-NAT WIRING property: the public SSO
// endpoints ride their own (larger) bucket while /auth/login keeps its strict one, and
// neither burns the other's budget. authCfg/ssoCfg are deliberately DIFFERENT sizes so a
// wiring mistake (SSO still on the auth limiter, or vice-versa) trips the assertions.
func buildSSORateLimitedRouter(t *testing.T, authCfg middleware.RateLimitConfig, ssoCfg middleware.SSORateLimitConfig) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret-pw"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	repo := &stubUserRepo{
		tenant: &auth.Tenant{ID: "t-1", Slug: "acme", Status: "active"},
		user: &auth.User{
			ID: "u-1", TenantID: "t-1", Email: "alice@acme.test",
			PasswordHash: string(hash), Status: "active", IsAdmin: true,
		},
	}
	tokens, err := auth.NewTokenService("super-secret-test-key-0123456789", 15*60*time.Second, 7*24*60*60*time.Second)
	if err != nil {
		t.Fatalf("token service: %v", err)
	}
	svc := services.NewAuthService(repo, newStubRefreshRepo(), tokens)

	// fakeSSOSvc (sso_handler_test.go): StartLogin → 302 redirect; HandleCallback with
	// callbackOut set → 200 login envelope. Both handlers therefore exercise the FULL happy
	// path underneath the limiter, so a 429 in these tests can only come from the limiter.
	ssoSvc := &fakeSSOSvc{callbackOut: &services.LoginOutput{AccessToken: "at", RefreshToken: "rt"}}

	r := gin.New()
	api := r.Group("/api/v1")
	RegisterAPIRoutes(api, tokens, nil, APIHandlers{
		Auth: NewAuthHandler(svc, zap.NewNop()),
		SSO:  NewSSOHandler(ssoSvc, ssoTestRedirectBase, zap.NewNop()),
	}, zap.NewNop(),
		WithAuthRateLimiter(middleware.AuthRateLimit(authCfg)),
		WithSSORateLimiter(middleware.SSORateLimit(ssoCfg)),
	)
	return r
}

func getSSOStartRL(r *gin.Engine, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sso/acme/start?provider=google", nil)
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func getSSOCallbackRL(r *gin.Engine, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sso/callback?code=c&state=s", nil)
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func postLoginSSORL(r *gin.Engine, body, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestSSOStart_UsesSSOLimiter_NotAuthLimiter is the headline SSO-RATELIMIT-NAT assertion:
// with the auth bucket at 3 and the SSO bucket at 6, requests 4-6 to /start MUST still pass
// (under the old shared wiring they 429'd at the auth threshold) and request 7 MUST 429 at
// the SSO threshold.
func TestSSOStart_UsesSSOLimiter_NotAuthLimiter(t *testing.T) {
	r := buildSSORateLimitedRouter(t,
		middleware.RateLimitConfig{MaxAttempts: 3, IPMaxAttempts: 3, Window: time.Minute},
		middleware.SSORateLimitConfig{MaxAttempts: 6, Window: time.Minute},
	)

	ip := "7.7.7.7:7000" // one NAT egress: every request shares one (ip,tenant,provider) bucket
	for i := 0; i < 6; i++ {
		w := getSSOStartRL(r, ip)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d: /start 429'd below the SSO threshold — still sharing the strict /auth bucket", i+1)
		}
		if w.Code != http.StatusFound {
			t.Fatalf("request %d: /start = %d, want 302 (body %s)", i+1, w.Code, w.Body.String())
		}
	}

	w := getSSOStartRL(r, ip)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("request 7: /start = %d, want 429 at the SSO threshold", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header on 429")
	}
	if !strings.Contains(w.Body.String(), "RATE_LIMITED") {
		t.Errorf("expected code=RATE_LIMITED in 429 body, got: %s", w.Body.String())
	}
}

// TestSSOCallback_UsesSSOLimiter proves /callback also rides the dedicated bucket (per-IP
// variant): it admits the full SSO budget — beyond the auth threshold — then 429s.
func TestSSOCallback_UsesSSOLimiter(t *testing.T) {
	r := buildSSORateLimitedRouter(t,
		middleware.RateLimitConfig{MaxAttempts: 3, IPMaxAttempts: 3, Window: time.Minute},
		middleware.SSORateLimitConfig{MaxAttempts: 6, Window: time.Minute},
	)

	ip := "7.7.7.8:7000"
	for i := 0; i < 6; i++ {
		w := getSSOCallbackRL(r, ip)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d: /callback 429'd below the SSO threshold", i+1)
		}
	}
	if w := getSSOCallbackRL(r, ip); w.Code != http.StatusTooManyRequests {
		t.Fatalf("request 7: /callback = %d, want 429 at the SSO threshold", w.Code)
	}
}

// TestSSOBurst_DoesNotBurnAuthBudget: after an SSO burst has TRIPPED the SSO limiter, a
// password login from the SAME IP must still be admitted — the whole point of the slice is
// that an enterprise's SSO traffic can no longer lock the company out of /auth/login.
func TestSSOBurst_DoesNotBurnAuthBudget(t *testing.T) {
	r := buildSSORateLimitedRouter(t,
		middleware.RateLimitConfig{MaxAttempts: 3, IPMaxAttempts: 3, Window: time.Minute},
		middleware.SSORateLimitConfig{MaxAttempts: 6, Window: time.Minute},
	)

	ip := "7.7.7.9:7000"
	for i := 0; i < 7; i++ {
		getSSOStartRL(r, ip) // 6 admitted + 1 tripped — SSO bucket fully burned
	}

	w := postLoginSSORL(r, `{"tenant":"acme","email":"alice@acme.test","password":"s3cret-pw"}`, ip)
	if w.Code == http.StatusTooManyRequests {
		t.Fatalf("/auth/login 429'd after an SSO burst — SSO traffic is burning the /auth budget")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("/auth/login = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
}

// TestAuthLogin_StillStrict_AndDoesNotBurnSSOBudget: the converse isolation — /auth/login
// still trips at its OWN (lower) threshold, and a login lockout does not throttle SSO.
func TestAuthLogin_StillStrict_AndDoesNotBurnSSOBudget(t *testing.T) {
	r := buildSSORateLimitedRouter(t,
		middleware.RateLimitConfig{MaxAttempts: 3, IPMaxAttempts: 100, Window: time.Minute},
		middleware.SSORateLimitConfig{MaxAttempts: 6, Window: time.Minute},
	)

	ip := "7.7.8.1:7000"
	body := `{"tenant":"acme","email":"alice@acme.test","password":"wrong-pw"}`
	got429 := false
	for i := 0; i < 6; i++ {
		if w := postLoginSSORL(r, body, ip); w.Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatalf("expected /auth/login to 429 at its own strict threshold (RM-15 intact)")
	}

	// The login lockout must not have consumed any SSO budget.
	if w := getSSOStartRL(r, ip); w.Code != http.StatusFound {
		t.Fatalf("/start = %d after login lockout, want 302 (independent budgets)", w.Code)
	}
}

// TestSSORoutes_FallBackToAuthLimiter_WhenSSOOptionAbsent guards the fail-secure fallback:
// a caller wiring ONLY WithAuthRateLimiter (pre-slice harnesses) keeps SSO throttled by the
// auth limiter — moving SSO off the auth group must never leave it unthrottled by omission.
func TestSSORoutes_FallBackToAuthLimiter_WhenSSOOptionAbsent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ssoSvc := &fakeSSOSvc{}
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), nil, nil, APIHandlers{
		SSO: NewSSOHandler(ssoSvc, ssoTestRedirectBase, zap.NewNop()),
	}, zap.NewNop(),
		WithAuthRateLimiter(middleware.AuthRateLimit(
			middleware.RateLimitConfig{MaxAttempts: 2, Window: time.Minute})),
	)

	ip := "7.7.8.2:7000"
	getSSOStartRL(r, ip)
	getSSOStartRL(r, ip)
	if w := getSSOStartRL(r, ip); w.Code != http.StatusTooManyRequests {
		t.Fatalf("/start = %d, want 429 from the auth-limiter fallback (SSO must not be unthrottled)", w.Code)
	}
}
