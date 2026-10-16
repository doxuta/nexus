package services

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
)

// SSOAllowPrivateNetworksEnv is the operator opt-in that lifts the SSRF guard (reviewer FIX 3,
// MED) below, allowing OIDC discovery/JWKS/token-exchange dials to land on
// private/loopback/link-local addresses. OFF by default in production: an admin-entered `issuer`
// URL (../specs/004-sso-oidc/spec.md §"#8" — CreateProvider only shape-checks it, never live-fetches
// at creation time) is untrusted input that otherwise gets dialed server-side by this backend at
// /start and /callback — without this guard, an admin (or an attacker who compromised an admin
// session) could point `issuer` at 127.0.0.1, an internal 10.x host, or a cloud metadata endpoint
// (169.254.169.254) and use this server as an SSRF proxy into its own network. Real enterprise
// on-prem IdPs sometimes DO live on an internal network reachable only from inside the
// deployment's own perimeter, so this is an explicit, documented escape hatch — NOT a default.
// The httptest-stubbed SSO integration suite also uses it (set via t.Setenv, loopback only) to
// keep exercising the REAL discovery -> JWKS -> exchange code path end to end without weakening
// the production default.
const SSOAllowPrivateNetworksEnv = "SSO_ALLOW_PRIVATE_NETWORK_IDP"

// ssoDialTimeout / ssoHTTPTimeout bound how long a single OIDC discovery/JWKS/token-exchange
// round trip is allowed to hang against a slow or unreachable admin-entered issuer.
const (
	ssoDialTimeout = 10 * time.Second
	ssoHTTPTimeout = 15 * time.Second
)

// isBlockedSSRFTarget reports whether ip must never be dialed by this server on behalf of an
// admin-entered OIDC issuer: loopback, RFC 1918 / IPv6 ULA private ranges, link-local (which
// covers the 169.254.169.254 cloud-metadata address used by AWS/GCP/Azure instance-metadata
// services — the classic SSRF-to-credential-theft target), unspecified (0.0.0.0/::), and
// multicast. net.IP's own IsPrivate/IsLoopback/IsLinkLocalUnicast correctly normalize
// IPv4-mapped-IPv6 literals (e.g. "::ffff:127.0.0.1") to their IPv4 meaning, so those are covered
// without a separate case.
func isBlockedSSRFTarget(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

// ssoAllowPrivateNetworks reads the escape-hatch env var fresh on every dial attempt (cheap — a
// single os.Getenv call per OIDC HTTP round trip, not a hot per-record loop) so a test can flip
// it per-subtest via t.Setenv without needing to reconstruct any cached client.
func ssoAllowPrivateNetworks() bool {
	v := strings.TrimSpace(os.Getenv(SSOAllowPrivateNetworksEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// ssrfSafeDialer returns a *net.Dialer whose Control hook inspects the POST-DNS-RESOLUTION
// address before the stack connects to it — this is what defeats DNS-rebinding: a hostname that
// resolves to a public IP on first lookup and a private one on a later lookup is checked at the
// address actually being connected to on EACH dial attempt (net.Dialer.DialContext calls Control
// once per resolved candidate address, in order, until one succeeds), not at the original
// hostname string.
func ssrfSafeDialer() *net.Dialer {
	return &net.Dialer{
		Timeout: ssoDialTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			if ssoAllowPrivateNetworks() {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("services: sso ssrf guard: invalid dial address %q: %w", address, err)
			}
			ip := net.ParseIP(host)
			if ip == nil {
				// DialContext always calls Control with a resolved literal IP, never a hostname —
				// an unparseable host here would be unexpected, but fail closed rather than dial it.
				return fmt.Errorf("services: sso ssrf guard: dial address %q did not resolve to a literal IP", host)
			}
			if isBlockedSSRFTarget(ip) {
				return fmt.Errorf("services: sso ssrf guard: refusing to dial private/internal address %s (set %s=1 to allow)", ip, SSOAllowPrivateNetworksEnv)
			}
			return nil
		},
	}
}

// ssrfSafeHTTPClient builds the *http.Client used for EVERY outbound OIDC HTTP call this package
// makes on behalf of an admin-entered issuer — discovery, JWKS refresh, AND the authorization-
// code-for-token exchange (sso_oidc_client.go injects this same client at both call sites via
// oidc.ClientContext and oauth2.HTTPClient respectively). Redirects are refused outright
// (defense in depth: a redirect hop would re-dial through this SAME guarded Transport anyway, so
// refusing removes the need to reason about intermediate hops or header-leak-across-origin
// concerns entirely).
func ssrfSafeHTTPClient() *http.Client {
	return &http.Client{
		Timeout: ssoHTTPTimeout,
		Transport: &http.Transport{
			DialContext:     ssrfSafeDialer().DialContext,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("services: sso ssrf guard: redirects are not allowed during OIDC discovery/exchange")
		},
	}
}
