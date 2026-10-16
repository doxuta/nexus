//go:build integration

// SSO-FE-REDIRECT integration suite: proves /callback's redirect mode (SSOHandler.WithFrontendBase)
// end to end over the REAL HTTP+MySQL+fake-OIDC-IdP stack established by
// eng_sso_integration_test.go — GET /start -> fake IdP -> GET /callback (302 to the frontend
// carrying a one-time handoff code, never tokens) -> POST /auth/sso/exchange (redeems the code for
// a session) -> single-use enforcement observed directly on MySQL (sso_handoff_tokens.used_at) ->
// the cookie-bind-failure path that must redirect with the coarse error code AND never persist a
// handoff row (i.e. the authorization code is never redeemed).
//
// Reuses every fixture eng_sso_integration_test.go already built (same package, same build tag —
// this file adds no new production code, only tests): fakeOIDCIdP/newFakeOIDCIdP,
// a4SetupMigratedDB/a4SeedTenant/a4SeedUser, seedSSOProvider, ssoStartAndExtract, ssoCallback,
// ssoITCallbackResponse, ssoITCleanup, itJWTSecret/ssoITTestKeyB64/mfaITTestKeyB64. The router
// builder below (newSSORedirectITRouter) is a near-duplicate of newSSOITRouter for exactly one
// reason: it must additionally call WithHandoffStore on the service and WithFrontendBase on the
// handler, and newSSOITRouter is deliberately the JSON-mode (legacy) fixture — see
// TestSSORedirect_LegacyJSONMode_AlreadyCoveredByExistingSuite's doc comment below for why legacy
// mode itself gets no NEW test here.
//
// Run with:
//
//	go test -tags=integration ./internal/handlers/ -run 'SSORedirect|SSOHandoff' -count=1 -race
package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/config"
	"nexus/platform/internal/crypto"
	"nexus/platform/internal/services"
)

// ssoRedirectITFrontendBase is the fake frontend origin every test in this file wires via
// WithFrontendBase — never dialed; the suite only ever inspects the Location header the handler
// builds against it.
const ssoRedirectITFrontendBase = "https://fe.example"

// newSSORedirectITRouter is newSSOITRouter (eng_sso_integration_test.go) plus the two
// SSO-FE-REDIRECT wirings that fixture omits on purpose: WithHandoffStore on the service (so
// StartHandoff/ExchangeHandoff are live instead of answering ErrSSOUnavailable) and
// WithFrontendBase on the handler (so /callback switches from the legacy JSON response into
// 302-to-frontend mode). Every env var / config load below mirrors newSSOITRouter exactly, so this
// suite exercises the SAME production wiring newSSOITRouter does, just with redirect mode turned on.
func newSSORedirectITRouter(t *testing.T, gdb *gorm.DB) *gin.Engine {
	t.Helper()

	t.Setenv("JWT_SECRET", itJWTSecret)
	t.Setenv("JWT_ACCESS_TTL", "15m")
	t.Setenv("JWT_REFRESH_TTL", "168h")
	t.Setenv("SSO_SECRET_ENC_KEY", ssoITTestKeyB64)
	t.Setenv("SSO_REDIRECT_BASE", "http://127.0.0.1:1/api/v1") // never dialed in this suite
	t.Setenv("MFA_SECRET_ENC_KEY", mfaITTestKeyB64)
	t.Setenv(services.SSOAllowPrivateNetworksEnv, "1") // fake IdP genuinely listens on 127.0.0.1

	jwtCfg, err := config.LoadJWTConfig()
	if err != nil {
		t.Fatalf("config.LoadJWTConfig: %v", err)
	}
	tokenSvc, err := auth.NewTokenService(jwtCfg.JWTSecret, jwtCfg.AccessTTL, jwtCfg.RefreshTTL)
	if err != nil {
		t.Fatalf("auth.NewTokenService: %v", err)
	}
	ssoCfg, err := config.LoadSSOConfig()
	if err != nil {
		t.Fatalf("config.LoadSSOConfig: %v", err)
	}
	if !ssoCfg.Enabled {
		t.Fatal("expected SSO config Enabled=true with SSO_SECRET_ENC_KEY set")
	}
	sealer, err := crypto.New(ssoCfg.KeyID, ssoCfg.Key)
	if err != nil {
		t.Fatalf("crypto.New: %v", err)
	}

	mfaCfg, err := config.LoadMFAConfig()
	if err != nil {
		t.Fatalf("config.LoadMFAConfig: %v", err)
	}
	if !mfaCfg.Enabled {
		t.Fatal("expected MFA config Enabled=true with MFA_SECRET_ENC_KEY set")
	}
	mfaSealer, err := crypto.New(mfaCfg.KeyID, mfaCfg.Key)
	if err != nil {
		t.Fatalf("crypto.New (mfa): %v", err)
	}

	userRepo := auth.NewUserRepository(gdb)
	refreshRepo := auth.NewRefreshTokenRepository(gdb)
	mfaRepo := auth.NewUserMFARepository(gdb)
	authSvc := services.NewAuthService(userRepo, refreshRepo, tokenSvc).
		WithMFAEnrollmentReader(mfaRepo, mfaCfg.FailOpen).
		WithMFA(mfaRepo, auth.NewMFABackupCodeRepository(gdb), auth.NewMFAPendingTokenRepository(gdb), mfaSealer)
	ssoSvc := services.NewSSOService(authSvc, auth.NewSSOProviderRepository(gdb), auth.NewSSOIdentityRepository(gdb)).
		WithSealer(sealer, ssoCfg.RedirectBase).
		WithHandoffStore(auth.NewSSOHandoffTokenRepository(gdb)) // SSO-FE-REDIRECT

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), tokenSvc, nil, APIHandlers{
		Auth: NewAuthHandler(authSvc, zap.NewNop()),
		SSO:  NewSSOHandler(ssoSvc, ssoCfg.RedirectBase, zap.NewNop()).WithFrontendBase(ssoRedirectITFrontendBase),
	}, zap.NewNop())
	return r
}

// ssoRedirectITCleanupHandoffs deletes sso_handoff_tokens rows for the given tenants. Migration
// 000041 gives that table plain CHAR(36) tenant_id/user_id/provider_id columns with NO foreign
// key (see migrations/000041_create_sso_handoff.up.sql) — so, unlike TestSSOIT_TrustIdpMfa's two
// registration order, there is no referential-integrity reason this must run before or after
// ssoITCleanup. It is still registered AFTER ssoITCleanup in every test below (t.Cleanup is LIFO,
// so it then runs FIRST) to keep the same "narrowest-table-first" convention the rest of this
// package follows.
func ssoRedirectITCleanupHandoffs(t *testing.T, gdb *gorm.DB, tenantIDs ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, tid := range tenantIDs {
			gdb.Exec("DELETE FROM sso_handoff_tokens WHERE tenant_id = ?", tid)
		}
	})
}

// ssoRedirectExtractCode asserts the redirect-mode success Location has the exact contract
// sso_handler.go's Callback doc promises ("<frontendBase>/auth/sso/complete?code=<raw>") and
// returns the decoded one-time handoff code.
func ssoRedirectExtractCode(t *testing.T, location string) string {
	t.Helper()
	wantPrefix := ssoRedirectITFrontendBase + "/auth/sso/complete?code="
	if !strings.HasPrefix(location, wantPrefix) {
		t.Fatalf("callback redirect Location = %q; want prefix %q", location, wantPrefix)
	}
	loc, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse Location %q: %v", location, err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("callback redirect Location %q carries no code", location)
	}
	return code
}

// ssoRedirectExchange drives POST /api/v1/auth/sso/exchange {"code": code} and decodes the
// response into the SAME envelope shape ssoCallback already parses for JSON-mode responses
// (ssoITCallbackResponse, eng_sso_integration_test.go) — Exchange's success/failure bodies are
// byte-for-byte the same shape.
func ssoRedirectExchange(t *testing.T, r *gin.Engine, code string) (*httptest.ResponseRecorder, ssoITCallbackResponse) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		t.Fatalf("marshal exchange body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/sso/exchange", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var resp ssoITCallbackResponse
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
	}
	return w, resp
}

// TestSSORedirect_HappyPath_ExchangeIssuesSession_ThenSingleUse covers the full SSO-FE-REDIRECT
// round trip AND single-use enforcement in one flow (the second needs the code the first mints):
//
//  1. GET /start -> real discovery/JWKS + sso_state bind cookie (ssoStartAndExtract).
//  2. GET /callback (with the bind cookie) -> 302 to the frontend with ONLY a one-time code, no
//     tokens anywhere in the response.
//  3. POST /auth/sso/exchange redeems that code -> 200, a real access+refresh token pair that
//     genuinely authorizes a protected call (GET /auth/me) and resolves to the seeded user.
//  4. The sso_handoff_tokens row is observably marked used (used_at NOT NULL) directly on MySQL.
//  5. Exchanging the SAME code again -> 401 SSO_HANDOFF_INVALID, no second session.
func TestSSORedirect_HappyPath_ExchangeIssuesSession_ThenSingleUse(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	r := newSSORedirectITRouter(t, gdb)
	idp := newFakeOIDCIdP(t)

	tenantID := uuid.NewString()
	const tenantSlug = "sso-redirect-r"
	a4SeedTenant(t, gdb, tenantID, tenantSlug, "SSO Redirect Tenant R")
	ssoITCleanup(t, gdb, tenantID)
	ssoRedirectITCleanupHandoffs(t, gdb, tenantID) // registered after -> runs first (LIFO)
	userID := a4SeedUser(t, gdb, tenantID, "rita@"+tenantSlug, false, nil)
	provider := seedSSOProvider(t, gdb, uuid.NewString(), tenantID, idp.server.URL, "client-redirect-r", false)
	if err := gdb.Create(&auth.SSOIdentity{
		ID: uuid.NewString(), TenantID: tenantID, ProviderID: provider.ID,
		UserID: userID, Subject: "sub-rita", Email: "rita@" + tenantSlug,
	}).Error; err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	state, nonce, bindCookie := ssoStartAndExtract(t, r, tenantSlug, provider.ID)
	idp.nextClaims = map[string]any{"sub": "sub-rita", "email": "rita@" + tenantSlug, "email_verified": true, "nonce": nonce}

	w, resp := ssoCallback(t, r, idp.acceptCode, state, "", bindCookie)
	if w.Code != http.StatusFound {
		t.Fatalf("callback (redirect mode): got %d, want 302; body=%s", w.Code, w.Body.String())
	}
	if resp.Data.AccessToken != "" || resp.Data.RefreshToken != "" {
		t.Fatal("redirect-mode callback response leaked session tokens directly — must only carry a one-time code")
	}
	code := ssoRedirectExtractCode(t, w.Header().Get("Location"))

	ew, eresp := ssoRedirectExchange(t, r, code)
	if ew.Code != http.StatusOK {
		t.Fatalf("exchange: got %d, want 200; body=%s", ew.Code, ew.Body.String())
	}
	if eresp.Data.AccessToken == "" || eresp.Data.RefreshToken == "" {
		t.Fatalf("exchange did not issue a token pair: %s", ew.Body.String())
	}
	if eresp.Data.User.ID != userID {
		t.Fatalf("exchange resolved wrong user: got %q, want %q", eresp.Data.User.ID, userID)
	}

	// The exchanged access token must genuinely authorize a protected call.
	meReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	meReq.Header.Set("Authorization", "Bearer "+eresp.Data.AccessToken)
	meW := httptest.NewRecorder()
	r.ServeHTTP(meW, meReq)
	if meW.Code != http.StatusOK {
		t.Fatalf("GET /auth/me with exchanged access token: got %d, want 200; body=%s", meW.Code, meW.Body.String())
	}

	// MySQL-level single-use proof: the row itself is marked used, not just "exchange succeeded once".
	var usedAt sql.NullTime
	if err := sx.Get(&usedAt, "SELECT used_at FROM sso_handoff_tokens WHERE token_hash = ?", auth.HashRefreshToken(code)); err != nil {
		t.Fatalf("read sso_handoff_tokens row: %v", err)
	}
	if !usedAt.Valid {
		t.Fatal("sso_handoff_tokens.used_at is NULL after a successful exchange; want NOT NULL")
	}

	// Single-use enforcement: exchanging the SAME code again must fail, not mint a second session.
	ew2, eresp2 := ssoRedirectExchange(t, r, code)
	if ew2.Code != http.StatusUnauthorized || eresp2.Code != "SSO_HANDOFF_INVALID" {
		t.Fatalf("second exchange of the same code: got %d/%s, want 401/SSO_HANDOFF_INVALID; body=%s", ew2.Code, eresp2.Code, ew2.Body.String())
	}
	if eresp2.Data.AccessToken != "" {
		t.Fatal("second exchange of an already-used code must never issue tokens")
	}
}

// TestSSORedirect_NoBindCookie_ErrorRedirect_NoHandoffStored proves the redirect-mode failure arm
// of SSO-STATE-COOKIE: a /callback request missing the sso_state bind cookie is refused with a
// 302 to "<frontendBase>/login?ssoError=SSO_STATE_MISMATCH" — the coarse machine code only, never
// the detailed message, matching Callback's doc comment — AND the authorization code is never
// redeemed. There is no call counter on the shared fakeOIDCIdP to assert "the token endpoint was
// never hit" directly, so this proves the stronger, DB-observable consequence instead: zero
// sso_handoff_tokens rows exist for this tenant afterward. That is load-bearing, not incidental —
// services.SSOService.resolveSSOUser checks the cookie bind (subtle.ConstantTimeCompare against
// browserNonce) BEFORE it re-loads the provider or calls the OIDC exchange at all
// (internal/services/sso_service.go), so StartHandoff returns before ever reaching its Store call.
func TestSSORedirect_NoBindCookie_ErrorRedirect_NoHandoffStored(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newSSORedirectITRouter(t, gdb)
	idp := newFakeOIDCIdP(t)

	tenantID := uuid.NewString()
	const tenantSlug = "sso-redirect-s"
	a4SeedTenant(t, gdb, tenantID, tenantSlug, "SSO Redirect Tenant S")
	ssoITCleanup(t, gdb, tenantID)
	ssoRedirectITCleanupHandoffs(t, gdb, tenantID)
	userID := a4SeedUser(t, gdb, tenantID, "sam@"+tenantSlug, false, nil)
	provider := seedSSOProvider(t, gdb, uuid.NewString(), tenantID, idp.server.URL, "client-redirect-s", false)
	if err := gdb.Create(&auth.SSOIdentity{
		ID: uuid.NewString(), TenantID: tenantID, ProviderID: provider.ID,
		UserID: userID, Subject: "sub-sam", Email: "sam@" + tenantSlug,
	}).Error; err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	state, nonce, _ := ssoStartAndExtract(t, r, tenantSlug, provider.ID) // bind cookie deliberately dropped
	idp.nextClaims = map[string]any{"sub": "sub-sam", "email": "sam@" + tenantSlug, "nonce": nonce}

	w, _ := ssoCallback(t, r, idp.acceptCode, state, "", nil)
	if w.Code != http.StatusFound {
		t.Fatalf("cookieless callback (redirect mode): got %d, want 302; body=%s", w.Code, w.Body.String())
	}
	wantLocation := ssoRedirectITFrontendBase + "/login?ssoError=SSO_STATE_MISMATCH"
	if got := w.Header().Get("Location"); got != wantLocation {
		t.Fatalf("cookieless callback Location = %q, want %q", got, wantLocation)
	}

	var count int64
	if err := gdb.Model(&auth.SSOHandoffToken{}).Where("tenant_id = ?", tenantID).Count(&count).Error; err != nil {
		t.Fatalf("count sso_handoff_tokens: %v", err)
	}
	if count != 0 {
		t.Fatalf("cookieless callback stored %d sso_handoff_tokens row(s); want 0 (authorization code must never be redeemed)", count)
	}
}

// Legacy JSON mode (no WithFrontendBase, non-breaking): no new test needed here.
// newSSOITRouter (eng_sso_integration_test.go) never calls WithFrontendBase, so every existing
// test built on it — TestSSOIT_FullFlow_LinkedIdentity, TestSSOIT_JITOn_..., etc. — already IS the
// legacy-JSON-mode proof for /callback, re-run and gated green by
// `go test -tags=integration ./internal/handlers/ -run SSO` below.
