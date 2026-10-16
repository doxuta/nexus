// Package middleware — client_ip.go: the ONE shared client-IP extractor used by every per-IP
// limiter in this package (AuthRateLimit, SSORateLimit) and wherever the real client address
// matters. It exists because Nexus runs behind the shipped Caddy reverse proxy in production:
// with a proxy in front, r.RemoteAddr is ALWAYS the proxy's address, so keying a per-IP limiter
// on it collapses every request into ONE global bucket (HIGH-2: a single junk-login stream then
// 429s every user's login platform-wide — an unauthenticated login DoS).
//
// ⚠️ THE SAFETY INVARIANT (a wrong fix is worse than the bug): X-Forwarded-For is
// attacker-controlled. If we read the client IP from XFF UNCONDITIONALLY, an attacker sets a
// fresh X-Forwarded-For per request, mints a fresh limiter key each time, and BYPASSES the
// per-IP limiter entirely (unlimited password spray). So XFF is consulted ONLY when the DIRECT
// PEER (r.RemoteAddr) is inside a configured trusted-proxy CIDR set. With no trusted CIDRs
// configured (the default) the peer address is used verbatim — exactly today's behaviour, and
// safe: there is no XFF trust to abuse. The fix is strictly OPT-IN via TRUSTED_PROXY_CIDRS.
package middleware

import (
	"net"
	"net/http"
	"strings"
)

// clientIP returns the client IP to key per-IP limiters on (and, via gin's own SetTrustedProxies,
// what the access log records). It honours X-Forwarded-For ONLY when the direct peer is a trusted
// proxy; otherwise it returns the direct peer address so a spoofed XFF cannot rotate the key.
//
// Safety rests on TWO facts, both required:
//  1. We only read XFF after confirming the direct peer (r.RemoteAddr) is in trustedCIDRs — an
//     external attacker connecting directly is never trusted, so their XFF is ignored outright.
//  2. A trusted reverse proxy APPENDS the real client to the RIGHT of any client-sent XFF (Caddy's
//     reverse_proxy does exactly this). So walking the header right→left and taking the first entry
//     that is either the leftmost hop or not itself a trusted proxy yields the real client as seen
//     by the OUTERMOST trusted hop — attacker-injected left-hand entries are never reached because
//     the proxy-appended real peer sits to their right. Neither fact alone is sufficient: in
//     isolation a single spoofed XFF entry from a trusted peer would be returned verbatim; the
//     proxy-appends-right topology is what makes the selection safe.
//
// This mirrors gin's own validateHeader algorithm verbatim, so the limiter key equals the
// access-log client_ip gin derives once SetTrustedProxies is wired (one algorithm, no drift).
// trustedCIDRs empty → always the peer address (default-safe: XFF never trusted).
func clientIP(r *http.Request, trustedCIDRs []*net.IPNet) string {
	peer := remoteIP(r)
	if len(trustedCIDRs) == 0 {
		return peer
	}
	peerIP := net.ParseIP(peer)
	if peerIP == nil || !ipInCIDRs(peerIP, trustedCIDRs) {
		// Direct peer is not a trusted proxy → NEVER trust XFF (anti-spoof: otherwise the
		// attacker rotates the header to mint a fresh limiter key every request).
		return peer
	}
	// Peer is a trusted proxy → the real client is the rightmost non-trusted XFF entry. A missing
	// or malformed header falls back to the peer (fail-safe: coarser aggregation, never looser).
	if real, ok := clientFromXFF(r.Header.Get("X-Forwarded-For"), trustedCIDRs); ok {
		return real
	}
	return peer
}

// clientFromXFF returns the real client from an X-Forwarded-For header using the same right→left
// walk gin's validateHeader uses: stop at the first entry that is the leftmost hop OR not a
// trusted proxy. A non-IP (malformed) entry stops the walk with ok=false so the caller falls back
// to the peer address — an attacker cannot inject a bogus token to force a chosen fallback because
// the walk only reaches entries to the LEFT of the proxy-appended real peer.
func clientFromXFF(header string, trustedCIDRs []*net.IPNet) (string, bool) {
	if header == "" {
		return "", false
	}
	items := strings.Split(header, ",")
	for i := len(items) - 1; i >= 0; i-- {
		ipStr := strings.TrimSpace(items[i])
		ip := net.ParseIP(ipStr)
		if ip == nil {
			return "", false // malformed hop — stop; caller uses the trusted peer address
		}
		if i == 0 || !ipInCIDRs(ip, trustedCIDRs) {
			return ipStr, true
		}
	}
	return "", false
}

// ipInCIDRs reports whether ip is contained in any of the trusted-proxy CIDR blocks.
func ipInCIDRs(ip net.IP, cidrs []*net.IPNet) bool {
	for _, c := range cidrs {
		if c.Contains(ip) {
			return true
		}
	}
	return false
}
