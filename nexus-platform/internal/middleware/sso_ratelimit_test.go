package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// newSSORateLimitRouter builds a gin engine mirroring the production SSO group shape:
// SSORateLimit(cfg) mounted on a group carrying GET /auth/sso/:tenant/start and
// GET /auth/sso/callback stubs that always return 200. The limiter fires BEFORE the
// handlers, so once a bucket is exhausted the stub is never reached and 429 comes back.
func newSSORateLimitRouter(cfg SSORateLimitConfig) *gin.Engine {
	r := gin.New()
	okStub := func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) }
	grp := r.Group("")
	grp.Use(SSORateLimit(cfg))
	grp.GET("/auth/sso/:tenant/start", okStub)
	grp.GET("/auth/sso/callback", okStub)
	grp.POST("/auth/sso/exchange", okStub)
	return r
}

// doSSOExchange fires POST /auth/sso/exchange from remoteAddr (SSO-FE-REDIRECT).
func doSSOExchange(r *gin.Engine, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/auth/sso/exchange", strings.NewReader(`{"code":"x"}`))
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// doSSOStart fires GET /auth/sso/<tenant>/start?provider=<provider> from remoteAddr.
func doSSOStart(r *gin.Engine, remoteAddr, tenant, provider string) *httptest.ResponseRecorder {
	url := fmt.Sprintf("/auth/sso/%s/start?provider=%s", tenant, provider)
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// doSSOCallback fires GET /auth/sso/callback?code=c&state=s from remoteAddr.
func doSSOCallback(r *gin.Engine, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/auth/sso/callback?code=c&state=s", nil)
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestSSORateLimit_Start_TripsAtSSOThreshold_NotBefore proves the core SSO-RATELIMIT-NAT
// property: the SAME (ip, tenant, provider) bucket admits exactly MaxAttempts requests —
// crucially MORE than the strict /auth default would — then 429s with the canonical
// Retry-After + RATE_LIMITED envelope.
func TestSSORateLimit_Start_TripsAtSSOThreshold_NotBefore(t *testing.T) {
	cfg := SSORateLimitConfig{MaxAttempts: 6, Window: 10 * time.Second}
	r := newSSORateLimitRouter(cfg)

	ip := "10.1.1.1:9999" // one NAT egress IP, one tenant, one provider → one shared bucket
	for i := 0; i < 6; i++ {
		w := doSSOStart(r, ip, "acme", "google")
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200 within SSO budget, got %d", i+1, w.Code)
		}
	}

	w := doSSOStart(r, ip, "acme", "google")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("request 7: expected 429 past SSO budget, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header on 429 response")
	}
	if !strings.Contains(w.Body.String(), "RATE_LIMITED") {
		t.Errorf("expected code=RATE_LIMITED in 429 body, got: %s", w.Body.String())
	}
}

// TestSSORateLimit_Start_DifferentTenantOrProvider_IndependentBuckets: one tenant's (or
// provider's) exhausted bucket must not throttle another — the key is (ip, tenant, provider),
// not bare IP, so two companies sharing an egress (hosting provider, carrier-grade NAT) do
// not lock each other out.
func TestSSORateLimit_Start_DifferentTenantOrProvider_IndependentBuckets(t *testing.T) {
	cfg := SSORateLimitConfig{MaxAttempts: 2, Window: 10 * time.Second}
	r := newSSORateLimitRouter(cfg)

	ip := "10.2.2.2:1234"
	// Exhaust (acme, google).
	doSSOStart(r, ip, "acme", "google")
	doSSOStart(r, ip, "acme", "google")
	if w := doSSOStart(r, ip, "acme", "google"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("(acme,google): expected 429 after limit, got %d", w.Code)
	}

	// Same tenant, DIFFERENT provider → independent bucket.
	if w := doSSOStart(r, ip, "acme", "okta"); w.Code != http.StatusOK {
		t.Errorf("(acme,okta): expected 200 (independent bucket per provider), got %d", w.Code)
	}
	// DIFFERENT tenant, same provider → independent bucket.
	if w := doSSOStart(r, ip, "beta", "google"); w.Code != http.StatusOK {
		t.Errorf("(beta,google): expected 200 (independent bucket per tenant), got %d", w.Code)
	}
}

// TestSSORateLimit_Start_DifferentIPs_Independent documents the deliberate inclusion of the
// source IP in the /start key: a remote attacker exhausting (tenant, provider) from THEIR IP
// must not lock the tenant's real users (different IP) out of SSO. Without the IP component
// a bare (tenant,provider) key would be a cross-IP tenant-wide DoS lever.
func TestSSORateLimit_Start_DifferentIPs_Independent(t *testing.T) {
	cfg := SSORateLimitConfig{MaxAttempts: 2, Window: 10 * time.Second}
	r := newSSORateLimitRouter(cfg)

	attacker := "6.6.6.6:6666"
	victim := "10.3.3.3:3333"

	doSSOStart(r, attacker, "acme", "google")
	doSSOStart(r, attacker, "acme", "google")
	if w := doSSOStart(r, attacker, "acme", "google"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("attacker: expected 429 after exhausting its own bucket, got %d", w.Code)
	}

	// The enterprise's own egress IP still has its full budget for the SAME (tenant, provider).
	if w := doSSOStart(r, victim, "acme", "google"); w.Code != http.StatusOK {
		t.Errorf("victim IP: expected 200 (attacker must not drain the tenant's bucket cross-IP), got %d", w.Code)
	}
}

// TestSSORateLimit_Callback_PerIP_TripsAndIsolates: /callback cannot see tenant/provider
// pre-handler (they are sealed inside `state`), so it is keyed per IP under the same SSO
// threshold — trips at MaxAttempts for one IP, independent across IPs.
func TestSSORateLimit_Callback_PerIP_TripsAndIsolates(t *testing.T) {
	cfg := SSORateLimitConfig{MaxAttempts: 3, Window: 10 * time.Second}
	r := newSSORateLimitRouter(cfg)

	ipA := "10.4.4.4:1111"
	ipB := "10.5.5.5:2222"
	for i := 0; i < 3; i++ {
		if w := doSSOCallback(r, ipA); w.Code != http.StatusOK {
			t.Fatalf("ipA request %d: expected 200 within budget, got %d", i+1, w.Code)
		}
	}
	if w := doSSOCallback(r, ipA); w.Code != http.StatusTooManyRequests {
		t.Fatalf("ipA: expected 429 past budget, got %d", w.Code)
	}
	if w := doSSOCallback(r, ipB); w.Code != http.StatusOK {
		t.Errorf("ipB: expected 200 (independent per-IP callback bucket), got %d", w.Code)
	}
}

// TestSSORateLimit_StartAndCallback_IndependentBuckets: the "sso:start:" / "sso:cb:" key
// namespaces keep the two routes' budgets separate — a /start flood must not consume the
// /callback budget of legitimate logins already in flight (and vice-versa).
func TestSSORateLimit_StartAndCallback_IndependentBuckets(t *testing.T) {
	cfg := SSORateLimitConfig{MaxAttempts: 2, Window: 10 * time.Second}
	r := newSSORateLimitRouter(cfg)

	ip := "10.6.6.6:4242"
	// Exhaust the /start bucket for (ip, acme, google).
	doSSOStart(r, ip, "acme", "google")
	doSSOStart(r, ip, "acme", "google")
	if w := doSSOStart(r, ip, "acme", "google"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("start: expected 429 after limit, got %d", w.Code)
	}

	// /callback from the same IP still has its own full budget.
	if w := doSSOCallback(r, ip); w.Code != http.StatusOK {
		t.Errorf("callback: expected 200 (independent namespace from start), got %d", w.Code)
	}
}

// TestSSORateLimit_Exchange_IndependentBucket (SSO-FE-REDIRECT reviewer MED-1): POST
// /auth/sso/exchange rides the SSO limiter but in its OWN per-IP namespace ("sso:ex:"), so a
// redirect-mode login's exchange does NOT double-burn the same IP's /callback budget — the
// NAT-workforce sizing SSO-RATELIMIT-NAT protects stays intact.
func TestSSORateLimit_Exchange_IndependentBucket(t *testing.T) {
	cfg := SSORateLimitConfig{MaxAttempts: 2, Window: 10 * time.Second}
	r := newSSORateLimitRouter(cfg)

	ip := "10.7.7.7:5353"
	// Exhaust the /callback bucket for this IP.
	doSSOCallback(r, ip)
	doSSOCallback(r, ip)
	if w := doSSOCallback(r, ip); w.Code != http.StatusTooManyRequests {
		t.Fatalf("callback: expected 429 after limit, got %d", w.Code)
	}
	// /exchange from the SAME IP still has its own full budget (would 429 if it shared sso:cb).
	if w := doSSOExchange(r, ip); w.Code != http.StatusOK {
		t.Errorf("exchange: expected 200 (independent namespace from callback), got %d", w.Code)
	}
	if w := doSSOExchange(r, ip); w.Code != http.StatusOK {
		t.Errorf("exchange (2nd): expected 200 within its own budget, got %d", w.Code)
	}
	if w := doSSOExchange(r, ip); w.Code != http.StatusTooManyRequests {
		t.Errorf("exchange (3rd): expected 429 after its OWN limit, got %d", w.Code)
	}
}

// TestSSORateLimit_ZeroConfig_NoOp mirrors AuthRateLimit: MaxAttempts<=0 or Window<=0
// disables the limiter entirely.
func TestSSORateLimit_ZeroConfig_NoOp(t *testing.T) {
	r := newSSORateLimitRouter(SSORateLimitConfig{MaxAttempts: 0, Window: 10 * time.Second})
	ip := "10.7.7.7:8080"
	for i := 0; i < 100; i++ {
		if w := doSSOStart(r, ip, "acme", "google"); w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d: got unexpected 429 when limiter is disabled", i+1)
		}
	}
}

// TestSSORateLimit_WindowSlides: after the window expires the bucket refills (shared
// memRateLimiter machinery, asserted here through the SSO key path).
func TestSSORateLimit_WindowSlides(t *testing.T) {
	cfg := SSORateLimitConfig{MaxAttempts: 2, Window: 50 * time.Millisecond}
	r := newSSORateLimitRouter(cfg)

	ip := "10.8.8.8:9091"
	doSSOStart(r, ip, "acme", "google")
	doSSOStart(r, ip, "acme", "google")
	if w := doSSOStart(r, ip, "acme", "google"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after limit, got %d", w.Code)
	}

	time.Sleep(60 * time.Millisecond)

	if w := doSSOStart(r, ip, "acme", "google"); w.Code != http.StatusOK {
		t.Errorf("expected 200 after window expired, got %d", w.Code)
	}
}

// TestSSORateLimit_ConcurrentStormAdmitsExactlyMax: a NAT'd workforce's login burst lands on ONE
// (ip, tenant, provider) bucket in parallel. Check-and-record must be atomic, or every request
// that checked before the first record slips past the cap.
func TestSSORateLimit_ConcurrentStormAdmitsExactlyMax(t *testing.T) {
	const max = 5
	assertStormAdmitsExactly(t, max,
		func() *gin.Engine {
			return newSSORateLimitRouter(SSORateLimitConfig{MaxAttempts: max, Window: time.Minute})
		},
		func(r *gin.Engine) int { return doSSOStart(r, "10.9.9.9:1234", "acme", "google").Code })
}
