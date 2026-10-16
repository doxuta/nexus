// Package middleware — data_ratelimit.go: a PER-API-KEY sliding-window rate limit on the
// protected data plane (DAILY-QUEUE #52, ../specs/045-api-key-throttle/spec.md).
//
// Why: the three other limiters (AuthRateLimit, SSORateLimit, AssistantRateLimit) all sit OFF the
// data path. A customer integration stuck in a retry loop on its API key (`nxk_`) could therefore
// eat the shared connection pool and slow the obj_ tables for EVERY tenant on the host. This puts a
// per-key brake in front of every protected route.
//
// Who is counted — the whole point of the slice: ONLY API-key principals (auth.TokenTypeAPIKey,
// minted by APIKeyService.VerifyKey — never a JWT: TokenService issues/accepts access|refresh
// only, so a client cannot forge the type). Browser sessions (JWT access tokens) ride the same
// protected group and are passed through untouched; mounting a count-everyone limiter there
// would throttle staff clicking through a grid. The discriminator is the VERIFIED claim, never a
// header prefix.
//
// Bucket: per (tenant, key id) — deliberately NO tenant-level bucket, so a runaway integration
// never throttles a sibling integration or the customer's staff. A key principal with an empty id
// (unreachable: the id is the row's primary key) still gets a bucket — fail-secure, never a bypass.
//
// Response: the canonical RATE_LIMITED 429 + Retry-After (writeRateLimited429, byte-identical to
// the other limiters) and nothing else — no X-RateLimit-* counter headers.
//
// Two brakes per key, because a rate budget alone does not bound concurrency: the sliding window
// admits its whole budget AT ONCE (all 600 in the first second of a window, and since those
// timestamps expire together the burst repeats every window). So the key also gets an IN-FLIGHT
// cap (MaxInFlight, default 8 — DATA_MAX_INFLIGHT_PER_KEY): at most that many of its requests run
// at the same moment, i.e. one key holds at most that many pooled DB connections at once (no
// handler fans out parallel queries). The slot is taken BEFORE the rate window is touched, so a
// refusal on a full slot spends no budget, and answers Retry-After: 1 (a slot frees as soon as one
// running request returns) instead of the window.
//
// Ceiling: in-memory (memRateLimiter, janitor-evicted) ⇒ PER PROCESS. N instances behind a load
// balancer give a key up to N× the budget; a shared Redis sliding window is the upgrade path (same
// note as RM-15). A throttled request still pays the key verify (one point-read on sys_api_keys'
// unique hash index) because the limiter keys on the verified identity, after AuthMiddleware. A
// dead key (revoked / expired / never issued) never reaches this limiter; APIKeyService.VerifyKey
// refuses its repeats from memory without the read (queue #155).
// Both brakes are PER KEY: K runaway keys (of any tenants) together hold up to K×MaxInFlight
// connections — at the defaults, 4 of them can fill a 25-connection pool. A global/per-tenant
// in-flight budget is the upgrade path if that is ever measured.
package middleware

import (
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/auth"
)

// DataRateLimitConfig holds the per-API-key data-plane rate-limit parameters.
type DataRateLimitConfig struct {
	// MaxAttempts is the maximum requests one API key may make within Window.
	MaxAttempts int
	// Window is the sliding-window duration.
	Window time.Duration
	// MaxInFlight caps how many requests ONE key may have running at the same moment. <= 0 = no
	// in-flight cap (test-only shape: the env cannot produce it, see config getEnvInt).
	MaxInFlight int
}

// keyInFlight counts the requests each key has running. An idle key's entry is deleted on
// release, so the map only ever holds keys with a request in progress — no janitor needed.
type keyInFlight struct {
	mu  sync.Mutex
	n   map[string]int
	max int
}

// acquire takes one of key's slots; false = all max are busy.
func (f *keyInFlight) acquire(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n[key] >= f.max {
		return false
	}
	f.n[key]++
	return true
}

// release gives back a slot taken by acquire.
func (f *keyInFlight) release(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n[key] <= 1 {
		delete(f.n, key)
		return
	}
	f.n[key]--
}

// DataRateLimit returns a gin middleware enforcing a per-API-key sliding-window limit. Mount it on
// the protected group AFTER AuthMiddleware + RequireTenant (it reads the verified claims) and
// BEFORE any route is registered. A request with no verified claims, or with a non-API-key
// principal, is passed through. MaxAttempts or Window <= 0 disables it (mirrors the other
// limiters).
func DataRateLimit(cfg DataRateLimitConfig) gin.HandlerFunc {
	if cfg.MaxAttempts <= 0 || cfg.Window <= 0 {
		return func(c *gin.Context) { c.Next() }
	}
	lim := newMemRateLimiter(cfg.MaxAttempts, cfg.Window)
	var inFlight *keyInFlight
	if cfg.MaxInFlight > 0 {
		inFlight = &keyInFlight{n: map[string]int{}, max: cfg.MaxInFlight}
	}

	return func(c *gin.Context) {
		claims, ok := ClaimsFromContext(c)
		if !ok || claims.TokenType != auth.TokenTypeAPIKey {
			c.Next()
			return
		}
		key := "apikey:" + claims.TenantID + ":" + claims.UserID
		// Slot FIRST: a request refused for concurrency must not spend rate budget.
		if inFlight != nil {
			if !inFlight.acquire(key) {
				writeRateLimited429(c, time.Second)
				return
			}
			// defer, not a call after c.Next(): a panicking handler unwinds through here
			// (Recovery sits upstream), and a leaked slot locks the key out for good.
			defer inFlight.release(key)
		}
		// Atomic check-and-record: a parallel retry storm must not slip past the cap.
		if !lim.allow(key) {
			writeRateLimited429(c, cfg.Window)
			return
		}
		c.Next()
	}
}
