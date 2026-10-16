package handlers

// router_freeze_test.go — specs/068-tenant-backup-restore T039. The FULL production route table
// (every APIHandlers field set, same trick as router_preview_scan_test.go) with a tenant under a
// write freeze: every write route under `protected` must answer 503 TENANT_MAINTENANCE, the exempt
// list must name routes that really exist, and RequireTenantWritable must sit in the chain exactly
// where plan D-11 says (Auth → RequireTenant → PreviewReadOnly → RequireTenantWritable → … → limiter).

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

type frozenTenants map[string]time.Time

func (f frozenTenants) FrozenUntil(_ context.Context, tenantID string) (time.Time, bool, error) {
	u, ok := f[tenantID]
	return u, ok, nil
}

// stubSessions makes the conditional /admin/users/:id/sessions routes register (main.go always wires them).
type stubSessions struct{}

func (stubSessions) ListSessions(context.Context, string, string, string) ([]services.SessionView, error) {
	return nil, nil
}
func (stubSessions) EndSession(context.Context, string, string, string) error { return nil }

type stubResetIssuer struct{}

func (stubResetIssuer) IssueResetLinkForUser(context.Context, string, string) (*services.ResetLinkIssue, error) {
	return nil, nil
}

func freezeEngine(t *testing.T, frozen frozenTenants, limiterHits *int) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	admin := &auth.Claims{Subject: auth.Subject{UserID: "u-admin", TenantID: "t1", IsAdmin: true}, TokenType: auth.TokenTypeAccess}
	other := &auth.Claims{Subject: auth.Subject{UserID: "u-other", TenantID: "t2", IsAdmin: true}, TokenType: auth.TokenTypeAccess}
	preview := &auth.Claims{Subject: auth.Subject{UserID: "principal", TenantID: "t1", RoleID: "r1", SessionID: "ps",
		ActorUserID: "u-op", ActorTenantID: "t1"}, TokenType: auth.TokenTypePreview}
	ver := &stubVerifier{byToken: map[string]*auth.Claims{"tok-t1": admin, "tok-t2": other, "tok-preview": preview}}
	pass := func(c *gin.Context) { c.Next() }
	r := gin.New()
	r.Use(gin.Recovery())
	hs := allHandlers(t)
	hs.User.WithSessionManager(stubSessions{})
	hs.OrgAdmin.WithSessionManager(stubSessions{})     // lát C: owner session routes register only when wired
	hs.OrgAdmin.WithResetLinkIssuer(stubResetIssuer{}) // …and so does the invite-link route
	RegisterAPIRoutes(r.Group("/api/v1"), ver, nil, hs, zap.NewNop(),
		WithOperatorTier(pass),
		WithUserManagerGate(pass), // specs/065: /org-admin/* is part of the scanned table
		WithLiveSubject(adminReader{}),
		WithSessionGuard(neverEnded{}),
		WithTenantFreeze(frozen),
		WithDataRateLimiter(func(c *gin.Context) { *limiterHits++; c.Next() }),
		WithAuthRateLimiter(pass), WithSSORateLimiter(pass), WithAssistantRateLimiter(pass),
	)
	return r
}

func freezeDo(r *gin.Engine, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func isFrozenBody(w *httptest.ResponseRecorder) bool {
	return w.Code == http.StatusServiceUnavailable && strings.Contains(w.Body.String(), `"code":"TENANT_MAINTENANCE"`)
}

func exemptKeys() map[string]bool {
	m := map[string]bool{}
	for _, e := range middleware.FreezeExemptRoutes() {
		m[e[0]+" /api/v1"+e[1]] = true
	}
	return m
}

// Every write route behind `protected` — sub-group routes (POST /admin/users), the dynamic
// dispatcher (POST /:entity) and anything a later lane adds — is refused while frozen, except the
// closed exempt list. A Use() placed after a sub-group is created turns this red.
func TestRouterFreeze_EveryProtectedWriteIsRefused(t *testing.T) {
	hits := 0
	r := freezeEngine(t, frozenTenants{"t1": time.Now().Add(10 * time.Minute)}, &hits)
	exempt := exemptKeys()
	scanned := 0
	for _, rt := range r.Routes() {
		key := rt.Method + " " + rt.Path
		if rt.Method == http.MethodGet || rt.Method == http.MethodHead || rt.Method == http.MethodOptions ||
			publicWriteRoutes[key] || exempt[key] {
			continue
		}
		w := freezeDo(r, rt.Method, fillPathParams(rt.Path), "tok-t1")
		scanned++
		if !isFrozenBody(w) {
			t.Errorf("%s: status %d body %s — want 503 TENANT_MAINTENANCE", key, w.Code, w.Body.String())
		}
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d write routes — the router is not the full production table", scanned)
	}
	if hits != 0 {
		t.Errorf("limiter ran %d times for refused writes; the freeze must stop them first", hits)
	}
}

// freezeExempt names routes that exist (a rename must turn this red, not empty the list), and an
// exempt route is not stopped by the freeze.
func TestRouterFreeze_ExemptRoutesAreRegistered(t *testing.T) {
	hits := 0
	r := freezeEngine(t, frozenTenants{"t1": time.Now().Add(10 * time.Minute)}, &hits)
	registered := map[string]bool{}
	for _, rt := range r.Routes() {
		registered[rt.Method+" "+rt.Path] = true
	}
	for key := range exemptKeys() {
		if !registered[key] {
			t.Errorf("freezeExempt names %q, which is not a registered route", key)
			continue
		}
		parts := strings.SplitN(key, " ", 2)
		if w := freezeDo(r, parts[0], fillPathParams(parts[1]), "tok-t1"); isFrozenBody(w) {
			t.Errorf("%s is exempt but was refused: %s", key, w.Body.String())
		}
	}
}

func TestRouterFreeze_ReadsOtherTenantsAndChainOrder(t *testing.T) {
	hits := 0
	r := freezeEngine(t, frozenTenants{"t1": time.Now().Add(10 * time.Minute)}, &hits)

	// Reads of the frozen tenant pass the freeze (and reach the limiter after it).
	if w := freezeDo(r, http.MethodGet, "/api/v1/admin/users", "tok-t1"); isFrozenBody(w) {
		t.Errorf("GET /admin/users must not be frozen: %d", w.Code)
	}
	if hits != 1 {
		t.Errorf("limiter hits %d, want 1 (the GET)", hits)
	}
	// Another tenant writes freely.
	if w := freezeDo(r, http.MethodPost, "/api/v1/crm_accounts", "tok-t2"); isFrozenBody(w) {
		t.Errorf("tenant t2 must not be frozen: %d %s", w.Code, w.Body.String())
	}
	// No token: the Auth stage answers first (401), not the freeze.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/crm_accounts", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d, want 401 from Auth", w.Code)
	}
	// Preview token on a frozen tenant: PreviewReadOnly (earlier in the chain) answers 403, not 503.
	pw := freezeDo(r, http.MethodPost, "/api/v1/crm_accounts", "tok-preview")
	if pw.Code != http.StatusForbidden || !strings.Contains(pw.Body.String(), "PREVIEW_READ_ONLY") {
		t.Errorf("preview write on frozen tenant: %d %s, want 403 PREVIEW_READ_ONLY before the freeze", pw.Code, pw.Body.String())
	}
}
