// Package middleware — assistant_ratelimit.go: a PER-USER sliding-window rate limit for the ⌘K
// end-user AI assistant (POST /assistant/ask).
//
// Why per-user (not the per-tenant BudgetGuard the endpoint already has): the assistant is
// callable by EVERY authenticated user, but the M4 BudgetGuard's turn window keys per TENANT
// (default 10 turns/60s, SHARED across the whole tenant AND the admin AI Builder). Without a
// per-user cap, one ordinary user bursting ⌘K consumes that whole shared window and freezes every
// colleague out of the assistant for the rest of the window. This limiter puts a per-(tenant,user)
// ceiling — set BELOW the tenant cap so it actually bites — IN FRONT of the tenant budget, giving
// each user a bounded fair-share of the shared burst (security-audit MED, per-user burst share now
// closed).
//
// SCOPE: this bounds a user's BURST (requests per window) only. It does NOT bound a user's daily
// token total — a per-user slice of the per-tenant DAILY token budget is a separate, still-open
// item (a steady ≤cap/window user can still contribute to the tenant's daily budget all day).
//
// Storage: the same in-memory sliding-window machinery as AuthRateLimit / SSORateLimit
// (memRateLimiter — reused, not duplicated; janitor-evicted). Single-instance correct; swap for
// Redis alongside RM-15 when horizontal scaling lands. The 429 envelope is byte-identical to the
// other limiters (RATE_LIMITED + Retry-After) so the FE's existing 429 handling covers it.
package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
)

// AssistantRateLimitConfig holds the per-user assistant rate-limit parameters.
type AssistantRateLimitConfig struct {
	// MaxAttempts is the maximum /assistant/ask requests allowed per (tenant,user) within Window.
	MaxAttempts int
	// Window is the sliding-window duration.
	Window time.Duration
}

// AssistantRateLimit returns a gin middleware enforcing a per-(tenant,user) sliding-window limit.
// Mount it on the /assistant/ask route AFTER the auth+tenant chain (it reads the verified JWT
// subject). When MaxAttempts or Window is <= 0 the middleware is a no-op (mirrors AuthRateLimit).
// A request with no verified subject is passed through untouched — the auth chain rejects it
// anyway, and rate-limiting an unauthenticated call here would key every such call together.
func AssistantRateLimit(cfg AssistantRateLimitConfig) gin.HandlerFunc {
	if cfg.MaxAttempts <= 0 || cfg.Window <= 0 {
		return func(c *gin.Context) { c.Next() }
	}
	lim := newMemRateLimiter(cfg.MaxAttempts, cfg.Window)

	return func(c *gin.Context) {
		sub, ok := SubjectFromContext(c)
		if !ok || sub.TenantID == "" || sub.UserID == "" {
			c.Next()
			return
		}
		// Atomic check-and-record: a parallel burst must not slip past the cap.
		if !lim.allow("assistant:" + sub.TenantID + ":" + sub.UserID) {
			writeRateLimited429(c, cfg.Window)
			return
		}
		c.Next()
	}
}
