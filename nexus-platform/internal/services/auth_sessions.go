package services

// Login sessions — "thiết bị đang đăng nhập" (queue #44/#45, ../specs/055-session-list-kill/spec.md).
//
// A session is one refresh-token chain: minted at a login (password, MFA step 2, SSO), rotated on
// every /auth/refresh, identified for its whole life by refresh_tokens.session_id. Rotation revokes
// the presented row before it stores the successor (auth_service_extra.go step 3), so a chain has
// exactly one live row at any moment and listing the live rows IS listing the sessions — no join,
// no chain walk.

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/cache"
)

// MaxListedSessions caps GET /auth/sessions: a script that logs in on every run and never logs out
// can own hundreds of live chains; the 50 most recently used are the ones a person can recognise,
// and "Đăng xuất mọi thiết bị" still ends every one of them.
const MaxListedSessions = 50

// maxUserAgentBytes matches refresh_tokens.user_agent VARCHAR(255).
const maxUserAgentBytes = 255

// ClientDevice is the device a token request came from, as the HTTP edge saw it. The handler that
// mints tokens attaches it to the request context (handlers.deviceContext); the service reads it
// when it stores the refresh row, so no use-case signature has to grow a transport parameter.
type ClientDevice struct {
	UserAgent string
	IP        string // raw client IP; masked before it is stored (maskIP)
}

type clientDeviceKey struct{}

// WithClientDevice attaches the calling device to ctx for the token mint that follows.
func WithClientDevice(ctx context.Context, d ClientDevice) context.Context {
	return context.WithValue(ctx, clientDeviceKey{}, d)
}

// clientDeviceFrom returns the device attached by WithClientDevice, or the zero value (a mint with
// no HTTP edge — tests, internal callers — records an unknown device rather than failing).
func clientDeviceFrom(ctx context.Context) ClientDevice {
	d, _ := ctx.Value(clientDeviceKey{}).(ClientDevice)
	return d
}

// maskIP shortens a client address to what the session list shows: the /24 of an IPv4 address
// ("113.161.42.x") or the /48 of an IPv6 one ("2001:db8:85a3::/48"). Only this form is stored — the
// screen needs "which network", not "which host", and the full address already lives in the
// access log. An unparsable value yields "" (never stored verbatim). Loopback identifies no one and
// masks into nonsense ("::/48"), so it is kept as is — on a dev box, or behind a proxy whose
// TRUSTED_PROXY_CIDRS is unset, every session then honestly reads "127.0.0.1".
func maskIP(raw string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	addr = addr.Unmap() // ::ffff:a.b.c.d is an IPv4 client
	if addr.IsLoopback() {
		return addr.String()
	}
	if addr.Is4() {
		b := addr.As4()
		return fmt.Sprintf("%d.%d.%d.x", b[0], b[1], b[2])
	}
	p, err := addr.WithZone("").Prefix(48)
	if err != nil {
		return ""
	}
	return p.String()
}

// truncateUserAgent makes a User-Agent storable: valid UTF-8 (net/http accepts obs-text bytes in a
// header, and the utf8mb4 column refuses them in strict mode — a failed Store is a failed login),
// cut to the column width without leaving half a rune. Clean first, then cut.
func truncateUserAgent(ua string) string {
	ua = strings.ToValidUTF8(strings.TrimSpace(ua), "")
	if len(ua) <= maxUserAgentBytes {
		return ua
	}
	return strings.ToValidUTF8(ua[:maxUserAgentBytes], "")
}

// describeUserAgent names the browser and operating system of a User-Agent for the session list.
// Order matters: Chromium forks (Cốc Cốc, Edge, Opera, Samsung) also say "Chrome/", every Chromium
// says "Safari/", iOS says "like Mac OS X" and Android says "Linux". Unknown → "".
//
// ponytail: substring checks over the handful of browsers people actually use in VN; a UA-parser
// dependency is the upgrade path if the list ever needs versions or device models.
func describeUserAgent(ua string) (browser, osName string) {
	has := func(s ...string) bool {
		for _, x := range s {
			if strings.Contains(ua, x) {
				return true
			}
		}
		return false
	}
	switch {
	case has("coc_coc_browser/"):
		browser = "Cốc Cốc"
	case has("Edg/", "EdgA/", "EdgiOS/"):
		browser = "Edge"
	case has("OPR/", "OPT/"):
		browser = "Opera"
	case has("SamsungBrowser/"):
		browser = "Samsung Internet"
	case has("Firefox/", "FxiOS/"):
		browser = "Firefox"
	case has("Chrome/", "CriOS/"):
		browser = "Chrome"
	case has("Safari/") && has("Version/"):
		browser = "Safari"
	}
	switch {
	case has("iPhone", "iPad", "iPod"):
		osName = "iOS"
	case has("Android"):
		osName = "Android"
	case has("CrOS"):
		osName = "ChromeOS"
	case has("Windows"):
		osName = "Windows"
	case has("Macintosh", "Mac OS X"):
		osName = "macOS"
	case has("Linux"):
		osName = "Linux"
	}
	return browser, osName
}

// SessionView is one live login session as GET /auth/sessions (and the admin twin) return it.
type SessionView struct {
	ID         string    `json:"id"` // the session (chain) id — stable across refreshes
	Browser    string    `json:"browser,omitempty"`
	OS         string    `json:"os,omitempty"`
	UserAgent  string    `json:"userAgent,omitempty"`
	IP         string    `json:"ip,omitempty"` // already masked (maskIP)
	LastUsedAt time.Time `json:"lastUsedAt"`   // login or last refresh — within one access TTL of the last request
	ExpiresAt  time.Time `json:"expiresAt"`    // when the chain dies if it is never refreshed again
	Current    bool      `json:"current"`
}

// ListSessions returns the live login sessions of one user of one tenant, most recently used first
// (at most MaxListedSessions). The caller decides WHOSE: the handler passes the verified claim's
// (tenant, user) for GET /auth/sessions, and the admin route passes the path user under the admin's
// verified tenant. The user is loaded tenant-pinned first, so an unknown or other-tenant id is an
// honest USER_NOT_FOUND instead of an empty list that reads like "no devices". currentSessionID is
// the CALLER's `sid` claim ("" when the caller is not that user): the matching row is "phiên này".
func (s *AuthService) ListSessions(ctx context.Context, tenantID, userID, currentSessionID string) ([]SessionView, error) {
	userID = strings.TrimSpace(userID)
	if _, err := s.users.GetUserByID(ctx, tenantID, userID); err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return nil, invalid("USER_NOT_FOUND", "không tìm thấy user")
		}
		return nil, fmt.Errorf("services.AuthService.ListSessions: load user: %w", err)
	}
	rows, err := s.refresh.ListLiveSessions(ctx, tenantID, userID, MaxListedSessions)
	if err != nil {
		return nil, fmt.Errorf("services.AuthService.ListSessions: %w", err)
	}
	out := make([]SessionView, 0, len(rows))
	for _, rt := range rows {
		v := SessionView{
			ID:         rt.SessionID,
			UserAgent:  rt.UserAgent,
			IP:         rt.IP,
			LastUsedAt: rt.CreatedAt,
			ExpiresAt:  rt.ExpiresAt,
		}
		if rt.LastUsedAt != nil {
			v.LastUsedAt = *rt.LastUsedAt
		}
		v.Current = currentSessionID != "" && rt.SessionID == currentSessionID
		v.Browser, v.OS = describeUserAgent(rt.UserAgent)
		out = append(out, v)
	}
	return out, nil
}

// --- ending a session (#45) ---------------------------------------------------------------------

// sessionGuard is the auth stage's "was this session ended?" reader as the auth service sees it
// (services.CachedSessionGuard in production — the SAME instance middleware.AuthMiddlewareLive
// reads): Me asks it, and every end invalidates it, so the ended device is refused on its very next
// request rather than when the 30 s cache backstop expires.
type sessionGuard interface {
	SessionEnded(ctx context.Context, tenantID, userID, sessionID string) (bool, error)
	Invalidate(tenantID, userID, sessionID string)
}

// noopSessionGuard is the default until WithSessionGuard: nothing is ever "ended" for Me, and an
// invalidation has nowhere to go (the repository write still ends the session for refresh).
type noopSessionGuard struct{}

func (noopSessionGuard) SessionEnded(context.Context, string, string, string) (bool, error) {
	return false, nil
}
func (noopSessionGuard) Invalidate(string, string, string) {}

// WithSessionGuard wires the session guard the auth stage reads (main.go passes the one
// CachedSessionGuard it also hands to handlers.WithSessionGuard). nil keeps the noop.
func (s *AuthService) WithSessionGuard(g sessionGuard) *AuthService {
	if g != nil {
		s.sessions = g
	}
	return s
}

// WithPreview opens GET /auth/me to preview-as-role tokens (specs/064 S9-2, main.go passes the
// PreviewService). Without it a preview token on /me is a plain 401.
func (s *AuthService) WithPreview(p previewProfiler) *AuthService {
	s.previews = p
	return s
}

// EndSession ends ONE login session of one user — "đăng xuất thiết bị này" (specs/055 FR-007): its
// live refresh row is revoked with revoke_reason = 'ended', so the device can no longer refresh,
// its access tokens are refused on their next request (the guard is invalidated here), and its
// refresh token replayed later is not mistaken for theft (FR-009). The query is pinned to
// (tenant, user, session): the caller passes the verified claim's tenant + user for the user's own
// route, or the admin's tenant + the path user for the admin route — so another user's session,
// another tenant's, an already-ended one or a made-up id are all one SESSION_NOT_FOUND. A blank id
// is refused here: it must never widen into "every session".
func (s *AuthService) EndSession(ctx context.Context, tenantID, userID, sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.TrimSpace(tenantID) == "" || strings.TrimSpace(userID) == "" {
		return invalid("SESSION_NOT_FOUND", "không tìm thấy phiên đăng nhập")
	}
	ended, err := s.refresh.EndSession(ctx, tenantID, userID, sessionID)
	if err != nil {
		return fmt.Errorf("services.AuthService.EndSession: %w", err)
	}
	if !ended {
		return invalid("SESSION_NOT_FOUND", "không tìm thấy phiên đăng nhập")
	}
	s.sessions.Invalidate(tenantID, userID, sessionID)
	// #311: the one shared place for /admin and /org-admin — ending SOMEONE ELSE's session is a
	// governance act. A user ending their own device (actor == user) or a call with no actor leaves no row.
	if actor := audit.ActorFromContext(ctx); actor != "" && actor != userID {
		s.auditor.Record(ctx, audit.Event{
			TenantID:    tenantID,
			ActorUserID: actor,
			EntityName:  audit.UserMeta,
			RecordID:    userID,
			Action:      audit.ActionUpdate,
			Diff:        &audit.Diff{After: map[string]any{"sessionEnded": sessionID}},
			At:          s.now(),
		})
	}
	return nil
}

// sessionEndedReader is the repository probe the guard caches (auth.RefreshTokenRepository).
type sessionEndedReader interface {
	IsSessionEnded(ctx context.Context, tenantID, userID, sessionID string) (bool, error)
}

// sessionKey is the TENANT- and USER-scoped cache key: a session id is only meaningful for the
// (tenant, user) the verified token names, exactly as the repository query is pinned.
type sessionKey struct {
	tenant  string
	user    string
	session string
}

// DefaultSessionGuardTTL bounds a MISSED invalidation (another replica ended the session: the
// invalidation is in-process) — the same 30 s backstop as the user-row and ACL caches.
const DefaultSessionGuardTTL = DefaultACLTTL

// CachedSessionGuard caches "is this login session ended?" per (tenant, user, session) so the auth
// stage can ask it on EVERY request carrying a `sid` without a query per request
// (middleware.LiveSessionChecker). Both answers are cached: an ended session never comes back, and
// a live one is invalidated the moment AuthService.EndSession / LogoutEverywhere ends it. A load
// error is returned and not cached (the auth stage fails closed).
//
// ponytail: invalidation is process-local like CachedUserLoader; other replicas converge within
// DefaultSessionGuardTTL. Upgrade path = the Tier-2 near-cache bus in cache.go.
type CachedSessionGuard struct {
	inner sessionEndedReader
	c     *cache.TTLCache[sessionKey, bool]
}

// NewCachedSessionGuard wraps the repository probe with a cache expiring after ttl.
func NewCachedSessionGuard(inner sessionEndedReader, ttl time.Duration) *CachedSessionGuard {
	return &CachedSessionGuard{inner: inner, c: cache.New[sessionKey, bool](ttl)}
}

// SessionEnded reports whether the session was ended, loading on a miss (single-flight; the load
// is detached from the first caller's cancellation, like CachedUserLoader.GetUserByID).
func (g *CachedSessionGuard) SessionEnded(ctx context.Context, tenantID, userID, sessionID string) (bool, error) {
	return g.c.GetOrLoad(sessionKey{tenantID, userID, sessionID}, func() (bool, error) {
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), userLoadTimeout)
		defer cancel()
		ended, err := g.inner.IsSessionEnded(lctx, tenantID, userID, sessionID)
		if err != nil {
			return false, fmt.Errorf("services.CachedSessionGuard.SessionEnded: %w", err)
		}
		return ended, nil
	})
}

// Invalidate drops one session's cached answer AFTER it was ended. Nil-safe.
func (g *CachedSessionGuard) Invalidate(tenantID, userID, sessionID string) {
	if g == nil || sessionID == "" {
		return
	}
	g.c.Invalidate(sessionKey{tenantID, userID, sessionID})
}
