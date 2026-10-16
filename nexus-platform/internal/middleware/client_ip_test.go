package middleware

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// mustCIDRs parses CIDR/bare-IP strings into *net.IPNet for the trusted-proxy set, failing the
// test on any unparseable entry. Mirrors config.ParseTrustedProxyCIDRs without the skip semantics.
func mustCIDRs(t *testing.T, entries ...string) []*net.IPNet {
	t.Helper()
	var nets []*net.IPNet
	for _, e := range entries {
		if _, ipnet, err := net.ParseCIDR(e); err == nil {
			nets = append(nets, ipnet)
			continue
		}
		if ip := net.ParseIP(e); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		t.Fatalf("mustCIDRs: unparseable entry %q", e)
	}
	return nets
}

// TestClientIP_Table exercises the shared clientIP extractor directly. The load-bearing property
// is the FIRST group: an UNTRUSTED direct peer means X-Forwarded-For is ignored entirely, so an
// attacker cannot rotate the header to escape a per-IP limiter.
func TestClientIP_Table(t *testing.T) {
	trusted := mustCIDRs(t, "10.0.0.0/8")
	trustedV6 := mustCIDRs(t, "2001:db8::/32")

	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		cidrs      []*net.IPNet
		want       string
	}{
		// --- untrusted / default path: XFF NEVER trusted -> peer wins ------------------------
		{
			name:       "empty trusted set ignores XFF (default-safe)",
			remoteAddr: "203.0.113.7:1234",
			xff:        "1.1.1.1",
			cidrs:      nil,
			want:       "203.0.113.7",
		},
		{
			name:       "untrusted peer ignores XFF even when a trusted set is configured",
			remoteAddr: "203.0.113.7:1234",
			xff:        "1.1.1.1",
			cidrs:      trusted,
			want:       "203.0.113.7",
		},
		{
			name:       "untrusted peer + multi-hop spoofed XFF still keys on peer",
			remoteAddr: "198.51.100.9:5000",
			xff:        "1.1.1.1, 2.2.2.2, 10.0.0.5",
			cidrs:      trusted,
			want:       "198.51.100.9",
		},
		// --- trusted path: strip trusted proxies from the right ------------------------------
		{
			name:       "trusted peer takes real client left of the trusted proxy",
			remoteAddr: "10.0.0.5:1234",
			xff:        "1.1.1.1, 10.0.0.5",
			cidrs:      trusted,
			want:       "1.1.1.1",
		},
		{
			name:       "trusted peer with a chain of trusted proxies strips all of them",
			remoteAddr: "10.0.0.5:1234",
			xff:        "1.1.1.1, 10.0.0.6, 10.0.0.5",
			cidrs:      trusted,
			want:       "1.1.1.1",
		},
		{
			// In ISOLATION a single spoofed entry from a trusted peer is returned verbatim (i==0).
			// Safe only because a real trusted proxy (Caddy) APPENDS the real client to the right;
			// this row documents that the topology, not the algorithm alone, is what protects us.
			name:       "trusted peer single XFF entry returns it (topology-dependent)",
			remoteAddr: "10.0.0.5:1234",
			xff:        "1.1.1.1",
			cidrs:      trusted,
			want:       "1.1.1.1",
		},
		{
			name:       "trusted peer missing XFF falls back to peer",
			remoteAddr: "10.0.0.5:1234",
			xff:        "",
			cidrs:      trusted,
			want:       "10.0.0.5",
		},
		{
			name:       "trusted peer malformed rightmost XFF falls back to peer (no panic)",
			remoteAddr: "10.0.0.5:1234",
			xff:        "garbage",
			cidrs:      trusted,
			want:       "10.0.0.5",
		},
		{
			name:       "trusted peer malformed hop right of a valid entry falls back to peer",
			remoteAddr: "10.0.0.5:1234",
			xff:        "1.1.1.1, garbage",
			cidrs:      trusted,
			want:       "10.0.0.5",
		},
		{
			name:       "trusted peer all-trusted chain returns leftmost hop",
			remoteAddr: "10.0.0.5:1234",
			xff:        "10.0.0.1, 10.0.0.2",
			cidrs:      trusted,
			want:       "10.0.0.1",
		},
		{
			name:       "trusted peer without a port on RemoteAddr",
			remoteAddr: "10.0.0.5",
			xff:        "1.1.1.1, 10.0.0.5",
			cidrs:      trusted,
			want:       "1.1.1.1",
		},
		{
			name:       "IPv6 trusted peer strips proxy and keeps client",
			remoteAddr: "[2001:db8::1]:443",
			xff:        "1.1.1.1, 2001:db8::1",
			cidrs:      trustedV6,
			want:       "1.1.1.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}
			if got := clientIP(req, tt.cidrs); got != tt.want {
				t.Errorf("clientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

// doRequestXFF fires POST /auth/login from remoteAddr with the given X-Forwarded-For header and
// JSON body (empty body → IP-only keying), so a test can prove which IP the limiter keyed on.
func doRequestXFF(r *gin.Engine, remoteAddr, xff, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestTrustedProxy_SpoofBypass_AuthRateLimit is THE spoof-bypass guard, proven end-to-end through
// the real AuthRateLimit middleware (not clientIP in isolation): an attacker whose direct peer is
// NOT in the trusted set rotates X-Forwarded-For per request. Both requests MUST share one limiter
// key (the peer), so the second is 429 — the header cannot be used to mint a fresh key and dodge
// the per-IP limiter.
func TestTrustedProxy_SpoofBypass_AuthRateLimit(t *testing.T) {
	// A trusted set IS configured (10.0.0.0/8 — the reverse-proxy network), but the attacker
	// connects from an address OUTSIDE it, so their XFF must be ignored.
	cfg := RateLimitConfig{
		MaxAttempts:       1,
		Window:            10 * time.Second,
		TrustedProxyCIDRs: mustCIDRs(t, "10.0.0.0/8"),
	}
	r := newRateLimitRouter(cfg)

	attackerPeer := "203.0.113.7:40000" // NOT in 10.0.0.0/8

	// First request from the attacker with one spoofed XFF — consumes the single allowed attempt.
	if w := doRequestXFF(r, attackerPeer, "1.1.1.1", ""); w.Code != http.StatusOK {
		t.Fatalf("first spoofed request: expected 200, got %d", w.Code)
	}
	// Second request from the SAME peer but a DIFFERENT spoofed XFF. If XFF were trusted the key
	// would rotate and this would pass (bypass). It MUST be 429 — the peer is the key.
	if w := doRequestXFF(r, attackerPeer, "2.2.2.2", ""); w.Code != http.StatusTooManyRequests {
		t.Errorf("rotated-XFF request from same untrusted peer: expected 429 (shared bucket), got %d — XFF SPOOF BYPASS", w.Code)
	}
}

// TestTrustedProxy_PerClientKeying_AuthRateLimit proves the flip side: when the direct peer IS a
// trusted proxy, the real client from X-Forwarded-For becomes the key, so two different real
// clients behind the same proxy get INDEPENDENT buckets (per-IP limiting restored behind Caddy).
func TestTrustedProxy_PerClientKeying_AuthRateLimit(t *testing.T) {
	cfg := RateLimitConfig{
		MaxAttempts:       1,
		Window:            10 * time.Second,
		TrustedProxyCIDRs: mustCIDRs(t, "10.0.0.0/8"),
	}
	r := newRateLimitRouter(cfg)

	proxyPeer := "10.0.0.5:8080" // the trusted reverse proxy

	// Client A behind the proxy: "<realclient>, <trustedproxy>".
	if w := doRequestXFF(r, proxyPeer, "1.1.1.1, 10.0.0.5", ""); w.Code != http.StatusOK {
		t.Fatalf("client A first request: expected 200, got %d", w.Code)
	}
	if w := doRequestXFF(r, proxyPeer, "1.1.1.1, 10.0.0.5", ""); w.Code != http.StatusTooManyRequests {
		t.Errorf("client A second request: expected 429 (own bucket exhausted), got %d", w.Code)
	}
	// Client B behind the SAME proxy but a different real client IP → independent bucket → passes.
	if w := doRequestXFF(r, proxyPeer, "2.2.2.2, 10.0.0.5", ""); w.Code != http.StatusOK {
		t.Errorf("client B (different real client, same proxy): expected 200 (independent bucket), got %d", w.Code)
	}
}

// TestTrustedProxy_SpoofThroughProxy_AuthRateLimit is the load-bearing HIGH-2 attack for the
// production topology: in prod the direct peer is ALWAYS Caddy (the only ingress), so every
// request — the attacker's included — takes the TRUSTED path. Caddy appends the real client IP to
// the RIGHT of any client-sent X-Forwarded-For, so an attacker can only inject entries to the
// LEFT. Rotating that left entry must NOT rotate the limiter key: the key is the rightmost
// non-trusted entry (the Caddy-appended real IP), so both requests share one bucket and the second
// is 429. This proves spoof-resistance ON the trusted path — the untrusted-peer spoof test above
// and this one together cover both ingress shapes.
func TestTrustedProxy_SpoofThroughProxy_AuthRateLimit(t *testing.T) {
	cfg := RateLimitConfig{
		MaxAttempts:       1,
		Window:            10 * time.Second,
		TrustedProxyCIDRs: mustCIDRs(t, "10.0.0.0/8"),
	}
	r := newRateLimitRouter(cfg)

	caddy := "10.0.0.5:8080"      // trusted peer — the ONLY ingress in prod
	attackerReal := "203.0.113.7" // appended rightmost by Caddy, non-trusted → the real key

	// First request: attacker injects a left spoof; Caddy appends the real IP on the right.
	if w := doRequestXFF(r, caddy, "9.9.9.9, "+attackerReal, ""); w.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", w.Code)
	}
	// Attacker rotates ONLY the left-hand spoofed entry; the Caddy-appended real IP stays rightmost.
	// The key must remain attackerReal → second request 429 (no fresh bucket minted).
	if w := doRequestXFF(r, caddy, "8.8.8.8, "+attackerReal, ""); w.Code != http.StatusTooManyRequests {
		t.Errorf("rotated left-spoof behind trusted proxy: expected 429 (keyed on rightmost real IP), got %d — SPOOF BYPASS THROUGH PROXY", w.Code)
	}
}

// TestTrustedProxy_EmptyConfig_KeysOnPeer_AuthRateLimit confirms the default path is unchanged:
// with no trusted proxies configured, the limiter keys on RemoteAddr and never trusts XFF.
func TestTrustedProxy_EmptyConfig_KeysOnPeer_AuthRateLimit(t *testing.T) {
	cfg := RateLimitConfig{MaxAttempts: 1, Window: 10 * time.Second} // no TrustedProxyCIDRs
	r := newRateLimitRouter(cfg)

	peer := "203.0.113.50:1234"
	if w := doRequestXFF(r, peer, "1.1.1.1", ""); w.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", w.Code)
	}
	// Rotated XFF must NOT create a fresh bucket — default-safe keys on peer.
	if w := doRequestXFF(r, peer, "9.9.9.9", ""); w.Code != http.StatusTooManyRequests {
		t.Errorf("rotated XFF with empty trusted set: expected 429, got %d", w.Code)
	}
}

// newSSORouter mounts SSORateLimit(cfg) on the /auth/sso/callback route (per-IP key path) with a
// 200 stub, so the SSO limiter's separate ssoRateLimitKey code path is exercised end-to-end.
func newSSORouter(cfg SSORateLimitConfig) *gin.Engine {
	r := gin.New()
	r.Use(SSORateLimit(cfg))
	r.GET("/auth/sso/callback", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	return r
}

func doGetXFF(r *gin.Engine, path, remoteAddr, xff string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestTrustedProxy_SpoofBypass_SSORateLimit repeats the spoof-bypass proof through the SSO limiter,
// since ssoRateLimitKey is a distinct edited call path. Untrusted peer + rotated XFF → shared key.
func TestTrustedProxy_SpoofBypass_SSORateLimit(t *testing.T) {
	cfg := SSORateLimitConfig{
		MaxAttempts:       1,
		Window:            10 * time.Second,
		TrustedProxyCIDRs: mustCIDRs(t, "10.0.0.0/8"),
	}
	r := newSSORouter(cfg)

	attackerPeer := "203.0.113.7:40000"
	if w := doGetXFF(r, "/auth/sso/callback", attackerPeer, "1.1.1.1"); w.Code != http.StatusOK {
		t.Fatalf("first SSO callback: expected 200, got %d", w.Code)
	}
	if w := doGetXFF(r, "/auth/sso/callback", attackerPeer, "2.2.2.2"); w.Code != http.StatusTooManyRequests {
		t.Errorf("rotated-XFF SSO callback from same untrusted peer: expected 429, got %d — XFF SPOOF BYPASS", w.Code)
	}
}
