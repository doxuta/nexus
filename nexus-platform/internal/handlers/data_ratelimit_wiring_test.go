package handlers

// data_ratelimit_wiring_test.go — DAILY-QUEUE #52 (specs/045-api-key-throttle) default-build lock
// (no DB, no build tag): WithDataRateLimiter mounts the per-API-key throttle on the PROTECTED
// group through the REAL RegisterAPIRoutes, in front of the dynamic /:entity surface — and a
// browser (JWT) session on the same routes is never counted. If the option is ever dropped, or
// the Use() slides below the route registrations (gin snapshots handlers at registration time,
// so a late Use silently leaves /:entity unthrottled), the key assertions go red.
//
// Fixture: a nil-service DynamicRecordHandler panics on any real dispatch and gin's recovery turns
// that into 500 — so "not 429" means the request got PAST the limiter, "429" means it did not.
// Reuses stubVerifier / claimsFor (acl_wiring_test.go) and fakeUserDirectoryRepo.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

func apiKeyClaims(tenantID, keyID string) *auth.Claims {
	return &auth.Claims{
		Subject:   auth.Subject{UserID: keyID, TenantID: tenantID, RoleID: "sales"},
		TokenType: auth.TokenTypeAPIKey,
	}
}

func buildDataRateLimitWiringEngine(t *testing.T, max int) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		"key-a": apiKeyClaims("t1", "key-a-id"),
		"key-b": apiKeyClaims("t1", "key-b-id"),
		"jwt":   claimsFor("t1", "sales", false), // non-admin browser session, SAME tenant
	}}
	repo := &fakeUserDirectoryRepo{rows: []auth.DirectoryUser{{ID: "u1", FullName: "A", Email: "a@x.test"}}, total: 1}

	r := gin.New()
	r.Use(gin.RecoveryWithWriter(io.Discard))
	RegisterAPIRoutes(r.Group("/api/v1"), ver, nil, APIHandlers{
		UserDirectory: NewUserDirectoryHandler(services.NewUserDirectoryService(repo), zap.NewNop()),
		Record:        NewDynamicRecordHandler(nil, zap.NewNop()),
	}, zap.NewNop(),
		WithDataRateLimiter(middleware.DataRateLimit(middleware.DataRateLimitConfig{MaxAttempts: max, Window: time.Minute})),
	)
	return r
}

func doDataWiringReq(r *gin.Engine, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// The key is admitted exactly `max` times on the data plane (/:entity), then refused with the
// canonical 429 + Retry-After — and no response ever carries a request counter.
func TestDataRateLimitWiring_APIKeyThrottledOnRecordSurface(t *testing.T) {
	const max = 3
	r := buildDataRateLimitWiringEngine(t, max)
	for i := 1; i <= max; i++ {
		w := doDataWiringReq(r, "/api/v1/leads", "key-a")
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d within budget got 429", i)
		}
		if w.Header().Get("X-RateLimit-Remaining") != "" || w.Header().Get("X-RateLimit-Limit") != "" {
			t.Fatalf("request %d carries a request counter header", i)
		}
	}
	w := doDataWiringReq(r, "/api/v1/leads", "key-a")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("request %d on /:entity = %d, want 429 (limiter not in front of the data plane)", max+1, w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
	if !strings.Contains(w.Body.String(), `"code":"RATE_LIMITED"`) {
		t.Errorf("429 body = %s, want code RATE_LIMITED", w.Body.String())
	}
	// The bucket is per key across every protected route, not per path.
	if w := doDataWiringReq(r, "/api/v1/users/directory", "key-a"); w.Code != http.StatusTooManyRequests {
		t.Errorf("exhausted key on /users/directory = %d, want 429", w.Code)
	}
}

// A non-admin browser session in the SAME tenant keeps loading the grid while a key there is
// throttled — the reason the limiter keys on the principal type instead of the route group.
func TestDataRateLimitWiring_BrowserSessionUntouched(t *testing.T) {
	r := buildDataRateLimitWiringEngine(t, 2)
	for i := 0; i < 3; i++ {
		doDataWiringReq(r, "/api/v1/leads", "key-a")
	}
	if w := doDataWiringReq(r, "/api/v1/leads", "key-a"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("precondition: key-a should be throttled, got %d", w.Code)
	}
	for i := 1; i <= 50; i++ {
		if w := doDataWiringReq(r, "/api/v1/leads", "jwt"); w.Code == http.StatusTooManyRequests {
			t.Fatalf("browser session request %d got 429 — JWT principals must never be counted", i)
		}
	}
	if w := doDataWiringReq(r, "/api/v1/users/directory", "jwt"); w.Code != http.StatusOK {
		t.Fatalf("browser session on /users/directory = %d, want 200", w.Code)
	}
}

// Per key, not per tenant: a sibling integration of the same customer keeps its own budget.
func TestDataRateLimitWiring_SiblingKeyUnaffected(t *testing.T) {
	r := buildDataRateLimitWiringEngine(t, 2)
	for i := 0; i < 3; i++ {
		doDataWiringReq(r, "/api/v1/leads", "key-a")
	}
	if w := doDataWiringReq(r, "/api/v1/leads", "key-b"); w.Code == http.StatusTooManyRequests {
		t.Fatalf("key-b throttled by key-a's retries (got 429) — bucket must be per key")
	}
}
