package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// fakeSSOSvc is a minimal, controllable stand-in for the ssoService interface — this file's tests
// need Callback's error-classification behavior plus the SSO-STATE-COOKIE plumbing (what nonce
// /start hands the handler, what browserNonce the handler forwards to HandleCallback), so the
// remaining methods are unused filler.
type fakeSSOSvc struct {
	callbackErr       error
	callbackOut       *services.LoginOutput
	startURL          string
	startNonce        string
	startErr          error
	lastBrowserNonce  string
	callbackWasCalled bool

	// UpdateProviderPolicy capture/config (SSO-MFA-POLICY PATCH tests).
	updateOut         *auth.SSOProvider
	updateErr         error
	updateWasCalled   bool
	lastUpdateTenant  string
	lastUpdateID      string
	lastUpdateEnabled *bool
	lastUpdateTrust   *bool

	// Actor threaded into the service context by the handler (SSO-ADMIN-AUDIT) — the last value
	// audit.ActorFromContext saw on a mutation call, so a handler test can prove auditContext(c)
	// wiring instead of only proving the service reads the actor.
	lastActor string

	// StartHandoff / ExchangeHandoff capture/config (SSO-FE-REDIRECT redirect-mode tests).
	startHandoffCode      string
	startHandoffErr       error
	startHandoffWasCalled bool
	exchangeOut           *services.LoginOutput
	exchangeErr           error
	lastExchangeCode      string

	// PublicProviders capture/config (SSO-PROVIDER-PUBLIC-LIST tests).
	publicProviders  []services.PublicSSOProvider
	publicErr        error
	lastPublicTenant string
}

func (f *fakeSSOSvc) PublicProviders(_ context.Context, tenantSlug string) ([]services.PublicSSOProvider, error) {
	f.lastPublicTenant = tenantSlug
	if f.publicErr != nil {
		return nil, f.publicErr
	}
	if f.publicProviders == nil {
		return []services.PublicSSOProvider{}, nil
	}
	return f.publicProviders, nil
}

func (f *fakeSSOSvc) CreateProvider(ctx context.Context, _ string, _ services.CreateSSOProviderInput) (*auth.SSOProvider, error) {
	f.lastActor = audit.ActorFromContext(ctx)
	return &auth.SSOProvider{ID: "p-new"}, nil
}
func (f *fakeSSOSvc) ListProviders(context.Context, string) ([]*auth.SSOProvider, error) {
	return nil, nil
}
func (f *fakeSSOSvc) UpdateProviderPolicy(ctx context.Context, tenantID, id string, enabled, trustIdpMfa *bool) (*auth.SSOProvider, error) {
	f.updateWasCalled = true
	f.lastActor = audit.ActorFromContext(ctx)
	f.lastUpdateTenant = tenantID
	f.lastUpdateID = id
	f.lastUpdateEnabled = enabled
	f.lastUpdateTrust = trustIdpMfa
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return f.updateOut, nil
}
func (f *fakeSSOSvc) DeleteProvider(ctx context.Context, _, _ string) error {
	f.lastActor = audit.ActorFromContext(ctx)
	return nil
}
func (f *fakeSSOSvc) StartLogin(context.Context, string, string) (string, string, error) {
	if f.startErr != nil {
		return "", "", f.startErr
	}
	url, nonce := f.startURL, f.startNonce
	if url == "" {
		url = "https://idp.example.com/authorize?x=1"
	}
	if nonce == "" {
		nonce = "nonce-fake"
	}
	return url, nonce, nil
}
func (f *fakeSSOSvc) HandleCallback(_ context.Context, _, _, browserNonce string) (*services.LoginOutput, error) {
	f.callbackWasCalled = true
	f.lastBrowserNonce = browserNonce
	if f.callbackErr != nil {
		return nil, f.callbackErr
	}
	return f.callbackOut, nil
}
func (f *fakeSSOSvc) StartHandoff(_ context.Context, _, _, browserNonce string) (string, error) {
	f.startHandoffWasCalled = true
	f.lastBrowserNonce = browserNonce
	if f.startHandoffErr != nil {
		return "", f.startHandoffErr
	}
	if f.startHandoffCode == "" {
		return "raw-handoff-code", nil
	}
	return f.startHandoffCode, nil
}
func (f *fakeSSOSvc) ExchangeHandoff(_ context.Context, rawCode string) (*services.LoginOutput, error) {
	f.lastExchangeCode = rawCode
	if f.exchangeErr != nil {
		return nil, f.exchangeErr
	}
	return f.exchangeOut, nil
}

// ssoTestRedirectBase is the SSO_REDIRECT_BASE the handler fixtures use — https so the derived
// cookie attrs exercise the Secure=true arm by default.
const ssoTestRedirectBase = "https://api.example.com/api/v1"

// newSSOTestRouter wires a gin engine over svc with redirectBase-derived cookie attrs.
func newSSOTestRouter(svc ssoService, redirectBase string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewSSOHandler(svc, redirectBase, zap.NewNop())
	h.RegisterRoutes(r.Group("/api/v1"))
	return r
}

// ssoCallbackResponse drives GET /api/v1/auth/sso/callback?code=c&state=s against a handler
// wired over svc and returns (status, raw body).
func ssoCallbackResponse(svc ssoService) (int, string) {
	r := newSSOTestRouter(svc, ssoTestRedirectBase)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sso/callback?code=c&state=s", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// TestSSOHandler_Callback_DenyLinkAndNotProvisioned_ProduceIdenticalResponse proves FIX 5 (LOW,
// enumeration oracle): before this fix, SSO_ACCOUNT_LINK_DENIED ("email already belongs to a
// local account") and SSO_USER_NOT_PROVISIONED ("no local account, JIT off") were two
// DISTINGUISHABLE client-facing outcomes — probing an issuer's `email` claim against this
// endpoint let a caller learn whether an arbitrary email string has a local account in a given
// tenant. The two SERVICE-LEVEL sentinels (services.ErrSSOAccountLinkDenied /
// ErrSSOUserNotProvisioned) stay distinct (so the server-side zap.Warn logs a diagnosable
// distinction — see sso_handler.go's writeSSOError) but the CLIENT-FACING response — status code,
// `code`, AND the human-readable `error` text — must be byte-for-byte identical for both, or the
// oracle survives in the message text even after the machine `code` is unified.
func TestSSOHandler_Callback_DenyLinkAndNotProvisioned_ProduceIdenticalResponse(t *testing.T) {
	linkDeniedCode, linkDeniedBody := ssoCallbackResponse(&fakeSSOSvc{callbackErr: services.ErrSSOAccountLinkDenied})
	notProvisionedCode, notProvisionedBody := ssoCallbackResponse(&fakeSSOSvc{callbackErr: services.ErrSSOUserNotProvisioned})

	if linkDeniedCode != http.StatusForbidden || notProvisionedCode != http.StatusForbidden {
		t.Fatalf("status codes = %d / %d; want both 403", linkDeniedCode, notProvisionedCode)
	}
	if linkDeniedCode != notProvisionedCode {
		t.Fatalf("status codes differ: link-denied=%d not-provisioned=%d", linkDeniedCode, notProvisionedCode)
	}
	if linkDeniedBody != notProvisionedBody {
		t.Fatalf("response bodies differ — enumeration oracle survives:\n  link-denied:    %s\n  not-provisioned: %s", linkDeniedBody, notProvisionedBody)
	}
}

// --- SSO-STATE-COOKIE (login-CSRF bind, #8 post-review LOW-5) ---------------------------------

// findSSOStateCookie pulls the sso_state Set-Cookie out of a recorded response (nil when absent).
func findSSOStateCookie(w *httptest.ResponseRecorder) *http.Cookie {
	for _, ck := range w.Result().Cookies() {
		if ck.Name == "sso_state" {
			return ck
		}
	}
	return nil
}

// TestSSOHandler_Start_SetsBindCookie proves /start sets the browser bind cookie with every
// load-bearing attribute: value = the flow's nonce, HttpOnly, SameSite=Lax (must survive the
// IdP's top-level redirect back), Secure (https redirect base), Path scoped to the callback
// route, Max-Age = the sealed state's TTL.
// TestSSOHandler_PublicProviders (SSO-PROVIDER-PUBLIC-LIST): the pre-auth login page fetches
// the tenant's enabled providers to render branded buttons. Table: populated list round-trips
// with issuer intact (FE brands Google off it); the anti-enumeration EMPTY list serializes as
// [] (never null — FE .map safety); SSO-disabled → the same 503 the other public routes map.
func TestSSOHandler_PublicProviders(t *testing.T) {
	t.Run("returns enabled providers with issuer for branding", func(t *testing.T) {
		svc := &fakeSSOSvc{publicProviders: []services.PublicSSOProvider{
			{ID: "p1", DisplayName: "Google", Issuer: "https://accounts.google.com"},
		}}
		r := newSSOTestRouter(svc, ssoTestRedirectBase)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/auth/sso/northwind/providers", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body %s)", w.Code, w.Body.String())
		}
		if svc.lastPublicTenant != "northwind" {
			t.Fatalf("tenant slug = %q; want northwind", svc.lastPublicTenant)
		}
		body := w.Body.String()
		for _, want := range []string{`"id":"p1"`, `"displayName":"Google"`, `"issuer":"https://accounts.google.com"`} {
			if !strings.Contains(body, want) {
				t.Fatalf("body %s missing %s", body, want)
			}
		}
	})
	t.Run("unknown tenant folds into empty [] (not null)", func(t *testing.T) {
		svc := &fakeSSOSvc{} // fake returns []PublicSSOProvider{} like the service's fold
		r := newSSOTestRouter(svc, ssoTestRedirectBase)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/auth/sso/no-such/providers", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200", w.Code)
		}
		if !strings.Contains(w.Body.String(), `"providers":[]`) {
			t.Fatalf("body %s; want providers:[]", w.Body.String())
		}
	})
	t.Run("sso disabled maps to 503", func(t *testing.T) {
		svc := &fakeSSOSvc{publicErr: services.ErrSSOUnavailable}
		r := newSSOTestRouter(svc, ssoTestRedirectBase)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/auth/sso/northwind/providers", nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d; want 503 (body %s)", w.Code, w.Body.String())
		}
	})
}

func TestSSOHandler_Start_SetsBindCookie(t *testing.T) {
	svc := &fakeSSOSvc{startURL: "https://idp.example.com/authorize?x=1", startNonce: "nonce-abc"}
	r := newSSOTestRouter(svc, ssoTestRedirectBase)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sso/acme/start?provider=p-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("start status = %d; want 302", w.Code)
	}
	ck := findSSOStateCookie(w)
	if ck == nil {
		t.Fatalf("no sso_state Set-Cookie on /start response; headers=%v", w.Header())
	}
	if ck.Value != "nonce-abc" {
		t.Errorf("cookie value = %q; want the StartLogin nonce %q", ck.Value, "nonce-abc")
	}
	if !ck.HttpOnly {
		t.Error("cookie must be HttpOnly")
	}
	if ck.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v; want Lax", ck.SameSite)
	}
	if !ck.Secure {
		t.Error("cookie must be Secure when SSO_REDIRECT_BASE is https")
	}
	if ck.Path != "/api/v1/auth/sso/callback" {
		t.Errorf("cookie Path = %q; want /api/v1/auth/sso/callback (scoped to the callback route)", ck.Path)
	}
	if ck.MaxAge != int(services.SSOStateTTL.Seconds()) {
		t.Errorf("cookie Max-Age = %d; want %d (the sealed state's own TTL)", ck.MaxAge, int(services.SSOStateTTL.Seconds()))
	}
}

// TestSSOHandler_Start_HTTPBase_CookieNotSecure proves the Secure flag honestly follows the
// deployment's scheme (local http dev keeps working) instead of being hardcoded either way.
func TestSSOHandler_Start_HTTPBase_CookieNotSecure(t *testing.T) {
	svc := &fakeSSOSvc{startNonce: "nonce-abc"}
	r := newSSOTestRouter(svc, "http://127.0.0.1:8090/api/v1")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sso/acme/start?provider=p-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	ck := findSSOStateCookie(w)
	if ck == nil {
		t.Fatal("no sso_state Set-Cookie on /start response")
	}
	if ck.Secure {
		t.Error("cookie must NOT be Secure when SSO_REDIRECT_BASE is http (local dev)")
	}
	if !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode {
		t.Errorf("HttpOnly/SameSite must not depend on scheme: %+v", ck)
	}
}

// TestSSOHandler_Start_Error_NoCookie proves a failed /start never sets the bind cookie.
func TestSSOHandler_Start_Error_NoCookie(t *testing.T) {
	svc := &fakeSSOSvc{startErr: services.ErrSSOUnavailable}
	r := newSSOTestRouter(svc, ssoTestRedirectBase)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sso/acme/start?provider=p-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("start status = %d; want 503", w.Code)
	}
	if ck := findSSOStateCookie(w); ck != nil {
		t.Fatalf("failed /start must not set the bind cookie, got %+v", ck)
	}
}

// TestSSOHandler_Callback_ForwardsCookieNonce_AndClearsCookie proves the handler hands the
// cookie's value to HandleCallback verbatim AND expires the one-shot cookie on a SUCCESSFUL
// callback.
func TestSSOHandler_Callback_ForwardsCookieNonce_AndClearsCookie(t *testing.T) {
	svc := &fakeSSOSvc{callbackOut: &services.LoginOutput{AccessToken: "at", RefreshToken: "rt"}}
	r := newSSOTestRouter(svc, ssoTestRedirectBase)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sso/callback?code=c&state=s", nil)
	req.AddCookie(&http.Cookie{Name: "sso_state", Value: "nonce-from-browser"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("callback status = %d; want 200; body=%s", w.Code, w.Body.String())
	}
	if svc.lastBrowserNonce != "nonce-from-browser" {
		t.Fatalf("HandleCallback browserNonce = %q; want the cookie value", svc.lastBrowserNonce)
	}
	ck := findSSOStateCookie(w)
	if ck == nil {
		t.Fatal("successful callback must clear (expire) the sso_state cookie")
	}
	if ck.MaxAge >= 0 && ck.Value != "" {
		t.Fatalf("callback Set-Cookie must expire the bind cookie, got %+v", ck)
	}
}

// TestSSOHandler_Callback_NoCookie_PassesEmptyNonce proves a cookieless callback forwards "" —
// the service layer (not the handler) owns the reject decision, keeping the refusal unit-tested
// where the constant-time compare lives.
func TestSSOHandler_Callback_NoCookie_PassesEmptyNonce(t *testing.T) {
	svc := &fakeSSOSvc{callbackErr: services.ErrSSOStateMismatch}
	r := newSSOTestRouter(svc, ssoTestRedirectBase)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sso/callback?code=c&state=s", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if !svc.callbackWasCalled {
		t.Fatal("HandleCallback was never called")
	}
	if svc.lastBrowserNonce != "" {
		t.Fatalf("browserNonce = %q; want empty for a cookieless callback", svc.lastBrowserNonce)
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("callback status = %d; want 403; body=%s", w.Code, w.Body.String())
	}
}

// --- PATCH /admin/sso/providers/:id (SSO-MFA-POLICY: enabled + trustIdpMfa partial update) ----

// newSSOAdminTestRouter mounts the ADMIN provider routes behind a context shim that injects the
// tenant id the way AuthMiddleware would (mirrors roleTestRouter — RequireAdmin is assumed
// satisfied upstream; these tests focus on the handler↔service PATCH contract).
func newSSOAdminTestRouter(svc ssoService, tenantID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", tenantID)
		c.Next()
	})
	h := NewSSOHandler(svc, ssoTestRedirectBase, zap.NewNop())
	h.RegisterAdminRoutes(r.Group("/api/v1"))
	return r
}

// ssoPatchProvider drives PATCH /api/v1/admin/sso/providers/:id with a JSON body.
func ssoPatchProvider(t *testing.T, r *gin.Engine, id string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/sso/providers/"+id, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestSSOHandler_UpdateProvider_TrustOnly proves a {"trustIdpMfa":true}-only PATCH forwards
// (enabled=nil, trustIdpMfa=&true) — the partial-update contract at the handler boundary — and
// that the response echoes the provider row (trustIdpMfa included, camelCase).
func TestSSOHandler_UpdateProvider_TrustOnly(t *testing.T) {
	svc := &fakeSSOSvc{updateOut: &auth.SSOProvider{ID: "p-1", TenantID: "t-1", Issuer: "https://idp.example.com", Enabled: true, TrustIdpMfa: true}}
	r := newSSOAdminTestRouter(svc, "t-1")

	w := ssoPatchProvider(t, r, "p-1", `{"trustIdpMfa":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH trustIdpMfa: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !svc.updateWasCalled || svc.lastUpdateTenant != "t-1" || svc.lastUpdateID != "p-1" {
		t.Fatalf("service call = called:%v tenant:%q id:%q; want called on (t-1, p-1)", svc.updateWasCalled, svc.lastUpdateTenant, svc.lastUpdateID)
	}
	if svc.lastUpdateEnabled != nil {
		t.Fatalf("enabled forwarded as %v; want nil (absent field = leave unchanged)", *svc.lastUpdateEnabled)
	}
	if svc.lastUpdateTrust == nil || !*svc.lastUpdateTrust {
		t.Fatalf("trustIdpMfa forwarded as %v; want &true", svc.lastUpdateTrust)
	}
	var resp struct {
		Data struct {
			TrustIdpMfa bool `json:"trustIdpMfa"`
			Enabled     bool `json:"enabled"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal body %q: %v", w.Body.String(), err)
	}
	if !resp.Data.TrustIdpMfa || !resp.Data.Enabled {
		t.Fatalf("response data = %+v; want trustIdpMfa=true enabled=true echoed", resp.Data)
	}
}

// TestSSOHandler_UpdateProvider_EnabledOnly proves the pre-existing enabled-only PATCH still
// works and forwards trustIdpMfa=nil (the OTHER field untouched — partial update both ways).
func TestSSOHandler_UpdateProvider_EnabledOnly(t *testing.T) {
	svc := &fakeSSOSvc{updateOut: &auth.SSOProvider{ID: "p-1", TenantID: "t-1", Enabled: false, TrustIdpMfa: true}}
	r := newSSOAdminTestRouter(svc, "t-1")

	w := ssoPatchProvider(t, r, "p-1", `{"enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH enabled: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if svc.lastUpdateEnabled == nil || *svc.lastUpdateEnabled {
		t.Fatalf("enabled forwarded as %v; want &false", svc.lastUpdateEnabled)
	}
	if svc.lastUpdateTrust != nil {
		t.Fatalf("trustIdpMfa forwarded as %v; want nil (absent field = leave unchanged)", *svc.lastUpdateTrust)
	}
}

// TestSSOHandler_UpdateProvider_TrustFalseIsNotMissing proves `{"trustIdpMfa":false}` is a REAL
// update (pointer to false), not confused with an absent field — turning trust back OFF must
// never be dropped (the security-relevant direction).
func TestSSOHandler_UpdateProvider_TrustFalseIsNotMissing(t *testing.T) {
	svc := &fakeSSOSvc{updateOut: &auth.SSOProvider{ID: "p-1", TenantID: "t-1", Enabled: true, TrustIdpMfa: false}}
	r := newSSOAdminTestRouter(svc, "t-1")

	w := ssoPatchProvider(t, r, "p-1", `{"trustIdpMfa":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH trustIdpMfa:false: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if svc.lastUpdateTrust == nil || *svc.lastUpdateTrust {
		t.Fatalf("trustIdpMfa forwarded as %v; want &false", svc.lastUpdateTrust)
	}
}

// TestSSOHandler_UpdateProvider_MissingBothFields_422 pins the at-least-one-field contract:
// an empty body is 422 SSO_PROVIDER_FIELD_REQUIRED and the service is never called.
func TestSSOHandler_UpdateProvider_MissingBothFields_422(t *testing.T) {
	svc := &fakeSSOSvc{}
	r := newSSOAdminTestRouter(svc, "t-1")

	w := ssoPatchProvider(t, r, "p-1", `{}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH {}: got %d, want 422; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal body %q: %v", w.Body.String(), err)
	}
	if resp.Code != "SSO_PROVIDER_FIELD_REQUIRED" {
		t.Fatalf("error code = %q; want SSO_PROVIDER_FIELD_REQUIRED", resp.Code)
	}
	if svc.updateWasCalled {
		t.Fatal("service must not be called when both fields are absent")
	}
}

// TestSSOHandler_UpdateProvider_NotFound_404 pins the ErrSSOProviderNotFound → 404 mapping.
func TestSSOHandler_UpdateProvider_NotFound_404(t *testing.T) {
	svc := &fakeSSOSvc{updateErr: auth.ErrSSOProviderNotFound}
	r := newSSOAdminTestRouter(svc, "t-1")

	w := ssoPatchProvider(t, r, "unknown", `{"trustIdpMfa":true}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("PATCH unknown id: got %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestSSOHandler_Callback_StateMismatch_403 pins the ErrSSOStateMismatch → 403 SSO_STATE_MISMATCH
// mapping (distinct from 400 SSO_STATE_INVALID) and that the failure path ALSO clears the cookie.
func TestSSOHandler_Callback_StateMismatch_403(t *testing.T) {
	svc := &fakeSSOSvc{callbackErr: services.ErrSSOStateMismatch}
	r := newSSOTestRouter(svc, ssoTestRedirectBase)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sso/callback?code=c&state=s", nil)
	req.AddCookie(&http.Cookie{Name: "sso_state", Value: "nonce-of-another-flow"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("callback status = %d; want 403; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal body %q: %v", w.Body.String(), err)
	}
	if resp.Code != "SSO_STATE_MISMATCH" {
		t.Fatalf("error code = %q; want SSO_STATE_MISMATCH", resp.Code)
	}
	ck := findSSOStateCookie(w)
	if ck == nil || (ck.MaxAge >= 0 && ck.Value != "") {
		t.Fatalf("failed callback must expire the bind cookie too, got %+v", ck)
	}
}

// --- SSO-FE-REDIRECT: redirect-mode /callback + POST /auth/sso/exchange ----------------------

// ssoTestFrontendBase is the SSO_FRONTEND_BASE the redirect-mode fixtures use.
const ssoTestFrontendBase = "https://fe.example"

// newSSORedirectTestRouter is newSSOTestRouter with redirect mode enabled (WithFrontendBase).
func newSSORedirectTestRouter(svc ssoService, frontendBase string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewSSOHandler(svc, ssoTestRedirectBase, zap.NewNop()).WithFrontendBase(frontendBase)
	h.RegisterRoutes(r.Group("/api/v1"))
	return r
}

// TestSSOHandler_Callback_RedirectMode_Success proves redirect-mode /callback 302s the browser
// to <frontendBase>/auth/sso/complete with the URL-escaped one-time code, still expires the
// one-shot sso_state cookie, and never puts a session token in the response body.
func TestSSOHandler_Callback_RedirectMode_Success(t *testing.T) {
	// A raw code with URL-hostile bytes pins the QueryEscape requirement.
	svc := &fakeSSOSvc{startHandoffCode: "raw+code/x="}
	r := newSSORedirectTestRouter(svc, ssoTestFrontendBase)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sso/callback?code=c&state=s", nil)
	req.AddCookie(&http.Cookie{Name: "sso_state", Value: "nonce-from-browser"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("callback status = %d; want 302; body=%s", w.Code, w.Body.String())
	}
	if !svc.startHandoffWasCalled {
		t.Fatal("StartHandoff was never called in redirect mode")
	}
	if svc.callbackWasCalled {
		t.Fatal("HandleCallback must NOT run in redirect mode (JSON path only)")
	}
	if svc.lastBrowserNonce != "nonce-from-browser" {
		t.Fatalf("StartHandoff browserNonce = %q; want the cookie value", svc.lastBrowserNonce)
	}
	wantLoc := ssoTestFrontendBase + "/auth/sso/complete?code=" + url.QueryEscape("raw+code/x=")
	if loc := w.Header().Get("Location"); loc != wantLoc {
		t.Fatalf("Location = %q; want %q", loc, wantLoc)
	}
	ck := findSSOStateCookie(w)
	if ck == nil || (ck.MaxAge >= 0 && ck.Value != "") {
		t.Fatalf("redirect-mode callback must expire the bind cookie too, got %+v", ck)
	}
	if body := w.Body.String(); strings.Contains(body, "accessToken") || strings.Contains(body, "refreshToken") {
		t.Fatalf("redirect response body must carry no session tokens, got %s", body)
	}
}

// TestSSOHandler_Callback_RedirectMode_Error proves a failed redirect-mode /callback 302s to the
// frontend login page carrying ONLY the coarse machine code (?ssoError=), and still expires the
// bind cookie.
func TestSSOHandler_Callback_RedirectMode_Error(t *testing.T) {
	svc := &fakeSSOSvc{startHandoffErr: services.ErrSSOStateMismatch}
	r := newSSORedirectTestRouter(svc, ssoTestFrontendBase)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sso/callback?code=c&state=s", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("callback status = %d; want 302; body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != ssoTestFrontendBase+"/login?ssoError=SSO_STATE_MISMATCH" {
		t.Fatalf("Location = %q; want %q", loc, ssoTestFrontendBase+"/login?ssoError=SSO_STATE_MISMATCH")
	}
	ck := findSSOStateCookie(w)
	if ck == nil || (ck.MaxAge >= 0 && ck.Value != "") {
		t.Fatalf("failed redirect-mode callback must expire the bind cookie, got %+v", ck)
	}
}

// TestSSOHandler_Callback_RedirectMode_AntiEnumeration proves the enumeration folding (FIX 5)
// survives redirect mode: deny-link and not-provisioned produce the SAME ?ssoError= code.
func TestSSOHandler_Callback_RedirectMode_AntiEnumeration(t *testing.T) {
	locations := make([]string, 0, 2)
	for _, callbackErr := range []error{services.ErrSSOAccountLinkDenied, services.ErrSSOUserNotProvisioned} {
		svc := &fakeSSOSvc{startHandoffErr: callbackErr}
		r := newSSORedirectTestRouter(svc, ssoTestFrontendBase)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sso/callback?code=c&state=s", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		locations = append(locations, w.Header().Get("Location"))
	}
	if locations[0] != locations[1] {
		t.Fatalf("redirect targets differ — enumeration oracle survives:\n  link-denied:     %s\n  not-provisioned: %s", locations[0], locations[1])
	}
	if locations[0] != ssoTestFrontendBase+"/login?ssoError=SSO_LOGIN_NOT_AUTHORIZED" {
		t.Fatalf("Location = %q; want the folded SSO_LOGIN_NOT_AUTHORIZED code", locations[0])
	}
}

// ssoExchange drives POST /api/v1/auth/sso/exchange with a JSON body against a router mounted
// via RegisterRoutes — passing proves the route wiring too.
func ssoExchange(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/sso/exchange", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestSSOHandler_Exchange_Success proves the happy path: the handler forwards the raw code
// verbatim and answers 200 with the SAME {"data":{...}} envelope as JSON-mode /callback.
func TestSSOHandler_Exchange_Success(t *testing.T) {
	svc := &fakeSSOSvc{exchangeOut: &services.LoginOutput{AccessToken: "at", RefreshToken: "rt"}}
	r := newSSOTestRouter(svc, ssoTestRedirectBase)

	w := ssoExchange(t, r, `{"code":"raw-handoff-code"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("exchange status = %d; want 200; body=%s", w.Code, w.Body.String())
	}
	if svc.lastExchangeCode != "raw-handoff-code" {
		t.Fatalf("ExchangeHandoff code = %q; want the body's code verbatim", svc.lastExchangeCode)
	}
	var resp struct {
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal body %q: %v", w.Body.String(), err)
	}
	if resp.Data.AccessToken != "at" || resp.Data.RefreshToken != "rt" {
		t.Fatalf("response data = %+v; want the minted session echoed in the login envelope", resp.Data)
	}
}

// TestSSOHandler_Exchange_InvalidCode_401 pins the opaque anti-probing refusal: every
// code-buys-nothing outcome is 401 SSO_HANDOFF_INVALID.
func TestSSOHandler_Exchange_InvalidCode_401(t *testing.T) {
	svc := &fakeSSOSvc{exchangeErr: auth.ErrSSOHandoffInvalid}
	r := newSSOTestRouter(svc, ssoTestRedirectBase)

	w := ssoExchange(t, r, `{"code":"burned-or-bogus"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("exchange status = %d; want 401; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal body %q: %v", w.Body.String(), err)
	}
	if resp.Code != "SSO_HANDOFF_INVALID" {
		t.Fatalf("error code = %q; want SSO_HANDOFF_INVALID", resp.Code)
	}
}

// TestSSOHandler_Exchange_MalformedBody_400 pins the bindJSON contract on the new endpoint.
func TestSSOHandler_Exchange_MalformedBody_400(t *testing.T) {
	svc := &fakeSSOSvc{}
	r := newSSOTestRouter(svc, ssoTestRedirectBase)

	w := ssoExchange(t, r, `{"code":`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("exchange status = %d; want 400; body=%s", w.Code, w.Body.String())
	}
	if svc.lastExchangeCode != "" {
		t.Fatal("service must not be called on a malformed body")
	}
}

// TestSSOHandler_Exchange_Unavailable_503 pins ErrSSOUnavailable (handoff store not wired /
// SSO off) → 503 SSO_NOT_CONFIGURED via the shared writeSSOError mapping.
func TestSSOHandler_Exchange_Unavailable_503(t *testing.T) {
	svc := &fakeSSOSvc{exchangeErr: services.ErrSSOUnavailable}
	r := newSSOTestRouter(svc, ssoTestRedirectBase)

	w := ssoExchange(t, r, `{"code":"x"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("exchange status = %d; want 503; body=%s", w.Code, w.Body.String())
	}
}

// --- SSO-ADMIN-AUDIT: handler threads the verified actor into the audit context --------------

// newSSOAdminAuthedRouter mounts the admin provider routes behind a shim that injects BOTH the
// tenant id AND the authenticated JWT claims (subject) the way the real Auth→Tenant→RequireAdmin
// chain would — so auditContext(c) can read a real actor. This is what proves the handler wires
// auditContext (the service-level tests only prove the service READS an actor once set).
func newSSOAdminAuthedRouter(svc ssoService, tenantID, actorUserID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", tenantID)
		c.Set("authClaims", &auth.Claims{Subject: auth.Subject{UserID: actorUserID, TenantID: tenantID, IsAdmin: true}})
		c.Next()
	})
	h := NewSSOHandler(svc, ssoTestRedirectBase, zap.NewNop())
	h.RegisterAdminRoutes(r.Group("/api/v1"))
	return r
}

// TestSSOHandler_AdminMutations_ThreadActorForAudit pins the SSO-ADMIN-AUDIT wiring fix: create /
// policy-flip / delete must forward auditContext(c), so the audit row is attributed to the
// verified caller rather than "" (an unattributable governance trail defeats the whole slice).
func TestSSOHandler_AdminMutations_ThreadActorForAudit(t *testing.T) {
	const actor = "u-admin-42"
	svc := &fakeSSOSvc{updateOut: &auth.SSOProvider{ID: "p-1", TenantID: "t-1", Enabled: true}}
	r := newSSOAdminAuthedRouter(svc, "t-1", actor)

	// Create
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/sso/providers",
		strings.NewReader(`{"issuer":"https://idp.example.com","clientId":"c","clientSecret":"s"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if svc.lastActor != actor {
		t.Fatalf("CreateProvider actor = %q; want %q (auditContext not wired) — status=%d body=%s", svc.lastActor, actor, w.Code, w.Body.String())
	}

	// Policy flip
	svc.lastActor = ""
	ssoPatchProvider(t, r, "p-1", `{"trustIdpMfa":true}`)
	if svc.lastActor != actor {
		t.Fatalf("UpdateProviderPolicy actor = %q; want %q", svc.lastActor, actor)
	}

	// Delete
	svc.lastActor = ""
	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/sso/providers/p-1", nil)
	dw := httptest.NewRecorder()
	r.ServeHTTP(dw, delReq)
	if svc.lastActor != actor {
		t.Fatalf("DeleteProvider actor = %q; want %q", svc.lastActor, actor)
	}
}
