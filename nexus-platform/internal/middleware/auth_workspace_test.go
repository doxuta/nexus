package middleware

// auth_workspace_test.go — specs/062-customer-workspace T008. The delegation branch of the Auth
// stage: a typ=access token carrying act/att/wsid (a builder's workspace-session token) is
// re-resolved on EVERY request through a WorkspaceResolver, fail-closed; the Subject the request
// runs under is rebuilt from that resolution, never from the token's own claims.

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"nexus/platform/internal/auth"
)

type fakeResolver struct {
	res   *auth.ResolvedWorkspace
	err   error
	calls int
	got   *auth.Claims
}

func (f *fakeResolver) Resolve(_ context.Context, claims *auth.Claims) (*auth.ResolvedWorkspace, error) {
	f.calls++
	f.got = claims
	return f.res, f.err
}

func wsClaims() *auth.Claims {
	return &auth.Claims{
		Subject: auth.Subject{
			UserID: "builder-1", TenantID: "customer-1", IsAdmin: true, SessionID: "login-sid-1",
			ActorUserID: "builder-1", ActorTenantID: "home-1", WorkspaceSessionID: "ws-1",
		},
		TokenType: auth.TokenTypeAccess,
	}
}

func liveResolution() *auth.ResolvedWorkspace {
	return &auth.ResolvedWorkspace{
		Session: auth.WorkspaceSessionLive{ID: "ws-1", TenantID: "customer-1", TenantSlug: "khach", TenantName: "Khách"},
		Actor:   auth.LiveActor{ID: "builder-1"},
	}
}

func TestAuthMiddleware_WorkspaceSessionToken(t *testing.T) {
	mutate := func(f func(c *auth.Claims)) *auth.Claims { c := wsClaims(); f(c); return c }
	cases := []struct {
		name         string
		claims       *auth.Claims
		resolver     *fakeResolver // nil pointer ⇒ no resolver wired (harness)
		wantStatus   int           // 0 ⇒ passes through
		wantResolver int
		wantCode     string
	}{
		{"valid session passes", wsClaims(), &fakeResolver{res: liveResolution()}, 0, 1, ""},
		{"no resolver wired (harness) fails closed", wsClaims(), nil, http.StatusUnauthorized, 0, "UNAUTHORIZED"},
		{"missing wsid", mutate(func(c *auth.Claims) { c.WorkspaceSessionID = "" }), &fakeResolver{res: liveResolution()}, http.StatusUnauthorized, 0, "UNAUTHORIZED"},
		{"missing act", mutate(func(c *auth.Claims) { c.ActorUserID = "" }), &fakeResolver{res: liveResolution()}, http.StatusUnauthorized, 0, "UNAUTHORIZED"},
		{"missing att", mutate(func(c *auth.Claims) { c.ActorTenantID = "" }), &fakeResolver{res: liveResolution()}, http.StatusUnauthorized, 0, "UNAUTHORIZED"},
		{"missing sid", mutate(func(c *auth.Claims) { c.SessionID = "" }), &fakeResolver{res: liveResolution()}, http.StatusUnauthorized, 0, "UNAUTHORIZED"},
		{"act != uid", mutate(func(c *auth.Claims) { c.UserID = "someone-else" }), &fakeResolver{res: liveResolution()}, http.StatusUnauthorized, 0, "UNAUTHORIZED"},
		{"att == tid (not a delegation)", mutate(func(c *auth.Claims) { c.ActorTenantID = c.TenantID }), &fakeResolver{res: liveResolution()}, http.StatusUnauthorized, 0, "UNAUTHORIZED"},
		{"resolver: no live row", wsClaims(), &fakeResolver{err: auth.ErrWorkspaceSessionNotLive}, http.StatusUnauthorized, 1, "UNAUTHORIZED"},
		{"resolver: builder not live", wsClaims(), &fakeResolver{err: auth.ErrActorNotLive}, http.StatusUnauthorized, 1, "UNAUTHORIZED"},
		{"resolver: DB error fails closed 500", wsClaims(), &fakeResolver{err: errors.New("db down")}, http.StatusInternalServerError, 1, "INTERNAL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var opts []AuthOption
			if tc.resolver != nil {
				opts = append(opts, WithWorkspaceResolver(tc.resolver))
			}
			c, rec := newRequest("Bearer ws.token")
			AuthMiddleware(&fakeVerifier{claims: tc.claims}, nil, opts...)(c)
			if tc.resolver != nil && tc.resolver.calls != tc.wantResolver {
				t.Errorf("resolver calls = %d, want %d", tc.resolver.calls, tc.wantResolver)
			}
			if tc.wantStatus != 0 {
				if !c.IsAborted() || rec.Code != tc.wantStatus {
					t.Fatalf("aborted=%v status=%d, want abort %d", c.IsAborted(), rec.Code, tc.wantStatus)
				}
				if got := rec.Body.String(); !contains(got, `"code":"`+tc.wantCode+`"`) {
					t.Errorf("body %s lacks code %s", got, tc.wantCode)
				}
				return
			}
			if c.IsAborted() {
				t.Fatalf("valid session refused: %d %s", rec.Code, rec.Body.String())
			}
			sub, _ := SubjectFromContext(c)
			if sub.TenantID != "customer-1" || !sub.IsAdmin || sub.RoleID != "" || !sub.IsPlatformOperator ||
				sub.UserID != "builder-1" || sub.SessionID != "login-sid-1" || sub.ActorUserID != "builder-1" ||
				sub.ActorTenantID != "home-1" || sub.WorkspaceSessionID != "ws-1" {
				t.Errorf("rebuilt subject wrong: %+v", sub)
			}
			if tid, _ := TenantFromContext(c); tid != "customer-1" {
				t.Errorf("tenant in context = %q, want the customer", tid)
			}
		})
	}
}

// The delegation branch is the whole authority: it never consults the home-tenant user reader or
// the home session guard (they would look for the builder in the CUSTOMER tenant), and it does not
// trust the token's own adm claim — IsAdmin / IsPlatformOperator come from the resolution.
func TestAuthMiddlewareLive_WorkspaceSessionSkipsLiveReaders(t *testing.T) {
	users := &adminForEveryone{}
	sessions := &fakeSessions{ended: true} // would 401 if it were consulted
	resolver := &fakeResolver{res: liveResolution()}
	claims := wsClaims()
	claims.IsAdmin = false // a token claim must not decide privilege
	c, rec := newRequest("Bearer ws.token")
	AuthMiddlewareLive(&fakeVerifier{claims: claims}, users, sessions, nil, WithWorkspaceResolver(resolver))(c)
	if c.IsAborted() {
		t.Fatalf("refused: %d %s", rec.Code, rec.Body.String())
	}
	if users.calls != 0 || len(sessions.calls) != 0 {
		t.Errorf("live readers consulted: users=%d sessions=%v", users.calls, sessions.calls)
	}
	if sub, _ := SubjectFromContext(c); !sub.IsAdmin || !sub.IsPlatformOperator {
		t.Errorf("subject not rebuilt from the resolution: %+v", sub)
	}
}

// A token without delegation claims, and claims of another type, keep the old path byte for byte:
// the resolver is never called.
func TestAuthMiddleware_NonDelegationTokensDoNotTouchResolver(t *testing.T) {
	apiKey := wsClaims()
	apiKey.TokenType = auth.TokenTypeAPIKey
	preview := previewClaims()
	preview.ActorTenantID = "home-1" // act/att present, uid != act: still not this branch
	normal := &auth.Claims{Subject: auth.Subject{UserID: "u1", TenantID: "t1", IsAdmin: true}, TokenType: auth.TokenTypeAccess}
	for name, claims := range map[string]*auth.Claims{"api key": apiKey, "preview": preview, "normal access": normal} {
		t.Run(name, func(t *testing.T) {
			resolver := &fakeResolver{err: auth.ErrWorkspaceSessionNotLive} // would 401 if consulted
			c, rec := newRequest("Bearer x.y.z")
			AuthMiddleware(&fakeVerifier{claims: claims}, nil, WithWorkspaceResolver(resolver))(c)
			if c.IsAborted() {
				t.Fatalf("refused: %d %s", rec.Code, rec.Body.String())
			}
			if resolver.calls != 0 {
				t.Errorf("resolver called %d time(s) for a %s token", resolver.calls, name)
			}
		})
	}
}
