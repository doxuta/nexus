package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/auth"
)

// newAssistantRateLimitRouter builds a gin engine mirroring the production wiring: a stub that
// seeds the verified JWT subject (as AuthMiddleware would), then AssistantRateLimit(cfg), then a
// 200 stub for POST /assistant/ask. The subject seeder runs first so the limiter reads the same
// (tenant,user) it does in production.
func newAssistantRateLimitRouter(cfg AssistantRateLimitConfig) *gin.Engine {
	r := gin.New()
	grp := r.Group("")
	grp.Use(AssistantRateLimit(cfg))
	grp.POST("/assistant/ask", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	return r
}

// doAssistantAsk fires POST /assistant/ask as the given (tenant,user). Identity is carried in test
// headers that the SHARED engine's seeder (buildSharedAssistantRouter) turns into a verified
// subject — so every call hits the same limiter state (a fresh engine per call would reset it and
// hide isolation bugs).
func doAssistantAsk(r *gin.Engine, tenantID, userID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/assistant/ask", strings.NewReader(`{"message":"hi"}`))
	req.Header.Set("X-Test-Tenant", tenantID)
	req.Header.Set("X-Test-User", userID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// buildSharedAssistantRouter mounts a header-driven subject seeder BEFORE a single shared
// AssistantRateLimit(cfg) instance, so multiple (tenant,user) identities hit the SAME limiter
// (the only way to prove per-(tenant,user) isolation — a per-call engine would reset state).
func buildSharedAssistantRouter(cfg AssistantRateLimitConfig) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if t := c.GetHeader("X-Test-Tenant"); t != "" {
			c.Set(ctxKeyClaims, &auth.Claims{Subject: auth.Subject{TenantID: t, UserID: c.GetHeader("X-Test-User")}})
		}
		c.Next()
	})
	r.Use(AssistantRateLimit(cfg))
	r.POST("/assistant/ask", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	return r
}

// TestAssistantRateLimit_TripsAtThreshold: one (tenant,user) is admitted exactly MaxAttempts times,
// then 429s with the canonical RATE_LIMITED + Retry-After envelope (same as the other limiters, so
// the FE's existing 429 handling covers it).
func TestAssistantRateLimit_TripsAtThreshold(t *testing.T) {
	r := buildSharedAssistantRouter(AssistantRateLimitConfig{MaxAttempts: 3, Window: 10 * time.Second})

	for i := 0; i < 3; i++ {
		if w := doAssistantAsk(r, "tenant-a", "user-1"); w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200 within budget, got %d", i+1, w.Code)
		}
	}
	w := doAssistantAsk(r, "tenant-a", "user-1")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("request 4: expected 429 past budget, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header on 429 response")
	}
	if !strings.Contains(w.Body.String(), "RATE_LIMITED") {
		t.Errorf("expected code=RATE_LIMITED in 429 body, got: %s", w.Body.String())
	}
}

// TestAssistantRateLimit_PerUserIsolation is THE property this slice exists for: one user
// exhausting their budget must NOT throttle a DIFFERENT user in the SAME tenant — otherwise the
// per-user cap would be no better than the per-tenant budget it sits in front of.
func TestAssistantRateLimit_PerUserIsolation(t *testing.T) {
	r := buildSharedAssistantRouter(AssistantRateLimitConfig{MaxAttempts: 2, Window: 10 * time.Second})

	// user-1 exhausts their own budget.
	doAssistantAsk(r, "tenant-a", "user-1")
	doAssistantAsk(r, "tenant-a", "user-1")
	if w := doAssistantAsk(r, "tenant-a", "user-1"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("user-1: expected 429 after exhausting own budget, got %d", w.Code)
	}
	// user-2 in the SAME tenant still has a full budget.
	if w := doAssistantAsk(r, "tenant-a", "user-2"); w.Code != http.StatusOK {
		t.Errorf("user-2: expected 200 (independent per-user bucket), got %d", w.Code)
	}
}

// TestAssistantRateLimit_PerTenantIsolation: the same user id under a DIFFERENT tenant is a
// distinct bucket (the key is tenant+user, not bare user), so two tenants that happen to mint the
// same user id do not share a limit.
func TestAssistantRateLimit_PerTenantIsolation(t *testing.T) {
	r := buildSharedAssistantRouter(AssistantRateLimitConfig{MaxAttempts: 2, Window: 10 * time.Second})

	doAssistantAsk(r, "tenant-a", "user-1")
	doAssistantAsk(r, "tenant-a", "user-1")
	if w := doAssistantAsk(r, "tenant-a", "user-1"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("tenant-a/user-1: expected 429 after limit, got %d", w.Code)
	}
	if w := doAssistantAsk(r, "tenant-b", "user-1"); w.Code != http.StatusOK {
		t.Errorf("tenant-b/user-1: expected 200 (independent per-tenant bucket), got %d", w.Code)
	}
}

// TestAssistantRateLimit_NoSubject_PassThrough: a request with no verified subject is passed
// through (the auth chain rejects it anyway; rate-limiting here would key every unauthenticated
// call together into one shared bucket — a self-inflicted DoS lever).
func TestAssistantRateLimit_NoSubject_PassThrough(t *testing.T) {
	r := newAssistantRateLimitRouter(AssistantRateLimitConfig{MaxAttempts: 1, Window: 10 * time.Second})

	for i := 0; i < 50; i++ {
		req := httptest.NewRequest(http.MethodPost, "/assistant/ask", strings.NewReader(`{"message":"hi"}`))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d: unexpected 429 for subject-less request (should pass through)", i+1)
		}
	}
}

// TestAssistantRateLimit_EmptyUserID_PassThrough: a verified token that somehow carries a tenant
// but an EMPTY user id (no current mint path produces one, but a future service-account/API-key
// path might) is passed through rather than collapsed into one shared "tenant:" bucket. This pins
// the fail-open-on-incomplete-subject invariant the security audit flagged (LOW: keep the empty-uid
// branch harmless — never a cross-user shared-bucket lever).
func TestAssistantRateLimit_EmptyUserID_PassThrough(t *testing.T) {
	r := buildSharedAssistantRouter(AssistantRateLimitConfig{MaxAttempts: 1, Window: 10 * time.Second})
	for i := 0; i < 25; i++ {
		if w := doAssistantAsk(r, "tenant-a", ""); w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d: empty user id must pass through, not 429 into a shared tenant bucket", i+1)
		}
	}
}

// TestAssistantRateLimit_ZeroConfig_NoOp mirrors the other limiters: MaxAttempts<=0 or Window<=0
// disables the middleware entirely.
func TestAssistantRateLimit_ZeroConfig_NoOp(t *testing.T) {
	r := buildSharedAssistantRouter(AssistantRateLimitConfig{MaxAttempts: 0, Window: 10 * time.Second})
	for i := 0; i < 100; i++ {
		if w := doAssistantAsk(r, "tenant-a", "user-1"); w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d: got unexpected 429 when limiter is disabled", i+1)
		}
	}
}

// TestAssistantRateLimit_WindowSlides: after the window expires the bucket refills (shared
// memRateLimiter machinery, asserted here through the assistant key path).
func TestAssistantRateLimit_WindowSlides(t *testing.T) {
	r := buildSharedAssistantRouter(AssistantRateLimitConfig{MaxAttempts: 2, Window: 50 * time.Millisecond})

	doAssistantAsk(r, "tenant-a", "user-1")
	doAssistantAsk(r, "tenant-a", "user-1")
	if w := doAssistantAsk(r, "tenant-a", "user-1"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after limit, got %d", w.Code)
	}

	time.Sleep(60 * time.Millisecond)

	if w := doAssistantAsk(r, "tenant-a", "user-1"); w.Code != http.StatusOK {
		t.Errorf("expected 200 after window expired, got %d", w.Code)
	}
}

// TestAssistantRateLimit_ConcurrentStormAdmitsExactlyMax: one user's parallel ⌘K burst must stop
// at exactly MaxAttempts — a non-atomic check-and-record lets it take more than its fair share of
// the tenant's shared turn window.
func TestAssistantRateLimit_ConcurrentStormAdmitsExactlyMax(t *testing.T) {
	const max = 5
	assertStormAdmitsExactly(t, max,
		func() *gin.Engine {
			return buildSharedAssistantRouter(AssistantRateLimitConfig{MaxAttempts: max, Window: time.Minute})
		},
		func(r *gin.Engine) int { return doAssistantAsk(r, "tenant-a", "user-1").Code })
}
