package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/auth"
)

// principal is the verified identity a test request runs under: kind "key" seeds an API-key
// principal (TokenTypeAPIKey, UserID = the key's id — exactly what APIKeyService.VerifyKey
// mints), kind "jwt" a browser session (TokenTypeAccess), kind "" no claims at all.
type principal struct {
	kind, tenant, id string
}

// buildDataRateLimitRouter mounts a header-driven claims seeder (standing in for AuthMiddleware)
// BEFORE one shared DataRateLimit(cfg) instance, so every call in a test hits the SAME limiter
// state — the only way to prove per-key isolation and the JWT carve-out.
func buildDataRateLimitRouter(cfg DataRateLimitConfig) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Recovery OUTSIDE the limiter, as in production (main.go: Recovery before the /api/v1
	// group) — a panicking handler unwinds through DataRateLimit's frame.
	r.Use(gin.RecoveryWithWriter(io.Discard))
	r.Use(func(c *gin.Context) {
		typ := auth.TokenTypeAccess
		switch c.GetHeader("X-Test-Kind") {
		case "":
			c.Next()
			return
		case "key":
			typ = auth.TokenTypeAPIKey
		}
		c.Set(ctxKeyClaims, &auth.Claims{
			Subject:   auth.Subject{TenantID: c.GetHeader("X-Test-Tenant"), UserID: c.GetHeader("X-Test-ID"), RoleID: "sales"},
			TokenType: typ,
		})
		c.Next()
	})
	r.Use(DataRateLimit(cfg))
	r.GET("/leads", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	return r
}

func doDataReq(r *gin.Engine, p principal) *httptest.ResponseRecorder {
	return doDataReqPath(r, p, "/leads")
}

func doDataReqPath(r *gin.Engine, p principal, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if p.kind != "" {
		req.Header.Set("X-Test-Kind", p.kind)
		req.Header.Set("X-Test-Tenant", p.tenant)
		req.Header.Set("X-Test-ID", p.id)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// assertNoCounterHeaders pins the "429 + Retry-After only, no request counter" contract: no
// X-RateLimit-* header leaks the budget on either an admitted or a refused response.
func assertNoCounterHeaders(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	for h := range w.Header() {
		if strings.HasPrefix(strings.ToLower(h), "x-ratelimit") {
			t.Errorf("response carries counter header %q — contract is 429 + Retry-After only", h)
		}
	}
}

var (
	keyA   = principal{"key", "t1", "key-a"}
	keyB   = principal{"key", "t1", "key-b"}
	keyT2  = principal{"key", "t2", "key-a"} // same key id string, different tenant
	jwtU1  = principal{"jwt", "t1", "user-1"}
	noAuth = principal{}
)

// TestDataRateLimit is the table of who is counted and who is not. Each case runs against a
// FRESH shared router: `warmup` requests are fired first, then `probe` must answer wantStatus.
func TestDataRateLimit(t *testing.T) {
	cases := []struct {
		name       string
		cfg        DataRateLimitConfig
		warmup     []principal
		probe      principal
		wantStatus int
	}{
		{
			name:       "api key admitted within budget",
			cfg:        DataRateLimitConfig{MaxAttempts: 5, Window: time.Minute},
			warmup:     []principal{keyA, keyA, keyA, keyA},
			probe:      keyA,
			wantStatus: http.StatusOK,
		},
		{
			name:       "api key refused on request max+1",
			cfg:        DataRateLimitConfig{MaxAttempts: 5, Window: time.Minute},
			warmup:     []principal{keyA, keyA, keyA, keyA, keyA},
			probe:      keyA,
			wantStatus: http.StatusTooManyRequests,
		},
		{
			// THE browser-session property: a JWT principal in the SAME tenant is never counted,
			// even after a key there is exhausted and even far past the cap itself.
			name:       "jwt session never counted",
			cfg:        DataRateLimitConfig{MaxAttempts: 1, Window: time.Minute},
			warmup:     []principal{keyA, keyA, jwtU1, jwtU1, jwtU1, jwtU1, jwtU1, jwtU1},
			probe:      jwtU1,
			wantStatus: http.StatusOK,
		},
		{
			// Per KEY, deliberately no tenant bucket: one runaway integration never throttles
			// a sibling integration of the same customer.
			name:       "second key same tenant has its own bucket",
			cfg:        DataRateLimitConfig{MaxAttempts: 2, Window: time.Minute},
			warmup:     []principal{keyA, keyA, keyA},
			probe:      keyB,
			wantStatus: http.StatusOK,
		},
		{
			name:       "same key id in another tenant is a different bucket",
			cfg:        DataRateLimitConfig{MaxAttempts: 2, Window: time.Minute},
			warmup:     []principal{keyA, keyA, keyA},
			probe:      keyT2,
			wantStatus: http.StatusOK,
		},
		{
			// Unreachable in production (VerifyKey always sets UserID = the row's primary key),
			// but pinned FAIL-SECURE: a key principal with no id is still throttled, never a
			// bypass of the limiter.
			name:       "api key with empty id is still throttled",
			cfg:        DataRateLimitConfig{MaxAttempts: 1, Window: time.Minute},
			warmup:     []principal{{"key", "t1", ""}},
			probe:      principal{"key", "t1", ""},
			wantStatus: http.StatusTooManyRequests,
		},
		{
			// No claims = the auth stage did not run (it refuses such a request before this
			// limiter in the production chain); keying it here would pool every anonymous call.
			name:       "no claims passes through",
			cfg:        DataRateLimitConfig{MaxAttempts: 1, Window: time.Minute},
			warmup:     []principal{noAuth, noAuth, noAuth},
			probe:      noAuth,
			wantStatus: http.StatusOK,
		},
		{
			name:       "zero MaxAttempts disables the limiter",
			cfg:        DataRateLimitConfig{MaxAttempts: 0, Window: time.Minute},
			warmup:     []principal{keyA, keyA, keyA},
			probe:      keyA,
			wantStatus: http.StatusOK,
		},
		{
			name:       "zero Window disables the limiter",
			cfg:        DataRateLimitConfig{MaxAttempts: 1, Window: 0},
			warmup:     []principal{keyA, keyA, keyA},
			probe:      keyA,
			wantStatus: http.StatusOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := buildDataRateLimitRouter(tc.cfg)
			for _, p := range tc.warmup {
				assertNoCounterHeaders(t, doDataReq(r, p))
			}
			w := doDataReq(r, tc.probe)
			if w.Code != tc.wantStatus {
				t.Fatalf("probe status = %d, want %d (body=%s)", w.Code, tc.wantStatus, w.Body.String())
			}
			assertNoCounterHeaders(t, w)
			if tc.wantStatus == http.StatusTooManyRequests {
				if got := w.Header().Get("Retry-After"); got != "60" {
					t.Errorf("Retry-After = %q, want %q (window seconds)", got, "60")
				}
				if !strings.Contains(w.Body.String(), `"code":"RATE_LIMITED"`) {
					t.Errorf("429 body = %s, want code RATE_LIMITED", w.Body.String())
				}
			}
		})
	}
}

// TestDataRateLimit_WindowSlides: once the window has passed the key is admitted again — the
// acceptance clause "chạy lại được sau khi cửa sổ trôi qua". Refused retries inside the window
// are NOT recorded, so a client hammering while throttled does not extend its own lockout.
func TestDataRateLimit_WindowSlides(t *testing.T) {
	r := buildDataRateLimitRouter(DataRateLimitConfig{MaxAttempts: 2, Window: 80 * time.Millisecond})
	doDataReq(r, keyA)
	doDataReq(r, keyA)
	for i := 0; i < 5; i++ {
		if w := doDataReq(r, keyA); w.Code != http.StatusTooManyRequests {
			t.Fatalf("retry %d inside the window: status %d, want 429", i+1, w.Code)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if w := doDataReq(r, keyA); w.Code != http.StatusOK {
		t.Fatalf("after the window slid: status %d, want 200", w.Code)
	}
}

// TestDataRateLimit_ConcurrentStormAdmitsExactlyMax is the retry-storm shape this slice exists
// for: many parallel workers of ONE integration fire at once. Check-and-record must be atomic —
// a separate peek()+record() lets every goroutine that peeked before the first record through.
func TestDataRateLimit_ConcurrentStormAdmitsExactlyMax(t *testing.T) {
	const max = 5
	assertStormAdmitsExactly(t, max,
		func() *gin.Engine {
			return buildDataRateLimitRouter(DataRateLimitConfig{MaxAttempts: max, Window: time.Minute})
		},
		func(r *gin.Engine) int { return doDataReq(r, keyA).Code })
}

// Storm shape shared by every single-window limiter's concurrency test.
const (
	stormRounds  = 50
	stormWorkers = 64
)

// assertStormAdmitsExactly fires stormWorkers parallel requests through one start barrier at a
// FRESH router, stormRounds times, and fails on the first round that admits (200) a count other
// than want. One round alone catches a non-atomic check-and-record only now and then — a separate
// peek()+record() overshot in 5 of 30 single-round runs (specs/045 review) — so the storm repeats
// until a race that exists has had fifty chances to show.
func assertStormAdmitsExactly(t *testing.T, want int, newRouter func() *gin.Engine, fire func(*gin.Engine) int) {
	t.Helper()
	for round := 1; round <= stormRounds; round++ {
		r := newRouter()
		var admitted atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < stormWorkers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if fire(r) == http.StatusOK {
					admitted.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if got := admitted.Load(); got != int32(want) {
			t.Fatalf("round %d: admitted %d of %d concurrent requests, want exactly %d", round, got, stormWorkers, want)
		}
	}
}

// TestDataRateLimit_InFlightCap: the rate budget alone admits a whole window AT ONCE (all 600 in
// the first second, and since those timestamps expire together the burst repeats every window),
// so on its own it does not stop one key from holding every pooled DB connection while its
// requests run. The in-flight cap bounds how many of the key's requests run at the same moment.
// It is checked BEFORE the rate window, so a refusal here does not spend rate budget.
func TestDataRateLimit_InFlightCap(t *testing.T) {
	r := buildDataRateLimitRouter(DataRateLimitConfig{MaxAttempts: 3, Window: time.Minute, MaxInFlight: 2})
	entered, release := make(chan struct{}), make(chan struct{})
	r.GET("/hold", func(c *gin.Context) {
		entered <- struct{}{}
		<-release
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	held := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() { held <- doDataReqPath(r, keyA, "/hold").Code }()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatalf("held request %d never reached the handler", i+1)
		}
	}

	// keyA has both slots busy: a third concurrent request is refused, told to come back in 1 s.
	w := doDataReq(r, keyA)
	if w.Code != http.StatusTooManyRequests {
		close(release)
		t.Fatalf("third concurrent keyA request: status %d, want 429 (in-flight cap 2)", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Errorf("in-flight 429 Retry-After = %q, want %q", got, "1")
	}
	if !strings.Contains(w.Body.String(), `"code":"RATE_LIMITED"`) {
		t.Errorf("in-flight 429 body = %s, want code RATE_LIMITED", w.Body.String())
	}
	assertNoCounterHeaders(t, w)
	// Per key: a sibling key of the same tenant runs while keyA is saturated.
	if w := doDataReq(r, keyB); w.Code != http.StatusOK {
		t.Errorf("keyB while keyA saturated: status %d, want 200", w.Code)
	}

	close(release)
	for i := 0; i < 2; i++ {
		if code := <-held; code != http.StatusOK {
			t.Errorf("held request finished with %d, want 200", code)
		}
	}
	// Slots are back, and the in-flight refusal spent no rate budget: 2 held + this one = 3 = max.
	if w := doDataReq(r, keyA); w.Code != http.StatusOK {
		t.Fatalf("keyA after its requests finished: status %d, want 200", w.Code)
	}
	// The rate window still applies on top: request 4 in the window is refused for the window.
	w = doDataReq(r, keyA)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "60" {
		t.Fatalf("keyA request 4 in the window: status %d Retry-After %q, want 429 / 60", w.Code, w.Header().Get("Retry-After"))
	}
}

// TestDataRateLimit_InFlightReleasedOnPanic: a panicking handler unwinds through the limiter
// (Recovery sits upstream). A slot that is not released there leaks for the life of the process,
// and after MaxInFlight panics the key is locked out for good.
func TestDataRateLimit_InFlightReleasedOnPanic(t *testing.T) {
	r := buildDataRateLimitRouter(DataRateLimitConfig{MaxAttempts: 100, Window: time.Minute, MaxInFlight: 1})
	r.GET("/boom", func(c *gin.Context) { panic("handler bug") })

	if w := doDataReqPath(r, keyA, "/boom"); w.Code != http.StatusInternalServerError {
		t.Fatalf("/boom: status %d, want 500 from Recovery", w.Code)
	}
	if w := doDataReq(r, keyA); w.Code != http.StatusOK {
		t.Fatalf("keyA after a panicking request: status %d, want 200 (in-flight slot leaked)", w.Code)
	}
}
