package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// newRateLimitRouter builds a gin engine with AuthRateLimit(cfg) mounted globally and a
// POST /auth/login stub handler that always returns 200 (simulating a valid login path).
// The rate limiter fires BEFORE the handler, so once the limit is exceeded the stub is
// never called and the limiter returns 429.
func newRateLimitRouter(cfg RateLimitConfig) *gin.Engine {
	r := gin.New()
	r.Use(AuthRateLimit(cfg))
	r.POST("/auth/login", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

// doRequest fires a POST /auth/login against the engine from remoteAddr with the given
// JSON body. Pass an empty string for body when no payload needs to be sent.
func doRequest(r *gin.Engine, remoteAddr, body string) *httptest.ResponseRecorder {
	var bodyReader *strings.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	} else {
		bodyReader = strings.NewReader("")
	}
	req := httptest.NewRequest(http.MethodPost, "/auth/login", bodyReader)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAuthRateLimit_WithinLimit_Passes(t *testing.T) {
	cfg := RateLimitConfig{MaxAttempts: 5, Window: 10 * time.Second}
	r := newRateLimitRouter(cfg)

	// First 5 requests from the same IP must pass (no tenant/email in body, IP-only key).
	for i := 0; i < 5; i++ {
		w := doRequest(r, "1.2.3.4:9999", "")
		if w.Code != http.StatusOK {
			t.Errorf("request %d: expected 200 within limit, got %d", i+1, w.Code)
		}
	}
}

func TestAuthRateLimit_ExceedLimit_Returns429(t *testing.T) {
	cfg := RateLimitConfig{MaxAttempts: 3, Window: 10 * time.Second}
	r := newRateLimitRouter(cfg)

	ip := "10.0.0.1:1234"
	body := `{"tenant":"acme","email":"alice@acme.io","password":"x"}`
	// Exhaust the limit for (ip, tenant, email) composite key.
	for i := 0; i < 3; i++ {
		doRequest(r, ip, body)
	}
	// The N+1 request on the same key must be rate-limited.
	w := doRequest(r, ip, body)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after limit exhausted, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header on 429 response")
	}
}

func TestAuthRateLimit_DifferentIPs_Independent(t *testing.T) {
	// RM-15: one IP's lockout must NOT affect a different IP with the same credentials.
	cfg := RateLimitConfig{MaxAttempts: 2, Window: 10 * time.Second}
	r := newRateLimitRouter(cfg)

	body := `{"tenant":"acme","email":"alice@acme.io","password":"x"}`
	ipA := "192.168.1.1:5000"
	ipB := "192.168.1.2:5000"

	// Exhaust the limit for ipA.
	doRequest(r, ipA, body)
	doRequest(r, ipA, body)
	// ipA is now at the limit; next request should be 429.
	if w := doRequest(r, ipA, body); w.Code != http.StatusTooManyRequests {
		t.Errorf("ipA: expected 429, got %d", w.Code)
	}

	// ipB has made no requests yet; its first request must still pass (independent key).
	if w := doRequest(r, ipB, body); w.Code != http.StatusOK {
		t.Errorf("ipB: expected 200 (independent window per IP), got %d", w.Code)
	}
}

// TestAuthRateLimit_DifferentEmails_Independent verifies the NAT / corporate-egress
// safety guarantee: two users sharing the same IP but using different email addresses
// each have their own window, so exhausting one user's window must NOT lock out the other.
//
// Without email in the key this test would fail: ipA+emailA exhausting the window would
// also block ipA+emailB because both hash to "auth:ipA::" without the email component.
func TestAuthRateLimit_DifferentEmails_Independent(t *testing.T) {
	cfg := RateLimitConfig{MaxAttempts: 2, Window: 10 * time.Second}
	r := newRateLimitRouter(cfg)

	ip := "10.0.0.99:5000"
	bodyA := `{"tenant":"acme","email":"alice@acme.io","password":"x"}`
	bodyB := `{"tenant":"acme","email":"bob@acme.io","password":"y"}`

	// Exhaust the limit for (ip, acme, alice@acme.io).
	doRequest(r, ip, bodyA)
	doRequest(r, ip, bodyA)
	if w := doRequest(r, ip, bodyA); w.Code != http.StatusTooManyRequests {
		t.Errorf("alice: expected 429 after limit, got %d", w.Code)
	}

	// Bob uses the same IP and same tenant but a different email.
	// His window must be completely independent — he should get 200.
	if w := doRequest(r, ip, bodyB); w.Code != http.StatusOK {
		t.Errorf("bob (same IP+tenant, different email): expected 200 (independent window), got %d: %s",
			w.Code, w.Body.String())
	}
}

func TestAuthRateLimit_ZeroConfig_NoOp(t *testing.T) {
	// MaxAttempts=0 or Window=0 disables rate limiting (no-op).
	cfg := RateLimitConfig{MaxAttempts: 0, Window: 10 * time.Second}
	r := newRateLimitRouter(cfg)

	ip := "5.5.5.5:80"
	body := `{"tenant":"t","email":"e@t.io"}`
	// Flood 100 requests — none should be blocked since the limiter is disabled.
	for i := 0; i < 100; i++ {
		w := doRequest(r, ip, body)
		if w.Code == http.StatusTooManyRequests {
			t.Errorf("request %d: got unexpected 429 when limiter is disabled", i+1)
		}
	}
}

func TestAuthRateLimit_429_IsGeneric(t *testing.T) {
	// The 429 body must not reveal whether the tenant/email exists (anti-enumeration).
	cfg := RateLimitConfig{MaxAttempts: 1, Window: 10 * time.Second}
	r := newRateLimitRouter(cfg)

	ip := "9.9.9.9:443"
	body := `{"tenant":"acme","email":"alice@acme.io","password":"x"}`
	doRequest(r, ip, body) // consume the single allowed attempt

	w := doRequest(r, ip, body)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}
	respBody := w.Body.String()
	// The body must contain the error code but must NOT mention specific tenant or email.
	if respBody == "" {
		t.Error("expected non-empty body for 429")
	}
	// Code must be RATE_LIMITED per the spec.
	if !strings.Contains(respBody, "RATE_LIMITED") {
		t.Errorf("expected code=RATE_LIMITED in 429 body, got: %s", respBody)
	}
	// Must NOT contain enumeration signals.
	for _, leak := range []string{"tenant", "email", "user", "password", "not found", "invalid"} {
		if strings.Contains(respBody, leak) {
			t.Errorf("429 body leaks %q — enumeration risk: %s", leak, respBody)
		}
	}
}

func TestAuthRateLimit_WindowSlides(t *testing.T) {
	// After the window expires, new requests should be allowed again.
	cfg := RateLimitConfig{MaxAttempts: 2, Window: 50 * time.Millisecond}
	r := newRateLimitRouter(cfg)

	ip := "3.3.3.3:80"
	body := `{"tenant":"t","email":"e@t.io"}`
	// Exhaust limit.
	doRequest(r, ip, body)
	doRequest(r, ip, body)
	if w := doRequest(r, ip, body); w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after limit, got %d", w.Code)
	}

	// Wait for the window to expire.
	time.Sleep(60 * time.Millisecond)

	// Now a fresh request should pass.
	if w := doRequest(r, ip, body); w.Code != http.StatusOK {
		t.Errorf("expected 200 after window expired, got %d: %s", w.Code, w.Body.String())
	}
}
