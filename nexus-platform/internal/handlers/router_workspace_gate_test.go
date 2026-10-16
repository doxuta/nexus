package handlers

// specs/062-customer-workspace T014 — the gates around the workspace-session routes, through the REAL
// router with the REAL tokens and Auth stage (resolver and services faked):
//
//   - who may open / close a session: only a home builder (admin + platform operator); a customer-side
//     admin or owner holding a plain token gets the SAME 403 as any builder route (never a 404 that
//     reveals the route exists);
//   - what a session token can never reach: the operator-only cluster (/admin/tenants*, retire and
//     purge of a workspace), the self-management routes of the home account, a nested open;
//   - the order Auth → handler of the close route: once the session is closed, the SAME token is 401
//     at the Auth stage and the handler is not called — the route is never exempt from the resolver.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

const gateForbiddenBody = `{"code":"FORBIDDEN","error":"Yêu cầu quyền quản trị viên"}`

// gateUsers answers the live user read per user id (the home tier gate + AuthMiddlewareLive).
type gateUsers struct{ byID map[string]*auth.User }

func (g gateUsers) GetUserByID(_ context.Context, tenantID, id string) (*auth.User, error) {
	if u, ok := g.byID[id]; ok {
		cp := *u
		cp.TenantID = tenantID
		return &cp, nil
	}
	return nil, auth.ErrUserNotFound
}

// statefulResolver answers "live" until markClosed is called, then "no row" — like GetLive after
// the row's closed_at is stamped.
type statefulResolver struct{ closed bool }

func (s *statefulResolver) Resolve(_ context.Context, _ *auth.Claims) (*auth.ResolvedWorkspace, error) {
	if s.closed {
		return nil, auth.ErrWorkspaceSessionNotLive
	}
	return &auth.ResolvedWorkspace{}, nil
}

type gateProvisioner struct{ calls int }

func (p *gateProvisioner) Provision(context.Context, services.ProvisionTenantInput) (*services.ProvisionedTenant, error) {
	p.calls++
	return &services.ProvisionedTenant{}, nil
}
func (p *gateProvisioner) ListTenants(context.Context) ([]services.TenantListItem, error) {
	p.calls++
	return nil, nil
}
func (p *gateProvisioner) RetireTenant(context.Context, string, string) error {
	p.calls++
	return nil
}
func (p *gateProvisioner) SetOwnerTier(context.Context, string, string, bool) (*services.OwnerTierResult, error) {
	p.calls++
	return &services.OwnerTierResult{}, nil
}
func (p *gateProvisioner) PurgeTenant(context.Context, string, string, string, string, bool) (*services.PurgeReport, error) {
	p.calls++
	return &services.PurgeReport{}, nil
}

type gateFixture struct {
	r        *gin.Engine
	tokens   *auth.TokenService
	resolver *statefulResolver
	ws       *fakeWorkspaceSvc
	prov     *gateProvisioner
}

func newGateFixture(t *testing.T) *gateFixture {
	t.Helper()
	tokens, err := auth.NewTokenService("a-sufficiently-long-real-secret-value-1234567890", 15*time.Minute, time.Hour)
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	users := gateUsers{byID: map[string]*auth.User{
		"builder":     {ID: "builder", Status: "active", IsAdmin: true, IsPlatformOperator: true},
		"legacyadmin": {ID: "legacyadmin", Status: "active", IsAdmin: true},
		"owner":       {ID: "owner", Status: "active", IsAdmin: false},
	}}
	f := &gateFixture{
		tokens: tokens, resolver: &statefulResolver{}, prov: &gateProvisioner{},
		ws: &fakeWorkspaceSvc{openRes: &services.WorkspaceOpenResult{Created: true}},
	}
	gin.SetMode(gin.TestMode)
	f.r = gin.New()
	RegisterAPIRoutes(f.r.Group("/api/v1"), tokens, nil, APIHandlers{
		Auth:             NewAuthHandler(services.NewAuthService(nil, nil, nil), zap.NewNop()),
		Tenant:           NewAdminTenantHandler(f.prov, users, zap.NewNop()),
		WorkspaceSession: NewAdminWorkspaceSessionHandler(f.ws, zap.NewNop()),
	}, zap.NewNop(),
		WithOperatorTier(middleware.RequireBuilderTier(users, zap.NewNop())),
		WithLiveSubject(users),
		WithWorkspaceResolver(f.resolver),
	)
	return f
}

func (f *gateFixture) homeToken(t *testing.T, uid string, admin bool) string {
	t.Helper()
	tok, err := f.tokens.IssueAccessToken(auth.Subject{UserID: uid, TenantID: "home-1", IsAdmin: admin, SessionID: "sid"})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return tok
}

func (f *gateFixture) sessionToken(t *testing.T) string {
	t.Helper()
	tok, err := f.tokens.IssueAccessTokenUntil(auth.Subject{
		UserID: "builder", TenantID: "customer-1", IsAdmin: true, SessionID: "sid",
		ActorUserID: "builder", ActorTenantID: "home-1", WorkspaceSessionID: "ws-1",
	}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	return tok
}

func (f *gateFixture) do(method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(`{"tenantSlug":"khach"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, req)
	return w
}

func TestWorkspaceGate_WhoMayOpenAndClose(t *testing.T) {
	f := newGateFixture(t)
	const open = "/api/v1/admin/workspace-sessions"
	const closeRoute = "/api/v1/admin/workspace-sessions/current"
	cases := []struct {
		name       string
		method     string
		path       string
		token      func() string
		want       int
		wantBody   string // exact body; "" ⇒ not checked
		wantCalled bool   // the workspace service was reached
	}{
		{"home builder opens", http.MethodPost, open, func() string { return f.homeToken(t, "builder", true) }, http.StatusCreated, "", true},
		{"legacy customer admin (no operator flag) opens: 403 byte-identical", http.MethodPost, open, func() string { return f.homeToken(t, "legacyadmin", true) }, http.StatusForbidden, gateForbiddenBody, false},
		{"customer owner (non-admin) opens: 403 byte-identical", http.MethodPost, open, func() string { return f.homeToken(t, "owner", false) }, http.StatusForbidden, gateForbiddenBody, false},
		{"a session token cannot open another (no nested open)", http.MethodPost, open, func() string { return f.sessionToken(t) }, http.StatusForbidden,
			`{"code":"WORKSPACE_SESSION_NOT_ALLOWED","error":"Không thực hiện được khi đang dựng trong không gian của khách — hãy về Nexus để làm việc này"}`, false},
		{"session token closes: 204", http.MethodDelete, closeRoute, func() string { return f.sessionToken(t) }, http.StatusNoContent, "", true},
		{"legacy customer admin closes: 403 byte-identical (not a 404 that reveals the route)", http.MethodDelete, closeRoute, func() string { return f.homeToken(t, "legacyadmin", true) }, http.StatusForbidden, gateForbiddenBody, false},
		{"customer owner closes: 403 byte-identical", http.MethodDelete, closeRoute, func() string { return f.homeToken(t, "owner", false) }, http.StatusForbidden, gateForbiddenBody, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.ws.openCalls, f.ws.closeSubs = 0, nil
			w := f.do(tc.method, tc.path, tc.token())
			if w.Code != tc.want {
				t.Fatalf("status = %d (%s), want %d", w.Code, w.Body.String(), tc.want)
			}
			if tc.wantBody != "" && w.Body.String() != tc.wantBody {
				t.Errorf("body = %s, want %s", w.Body.String(), tc.wantBody)
			}
			if called := f.ws.openCalls+len(f.ws.closeSubs) > 0; called != tc.wantCalled {
				t.Errorf("service reached = %v, want %v", called, tc.wantCalled)
			}
		})
	}
}

// A session token never reaches the operator-only cluster nor the home account's self-management;
// a home builder still reaches /admin/tenants.
func TestWorkspaceGate_SessionTokenCannotReachWhatBelongsToHome(t *testing.T) {
	f := newGateFixture(t)
	const notAllowed = `{"code":"WORKSPACE_SESSION_NOT_ALLOWED","error":"Không thực hiện được khi đang dựng trong không gian của khách — hãy về Nexus để làm việc này"}`
	cases := []struct {
		method, path string
		wantBody     string
	}{
		// Operator-only: byte-identical 403, handler never called.
		{http.MethodGet, "/api/v1/admin/tenants", gateForbiddenBody},
		{http.MethodPost, "/api/v1/admin/tenants", gateForbiddenBody},
		{http.MethodDelete, "/api/v1/admin/tenants/customer-1", gateForbiddenBody},
		{http.MethodPost, "/api/v1/admin/tenants/customer-1/purge", gateForbiddenBody},
		// The home account's own management: WORKSPACE_SESSION_NOT_ALLOWED.
		{http.MethodPut, "/api/v1/auth/me/password", notAllowed},
		{http.MethodPost, "/api/v1/auth/logout-all", notAllowed},
		{http.MethodGet, "/api/v1/auth/sessions", notAllowed},
		{http.MethodDelete, "/api/v1/auth/sessions/some-id", notAllowed},
		{http.MethodPost, "/api/v1/auth/mfa/enroll", notAllowed},
		{http.MethodPost, "/api/v1/auth/mfa/activate", notAllowed},
		{http.MethodPost, "/api/v1/auth/mfa/disable", notAllowed},
	}
	tok := f.sessionToken(t)
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := f.do(tc.method, tc.path, tok)
			if w.Code != http.StatusForbidden || w.Body.String() != tc.wantBody {
				t.Fatalf("%d %s, want 403 %s", w.Code, w.Body.String(), tc.wantBody)
			}
		})
	}
	if f.prov.calls != 0 {
		t.Errorf("the tenant provisioner was reached %d time(s) from a session token", f.prov.calls)
	}
	// The same cluster stays open to the home builder (the gate only narrowed the session token).
	if w := f.do(http.MethodGet, "/api/v1/admin/tenants", f.homeToken(t, "builder", true)); w.Code != http.StatusOK {
		t.Errorf("home builder GET /admin/tenants = %d, want 200", w.Code)
	}
}

// Order Auth → handler of the close route: after the close, the same token is 401 at the Auth stage.
func TestWorkspaceGate_CloseRouteIsNeverExemptFromTheResolver(t *testing.T) {
	f := newGateFixture(t)
	const closeRoute = "/api/v1/admin/workspace-sessions/current"
	tok := f.sessionToken(t)

	if w := f.do(http.MethodDelete, closeRoute, tok); w.Code != http.StatusNoContent {
		t.Fatalf("first close = %d, want 204", w.Code)
	}
	f.resolver.closed = true // the row is closed now
	f.ws.closeSubs = nil
	w := f.do(http.MethodDelete, closeRoute, tok)
	if w.Code != http.StatusUnauthorized || w.Body.String() != `{"code":"UNAUTHORIZED","error":"Yêu cầu xác thực không hợp lệ"}` {
		t.Fatalf("second close with the same token = %d %s, want the uniform 401", w.Code, w.Body.String())
	}
	if len(f.ws.closeSubs) != 0 {
		t.Error("the handler ran for a token whose session is closed — the close route must not be exempt from the resolver")
	}
}
