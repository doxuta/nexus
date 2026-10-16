package config

import (
	"net"
	"testing"
)

func TestParseTrustedProxyCIDRs(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantMatch   []string // IPs that must be contained in the parsed set
		wantNoMatch []string // IPs that must NOT be contained
		wantNets    int
		wantSkipped []string
	}{
		{
			name:        "empty input yields no trusted proxy (default-safe)",
			raw:         "",
			wantNets:    0,
			wantSkipped: nil,
		},
		{
			name:      "single CIDR",
			raw:       "10.0.0.0/8",
			wantMatch: []string{"10.1.2.3", "10.255.255.255"},
			// 11.0.0.1 is outside 10.0.0.0/8.
			wantNoMatch: []string{"11.0.0.1", "192.168.1.1"},
			wantNets:    1,
		},
		{
			name:      "RFC1918 comma list with whitespace",
			raw:       " 10.0.0.0/8 , 172.16.0.0/12 ,192.168.0.0/16 ",
			wantMatch: []string{"10.0.0.5", "172.20.0.9", "192.168.1.1"},
			// 172.32.0.1 is above the /12 (172.16.0.0–172.31.255.255).
			wantNoMatch: []string{"172.32.0.1", "8.8.8.8"},
			wantNets:    3,
		},
		{
			name:      "bare IPv4 becomes a /32",
			raw:       "203.0.113.7",
			wantMatch: []string{"203.0.113.7"},
			// A /32 must not match a neighbour.
			wantNoMatch: []string{"203.0.113.8"},
			wantNets:    1,
		},
		{
			name:      "bare IPv6 becomes a /128",
			raw:       "2001:db8::1",
			wantMatch: []string{"2001:db8::1"},
			wantNets:  1,
		},
		{
			name:        "invalid entry is skipped, valid one kept (degrade-to-default)",
			raw:         "10.0.0.0/8,not-a-cidr,300.1.2.3",
			wantMatch:   []string{"10.0.0.1"},
			wantNets:    1,
			wantSkipped: []string{"not-a-cidr", "300.1.2.3"},
		},
		{
			// A /0 trusts XFF from the WHOLE internet → full spoof-bypass. It must be rejected
			// (skipped, not trusted), fail-safe, not silently honoured.
			name:        "IPv4 /0 is rejected as overbroad (fail-safe)",
			raw:         "0.0.0.0/0",
			wantNoMatch: []string{"8.8.8.8", "10.0.0.1"},
			wantNets:    0,
			wantSkipped: []string{"0.0.0.0/0"},
		},
		{
			name:        "IPv6 ::/0 is rejected as overbroad",
			raw:         "::/0",
			wantNets:    0,
			wantSkipped: []string{"::/0"},
		},
		{
			// Overbroad entry skipped, a legitimate specific CIDR still kept.
			name:        "mixed: /0 skipped, specific CIDR kept",
			raw:         "0.0.0.0/0,172.16.0.0/12",
			wantMatch:   []string{"172.20.0.1"},
			wantNoMatch: []string{"8.8.8.8"},
			wantNets:    1,
			wantSkipped: []string{"0.0.0.0/0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nets, skipped := ParseTrustedProxyCIDRs(tt.raw)

			if len(nets) != tt.wantNets {
				t.Fatalf("got %d nets, want %d", len(nets), tt.wantNets)
			}
			if len(skipped) != len(tt.wantSkipped) {
				t.Fatalf("got skipped %v, want %v", skipped, tt.wantSkipped)
			}
			for i, s := range tt.wantSkipped {
				if skipped[i] != s {
					t.Errorf("skipped[%d] = %q, want %q", i, skipped[i], s)
				}
			}
			for _, ip := range tt.wantMatch {
				if !contains(nets, ip) {
					t.Errorf("expected %s to be inside the trusted set", ip)
				}
			}
			for _, ip := range tt.wantNoMatch {
				if contains(nets, ip) {
					t.Errorf("expected %s NOT to be inside the trusted set", ip)
				}
			}
		})
	}
}

func contains(nets []*net.IPNet, ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
