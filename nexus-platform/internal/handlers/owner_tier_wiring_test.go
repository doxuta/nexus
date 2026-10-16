package handlers

// owner_tier_wiring_test.go - specs/065-customer-owner-tier lat B: proves the customer-owner boundary is
// WIRED on the production route table, not just designed. No DB, no build tag.
//
//  1. Every /admin/* route (and /audit-log) refuses an owner (IsAdmin=false, flag on) with the
//     byte-identical FORBIDDEN body - a scan over r.Routes(), so a NEW /admin route mounted off the admin
//     gate turns this test red by itself. It pays the "per-group 403 pin" debt of specs/034.
//  2. GET /org-admin/users answers an owner (the handler is reached) and is not swallowed by the
//     dynamic /:entity dispatcher; staff get 403.
//  3. An API key, a preview token and a builder workspace-session token are refused at /org-admin with
//     the same body (explicit table, D-7 of specs/062).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
)

const forbiddenBody = `{"code":"FORBIDDEN","error":"Yêu cầu quyền quản trị viên"}`

// ownerGateRepo answers the gate's live read: u-owner is an active owner, everyone else plain staff.
type ownerGateRepo struct{}

func (ownerGateRepo) GetUserByID(_ context.Context, tenantID, id string) (*auth.User, error) {
	return &auth.User{ID: id, TenantID: tenantID, Status: "active", CanManageUsers: id == "u-owner"}, nil
}

// liveWorkspace resolves any workspace token as live (the resolver's own tests live elsewhere).
type liveWorkspaceResolver struct{}

func (liveWorkspaceResolver) Resolve(context.Context, *auth.Claims) (*auth.ResolvedWorkspace, error) {
	return &auth.ResolvedWorkspace{}, nil
}

func ownerTierEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	owner := auth.Subject{UserID: "u-owner", TenantID: "t1", SessionID: "s1"}
	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		"tok-owner": {Subject: owner, TokenType: auth.TokenTypeAccess},
		"tok-staff": {Subject: auth.Subject{UserID: "u-staff", TenantID: "t1", RoleID: "r1", SessionID: "s2"}, TokenType: auth.TokenTypeAccess},
		"tok-key":   {Subject: owner, TokenType: auth.TokenTypeAPIKey},
		"tok-prev":  {Subject: auth.Subject{UserID: "principal", TenantID: "t1", RoleID: "r1", SessionID: "ps", ActorUserID: "u-owner", ActorTenantID: "t1"}, TokenType: auth.TokenTypePreview},
		"tok-ws": {Subject: auth.Subject{UserID: "u-owner", TenantID: "t1", SessionID: "s3", ActorUserID: "u-owner", ActorTenantID: "home", WorkspaceSessionID: "ws1"},
			TokenType: auth.TokenTypeAccess},
	}}
	pass := func(c *gin.Context) { c.Next() }
	r := gin.New()
	r.Use(gin.Recovery()) // a zero-value handler that is REACHED panics into 500 - the "gate passed" signal
	RegisterAPIRoutes(r.Group("/api/v1"), ver, nil, allHandlers(t), zap.NewNop(),
		WithOperatorTier(pass),
		WithUserManagerGate(middleware.RequireUserManager(ownerGateRepo{}, zap.NewNop())),
		WithWorkspaceResolver(liveWorkspaceResolver{}),
		WithAuthRateLimiter(pass), WithSSORateLimiter(pass), WithAssistantRateLimiter(pass),
	)
	return r
}

func ownerTierDo(r *gin.Engine, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func fillParams(path string) string {
	path = strings.NewReplacer(":entity", "crm_accounts", ":id", "x", ":tenant", "northwind").Replace(path)
	for strings.Contains(path, "/:") {
		i := strings.Index(path, "/:")
		j := strings.Index(path[i+1:], "/")
		if j < 0 {
			path = path[:i] + "/x"
		} else {
			path = path[:i] + "/x" + path[i+1+j:]
		}
	}
	return path
}

func TestOwnerTier_EveryAdminRouteRefusesTheOwner(t *testing.T) {
	r := ownerTierEngine(t)
	scanned := 0
	for _, rt := range r.Routes() {
		rel := strings.TrimPrefix(rt.Path, "/api/v1")
		// /audit-log (list) and /audit-log/export.csv are admin surfaces living outside the /admin prefix;
		// /audit-log/:entity/:recordId is the per-record history every ACL-permitted user reads - not admin.
		if !strings.HasPrefix(rel, "/admin/") && rel != "/audit-log" && rel != "/audit-log/export.csv" {
			continue
		}
		scanned++
		w := ownerTierDo(r, rt.Method, fillParams(rt.Path), "tok-owner")
		if w.Code != http.StatusForbidden || w.Body.String() != forbiddenBody {
			t.Errorf("%s %s: owner got %d %s, want 403 %s", rt.Method, rt.Path, w.Code, w.Body.String(), forbiddenBody)
		}
	}
	t.Logf("scanned %d /admin routes with an owner token", scanned)
	if scanned < 100 {
		t.Fatalf("scanned only %d routes - the router is not the full production table", scanned)
	}
}

func TestOwnerTier_OrgAdminRoute(t *testing.T) {
	r := ownerTierEngine(t)
	// The owner passes the gate and REACHES the (zero-value) handler: Recovery turns its nil deref into
	// 500. 403 would mean the gate refused; 404 would mean the dispatcher swallowed the path.
	if w := ownerTierDo(r, http.MethodGet, "/api/v1/org-admin/users", "tok-owner"); w.Code != http.StatusInternalServerError {
		t.Fatalf("owner GET /org-admin/users = %d %s, want the handler reached (500 via Recovery)", w.Code, w.Body.String())
	}
	// Plain staff: byte-identical 403.
	if w := ownerTierDo(r, http.MethodGet, "/api/v1/org-admin/users", "tok-staff"); w.Code != http.StatusForbidden || w.Body.String() != forbiddenBody {
		t.Fatalf("staff GET /org-admin/users = %d %s", w.Code, w.Body.String())
	}
}

func TestOwnerTier_OrgAdminRefusesNonHumanTokens(t *testing.T) {
	r := ownerTierEngine(t)
	for _, tok := range []string{"tok-key", "tok-prev", "tok-ws"} {
		for _, rt := range []struct{ method, path string }{
			{http.MethodGet, "/api/v1/org-admin/users"},
			{http.MethodPut, "/api/v1/org-admin/users/x"},
			{http.MethodGet, "/api/v1/org-admin/roles"},
		} {
			if tok == "tok-prev" && rt.method != http.MethodGet {
				continue // PreviewReadOnly answers a preview write first (PREVIEW_READ_ONLY) - already pinned by the preview scan
			}
			w := ownerTierDo(r, rt.method, rt.path, tok)
			if w.Code != http.StatusForbidden || w.Body.String() != forbiddenBody {
				t.Errorf("%s %s %s = %d %s, want byte-identical 403", tok, rt.method, rt.path, w.Code, w.Body.String())
			}
		}
	}
}

// Fail closed: without the gate option the owner routes are not mounted at all.
func TestOwnerTier_NoGateNoRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ver := &stubVerifier{byToken: map[string]*auth.Claims{"tok": {Subject: auth.Subject{UserID: "u-owner", TenantID: "t1"}, TokenType: auth.TokenTypeAccess}}}
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), ver, nil, APIHandlers{OrgAdmin: NewOrgAdminHandler(nil, nil)}, zap.NewNop())
	for _, rt := range r.Routes() {
		if strings.Contains(rt.Path, "org-admin") {
			t.Fatalf("%s %s is mounted without its gate", rt.Method, rt.Path)
		}
	}
}
