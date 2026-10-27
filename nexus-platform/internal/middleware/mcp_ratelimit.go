// Package middleware — mcp_ratelimit.go: the request brakes in front of /mcp (issue #22). /mcp sits
// outside /api/v1, so none of the API's limiters covered it: every unauthenticated nxm_ guess cost one
// indexed SELECT with nothing slowing it, and a key holder had no request budget at all.
//
// Two brakes, both the API's own sliding window (memRateLimiter), and every request is charged to
// exactly ONE of them:
//   - a token not yet seen working (a guess, or a real key's first request) spends its client IP's
//     budget of lookups — the same per-minute budget the API-key brake gives an IP (queue #356);
//   - a token seen working spends its own budget — the per-key budget of the data plane (#52) — so
//     the real agents behind one NAT address never wait on a prober's spent IP budget.
//
// A token is "seen working" once /mcp answered it with a 2xx; a 401 forgets it, so a revoked or
// expired key goes back to its IP's budget. A request without an nxm_ bearer passes: the handler
// refuses it without a lookup. Refusals are the canonical RATE_LIMITED 429 (writeRateLimited429).
//
// Ceiling: in-memory, per process, like every limiter here (RM-15 note); a shared store is the upgrade.
package middleware

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/auth"
)

// mcpTokenPrefix is services.MCPTokenPrefix; this package does not import services.
const mcpTokenPrefix = "nxm_"

// mcpWorkingMax caps the memory of working tokens (~100 bytes an entry).
const mcpWorkingMax = 10000

// MCPRateLimitConfig holds the /mcp brakes. A budget <= 0 switches that brake off.
type MCPRateLimitConfig struct {
	// ProbeMax is how many lookups of not-yet-working tokens one client IP may make per Window.
	ProbeMax int
	// KeyMax is how many requests one working token may make per Window.
	KeyMax int
	// Window is the sliding-window span of both budgets.
	Window time.Duration
	// TrustedProxyCIDRs: see RateLimitConfig.TrustedProxyCIDRs (clientIP).
	TrustedProxyCIDRs []*net.IPNet
}

// MCPRateLimit returns the gin middleware to mount in front of the /mcp handler.
func MCPRateLimit(cfg MCPRateLimitConfig) gin.HandlerFunc {
	if cfg.Window <= 0 || (cfg.ProbeMax <= 0 && cfg.KeyMax <= 0) {
		return func(c *gin.Context) { c.Next() }
	}
	var probes, keys *memRateLimiter
	if cfg.ProbeMax > 0 {
		probes = newMemRateLimiter(cfg.ProbeMax, cfg.Window)
	}
	if cfg.KeyMax > 0 {
		keys = newMemRateLimiter(cfg.KeyMax, cfg.Window)
	}
	var mu sync.Mutex
	working := map[string]struct{}{} // token hash → seen answered 2xx

	return func(c *gin.Context) {
		tok, _ := extractBearer(c.GetHeader("Authorization"))
		if !strings.HasPrefix(tok, mcpTokenPrefix) {
			c.Next()
			return
		}
		hash := auth.HashRefreshToken(tok) // the SHA-256 sys_mcp_tokens stores; never the raw token
		mu.Lock()
		_, known := working[hash]
		mu.Unlock()
		lim, bucket := probes, "mcpip:"+clientIP(c.Request, cfg.TrustedProxyCIDRs)
		if known {
			lim, bucket = keys, "mcpkey:"+hash
		}
		if lim != nil && !lim.allow(bucket) {
			writeRateLimited429(c, cfg.Window)
			return
		}
		c.Next()
		status := c.Writer.Status()
		mu.Lock()
		defer mu.Unlock()
		switch {
		case status == http.StatusUnauthorized:
			delete(working, hash)
		case status < 300 && !known:
			// ponytail: wholesale reset when full, like APIKeyService.markDead; only more than
			// mcpWorkingMax live tokens in one process fill it, and each is back on its next 2xx.
			if len(working) >= mcpWorkingMax {
				working = map[string]struct{}{}
			}
			working[hash] = struct{}{}
		}
	}
}
