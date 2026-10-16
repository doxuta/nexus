// Package middleware — sso_ratelimit.go implements the DEDICATED rate limiter for the
// public SSO entry points (SSO-RATELIMIT-NAT): GET /auth/sso/:tenant/start,
// GET /auth/sso/callback, and POST /auth/sso/exchange (SSO-FE-REDIRECT — its own per-IP
// sub-bucket "sso:ex", see ssoRateLimitKey).
//
// Why a separate limiter (not the /auth/* AuthRateLimit): the RM-15 auth limiter is sized
// for CREDENTIAL GUESSING (10 attempts / 60 s — a human mistypes a password a handful of
// times). SSO login is not credential guessing: the browser never presents a password to
// Nexus, and an enterprise behind NAT funnels its ENTIRE workforce through one egress IP.
// During a morning login burst those employees would collectively exhaust the strict
// per-IP /auth bucket and lock the whole company out of login — including PASSWORD login,
// because the SSO traffic was burning the same shared budget. This limiter gives SSO its
// own, larger bucket (default 60 / 60 s, env-tunable) that never touches the /auth budget.
//
// Key strategy (honest about what each route can see BEFORE its handler runs):
//
//   - /start: the tenant slug is in the PATH (:tenant) and the provider in the QUERY
//     (?provider=...), so the key is "sso:start:<ip>:<tenant>:<provider>". A NAT'd
//     enterprise (one egress IP, one tenant, one provider) shares exactly one bucket sized
//     for a workforce burst. The source IP is DELIBERATELY part of the key — a key of bare
//     (tenant, provider) would let a remote attacker flood /start from anywhere and lock an
//     entire tenant out of SSO for the window (a cross-IP denial-of-service that does not
//     exist today); including the IP keeps the enterprise's bucket private to its own
//     egress while still fixing the NAT problem (the bucket is per-IP either way for a
//     single-egress enterprise — what matters is that it is DEDICATED and 6× larger).
//
//   - /callback: the tenant/provider live INSIDE the sealed `state` blob, which is opened
//     by the SERVICE, after this middleware has already run. Decrypting the state here
//     would duplicate the crypto (and an attacker controls the state param anyway, so any
//     key derived from an unverified state is attacker-chosen). The honest pre-handler
//     signal is the source IP alone: key "sso:cb:<ip>" under the SAME (higher) SSO
//     threshold. A NAT'd enterprise's callbacks share that per-IP bucket — same sizing
//     rationale as /start, and still fully separate from the strict /auth budget.
//
// Residual (accepted, documented): /start with a GARBAGE tenant/provider mints a fresh key
// per unique value, so a churn flood is not rate-bounded by this limiter (each request
// costs one indexed SELECT miss — the same cost class as any unauthenticated 404 or the
// unlimited /health/ready DB ping). The EXPENSIVE path (OIDC discovery + state seal) only
// exists for a REAL (tenant, provider) pair, and real pairs are finite and each capped by
// its own bucket. Key-map growth is bounded by the shared janitor (evicts entries idle for
// >2× window — see memRateLimiter).
//
// Storage: the same in-memory sliding-window machinery as AuthRateLimit (memRateLimiter —
// reused, not duplicated). Single-instance correct; swap for Redis alongside RM-15 when
// horizontal scaling lands.
package middleware

import (
	"net"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// SSORateLimitConfig holds the configurable parameters for SSORateLimit.
type SSORateLimitConfig struct {
	// MaxAttempts is the maximum number of requests allowed within Window per key
	// (per (ip,tenant,provider) on /start, per ip on /callback). Must be > 0.
	// Sized for a NAT'd workforce login burst, NOT for credential guessing — default
	// (config: SSO_RATELIMIT_MAX_ATTEMPTS) is 60, i.e. 6× the /auth login bucket.
	MaxAttempts int
	// Window is the sliding-window duration. Must be > 0.
	Window time.Duration
	// TrustedProxyCIDRs are the reverse-proxy networks whose X-Forwarded-For may be trusted for
	// client-IP extraction (HIGH-2). Empty (the default) → the direct peer (r.RemoteAddr) is the
	// key — XFF is never trusted. Shared with the /auth limiter via clientIP (client_ip.go).
	TrustedProxyCIDRs []*net.IPNet
}

// SSORateLimit returns a gin middleware enforcing the dedicated SSO sliding-window rate
// limit (SSO-RATELIMIT-NAT). Mount it ONLY on the group carrying the public SSO routes
// (GET /auth/sso/:tenant/start, GET /auth/sso/callback, POST /auth/sso/exchange) — the key
// derivation reads the :tenant path param and the route path to tell them apart (see
// ssoRateLimitKey).
//
// The 429 response is byte-identical in shape to the /auth limiter's (RATE_LIMITED +
// Retry-After): a distinct SSO code would leak nothing useful to a legitimate client and
// only fragment the FE's 429 handling.
//
// cfg.MaxAttempts and cfg.Window must both be positive; if either is <= 0 the middleware
// is a no-op (no rate limiting applied) — mirrors AuthRateLimit.
func SSORateLimit(cfg SSORateLimitConfig) gin.HandlerFunc {
	if cfg.MaxAttempts <= 0 || cfg.Window <= 0 {
		return func(c *gin.Context) { c.Next() }
	}

	// One shared limiter instance (one janitor); the "sso:start:" / "sso:cb:" key
	// namespaces keep the two routes' buckets independent — a /start flood never burns
	// /callback budget and vice-versa.
	lim := newMemRateLimiter(cfg.MaxAttempts, cfg.Window)

	return func(c *gin.Context) {
		// Atomic check-and-record: a parallel login burst must not slip past the cap.
		if !lim.allow(ssoRateLimitKey(c, cfg.TrustedProxyCIDRs)) {
			writeRateLimited429(c, cfg.Window)
			return
		}
		c.Next()
	}
}

// ssoRateLimitKey derives the bucket key for an SSO request (see the file header for the
// full rationale):
//
//   - /auth/sso/:tenant/start  → "sso:start:<ip>:<tenant>:<provider>" (tenant lowercased
//     from the path param, provider lowercased from ?provider= — empty provider is a valid
//     shared sub-bucket per (ip,tenant)).
//   - /auth/sso/callback       → "sso:cb:<ip>" (no :tenant param on the route; the real
//     tenant is sealed inside `state` and only the service can open it).
//   - /auth/sso/exchange       → "sso:ex:<ip>" (SSO-FE-REDIRECT: the frontend's handoff-code
//     redemption). It rides the SSO limiter but gets its OWN per-IP namespace so a redirect-mode
//     login (one /callback + one /exchange from the same egress IP within seconds) does NOT
//     double-burn the /callback bucket — that would halve the NAT-workforce budget the whole
//     SSO-RATELIMIT-NAT limiter exists to protect (reviewer MED-1). Path-distinguished because
//     /exchange, like /callback, carries no :tenant param.
//
// A start request whose :tenant param is somehow empty falls back to the per-IP callback
// key — fail-secure: the coarser per-IP bucket aggregates MORE traffic, never less.
func ssoRateLimitKey(c *gin.Context, trustedCIDRs []*net.IPNet) string {
	ip := clientIP(c.Request, trustedCIDRs)
	if strings.HasSuffix(c.FullPath(), "/auth/sso/exchange") {
		return "sso:ex:" + ip
	}
	tenant := strings.ToLower(strings.TrimSpace(c.Param("tenant")))
	if tenant == "" {
		return "sso:cb:" + ip
	}
	provider := strings.ToLower(strings.TrimSpace(c.Query("provider")))
	return "sso:start:" + ip + ":" + tenant + ":" + provider
}
