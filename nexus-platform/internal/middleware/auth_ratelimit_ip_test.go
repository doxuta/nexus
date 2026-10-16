package middleware

// W5-A3: the auth rate-limiter gains a coarser per-IP backstop window alongside the original
// per-(ip,tenant,email) window, so a password-spray (one source varying the email to dodge the
// per-email window) is throttled by total attempts per IP. Plus a body-restoration fix: the
// key extractor parses only the first 8 KiB but must forward the FULL body downstream.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestAuthRateLimit_PerIP_Backstop verifies that a single IP varying the email is throttled by
// the per-IP window even though each individual email stays under the (higher) per-email cap.
func TestAuthRateLimit_PerIP_Backstop_W5A3(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// Per-email cap is generous (10); the per-IP backstop is tight (3).
	cfg := RateLimitConfig{MaxAttempts: 10, IPMaxAttempts: 3, Window: 10 * time.Second}
	r := newRateLimitRouter(cfg)
	const ip = "203.0.113.7:5555"

	// Three attempts from the same IP, each a DIFFERENT email → each under its own per-email
	// window, but they consume the shared per-IP budget of 3.
	for i := 0; i < 3; i++ {
		body := fmt.Sprintf(`{"tenant":"acme","email":"user%d@x.com"}`, i)
		if w := doRequest(r, ip, body); w.Code != http.StatusOK {
			t.Fatalf("attempt %d from %s: code=%d, want 200", i, ip, w.Code)
		}
	}
	// Fourth attempt from the same IP with yet another fresh email: the per-email window would
	// allow it, but the per-IP backstop is now exhausted → 429.
	if w := doRequest(r, ip, `{"tenant":"acme","email":"user99@x.com"}`); w.Code != http.StatusTooManyRequests {
		t.Errorf("4th attempt from same IP (fresh email): code=%d, want 429 (per-IP backstop)", w.Code)
	}
	// A different IP is unaffected (windows are per-IP).
	if w := doRequest(r, "203.0.113.8:5555", `{"tenant":"acme","email":"user99@x.com"}`); w.Code != http.StatusOK {
		t.Errorf("different IP must be independent: code=%d, want 200", w.Code)
	}
}

// TestAuthRateLimit_PerIP_DisabledWhenZero confirms IPMaxAttempts<=0 preserves the original
// per-email-only behaviour (no per-IP throttle): many different emails from one IP all pass.
func TestAuthRateLimit_PerIP_DisabledWhenZero_W5A3(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := RateLimitConfig{MaxAttempts: 2, IPMaxAttempts: 0, Window: 10 * time.Second}
	r := newRateLimitRouter(cfg)
	const ip = "203.0.113.9:5555"
	for i := 0; i < 6; i++ { // 6 distinct emails, each well under the per-email cap of 2
		body := fmt.Sprintf(`{"tenant":"acme","email":"u%d@x.com"}`, i)
		if w := doRequest(r, ip, body); w.Code != http.StatusOK {
			t.Fatalf("IPMaxAttempts=0 must not throttle distinct emails; attempt %d code=%d", i, w.Code)
		}
	}
}

// TestAuthRateLimit_BodyRestoredFull_NotTruncated verifies the MultiReader fix: a body larger
// than the 8 KiB key-parse window is forwarded to the downstream handler in FULL.
func TestAuthRateLimit_BodyRestoredFull_NotTruncated_W5A3(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(AuthRateLimit(RateLimitConfig{MaxAttempts: 100, IPMaxAttempts: 100, Window: time.Second}))
	var gotLen int
	r.POST("/auth/login", func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		gotLen = len(b)
		c.JSON(http.StatusOK, gin.H{"n": len(b)})
	})

	// 20 KiB of padding → well past the 8 KiB parse window.
	big := `{"tenant":"acme","email":"a@b.com","pad":"` + strings.Repeat("x", 20000) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "198.51.100.1:1234"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200", w.Code)
	}
	if gotLen != len(big) {
		t.Errorf("downstream handler received %d bytes; want full %d (body was truncated!)", gotLen, len(big))
	}
}
