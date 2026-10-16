package services

import (
	"net"
	"testing"
)

// TestIsBlockedSSRFTarget_Table proves the SSRF guard's core decision (reviewer FIX 3, MED): any
// literal IP an admin-supplied OIDC issuer resolves to that lands inside a private, loopback,
// link-local (includes the cloud metadata address 169.254.169.254), or unspecified range must be
// refused — a malicious or misconfigured admin-entered issuer must not be able to make this
// server's own network position probe/reach internal infrastructure.
func TestIsBlockedSSRFTarget_Table(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want bool
	}{
		{"loopback v4", "127.0.0.1", true},
		{"loopback v6", "::1", true},
		{"private 10.x", "10.0.0.5", true},
		{"private 172.16", "172.16.5.5", true},
		{"private 192.168", "192.168.1.1", true},
		{"link-local / cloud metadata", "169.254.169.254", true},
		{"link-local v6", "fe80::1", true},
		{"unique-local v6 (ULA)", "fc00::1", true},
		{"unspecified v4", "0.0.0.0", true},
		{"unspecified v6", "::", true},
		{"multicast v4", "224.0.0.1", true},
		{"IPv4-mapped IPv6 loopback", "::ffff:127.0.0.1", true},
		{"IPv4-mapped IPv6 private", "::ffff:10.1.2.3", true},
		{"public v4 (google dns)", "8.8.8.8", false},
		{"public v6", "2001:4860:4860::8888", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("net.ParseIP(%q) = nil", tt.ip)
			}
			if got := isBlockedSSRFTarget(ip); got != tt.want {
				t.Fatalf("isBlockedSSRFTarget(%q) = %v; want %v", tt.ip, got, tt.want)
			}
		})
	}
}

// TestSSRFSafeDialerControl_BlocksPrivateAddressesByDefault proves the net.Dialer.Control hook
// wired into the production HTTP client refuses to dial a private/loopback target — checked at
// the POST-DNS-RESOLUTION address (defeats DNS-rebinding: the guard inspects the literal IP the
// stack is about to connect(2) to, not the original hostname).
func TestSSRFSafeDialerControl_BlocksPrivateAddressesByDefault(t *testing.T) {
	t.Setenv(SSOAllowPrivateNetworksEnv, "")
	dialer := ssrfSafeDialer()

	if err := dialer.Control("tcp4", "10.0.0.5:443", nil); err == nil {
		t.Fatal("Control(10.0.0.5:443) = nil error; want blocked (private range)")
	}
	if err := dialer.Control("tcp4", "127.0.0.1:8080", nil); err == nil {
		t.Fatal("Control(127.0.0.1:8080) = nil error; want blocked (loopback)")
	}
	if err := dialer.Control("tcp4", "169.254.169.254:80", nil); err == nil {
		t.Fatal("Control(169.254.169.254:80) = nil error; want blocked (cloud metadata / link-local)")
	}
	if err := dialer.Control("tcp4", "8.8.8.8:443", nil); err != nil {
		t.Fatalf("Control(8.8.8.8:443) = %v; want a public address allowed through", err)
	}
}

// TestSSRFSafeDialerControl_AllowsPrivateAddresses_WhenEscapeHatchSet proves the deliberate
// operator opt-in (SSOAllowPrivateNetworksEnv — real on-prem/enterprise IdPs sometimes DO live on
// an internal 10.x network reachable only from inside the deployment's own perimeter) lifts the
// block, and is ALSO how the httptest-stubbed integration suite keeps exercising the real
// discovery/JWKS/exchange path against a 127.0.0.1 fake IdP without weakening the production
// default (which stays blocked unless this is explicitly set).
func TestSSRFSafeDialerControl_AllowsPrivateAddresses_WhenEscapeHatchSet(t *testing.T) {
	t.Setenv(SSOAllowPrivateNetworksEnv, "1")
	dialer := ssrfSafeDialer()

	if err := dialer.Control("tcp4", "127.0.0.1:8080", nil); err != nil {
		t.Fatalf("Control(127.0.0.1:8080) with escape hatch set = %v; want allowed", err)
	}
	if err := dialer.Control("tcp4", "10.0.0.5:443", nil); err != nil {
		t.Fatalf("Control(10.0.0.5:443) with escape hatch set = %v; want allowed", err)
	}
}

// TestSSRFSafeHTTPClient_BlocksRedirects proves the shared client refuses to follow ANY redirect
// hop — defense in depth alongside the dial-level IP check (a redirect to a private target would
// otherwise re-dial through the SAME guarded transport anyway, but refusing outright removes the
// need to reason about it).
func TestSSRFSafeHTTPClient_BlocksRedirects(t *testing.T) {
	client := ssrfSafeHTTPClient()
	if client.CheckRedirect == nil {
		t.Fatal("ssrfSafeHTTPClient().CheckRedirect is nil; want redirects refused")
	}
	if err := client.CheckRedirect(nil, nil); err == nil {
		t.Fatal("CheckRedirect(...) = nil error; want redirects refused")
	}
}
