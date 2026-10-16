package handlers

// router_preview_scan_test.go — specs/064-preview-as-role-sample-data T014 (plan "Bảo mật · Chỉ
// đọc"). Builds the FULL production route table and sends every non-GET route a preview token:
// each must be refused by PreviewReadOnly (403 PREVIEW_READ_ONLY, or PREVIEW_FORBIDDEN on /auth/*
// and /assistant/*) before any gate or handler runs — except the single exit route and the public
// routes listed below, which never treat a Bearer token as identity.
//
// The route table is only complete if EVERY APIHandlers field is set: RegisterAPIRoutes skips nil
// handlers, so a partly-filled struct would scan a shrunken router and pass falsely. The fields
// are filled by reflection (zero-value handlers — a refused write never reaches their body) and the
// test fails if any stays nil, so a handler added later is in scope automatically.

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
)

// publicWriteRoutes are the non-GET routes mounted OUTSIDE the protected chain. None reads the
// Bearer header as identity: login/forgot/reset/mfa-verify take credentials or a pending token
// from the body, refresh/logout verify a REFRESH token from the body (VerifyRefreshToken refuses
// typ=preview — auth.TestVerifyAccessAndRefresh_RejectPreviewToken), and inbound hooks are
// HMAC-signed.
var publicWriteRoutes = map[string]bool{
	"POST /api/v1/auth/login":           true,
	"POST /api/v1/auth/refresh":         true,
	"POST /api/v1/auth/logout":          true,
	"POST /api/v1/auth/forgot-password": true,
	"POST /api/v1/auth/reset-password":  true,
	"POST /api/v1/auth/mfa/verify":      true,
	"POST /api/v1/auth/sso/exchange":    true, // one-time SSO handoff code in the body
	"POST /api/v1/hooks/:tenant/:hook":  true,
}

// publicGetRoutes are the GET routes mounted OUTSIDE the protected chain (checked against the live
// route table by TestPreviewRouteScan_PublicRoutesIgnorePreviewToken: an unauthenticated GET that
// is not 401 must be listed here). GET /auth/me is the only one that reads a Bearer header; it
// self-verifies with VerifyAccessToken, which refuses typ=preview. S9-2 (T044) opens it to preview
// tokens on purpose — update this entry and its assertion then.
var publicGetRoutes = map[string]bool{
	"GET /api/v1/auth/me":                    true,
	"GET /api/v1/auth/branding/:tenant":      true,
	"GET /api/v1/auth/sso/:tenant/start":     true,
	"GET /api/v1/auth/sso/:tenant/providers": true,
	"GET /api/v1/auth/sso/callback":          true,
}

// adminReader answers "active admin" for any uid — if a preview token ever reached refreshSubject
// the scan would see it pass the writes it must not.
type adminReader struct{}

func (adminReader) GetUserByID(_ context.Context, tenantID, id string) (*auth.User, error) {
	return &auth.User{ID: id, TenantID: tenantID, Status: "active", IsAdmin: true, IsPlatformOperator: true}, nil
}

// neverEnded is a session guard that never refuses.
type neverEnded struct{}

func (neverEnded) SessionEnded(context.Context, string, string, string) (bool, error) {
	return false, nil
}

// previewExitRoute is the one write a preview token may make.
const previewExitRoute = "DELETE /api/v1/preview-sessions/current"

// allHandlers returns an APIHandlers with every pointer field set to a zero-value handler.
func allHandlers(t *testing.T) APIHandlers {
	t.Helper()
	var h APIHandlers
	v := reflect.ValueOf(&h).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if f.Kind() != reflect.Ptr {
			t.Fatalf("APIHandlers.%s is not a pointer; extend the scan", v.Type().Field(i).Name)
		}
		f.Set(reflect.New(f.Type().Elem()))
	}
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsNil() {
			t.Fatalf("APIHandlers.%s is nil — the scan would miss its routes", v.Type().Field(i).Name)
		}
	}
	return h
}

// previewScanEngine wires the production route table with the production option set, a verifier
// that knows one preview token, and a data limiter that records whether it ever ran.
func previewScanEngine(t *testing.T, limiterHits *int) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	preview := &auth.Claims{Subject: auth.Subject{UserID: "principal", TenantID: "t1", RoleID: "r1", SessionID: "ps",
		ActorUserID: "u-op", ActorTenantID: "t1"}, TokenType: auth.TokenTypePreview}
	ver := &stubVerifier{byToken: map[string]*auth.Claims{"tok-preview": preview}}
	pass := func(c *gin.Context) { c.Next() }
	r := gin.New()
	r.Use(gin.Recovery())
	RegisterAPIRoutes(r.Group("/api/v1"), ver, nil, allHandlers(t), zap.NewNop(),
		WithOperatorTier(pass),
		WithUserManagerGate(pass), // specs/065: /org-admin/* is part of the scanned table
		WithLiveSubject(adminReader{}),
		WithSessionGuard(neverEnded{}),
		WithDataRateLimiter(func(c *gin.Context) { *limiterHits++; c.Next() }),
		WithAuthRateLimiter(pass),
		WithSSORateLimiter(pass),
		WithAssistantRateLimiter(pass),
	)
	return r
}

func TestPreviewRouteScan_EveryWriteIsRefused(t *testing.T) {
	hits := 0
	r := previewScanEngine(t, &hits)

	scanned := 0
	for _, rt := range r.Routes() {
		key := rt.Method + " " + rt.Path
		if rt.Method == http.MethodGet || publicWriteRoutes[key] {
			continue
		}
		path := strings.NewReplacer(":entity", "crm_accounts", ":id", "x", ":tenant", "northwind").Replace(rt.Path)
		for strings.Contains(path, "/:") { // any other param segment
			i := strings.Index(path, "/:")
			j := strings.Index(path[i+1:], "/")
			if j < 0 {
				path = path[:i] + "/x"
			} else {
				path = path[:i] + "/x" + path[i+1+j:]
			}
		}
		req := httptest.NewRequest(rt.Method, path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer tok-preview")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		rel := strings.TrimPrefix(rt.Path, "/api/v1")
		want := "PREVIEW_READ_ONLY"
		if strings.HasPrefix(rel, "/auth/") || strings.HasPrefix(rel, "/assistant/") {
			want = "PREVIEW_FORBIDDEN"
		}
		if key == previewExitRoute {
			if w.Code == http.StatusForbidden {
				t.Errorf("%s: the exit route was refused (%s)", key, w.Body.String())
			}
			continue
		}
		scanned++
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"code":"`+want+`"`) {
			t.Errorf("%s (%s): status %d body %s — want 403 %s", key, path, w.Code, w.Body.String(), want)
		}
	}
	t.Logf("scanned %d write routes with a preview token", scanned)
	if scanned < 100 {
		t.Fatalf("scanned only %d write routes — the router is not the full production table", scanned)
	}
	// Only the exit route (which PreviewReadOnly lets through) may reach the limiter.
	if hits != 1 {
		t.Errorf("the data rate limiter ran %d time(s); want 1 (the exit route) — refused preview writes must stop at PreviewReadOnly, before it", hits)
	}
}

// Order of the protected chain (plan A5): Auth → RequireTenant → PreviewReadOnly → rate limiter →
// sub-groups (RequireAdmin, operator tier). specs/068 (RequireTenantWritable) and specs/063
// (DraftCaller) add their line between PreviewReadOnly and the limiter when they land.
func TestPreviewRouteOrder_ProtectedChain(t *testing.T) {
	hits := 0
	r := previewScanEngine(t, &hits)
	do := func(method, path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	// No token: Auth refuses before PreviewReadOnly (401, not 403).
	if w := do(http.MethodPost, "/api/v1/crm_accounts", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d, want 401 from the Auth stage first", w.Code)
	}
	// Preview on an admin builder route: PreviewReadOnly answers before RequireAdmin (which would
	// say FORBIDDEN) and before the limiter.
	w := do(http.MethodPost, "/api/v1/admin/preview-sessions", "tok-preview")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "PREVIEW_READ_ONLY") {
		t.Errorf("preview POST /admin/preview-sessions: %d %s, want 403 PREVIEW_READ_ONLY (before RequireAdmin)", w.Code, w.Body.String())
	}
	// Preview read of an admin route: passes PreviewReadOnly, then RequireAdmin refuses (IsAdmin=false).
	w = do(http.MethodGet, "/api/v1/admin/users", "tok-preview")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"code":"FORBIDDEN"`) {
		t.Errorf("preview GET /admin/users: %d %s, want 403 FORBIDDEN from RequireAdmin", w.Code, w.Body.String())
	}
	if hits != 1 {
		t.Errorf("limiter hits = %d, want 1 (only the GET that passed PreviewReadOnly)", hits)
	}
}

// fillPathParams swaps every ":param" segment for a literal so a route can be requested.
func fillPathParams(path string) string {
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

// The public routes never treat a preview token as an identity. Two halves: (1) any GET that
// answers without a token must be a known public route (a new unauthenticated GET that might read
// Bearer cannot slip in unreviewed); (2) every public route — GET and write — given a preview token
// must not succeed on the strength of the header (the token is no identity).
func TestPreviewRouteScan_PublicRoutesIgnorePreviewToken(t *testing.T) {
	hits := 0
	r := previewScanEngine(t, &hits)
	do := func(method, path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	checked := 0
	for _, rt := range r.Routes() {
		key := rt.Method + " " + rt.Path
		path := fillPathParams(rt.Path)
		if rt.Method == http.MethodGet && !publicGetRoutes[key] {
			if w := do(rt.Method, path, ""); w.Code != http.StatusUnauthorized {
				t.Errorf("%s answered %d without a token but is not in publicGetRoutes — review whether it reads a Bearer token, then list it", key, w.Code)
			}
			continue
		}
		if rt.Method != http.MethodGet && !publicWriteRoutes[key] {
			continue
		}
		checked++
		// The preview token must never turn a refusal into a success. (Zero-value handlers may
		// answer 500 differently once a header is present, so only a 2xx that the bare call did not
		// get proves the token was taken as an identity.)
		bare, withTok := do(rt.Method, path, ""), do(rt.Method, path, "tok-preview")
		if withTok.Code >= 200 && withTok.Code < 300 && bare.Code != withTok.Code {
			t.Errorf("%s (%s): %d without a token but %d with a preview token — it treats the token as an identity", key, path, bare.Code, withTok.Code)
		}
	}
	if want := len(publicGetRoutes) + len(publicWriteRoutes); checked != want {
		t.Errorf("exercised %d public routes, want %d — a listed public route is missing from the router (stale allowlist)", checked, want)
	}
}

// routesOut: `go test ./internal/handlers -run TestPreviewRouteScan_GetRoutes -args -routes-out=<file>`
// writes every GET route of the production route table, one "<protected|public> <path>" line each, for
// scripts/acceptance-preview-role.py --scan-get. The list comes from the same table the write scan
// walks, so a GET route added later is scanned on a live server with no second list to forget (and no
// debug endpoint, which would itself be an orphan route).
var routesOut = flag.String("routes-out", "", "write the GET route list for acceptance-preview-role.py --scan-get")

func TestPreviewRouteScan_GetRoutes(t *testing.T) {
	hits := 0
	r := previewScanEngine(t, &hits)
	var lines []string
	protected, public := 0, 0
	for _, rt := range r.Routes() {
		if rt.Method != http.MethodGet {
			continue
		}
		kind := "protected"
		if publicGetRoutes["GET "+rt.Path] {
			kind = "public"
			public++
		} else {
			protected++
		}
		lines = append(lines, kind+" "+rt.Path)
	}
	sort.Strings(lines)
	if public != len(publicGetRoutes) {
		t.Errorf("listed %d public GET routes, want %d — a listed public route is missing from the router", public, len(publicGetRoutes))
	}
	if protected < 50 {
		t.Fatalf("only %d protected GET routes — the router is not the full production table", protected)
	}
	if *routesOut == "" {
		return
	}
	if err := os.WriteFile(*routesOut, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", *routesOut, err)
	}
	t.Logf("wrote %d GET routes (%d protected) to %s", len(lines), protected, *routesOut)
}
