package services

// Queue #44 session-list-kill-a (../specs/055-session-list-kill/spec.md): every token mint records the
// device on its refresh_tokens row, rotation keeps the device chain, and GET /auth/sessions lists
// exactly the caller's live chains.

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/auth"
)

const (
	uaChromeMac  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
	uaSafariIOS  = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1"
	uaEdgeWin    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36 Edg/128.0.2739.42"
	uaCocCoc     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) coc_coc_browser/128.0.212 Chrome/122.0.0.0 Safari/537.36"
	uaFirefoxLin = "Mozilla/5.0 (X11; Linux x86_64; rv:130.0) Gecko/20100101 Firefox/130.0"
	uaAndroid    = "Mozilla/5.0 (Linux; Android 14; SM-S918B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/25.0 Chrome/121.0.0.0 Mobile Safari/537.36"
	uaChromeIOS  = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/128.0.6613.98 Mobile/15E148 Safari/604.1"
	uaOperaWin   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36 OPR/113.0.0.0"
	uaChromeOS   = "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
)

func TestMaskIP(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"113.161.42.87", "113.161.42.x"},
		{" 10.0.0.1 ", "10.0.0.x"},
		{"127.0.0.1", "127.0.0.1"},               // loopback identifies no one: kept verbatim
		{"::ffff:113.161.42.87", "113.161.42.x"}, // IPv4-mapped IPv6 is still an IPv4 client
		{"2001:db8:85a3:8d3:1319:8a2e:370:7348", "2001:db8:85a3::/48"},
		{"::1", "::1"},
		{"", ""},
		{"not-an-ip", ""}, // garbage is dropped, never stored verbatim
	} {
		if got := maskIP(tc.in); got != tc.want {
			t.Errorf("maskIP(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDescribeUserAgent(t *testing.T) {
	for _, tc := range []struct{ ua, browser, os string }{
		{uaChromeMac, "Chrome", "macOS"},
		{uaSafariIOS, "Safari", "iOS"},
		{uaEdgeWin, "Edge", "Windows"},
		{uaCocCoc, "Cốc Cốc", "Windows"},
		{uaFirefoxLin, "Firefox", "Linux"},
		{uaAndroid, "Samsung Internet", "Android"},
		{uaChromeIOS, "Chrome", "iOS"},
		{uaOperaWin, "Opera", "Windows"},
		{uaChromeOS, "Chrome", "ChromeOS"},
		{"curl/8.4.0", "", ""},
		{"", "", ""},
	} {
		b, o := describeUserAgent(tc.ua)
		if b != tc.browser || o != tc.os {
			t.Errorf("describeUserAgent(%.40q…) = (%q, %q), want (%q, %q)", tc.ua, b, o, tc.browser, tc.os)
		}
	}
}

// liveRowsFor returns the live refresh rows of (tenant, user) in the fake store.
func (f *fakeRefreshRepo) liveRowsFor(tenantID, userID string) []auth.RefreshToken {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []auth.RefreshToken
	for _, rt := range f.rows {
		if rt.TenantID == tenantID && rt.UserID == userID && rt.RevokedAt == nil {
			out = append(out, *rt)
		}
	}
	return out
}

func deviceCtx(ua, ip string) context.Context {
	return WithClientDevice(context.Background(), ClientDevice{UserAgent: ua, IP: ip})
}

func TestLogin_RecordsDeviceOnTheSessionRow(t *testing.T) {
	svc, password := newTestService(t)
	refresh := svc.refresh.(*fakeRefreshRepo)

	if _, err := svc.Login(deviceCtx(uaChromeMac, "113.161.42.87"), LoginInput{Tenant: "acme", Email: "alice@acme.test", Password: password}); err != nil {
		t.Fatalf("login: %v", err)
	}
	rows := refresh.liveRowsFor("t-1", "u-1")
	if len(rows) != 1 {
		t.Fatalf("live rows = %d, want 1", len(rows))
	}
	rt := rows[0]
	if rt.SessionID == "" {
		t.Error("SessionID empty: a login must open a new session chain")
	}
	if rt.UserAgent != uaChromeMac {
		t.Errorf("UserAgent = %q, want the request's User-Agent", rt.UserAgent)
	}
	if rt.IP != "113.161.42.x" {
		t.Errorf("IP = %q, want the MASKED address 113.161.42.x (the full IP is never stored)", rt.IP)
	}
	if rt.LastUsedAt == nil || rt.LastUsedAt.IsZero() {
		t.Error("LastUsedAt not stamped at login")
	}

	// A second login is a second device: a different session chain.
	if _, err := svc.Login(deviceCtx(uaSafariIOS, "42.118.7.9"), LoginInput{Tenant: "acme", Email: "alice@acme.test", Password: password}); err != nil {
		t.Fatalf("second login: %v", err)
	}
	rows = refresh.liveRowsFor("t-1", "u-1")
	if len(rows) != 2 || rows[0].SessionID == rows[1].SessionID {
		t.Fatalf("two logins must be two sessions, got %d rows (%+v)", len(rows), rows)
	}
}

func TestLogin_UserAgentIsCappedAt255BytesOfValidUTF8(t *testing.T) {
	svc, password := newTestService(t)
	refresh := svc.refresh.(*fakeRefreshRepo)
	long := strings.Repeat("a", 254) + "ố" + strings.Repeat("b", 50) // the 255-byte cut lands inside "ố"

	if _, err := svc.Login(deviceCtx(long, ""), LoginInput{Tenant: "acme", Email: "alice@acme.test", Password: password}); err != nil {
		t.Fatalf("login: %v", err)
	}
	ua := refresh.liveRowsFor("t-1", "u-1")[0].UserAgent
	if len(ua) > 255 {
		t.Errorf("stored UA is %d bytes, want ≤ 255 (VARCHAR(255))", len(ua))
	}
	if !strings.HasPrefix(ua, strings.Repeat("a", 254)) || strings.ContainsRune(ua, '�') || !isValidUTF8(ua) {
		t.Errorf("stored UA %q is not a clean prefix of the header", ua)
	}
}

func isValidUTF8(s string) bool { return strings.ToValidUTF8(s, "\x00") == s }

// net/http accepts obs-text (bytes ≥ 0x80, RFC 7230) in a header, so a SHORT User-Agent can carry
// bytes that are not UTF-8 — and the utf8mb4 column refuses them in strict mode ("Incorrect string
// value"), which failed the login (500 LOGIN_FAILED) and the refresh of a legacy row. Every UA is
// cleaned, not only the ones long enough to be cut.
func TestTruncateUserAgent(t *testing.T) {
	long := strings.Repeat("a", 254) + "ố" + strings.Repeat("b", 50)
	for _, tc := range []struct{ name, in, want string }{
		{"plain", uaChromeMac, uaChromeMac},
		{"trimmed", "  curl/8.4.0 \t", "curl/8.4.0"},
		{"empty", "", ""},
		{"short latin-1 byte", "Mozilla/5.0 caf\xe9", "Mozilla/5.0 caf"},
		{"short lone continuation bytes", "\x80\xbfUA", "UA"},
		{"valid multibyte kept", "Trình duyệt Cốc Cốc", "Trình duyệt Cốc Cốc"},
		{"long cut inside a rune", long, strings.Repeat("a", 254)},
		{"long with invalid byte before the cut", "\xff" + strings.Repeat("a", 300), strings.Repeat("a", 255)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateUserAgent(tc.in)
			if got != tc.want {
				t.Errorf("truncateUserAgent(%.40q) = %.60q, want %.60q", tc.in, got, tc.want)
			}
			if len(got) > maxUserAgentBytes || !isValidUTF8(got) {
				t.Errorf("truncateUserAgent(%.40q) = %.60q: not ≤ %d bytes of valid UTF-8", tc.in, got, maxUserAgentBytes)
			}
		})
	}
}

// Rotation is the same device: session id and User-Agent carry over from the presented row, the
// IP is the one the refresh came from, and exactly one row of the chain stays alive.
func TestRefresh_RotationKeepsTheSessionChain(t *testing.T) {
	svc, password := newTestService(t)
	refresh := svc.refresh.(*fakeRefreshRepo)
	out, err := svc.Login(deviceCtx(uaChromeMac, "113.161.42.87"), LoginInput{Tenant: "acme", Email: "alice@acme.test", Password: password})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	before := refresh.liveRowsFor("t-1", "u-1")[0]

	later := time.Now().Add(10 * time.Minute)
	svc.now = func() time.Time { return later }
	// The refresh request carries a different UA string (a browser update mid-session) and a new
	// network: the chain keeps its device label, the IP follows the device.
	if _, err := svc.Refresh(deviceCtx("SomethingElse/1.0", "14.232.1.2"), out.RefreshToken); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	rows := refresh.liveRowsFor("t-1", "u-1")
	if len(rows) != 1 {
		t.Fatalf("live rows after rotation = %d, want exactly 1 per device chain", len(rows))
	}
	after := rows[0]
	if after.ID == before.ID {
		t.Fatal("rotation did not mint a new row")
	}
	if after.SessionID != before.SessionID {
		t.Errorf("SessionID changed on rotation: %q → %q (the chain id must survive)", before.SessionID, after.SessionID)
	}
	if after.UserAgent != uaChromeMac {
		t.Errorf("UserAgent = %q, want the chain's original %q", after.UserAgent, uaChromeMac)
	}
	if after.IP != "14.232.1.x" {
		t.Errorf("IP = %q, want the refresh request's masked IP 14.232.1.x", after.IP)
	}
	if after.LastUsedAt == nil || !after.LastUsedAt.Equal(later) {
		t.Errorf("LastUsedAt = %v, want the rotation time %v", after.LastUsedAt, later)
	}
}

// A pre-migration chain (no session id on the row) adopts one on its first rotation instead of
// staying anonymous forever.
func TestRefresh_LegacyRowWithoutSessionAdoptsOne(t *testing.T) {
	svc, password := newTestService(t)
	refresh := svc.refresh.(*fakeRefreshRepo)
	out, err := svc.Login(context.Background(), LoginInput{Tenant: "acme", Email: "alice@acme.test", Password: password})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	refresh.mu.Lock()
	for _, rt := range refresh.rows {
		rt.SessionID, rt.UserAgent, rt.IP, rt.LastUsedAt = "", "", "", nil
	}
	refresh.mu.Unlock()

	if _, err := svc.Refresh(deviceCtx(uaFirefoxLin, "10.1.2.3"), out.RefreshToken); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	rt := refresh.liveRowsFor("t-1", "u-1")[0]
	if rt.SessionID == "" || rt.UserAgent != uaFirefoxLin || rt.IP != "10.1.2.x" {
		t.Errorf("legacy chain after rotation = {sid:%q ua:%q ip:%q}, want a fresh sid + this request's device", rt.SessionID, rt.UserAgent, rt.IP)
	}
}

func TestListSessions_OnlyTheCallersLiveSessions(t *testing.T) {
	svc, password := newTestService(t)
	refresh := svc.refresh.(*fakeRefreshRepo)
	ctx := context.Background()

	t0 := time.Now().Add(-2 * time.Hour).Truncate(time.Second) // relative: rows must still be unexpired whenever this runs
	svc.now = func() time.Time { return t0 }
	if _, err := svc.Login(deviceCtx(uaChromeMac, "113.161.42.87"), LoginInput{Tenant: "acme", Email: "alice@acme.test", Password: password}); err != nil {
		t.Fatalf("login mac: %v", err)
	}
	svc.now = func() time.Time { return t0.Add(time.Hour) }
	if _, err := svc.Login(deviceCtx(uaSafariIOS, "42.118.7.9"), LoginInput{Tenant: "acme", Email: "alice@acme.test", Password: password}); err != nil {
		t.Fatalf("login iphone: %v", err)
	}
	svc.now = time.Now

	// Noise that must never be listed: a revoked chain, an expired chain, another user, another tenant.
	past := t0.Add(-time.Hour)
	for _, rt := range []*auth.RefreshToken{
		{ID: "r-rev", SessionID: "s-rev", TenantID: "t-1", UserID: "u-1", TokenHash: "h-rev", ExpiresAt: time.Now().Add(time.Hour), RevokedAt: &past},
		{ID: "r-exp", SessionID: "s-exp", TenantID: "t-1", UserID: "u-1", TokenHash: "h-exp", ExpiresAt: past},
		{ID: "r-bob", SessionID: "s-bob", TenantID: "t-1", UserID: "u-2", TokenHash: "h-bob", ExpiresAt: time.Now().Add(time.Hour)},
		{ID: "r-x", SessionID: "s-x", TenantID: "t-2", UserID: "u-1", TokenHash: "h-x", ExpiresAt: time.Now().Add(time.Hour)},
	} {
		if err := refresh.Store(ctx, rt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	got, err := svc.ListSessions(ctx, "t-1", "u-1", "")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("sessions = %d (%+v), want exactly the 2 live logins", len(got), got)
	}
	// Most recently used first.
	if got[0].Browser != "Safari" || got[0].OS != "iOS" || got[0].IP != "42.118.7.x" {
		t.Errorf("first row = %+v, want the iPhone (used last)", got[0])
	}
	if got[1].Browser != "Chrome" || got[1].OS != "macOS" || got[1].IP != "113.161.42.x" || got[1].UserAgent != uaChromeMac {
		t.Errorf("second row = %+v, want the Mac", got[1])
	}
	if !got[1].LastUsedAt.Equal(t0) {
		t.Errorf("LastUsedAt = %v, want the login time %v", got[1].LastUsedAt, t0)
	}
	for _, s := range got {
		if s.ID == "" || strings.HasPrefix(s.ID, "s-") {
			t.Errorf("unexpected session id %q in the caller's list", s.ID)
		}
	}
}

func TestListSessions_UnknownUserIsNotFound(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.ListSessions(context.Background(), "t-1", "ghost", "")
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "USER_NOT_FOUND" {
		t.Fatalf("err = %v, want ValidationError USER_NOT_FOUND", err)
	}
	// Tenant-pinned: a real user id under another tenant misses the same way.
	if _, err := svc.ListSessions(context.Background(), "t-2", "u-1", ""); !errors.As(err, &ve) || ve.Code != "USER_NOT_FOUND" {
		t.Fatalf("cross-tenant err = %v, want USER_NOT_FOUND", err)
	}
}

// ListLiveSessions mirrors the real repository: the (tenant, user) rows that are neither revoked
// nor expired, most recently used first, capped at limit.
func (f *fakeRefreshRepo) ListLiveSessions(_ context.Context, tenantID, userID string, limit int) ([]auth.RefreshToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []auth.RefreshToken
	for _, rt := range f.rows {
		if rt.TenantID == tenantID && rt.UserID == userID && rt.RevokedAt == nil && rt.ExpiresAt.After(f.now()) {
			out = append(out, *rt)
		}
	}
	lastUsed := func(rt auth.RefreshToken) time.Time {
		if rt.LastUsedAt != nil {
			return *rt.LastUsedAt
		}
		return rt.CreatedAt
	}
	sort.Slice(out, func(i, j int) bool { return lastUsed(out[i]).After(lastUsed(out[j])) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *raceLoserRefreshRepo) ListLiveSessions(context.Context, string, string, int) ([]auth.RefreshToken, error) {
	return nil, nil
}
