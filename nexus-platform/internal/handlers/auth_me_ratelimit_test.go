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

// buildRateLimitedAuthRouter wires the REAL production route structure (RegisterAPIRoutes +
// WithAuthRateLimiter) over a stub-backed AuthService, so the test verifies the WIRING property —
// GET /auth/me is exempt from the /auth/* limiter while the credential endpoints are not. It
// returns the engine and a valid Bearer access token for the seeded user.
func buildRateLimitedAuthRouter(t *testing.T, cfg middleware.RateLimitConfig) (*gin.Engine, string) {
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

	r := gin.New()
	api := r.Group("/api/v1")
	RegisterAPIRoutes(api, tokens, nil, APIHandlers{Auth: NewAuthHandler(svc, zap.NewNop())}, zap.NewNop(),
		WithAuthRateLimiter(middleware.AuthRateLimit(cfg)))

	access, err := tokens.IssueAccessToken(auth.Subject{UserID: "u-1", TenantID: "t-1", IsAdmin: true})
	if err != nil {
		t.Fatalf("issue access token: %v", err)
	}
	return r, access
}

func getMeRL(r *gin.Engine, bearer, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func postLoginRL(r *gin.Engine, body, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestAuthMe_ExemptFromRateLimiter hammers GET /auth/me far past the limit and asserts it never
// 429s (AUTH-ME-RATELIMIT / catalog B11): fast FE navigation must not self-lock login.
func TestAuthMe_ExemptFromRateLimiter(t *testing.T) {
	cfg := middleware.RateLimitConfig{MaxAttempts: 3, IPMaxAttempts: 3, Window: time.Minute}
	r, access := buildRateLimitedAuthRouter(t, cfg)

	const hammer = 10 // > both MaxAttempts and IPMaxAttempts
	for i := 0; i < hammer; i++ {
		w := getMeRL(r, access, "9.9.9.9:5555")
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d: /auth/me returned 429 — it must be exempt from the /auth/* limiter", i+1)
		}
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: /auth/me = %d, want 200 (body %s)", i+1, w.Code, w.Body.String())
		}
	}
}

// TestAuthLogin_StillRateLimited proves the RM-15 protection is intact: the credential endpoint
// still 429s once the per-(tenant+email+IP) window is exceeded. Guards against the exemption being
// implemented by disabling the limiter wholesale.
func TestAuthLogin_StillRateLimited(t *testing.T) {
	cfg := middleware.RateLimitConfig{MaxAttempts: 3, IPMaxAttempts: 100, Window: time.Minute}
	r, _ := buildRateLimitedAuthRouter(t, cfg)

	const body = `{"tenant":"acme","email":"alice@acme.test","password":"wrong-pw"}`
	got429 := false
	for i := 0; i < 6; i++ {
		w := postLoginRL(r, body, "8.8.8.8:4444")
		if w.Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatalf("expected /auth/login to 429 after exceeding the limit (RM-15) — limiter not applied")
	}
}
