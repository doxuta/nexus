// Package middleware — auth_ratelimit.go implements the brute-force throttle for the
// public /auth/* endpoints (RM-15). It uses an in-memory sliding-window limiter keyed
// per (lowercased-tenant + lowercased-email + source-IP) so that:
//
//   - Different principals are throttled independently (tenant A's lockout does not
//     affect tenant B, and email-A does not lock out email-B on the same IP).
//   - NAT / corporate egress: employees sharing an IP but using different email addresses
//     each have their own window, preventing one user from locking out colleagues.
//   - Enumeration is prevented: the 429 response is intentionally generic and identical
//     regardless of whether the tenant or email exists.
//   - Memory is bounded: a janitor goroutine evicts windows whose last attempt was more
//     than 2× the window duration ago, so the map cannot grow without bound even under
//     sustained attack (no unbounded-map DoS).
//
// Key extraction: the limiter reads the raw request body (JSON), extracts "tenant" and
// "email" fields, then restores the body for the downstream auth handler. On unmarshal
// failure (refresh/logout bodies, malformed JSON) it falls back to IP-only keying, which
// is exactly the "email-if-present" behaviour the spec requires.
//
// Storage: in-memory. This is correct for a single-instance deployment. For multi-instance
// (horizontal scale) the limiter must be replaced with a Redis-backed sliding window;
// a drop-in swap is straightforward because the middleware is constructed via AuthRateLimit()
// and is otherwise opaque to the rest of the system.
//
// NOTE: Redis is NOT yet wired in this codebase (grep confirms zero Redis dependency).
// When Redis is added, replace the memRateLimiter internals with a Lua-scripted sliding
// window (ZADD/ZREMRANGEBYSCORE/ZCARD pattern) and remove the janitor goroutine.
package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// RateLimitConfig holds the configurable parameters for AuthRateLimit.
type RateLimitConfig struct {
	// MaxAttempts is the maximum number of requests allowed within Window for the per-
	// (ip,tenant,email) window before a 429 is returned. Must be > 0.
	MaxAttempts int
	// IPMaxAttempts is the maximum number of requests allowed within Window for the coarser
	// per-IP window (W5-A3 password-spray backstop: one source hitting many emails). It should
	// be set comfortably ABOVE a busy office's legitimate burst (NAT/corporate egress share an
	// IP). When <= 0 the per-IP window is disabled and only the per-(ip,tenant,email) window
	// applies (preserves the original RM-15 behaviour).
	IPMaxAttempts int
	// Window is the sliding-window duration shared by both windows. Attempts older than Window
	// are dropped before each check. Must be > 0.
	Window time.Duration
	// TrustedProxyCIDRs are the reverse-proxy networks whose X-Forwarded-For may be trusted for
	// client-IP extraction (HIGH-2). Empty (the default) → the direct peer (r.RemoteAddr) is the
	// key, exactly as before — XFF is never trusted. See clientIP in client_ip.go.
	TrustedProxyCIDRs []*net.IPNet
}

// AuthRateLimit returns a gin middleware that enforces a sliding-window rate limit on
// the public /auth/* group (RM-15). It is keyed per (lowercased-tenant + lowercased-email
// + source-IP) so that each independent credential combination has its own window.
//
// Key derivation reads the raw JSON body, extracts "tenant" and "email" fields, then
// restores the body so the downstream handler sees an unmodified stream. On any read or
// parse failure the limiter falls back to IP-only keying, which covers refresh/logout
// endpoints that carry no email.
//
// Security properties:
//   - Non-enumerable: 429 body is identical regardless of whether tenant/email exists.
//   - Per-key, not global: one attacker's lockout does not affect other keys.
//   - NAT-safe: users sharing an IP but using different emails have independent windows.
//   - Memory-bounded: janitor evicts stale windows every window duration.
//   - Constant-time invariant preserved: the limiter fires BEFORE bcrypt; requests within
//     the limit still go through the full AuthService.Login constant-time bcrypt path.
//
// cfg.MaxAttempts and cfg.Window must both be positive; if either is <= 0 the middleware
// is a no-op (no rate limiting applied).
func AuthRateLimit(cfg RateLimitConfig) gin.HandlerFunc {
	if cfg.MaxAttempts <= 0 || cfg.Window <= 0 {
		return func(c *gin.Context) { c.Next() }
	}

	lim := newMemRateLimiter(cfg.MaxAttempts, cfg.Window)
	// W5-A3: an optional coarser per-IP window backstops the per-(ip,tenant,email) window so a
	// password-spray (one password × many emails from a single source) is throttled by total
	// attempts per IP, not just per email. Disabled when IPMaxAttempts <= 0.
	var ipLim *memRateLimiter
	if cfg.IPMaxAttempts > 0 {
		ipLim = newMemRateLimiter(cfg.IPMaxAttempts, cfg.Window)
	}

	return func(c *gin.Context) {
		ip := clientIP(c.Request, cfg.TrustedProxyCIDRs)
		tenant, email := extractCreds(c)
		compositeKey := "auth:" + ip + ":" + tenant + ":" + email
		ipKey := "authip:" + ip

		// Check-and-record BOTH windows atomically (#354): a rejection by one window must not charge
		// an attempt against the other (an IP-flood must not burn a specific victim's per-email
		// budget, and one account's lockout must not consume the shared per-IP budget), and a
		// parallel burst must not overshoot either cap.
		if !allowBoth(lim, compositeKey, ipLim, ipKey) {
			writeRateLimited429(c, cfg.Window)
			return
		}
		c.Next()
	}
}

// writeRateLimited429 aborts the request with the ONE canonical rate-limit refusal shared by
// every limiter in this package (AuthRateLimit, SSORateLimit): 429 + Retry-After header + the
// generic RATE_LIMITED envelope. Centralized so the anti-enumeration property (the body carries
// no signal about whether any tenant/email/provider exists) cannot drift between limiters.
func writeRateLimited429(c *gin.Context, window time.Duration) {
	c.Header("Retry-After", strconv.Itoa(int(window.Seconds())))
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
		"error":   "Too many requests. Please try again later.",
		"code":    "RATE_LIMITED",
		"details": gin.H{},
	})
}

// extractCreds reads the JSON request body to pull the lowercased "tenant" and "email" fields
// for rate-limit keying, then RESTORES the full body so the downstream handler sees an
// unmodified stream. Returns empty strings for non-JSON bodies (refresh/logout) so the caller
// falls through to IP-only keying.
//
// Only the first 8 KiB is parsed for keys (enough for any /auth payload), but the body is
// restored in FULL via io.MultiReader (W5-A3 fix): the previous version forwarded only the
// 8 KiB it read, truncating any larger body and corrupting the downstream parse. The global
// MaxBodySize middleware already bounds the total size, so reconstituting the remainder here
// re-enforces no limit — it merely stitches the unread tail back onto the stream.
//
// The IP is built by the caller via clientIP (client_ip.go): it comes from RemoteAddr UNLESS the
// direct peer is a configured trusted proxy (TRUSTED_PROXY_CIDRS), in which case it is the real
// client from X-Forwarded-For. With no trusted proxy configured — the default — X-Forwarded-For is
// deliberately NOT trusted, so a client cannot set that header to rotate the limiter key.
func extractCreds(c *gin.Context) (tenant, email string) {
	if c.Request.Body == nil {
		return "", ""
	}
	const maxParseBytes = 8 << 10 // 8 KiB parsed for keys
	head, err := io.ReadAll(io.LimitReader(c.Request.Body, maxParseBytes))
	// Restore the FULL body: the head we consumed PLUS whatever remains unread on the original
	// body. For bodies <= 8 KiB the remainder is empty and this is just `head`.
	c.Request.Body = io.NopCloser(io.MultiReader(bytes.NewReader(head), c.Request.Body))
	if err != nil || len(head) == 0 {
		return "", ""
	}
	var creds struct {
		Tenant string `json:"tenant"`
		Email  string `json:"email"`
	}
	// Unmarshal failure is silent: non-JSON bodies fall through to empty (IP-only) keying.
	_ = json.Unmarshal(head, &creds)
	return strings.ToLower(strings.TrimSpace(creds.Tenant)), strings.ToLower(strings.TrimSpace(creds.Email))
}

// remoteIP extracts the host portion of r.RemoteAddr using net.SplitHostPort, which
// handles both IPv4 ("1.2.3.4:port") and IPv6 ("[::1]:port") correctly.
// Falls back to the raw RemoteAddr string if SplitHostPort fails.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// SplitHostPort fails for bare addresses without a port (rare in practice for
		// HTTP servers but guard defensively). Use the raw value as-is.
		return r.RemoteAddr
	}
	return host
}

// ---- in-memory sliding-window implementation ------------------------------------

// windowEntry tracks the timestamps of recent attempts for a single rate-limit key.
type windowEntry struct {
	mu         sync.Mutex
	timestamps []time.Time // kept sorted ascending; old entries pruned on each allow() call
}

// memRateLimiter is a thread-safe, in-memory sliding-window rate limiter.
// It is bounded by the janitor goroutine which evicts entries that have been
// inactive for more than 2× the window duration, preventing unbounded map growth
// under sustained attack (no unbounded-map DoS).
type memRateLimiter struct {
	mu          sync.Mutex
	windows     map[string]*windowEntry
	maxAttempts int
	window      time.Duration
}

func newMemRateLimiter(maxAttempts int, window time.Duration) *memRateLimiter {
	lim := &memRateLimiter{
		windows:     make(map[string]*windowEntry),
		maxAttempts: maxAttempts,
		window:      window,
	}
	// Start the janitor goroutine. It runs for the lifetime of the process and
	// evicts stale entries every window duration to bound memory.
	go lim.janitor()
	return lim
}

// allow is the ATOMIC check-and-record for a single-window caller: under ONE hold of the entry
// lock it prunes, admits iff the key is under the limit, and records the admitted attempt. A
// separate peek()+record() pair lets every goroutine that peeked before the first record through,
// overshooting the cap under a concurrent burst — exactly the shape of a parallel retry storm
// (specs/045-api-key-throttle: 64 concurrent requests at cap 5 admitted 6-7 in ~9% of runs).
// Every single-window limiter (DataRateLimit, SSORateLimit, AssistantRateLimit) uses it.
// Multi-window callers (AuthRateLimit) use allowBoth so a rejection by one window never charges
// the other.
func (l *memRateLimiter) allow(key string) bool {
	entry := l.getOrCreate(key)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	l.pruneLocked(entry)
	if len(entry.timestamps) >= l.maxAttempts {
		return false
	}
	entry.timestamps = append(entry.timestamps, time.Now())
	return true
}

// allowBoth is the ATOMIC check-and-record across two windows (#354): it holds the lock of BOTH
// entries, admits iff both are under their cap, and records the attempt in both only on admission.
// A refusal by either window records nothing in either. b may be nil (IPMaxAttempts <= 0
// disables it → plain single-window allow).
//
// Deadlock-free by a fixed lock order: the first entry (a's, per-(ip,tenant,email)) is always taken
// before the second (b's, per-IP); the two windows belong to different limiters so no call ever
// takes them in the opposite order, and nothing takes an entry lock while holding limiter.mu
// (getOrCreate releases it first; the janitor takes limiter.mu → entry.mu only for stale entries
// no caller holds).
func allowBoth(a *memRateLimiter, aKey string, b *memRateLimiter, bKey string) bool {
	if b == nil {
		return a.allow(aKey)
	}
	ea, eb := a.getOrCreate(aKey), b.getOrCreate(bKey)
	ea.mu.Lock()
	defer ea.mu.Unlock()
	eb.mu.Lock()
	defer eb.mu.Unlock()
	a.pruneLocked(ea)
	b.pruneLocked(eb)
	if len(ea.timestamps) >= a.maxAttempts || len(eb.timestamps) >= b.maxAttempts {
		return false
	}
	now := time.Now()
	ea.timestamps = append(ea.timestamps, now)
	eb.timestamps = append(eb.timestamps, now)
	return true
}

// pruneLocked drops timestamps outside the current sliding window. Caller must hold entry.mu.
func (l *memRateLimiter) pruneLocked(entry *windowEntry) {
	cutoff := time.Now().Add(-l.window)
	valid := entry.timestamps[:0]
	for _, t := range entry.timestamps {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	entry.timestamps = valid
}

func (l *memRateLimiter) getOrCreate(key string) *windowEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.windows[key]
	if !ok {
		e = &windowEntry{}
		l.windows[key] = e
	}
	return e
}

// janitor evicts window entries whose last recorded attempt is older than 2× the window
// duration, preventing unbounded map growth. It runs in a background goroutine and wakes
// every window duration. On process exit the goroutine is collected by the runtime.
func (l *memRateLimiter) janitor() {
	ticker := time.NewTicker(l.window)
	defer ticker.Stop()
	for range ticker.C {
		l.evictStale()
	}
}

// evictStale removes entries where all recorded timestamps are outside 2× the window.
func (l *memRateLimiter) evictStale() {
	cutoff := time.Now().Add(-2 * l.window)
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, entry := range l.windows {
		entry.mu.Lock()
		if len(entry.timestamps) == 0 {
			delete(l.windows, key)
		} else if entry.timestamps[len(entry.timestamps)-1].Before(cutoff) {
			// Last attempt is older than 2× window: the entry is completely stale.
			delete(l.windows, key)
		}
		entry.mu.Unlock()
	}
}
