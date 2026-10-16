//go:build integration

// ENG-AUTH2 #8 SSO/OIDC — integration suite against a REAL MySQL 8, migrated through 000031
// (sso_providers, sso_identities) via the SAME a4SetupMigratedDB / a4SeedTenant / a4SeedUser
// harness the A4/MFA integration suites use (mirrors their splitDSN/TEST_DATABASE_URL
// single-source DSN discipline — ../specs/004-sso-oidc/spec.md §"#8"). The IdP side is a real
// httptest.Server implementing OIDC discovery + JWKS + token-exchange (RS256-signed id_tokens
// via golang-jwt/jwt/v5, JWKS via go-jose) — NOT a mock of go-oidc itself, so the real
// discovery -> JWKS -> signature -> issuer/audience/expiry verification chain in
// sso_oidc_client.go is genuinely exercised end to end. Plain HTTP (no TLS): go-oidc's discovery
// and JWKS HTTP calls go through http.DefaultClient, which reaches 127.0.0.1 fine without a
// custom client injected via oidc.ClientContext.
//
// Run with:
//
//	go test -tags=integration -race ./internal/handlers/ -run TestSSOIT
package handlers

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	gojose "github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/config"
	"nexus/platform/internal/crypto"
	"nexus/platform/internal/services"
)

// ssoITTestKeyB64 is a fixed, valid 32-byte AES-256 key (base64) for SSO_SECRET_ENC_KEY.
const ssoITTestKeyB64 = "MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=" // base64("12345678901234567890123456789012")

// --- fake OIDC IdP -----------------------------------------------------------------------------

// fakeOIDCIdP is a minimal, real (non-mocked-library) OIDC provider: discovery, JWKS, and a
// token endpoint that mints an RS256-signed id_token for exactly one accepted authorization
// code per test scenario (nextClaims/acceptCode are mutated between subtests).
type fakeOIDCIdP struct {
	t          *testing.T
	server     *httptest.Server
	privateKey *rsa.PrivateKey
	kid        string
	acceptCode string
	nextClaims map[string]any
}

func newFakeOIDCIdP(t *testing.T) *fakeOIDCIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	idp := &fakeOIDCIdP{t: t, privateKey: key, kid: "test-kid", acceptCode: "test-auth-code"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", idp.handleDiscovery)
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/token", idp.handleToken)
	mux.HandleFunc("/jwks", idp.handleJWKS)
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

func (idp *fakeOIDCIdP) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                idp.server.URL,
		"authorization_endpoint":                idp.server.URL + "/authorize",
		"token_endpoint":                        idp.server.URL + "/token",
		"jwks_uri":                              idp.server.URL + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (idp *fakeOIDCIdP) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	jwk := gojose.JSONWebKey{Key: &idp.privateKey.PublicKey, KeyID: idp.kid, Algorithm: "RS256", Use: "sig"}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(gojose.JSONWebKeySet{Keys: []gojose.JSONWebKey{jwk}})
}

func (idp *fakeOIDCIdP) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	if r.FormValue("code") != idp.acceptCode {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	}
	idToken, err := idp.signIDToken(requestClientID(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "test-access-token",
		"token_type":   "Bearer",
		"id_token":     idToken,
	})
}

// requestClientID extracts client_id from either the form body OR HTTP Basic Auth — the oauth2
// package defaults to sending client credentials via Basic Auth (AuthStyleInHeader) for a
// provider it has no cached auth-style preference for, so a token endpoint that ONLY reads
// r.FormValue("client_id") would silently see an empty string and mint an id_token with a blank
// `aud` claim (this was the root cause the very first run of this suite exposed:
// "oidc: expected audience ... got [\"\"]").
func requestClientID(r *http.Request) string {
	if v := r.FormValue("client_id"); v != "" {
		return v
	}
	if user, _, ok := r.BasicAuth(); ok {
		return user
	}
	return ""
}

func (idp *fakeOIDCIdP) signIDToken(clientID string) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": idp.server.URL,
		"aud": clientID,
		"exp": now.Add(5 * time.Minute).Unix(),
		"iat": now.Unix(),
	}
	for k, v := range idp.nextClaims {
		claims[k] = v
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = idp.kid
	signed, err := tok.SignedString(idp.privateKey)
	if err != nil {
		return "", fmt.Errorf("sign id_token: %w", err)
	}
	return signed, nil
}

// --- router + seeding ----------------------------------------------------------------------

func newSSOITRouter(t *testing.T, gdb *gorm.DB) *gin.Engine {
	t.Helper()

	t.Setenv("JWT_SECRET", itJWTSecret)
	t.Setenv("JWT_ACCESS_TTL", "15m")
	t.Setenv("JWT_REFRESH_TTL", "168h")
	t.Setenv("SSO_SECRET_ENC_KEY", ssoITTestKeyB64)
	t.Setenv("SSO_REDIRECT_BASE", "http://127.0.0.1:1/api/v1") // never dialed in this suite
	// SSO-MFA-POLICY: full LOCAL MFA support is wired (key present — mfaITTestKeyB64 from the
	// MFA IT suite, same package) so this suite can prove the found-identity ↔ local-MFA
	// interaction end to end: a trust_idp_mfa=0 provider must return {mfaPending, pendingToken}
	// for an enrolled account, a trust_idp_mfa=1 provider must skip straight to a session.
	// Pre-existing SSO tests are unaffected — none of their users enroll in MFA.
	t.Setenv("MFA_SECRET_ENC_KEY", mfaITTestKeyB64)
	// FIX 3 (SSRF guard): production refuses to dial loopback/private addresses for OIDC
	// discovery/JWKS/exchange. This suite's fake IdP genuinely listens on 127.0.0.1 — it needs
	// the SAME operator opt-in a real on-prem/internal IdP deployment would use, so the real
	// discovery -> JWKS -> exchange code path keeps being exercised end to end without weakening
	// the production DEFAULT (which stays blocked — see TestSSOIT_SSRFGuard_BlocksLoopbackByDefault
	// below, which explicitly unsets this for one subtest to prove the block is real).
	t.Setenv(services.SSOAllowPrivateNetworksEnv, "1")

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
		WithSealer(sealer, ssoCfg.RedirectBase)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), tokenSvc, nil, APIHandlers{
		Auth: NewAuthHandler(authSvc, zap.NewNop()),
		SSO:  NewSSOHandler(ssoSvc, ssoCfg.RedirectBase, zap.NewNop()),
	}, zap.NewNop())
	return r
}

// ssoITSealer builds a crypto.AESGCM sealer over the SAME key the router above loads, so a test
// can seed a provider row directly (bypassing the admin CreateProvider https-issuer check —
// mirrors how a real Google/Azure/Okta issuer would be seeded, letting this suite point at a
// plain-http httptest IdP) with a client_secret that HandleCallback can genuinely decrypt.
func ssoITSealer(t *testing.T) *crypto.AESGCM {
	t.Helper()
	cfg, err := config.LoadSSOConfig()
	if err != nil || !cfg.Enabled {
		t.Fatalf("ssoITSealer: SSO config not enabled (call after newSSOITRouter's Setenv): %v", err)
	}
	sealer, err := crypto.New(cfg.KeyID, cfg.Key)
	if err != nil {
		t.Fatalf("crypto.New: %v", err)
	}
	return sealer
}

const ssoITClientSecret = "test-client-secret"

// seedSSOProvider inserts a sso_providers row directly (GORM) for (tenantID, issuer), sealing
// ssoITClientSecret with the SAME key the router uses. trust_idp_mfa stays at its safe default
// (false) — use seedSSOProviderTrust for the SSO-MFA-POLICY tests.
func seedSSOProvider(t *testing.T, gdb *gorm.DB, id, tenantID, issuer, clientID string, jit bool) *auth.SSOProvider {
	t.Helper()
	return seedSSOProviderTrust(t, gdb, id, tenantID, issuer, clientID, jit, false)
}

// seedSSOProviderTrust is seedSSOProvider with an explicit trust_idp_mfa value (SSO-MFA-POLICY,
// migration 000040).
func seedSSOProviderTrust(t *testing.T, gdb *gorm.DB, id, tenantID, issuer, clientID string, jit, trust bool) *auth.SSOProvider {
	t.Helper()
	sealed, err := ssoITSealer(t).Seal([]byte(ssoITClientSecret))
	if err != nil {
		t.Fatalf("seal client secret: %v", err)
	}
	p := &auth.SSOProvider{
		ID: id, TenantID: tenantID, Issuer: issuer, ClientID: clientID,
		ClientSecretEnc: sealed, DisplayName: "Test IdP", Enabled: true, JITProvisioning: jit,
		TrustIdpMfa: trust,
	}
	if err := gdb.Create(p).Error; err != nil {
		t.Fatalf("seed sso provider: %v", err)
	}
	return p
}

func ssoITCleanup(t *testing.T, gdb *gorm.DB, tenantIDs ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, tid := range tenantIDs {
			gdb.Exec("DELETE FROM sso_identities WHERE tenant_id = ?", tid)
			gdb.Exec("DELETE FROM sso_providers WHERE tenant_id = ?", tid)
			gdb.Exec("DELETE FROM refresh_tokens WHERE tenant_id = ?", tid)
			gdb.Exec("DELETE FROM users WHERE tenant_id = ?", tid)
			gdb.Exec("DELETE FROM tenants WHERE id = ?", tid)
		}
	})
}

// ssoStartAndExtract drives GET /auth/sso/:tenant/start and returns the (state, nonce) query
// params embedded in the 302 Location header pointing at the IdP's /authorize endpoint, PLUS the
// sso_state bind cookie the same response sets (SSO-STATE-COOKIE — the browser would carry it
// back to /callback automatically; these tests thread it explicitly). nonce is NOT secret (it is
// sent to the IdP in the authorization request URL by design, mirroring what a real browser
// redirect would carry) — extracting it here stands in for "the IdP echoes it back inside the
// id_token it mints", which this suite's fake IdP does via idp.nextClaims["nonce"].
func ssoStartAndExtract(t *testing.T, r *gin.Engine, tenantSlug, providerParam string) (state, nonce string, bindCookie *http.Cookie) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sso/"+tenantSlug+"/start?provider="+providerParam, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("start: got %d, want 302; body=%s", w.Code, w.Body.String())
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location header %q: %v", w.Header().Get("Location"), err)
	}
	q := loc.Query()
	state = q.Get("state")
	nonce = q.Get("nonce")
	if state == "" || nonce == "" {
		t.Fatalf("start redirect missing state/nonce: %s", loc.String())
	}
	for _, ck := range w.Result().Cookies() {
		if ck.Name == "sso_state" {
			bindCookie = ck
		}
	}
	if bindCookie == nil {
		t.Fatalf("start response set no sso_state bind cookie; headers=%v", w.Header())
	}
	if bindCookie.Value != nonce {
		t.Fatalf("bind cookie value %q != authorize-URL nonce %q", bindCookie.Value, nonce)
	}
	return state, nonce, bindCookie
}

type ssoITCallbackResponse struct {
	Data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		MFAPending   bool   `json:"mfaPending"`
		PendingToken string `json:"pendingToken"`
		User         struct {
			ID       string `json:"id"`
			TenantID string `json:"tenantId"`
			Email    string `json:"email"`
			IsAdmin  bool   `json:"isAdmin"`
		} `json:"user"`
	} `json:"data"`
	Error string `json:"error"`
	Code  string `json:"code"`
}

func ssoCallback(t *testing.T, r *gin.Engine, code, state string, extraQuery string, bindCookie *http.Cookie) (*httptest.ResponseRecorder, ssoITCallbackResponse) {
	t.Helper()
	path := "/api/v1/auth/sso/callback?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state)
	if extraQuery != "" {
		path += "&" + extraQuery
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if bindCookie != nil { // nil = deliberately cookieless (the SSO-STATE-COOKIE negative case)
		req.AddCookie(&http.Cookie{Name: bindCookie.Name, Value: bindCookie.Value})
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var resp ssoITCallbackResponse
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
	}
	return w, resp
}

// --- tests -----------------------------------------------------------------------------------

// TestSSOIT_FullFlow_LinkedIdentity proves the complete happy path through the REAL HTTP surface
// + MySQL + a real (non-mocked-library) OIDC IdP: start -> real discovery+JWKS+token exchange ->
// id_token verified (signature/issuer/audience/expiry) -> identity resolved -> token pair
// issued -> the access token genuinely authorizes a protected call (GET /auth/me).
func TestSSOIT_FullFlow_LinkedIdentity(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newSSOITRouter(t, gdb)
	idp := newFakeOIDCIdP(t)

	const tenantID = "5550e000-0000-0000-0000-0000000000a1"
	a4SeedTenant(t, gdb, tenantID, "sso-it-a", "SSO IT Tenant A")
	ssoITCleanup(t, gdb, tenantID)
	userID := a4SeedUser(t, gdb, tenantID, "alice@sso-it-a", false, nil)
	provider := seedSSOProvider(t, gdb, "6660e000-0000-0000-0000-0000000000p1", tenantID, idp.server.URL, "client-1", false)
	if err := gdb.Create(&auth.SSOIdentity{
		ID: "7770e000-0000-0000-0000-0000000000i1", TenantID: tenantID, ProviderID: provider.ID,
		UserID: userID, Subject: "sub-alice", Email: "alice@sso-it-a",
	}).Error; err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	state, nonce, bindCookie := ssoStartAndExtract(t, r, "sso-it-a", provider.ID)
	idp.nextClaims = map[string]any{"sub": "sub-alice", "email": "alice@sso-it-a", "email_verified": true, "nonce": nonce}

	w, resp := ssoCallback(t, r, idp.acceptCode, state, "", bindCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("callback: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if resp.Data.AccessToken == "" || resp.Data.RefreshToken == "" {
		t.Fatalf("callback did not issue a token pair: %s", w.Body.String())
	}
	if resp.Data.User.ID != userID {
		t.Fatalf("callback resolved wrong user: got %q, want %q", resp.Data.User.ID, userID)
	}

	// The access token must genuinely authorize a protected call.
	meReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	meReq.Header.Set("Authorization", "Bearer "+resp.Data.AccessToken)
	meW := httptest.NewRecorder()
	r.ServeHTTP(meW, meReq)
	if meW.Code != http.StatusOK {
		t.Fatalf("GET /auth/me with SSO-issued access token: got %d, want 200; body=%s", meW.Code, meW.Body.String())
	}
}

// TestSSOIT_DenyLink_EmailMatchesUnlinkedLocalUser proves the safe-default: an IdP-asserted
// email that matches an EXISTING local user who has never linked this subject is REFUSED, not
// silently auto-linked.
func TestSSOIT_DenyLink_EmailMatchesUnlinkedLocalUser(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newSSOITRouter(t, gdb)
	idp := newFakeOIDCIdP(t)

	const tenantID = "5550e000-0000-0000-0000-0000000000b1"
	a4SeedTenant(t, gdb, tenantID, "sso-it-b", "SSO IT Tenant B")
	ssoITCleanup(t, gdb, tenantID)
	a4SeedUser(t, gdb, tenantID, "bob@sso-it-b", false, nil) // local account, NEVER linked
	provider := seedSSOProvider(t, gdb, "6660e000-0000-0000-0000-0000000000p2", tenantID, idp.server.URL, "client-2", false)

	state, nonce, bindCookie := ssoStartAndExtract(t, r, "sso-it-b", provider.ID)
	idp.nextClaims = map[string]any{"sub": "sub-rogue", "email": "bob@sso-it-b", "nonce": nonce}

	// FIX 5: client-facing code is the UNIFIED SSO_LOGIN_NOT_AUTHORIZED (was SSO_ACCOUNT_LINK_DENIED
	// before the enumeration-oracle fix) — the underlying service sentinel (ErrSSOAccountLinkDenied)
	// and its distinct server-side zap.Warn line are unchanged (see sso_handler.go writeSSOError).
	w, resp := ssoCallback(t, r, idp.acceptCode, state, "", bindCookie)
	if w.Code != http.StatusForbidden || resp.Code != "SSO_LOGIN_NOT_AUTHORIZED" {
		t.Fatalf("callback(email matches unlinked local user): got %d/%s, want 403/SSO_LOGIN_NOT_AUTHORIZED; body=%s", w.Code, resp.Code, w.Body.String())
	}
}

// TestSSOIT_JITOff_NotProvisioned proves the safe DEFAULT (JIT provisioning off) rejects an
// unrecognized subject with no local match.
func TestSSOIT_JITOff_NotProvisioned(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newSSOITRouter(t, gdb)
	idp := newFakeOIDCIdP(t)

	const tenantID = "5550e000-0000-0000-0000-0000000000c1"
	a4SeedTenant(t, gdb, tenantID, "sso-it-c", "SSO IT Tenant C")
	ssoITCleanup(t, gdb, tenantID)
	provider := seedSSOProvider(t, gdb, "6660e000-0000-0000-0000-0000000000p3", tenantID, idp.server.URL, "client-3", false) // jit=false

	state, nonce, bindCookie := ssoStartAndExtract(t, r, "sso-it-c", provider.ID)
	idp.nextClaims = map[string]any{"sub": "sub-new", "email": "newperson@sso-it-c", "nonce": nonce}

	// FIX 5: unified client-facing code (was SSO_USER_NOT_PROVISIONED — see the DenyLink test above).
	w, resp := ssoCallback(t, r, idp.acceptCode, state, "", bindCookie)
	if w.Code != http.StatusForbidden || resp.Code != "SSO_LOGIN_NOT_AUTHORIZED" {
		t.Fatalf("callback(JIT off, no local match): got %d/%s, want 403/SSO_LOGIN_NOT_AUTHORIZED; body=%s", w.Code, resp.Code, w.Body.String())
	}
}

// TestSSOIT_JITOn_ProvisionsAndIssuesSession proves the opt-in path: a provider with
// jit_provisioning=1 auto-creates a local user + identity on first login and issues a session.
func TestSSOIT_JITOn_ProvisionsAndIssuesSession(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newSSOITRouter(t, gdb)
	idp := newFakeOIDCIdP(t)

	const tenantID = "5550e000-0000-0000-0000-0000000000d1"
	a4SeedTenant(t, gdb, tenantID, "sso-it-d", "SSO IT Tenant D")
	ssoITCleanup(t, gdb, tenantID)
	provider := seedSSOProvider(t, gdb, "6660e000-0000-0000-0000-0000000000p4", tenantID, idp.server.URL, "client-4", true) // jit=true

	state, nonce, bindCookie := ssoStartAndExtract(t, r, "sso-it-d", provider.ID)
	idp.nextClaims = map[string]any{"sub": "sub-carol", "email": "carol@sso-it-d", "email_verified": true, "nonce": nonce}

	w, resp := ssoCallback(t, r, idp.acceptCode, state, "", bindCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("callback(JIT on): got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if resp.Data.User.Email != "carol@sso-it-d" || resp.Data.User.IsAdmin {
		t.Fatalf("JIT-provisioned user unexpected: %+v", resp.Data.User)
	}

	var count int64
	gdb.Model(&auth.SSOIdentity{}).Where("tenant_id = ? AND subject = ?", tenantID, "sub-carol").Count(&count)
	if count != 1 {
		t.Fatalf("expected exactly one sso_identities row for sub-carol, got %d", count)
	}
}

// TestSSOIT_JITOn_EmailNotVerified_NotProvisioned proves FIX 4 through the REAL HTTP+MySQL+IdP
// stack: an IdP that omits (or explicitly denies) `email_verified` must not be trusted to JIT-
// create a local account under the asserted email, even with jit_provisioning=1.
func TestSSOIT_JITOn_EmailNotVerified_NotProvisioned(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newSSOITRouter(t, gdb)
	idp := newFakeOIDCIdP(t)

	const tenantID = "5550e000-0000-0000-0000-0000000000l1"
	a4SeedTenant(t, gdb, tenantID, "sso-it-l", "SSO IT Tenant L")
	ssoITCleanup(t, gdb, tenantID)
	provider := seedSSOProvider(t, gdb, "6660e000-0000-0000-0000-000000000p11", tenantID, idp.server.URL, "client-11", true) // jit=true

	state, nonce, bindCookie := ssoStartAndExtract(t, r, "sso-it-l", provider.ID)
	idp.nextClaims = map[string]any{"sub": "sub-unverified", "email": "gina@sso-it-l", "email_verified": false, "nonce": nonce}

	// FIX 5: unified client-facing code (was SSO_USER_NOT_PROVISIONED).
	w, resp := ssoCallback(t, r, idp.acceptCode, state, "", bindCookie)
	if w.Code != http.StatusForbidden || resp.Code != "SSO_LOGIN_NOT_AUTHORIZED" {
		t.Fatalf("callback(JIT on, email_verified=false): got %d/%s, want 403/SSO_LOGIN_NOT_AUTHORIZED; body=%s", w.Code, resp.Code, w.Body.String())
	}
	var count int64
	gdb.Model(&auth.User{}).Where("tenant_id = ? AND email = ?", tenantID, "gina@sso-it-l").Count(&count)
	if count != 0 {
		t.Fatalf("callback(JIT on, email_verified=false) created a local user anyway: count=%d", count)
	}
}

// TestSSOIT_CallbackIgnoresTenantQuery proves the tenant is taken STRICTLY from the signed
// state, never from the callback's own query string — even when a caller supplies a DIFFERENT
// tenant slug/id in ?tenant=..., the login still resolves within the state's tenant.
func TestSSOIT_CallbackIgnoresTenantQuery(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newSSOITRouter(t, gdb)
	idp := newFakeOIDCIdP(t)

	const tenantAID = "5550e000-0000-0000-0000-0000000000e1"
	const tenantBID = "5550e000-0000-0000-0000-0000000000e2"
	a4SeedTenant(t, gdb, tenantAID, "sso-it-e-a", "SSO IT Tenant E-A")
	a4SeedTenant(t, gdb, tenantBID, "sso-it-e-b", "SSO IT Tenant E-B")
	ssoITCleanup(t, gdb, tenantAID, tenantBID)
	userID := a4SeedUser(t, gdb, tenantAID, "eve@sso-it-e-a", false, nil)
	provider := seedSSOProvider(t, gdb, "6660e000-0000-0000-0000-0000000000p5", tenantAID, idp.server.URL, "client-5", false)
	if err := gdb.Create(&auth.SSOIdentity{
		ID: "7770e000-0000-0000-0000-0000000000i5", TenantID: tenantAID, ProviderID: provider.ID,
		UserID: userID, Subject: "sub-eve", Email: "eve@sso-it-e-a",
	}).Error; err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	state, nonce, bindCookie := ssoStartAndExtract(t, r, "sso-it-e-a", provider.ID)
	idp.nextClaims = map[string]any{"sub": "sub-eve", "email": "eve@sso-it-e-a", "nonce": nonce}

	// Attach a bogus ?tenant=sso-it-e-b to the callback request — the handler must ignore it.
	w, resp := ssoCallback(t, r, idp.acceptCode, state, "tenant="+tenantBID, bindCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("callback with spoofed ?tenant=: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if resp.Data.User.TenantID != tenantAID {
		t.Fatalf("callback resolved tenant %q; want %q (from state, spoofed query must be ignored)", resp.Data.User.TenantID, tenantAID)
	}
}

// TestSSOIT_CrossTenant_IdentityScopedToOwnTenant proves a linked identity in tenant A cannot
// be used to log into tenant B, even against a provider registered for the SAME issuer.
func TestSSOIT_CrossTenant_IdentityScopedToOwnTenant(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newSSOITRouter(t, gdb)
	idp := newFakeOIDCIdP(t)

	const tenantAID = "5550e000-0000-0000-0000-0000000000f1"
	const tenantBID = "5550e000-0000-0000-0000-0000000000f2"
	a4SeedTenant(t, gdb, tenantAID, "sso-it-f-a", "SSO IT Tenant F-A")
	a4SeedTenant(t, gdb, tenantBID, "sso-it-f-b", "SSO IT Tenant F-B")
	ssoITCleanup(t, gdb, tenantAID, tenantBID)
	userIDA := a4SeedUser(t, gdb, tenantAID, "shared@sso-it-f", false, nil)
	providerA := seedSSOProvider(t, gdb, "6660e000-0000-0000-0000-0000000000p6", tenantAID, idp.server.URL, "client-6a", false)
	providerB := seedSSOProvider(t, gdb, "6660e000-0000-0000-0000-0000000000p7", tenantBID, idp.server.URL, "client-6b", false) // same issuer, different tenant/provider
	if err := gdb.Create(&auth.SSOIdentity{
		ID: "7770e000-0000-0000-0000-0000000000i6", TenantID: tenantAID, ProviderID: providerA.ID,
		UserID: userIDA, Subject: "sub-shared",
	}).Error; err != nil {
		t.Fatalf("seed identity for tenant A: %v", err)
	}

	// Login through tenant A's own provider succeeds and resolves to tenant A's user.
	stateA, nonceA, bindCookieA := ssoStartAndExtract(t, r, "sso-it-f-a", providerA.ID)
	idp.nextClaims = map[string]any{"sub": "sub-shared", "email": "shared@sso-it-f", "nonce": nonceA}
	wA, respA := ssoCallback(t, r, idp.acceptCode, stateA, "", bindCookieA)
	if wA.Code != http.StatusOK || respA.Data.User.ID != userIDA {
		t.Fatalf("tenant A login failed or resolved wrong user: %d %+v", wA.Code, respA.Data.User)
	}

	// The SAME subject through tenant B's DIFFERENT provider must NOT resolve to tenant A's
	// identity (identity lookup is scoped by (tenant_id, provider_id, subject), and provider_id
	// differs) — with no local match in tenant B and JIT off, it is refused.
	stateB, nonceB, bindCookieB := ssoStartAndExtract(t, r, "sso-it-f-b", providerB.ID)
	idp.nextClaims = map[string]any{"sub": "sub-shared", "email": "someone-else@sso-it-f-b", "nonce": nonceB}
	// FIX 5: unified client-facing code (was SSO_USER_NOT_PROVISIONED). The tenant-isolation
	// assertion below (User.ID != userIDA) is the load-bearing check of THIS test and is untouched.
	wB, respB := ssoCallback(t, r, idp.acceptCode, stateB, "", bindCookieB)
	if wB.Code != http.StatusForbidden || respB.Code != "SSO_LOGIN_NOT_AUTHORIZED" {
		t.Fatalf("cross-tenant callback: got %d/%s, want 403/SSO_LOGIN_NOT_AUTHORIZED (must never resolve tenant A's identity); body=%s", wB.Code, respB.Code, wB.Body.String())
	}
	if respB.Data.User.ID == userIDA {
		t.Fatal("cross-tenant callback resolved tenant A's user id — tenant isolation broken")
	}
}

// TestSSOIT_SSRFGuard_BlocksLoopbackByDefault proves the reviewer FIX 3 guard is genuinely wired
// into the REAL discovery HTTP path (not just unit-tested in isolation — sso_ssrf_test.go covers
// the pure decision function): with the suite-wide loopback-allow escape hatch explicitly
// overridden OFF for this one subtest, /start against a provider whose issuer is the fake IdP's
// OWN 127.0.0.1 URL must fail discovery — classified as 502 SSO_PROVIDER_DISCOVERY_FAILED
// (FIX 6) rather than silently succeeding (which would prove the SSRF guard is not actually
// wired) or surfacing as an undifferentiated 500.
func TestSSOIT_SSRFGuard_BlocksLoopbackByDefault(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newSSOITRouter(t, gdb) // sets services.SSOAllowPrivateNetworksEnv=1 for the suite...
	idp := newFakeOIDCIdP(t)
	t.Setenv(services.SSOAllowPrivateNetworksEnv, "") // ...explicitly override back OFF here.

	const tenantID = "5550e000-0000-0000-0000-0000000000k1"
	a4SeedTenant(t, gdb, tenantID, "sso-it-k", "SSO IT Tenant K")
	ssoITCleanup(t, gdb, tenantID)
	provider := seedSSOProvider(t, gdb, "6660e000-0000-0000-0000-000000000p10", tenantID, idp.server.URL, "client-10", false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sso/sso-it-k/start?provider="+provider.ID, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("GET /start against a loopback issuer with the SSRF guard ON: got %d, want 502; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Code != "SSO_PROVIDER_DISCOVERY_FAILED" {
		t.Fatalf("error code = %q; want SSO_PROVIDER_DISCOVERY_FAILED", resp.Code)
	}
}

// TestSSOIT_StateTampered_400 proves a tampered `state` value is rejected before any exchange
// is attempted.
func TestSSOIT_StateTampered_400(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newSSOITRouter(t, gdb)
	idp := newFakeOIDCIdP(t)

	const tenantID = "5550e000-0000-0000-0000-0000000000g1"
	a4SeedTenant(t, gdb, tenantID, "sso-it-g", "SSO IT Tenant G")
	ssoITCleanup(t, gdb, tenantID)
	provider := seedSSOProvider(t, gdb, "6660e000-0000-0000-0000-0000000000p8", tenantID, idp.server.URL, "client-8", false)

	state, _, bindCookie := ssoStartAndExtract(t, r, "sso-it-g", provider.ID)
	// Flip the FIRST character, not the last (pre-existing flakiness found while hardening this
	// suite — reviewer pass 2026-07-02): `state` is sealed with base64.RawURLEncoding (no
	// padding, internal/crypto/aesgcm.go). The final 1-2 characters of an unpadded base64 string
	// can encode a byte using fewer than 6 significant bits, so occasionally mutating ONLY the
	// last character round-trips to the SAME decoded byte (no actual tamper), letting the sealed
	// envelope open successfully and the flow proceed to a token exchange that then fails for a
	// DIFFERENT reason (SSO_EXCHANGE_FAILED) — an intermittent, non-deterministic false negative
	// for this test, reproduced directly (~2/8 runs) before this fix. The first character is
	// always part of a full 4-character/3-byte group, so changing it deterministically changes
	// the decoded ciphertext byte and reliably fails GCM authentication every run.
	firstChar := byte('X')
	if state[0] == firstChar {
		firstChar = 'Y'
	}
	tampered := string(firstChar) + state[1:]

	w, resp := ssoCallback(t, r, "irrelevant-code", tampered, "", bindCookie)
	if w.Code != http.StatusBadRequest || resp.Code != "SSO_STATE_INVALID" {
		t.Fatalf("callback(tampered state): got %d/%s, want 400/SSO_STATE_INVALID; body=%s", w.Code, resp.Code, w.Body.String())
	}
}

// TestSSOIT_AdminCreateListDeleteProvider proves the ADMIN CRUD HTTP surface end to end: an
// admin creates a provider via POST /admin/sso/providers (real sealing + real DB round trip via
// the production wiring), lists it without leaking the secret, then deletes it.
//
// Renamed from the original TestSSOIT_AdminCreateProvider_ThenFullLoginFlow (reviewer FIX 7,
// post-review hardening pass): the name over-promised "ThenFullLoginFlow" but the body never
// drives a login — it only exercises create/list/delete, exactly like this name now says.
func TestSSOIT_AdminCreateListDeleteProvider(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newSSOITRouter(t, gdb)
	idp := newFakeOIDCIdP(t)

	const tenantID = "5550e000-0000-0000-0000-0000000000h1"
	a4SeedTenant(t, gdb, tenantID, "sso-it-h", "SSO IT Tenant H")
	ssoITCleanup(t, gdb, tenantID)
	adminID := a4SeedUser(t, gdb, tenantID, "admin@sso-it-h", true, nil)
	admin := itLogin(t, r, "sso-it-h", "admin@sso-it-h", authPassword)
	if admin.Data.AccessToken == "" {
		t.Fatal("admin login failed")
	}
	_ = adminID

	createBody := map[string]any{
		"issuer": idp.server.URL, "clientId": "client-9", "clientSecret": ssoITClientSecret, "jitProvisioning": true,
	}
	// idp.server.URL is plain http:// — CreateProvider's https-only validation would reject it,
	// which is exactly why every OTHER test in this suite seeds the row directly (see
	// seedSSOProvider's doc). This test instead swaps the issuer scheme check by constructing an
	// https-shaped issuer string is not possible against a real http-only httptest server, so it
	// asserts the REJECTION path instead: the admin endpoint correctly refuses a non-https issuer.
	w := doAuthed(t, r, http.MethodPost, "/api/v1/admin/sso/providers", admin.Data.AccessToken, createBody, nil)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /admin/sso/providers with http:// issuer: got %d, want 422 (https-only); body=%s", w.Code, w.Body.String())
	}
	var errResp struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.Code != "SSO_PROVIDER_ISSUER_INVALID" {
		t.Fatalf("error code = %q; want SSO_PROVIDER_ISSUER_INVALID", errResp.Code)
	}

	// Now prove the full admin round trip WITH an https-shaped issuer string (accepted by
	// CreateProvider's validation; StartLogin's own OIDC discovery against it would fail against
	// a real IdP, but that is a separate concern from admin CRUD's own contract, which is what
	// this half of the test asserts): create -> list (secret never exposed) -> delete -> 404.
	createBody["issuer"] = "https://accounts.example.com"
	w = doAuthed(t, r, http.MethodPost, "/api/v1/admin/sso/providers", admin.Data.AccessToken, createBody, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /admin/sso/providers: got %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), ssoITClientSecret) {
		t.Fatal("admin create response leaked the plaintext client secret")
	}
	var createResp struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &createResp)

	w = doAuthed(t, r, http.MethodGet, "/api/v1/admin/sso/providers", admin.Data.AccessToken, nil, nil)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), ssoITClientSecret) {
		t.Fatalf("GET /admin/sso/providers leaked secret or failed: %d %s", w.Code, w.Body.String())
	}

	w = doAuthed(t, r, http.MethodDelete, "/api/v1/admin/sso/providers/"+createResp.Data.ID, admin.Data.AccessToken, nil, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE /admin/sso/providers/:id: got %d, want 204; body=%s", w.Code, w.Body.String())
	}
	w = doAuthed(t, r, http.MethodDelete, "/api/v1/admin/sso/providers/"+createResp.Data.ID, admin.Data.AccessToken, nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("DELETE (again): got %d, want 404", w.Code)
	}
}

// TestSSOIT_CreateProvider_EnabledFalse_PersistsDisabled is the reviewer FIX 1 regression test
// (HIGH blocker, fail-open on the auth surface): auth.SSOProvider.Enabled used to carry the GORM
// tag `default:1`. GORM's Create() omits any field from the INSERT statement when its Go value
// equals the zero value AND the field carries a `default` tag — so `{enabled:false}` at creation
// time silently became `enabled=1` in the database (the row that was meant to be born disabled
// went live immediately). CreateProviderInput ALWAYS resolves to a concrete bool before it ever
// reaches the repository (nil -> true, non-nil -> *value), so the GORM-level default was always
// redundant application-logic-wise and only ever misfired this one way. This proves the FULL
// round trip through the real HTTP+GORM+MySQL stack (a fake repo cannot reproduce this — it
// stores the struct verbatim, see sso_service_test.go).
func TestSSOIT_CreateProvider_EnabledFalse_PersistsDisabled(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newSSOITRouter(t, gdb)

	const tenantID = "5550e000-0000-0000-0000-0000000000i1"
	a4SeedTenant(t, gdb, tenantID, "sso-it-i", "SSO IT Tenant I")
	ssoITCleanup(t, gdb, tenantID)
	a4SeedUser(t, gdb, tenantID, "admin@sso-it-i", true, nil)
	admin := itLogin(t, r, "sso-it-i", "admin@sso-it-i", authPassword)
	if admin.Data.AccessToken == "" {
		t.Fatal("admin login failed")
	}

	createBody := map[string]any{
		"issuer": "https://accounts.example.com", "clientId": "client-disabled",
		"clientSecret": ssoITClientSecret, "enabled": false,
	}
	w := doAuthed(t, r, http.MethodPost, "/api/v1/admin/sso/providers", admin.Data.AccessToken, createBody, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /admin/sso/providers(enabled:false): got %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var createResp struct {
		Data struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &createResp)
	// (a) the CREATE response itself must not lie about the row it just persisted.
	if createResp.Data.Enabled {
		t.Fatalf("create response enabled=true; want false (bug: %+v)", createResp.Data)
	}

	// (b) re-reading through a SEPARATE request (fresh GORM SELECT, not the in-memory struct
	// Create() populated) proves the row landed disabled in the database, not just in memory.
	w = doAuthed(t, r, http.MethodGet, "/api/v1/admin/sso/providers", admin.Data.AccessToken, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/sso/providers: got %d; body=%s", w.Code, w.Body.String())
	}
	var listResp struct {
		Data []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	found := false
	for _, p := range listResp.Data {
		if p.ID == createResp.Data.ID {
			found = true
			if p.Enabled {
				t.Fatalf("GET /admin/sso/providers row %q enabled=true; want false (persisted, not just in-memory)", p.ID)
			}
		}
	}
	if !found {
		t.Fatalf("created provider %q not found in list", createResp.Data.ID)
	}

	// (c) the PUBLIC login surface must treat it exactly like a disabled provider: /start answers
	// SSO_PROVIDER_NOT_FOUND, the same as GetEnabledByID would for a genuinely `enabled=0` row.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sso/sso-it-i/start?provider="+createResp.Data.ID, nil)
	sw := httptest.NewRecorder()
	r.ServeHTTP(sw, req)
	if sw.Code != http.StatusNotFound {
		t.Fatalf("GET /start on a create-disabled provider: got %d, want 404 SSO_PROVIDER_NOT_FOUND; body=%s", sw.Code, sw.Body.String())
	}
}

// TestSSOIT_AdminTogglePatchProvider proves the admin re-enable/disable gap the reviewer flagged
// alongside FIX 1: api-conventions.md §3c.4 promises "List hiện disabled để admin BẬT LẠI" but
// there was no endpoint to actually flip `enabled` back on. PATCH /admin/sso/providers/:id closes
// it, tenant-pinned and admin-gated like the rest of the CRUD surface, and — like FIX 1 — must
// not fall into the SAME GORM zero-value-skip trap in the UPDATE direction (disabling via
// `.Updates(&SSOProvider{Enabled:false})` would silently no-op).
func TestSSOIT_AdminTogglePatchProvider(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newSSOITRouter(t, gdb)

	const tenantID = "5550e000-0000-0000-0000-0000000000j1"
	a4SeedTenant(t, gdb, tenantID, "sso-it-j", "SSO IT Tenant J")
	ssoITCleanup(t, gdb, tenantID)
	a4SeedUser(t, gdb, tenantID, "admin@sso-it-j", true, nil)
	admin := itLogin(t, r, "sso-it-j", "admin@sso-it-j", authPassword)
	if admin.Data.AccessToken == "" {
		t.Fatal("admin login failed")
	}

	createBody := map[string]any{
		"issuer": "https://accounts.example.com", "clientId": "client-toggle", "clientSecret": ssoITClientSecret,
	}
	w := doAuthed(t, r, http.MethodPost, "/api/v1/admin/sso/providers", admin.Data.AccessToken, createBody, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /admin/sso/providers: got %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var createResp struct {
		Data struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &createResp)
	if !createResp.Data.Enabled {
		t.Fatalf("default-created provider enabled=false; want true (default): %+v", createResp.Data)
	}

	// Disable it.
	w = doAuthed(t, r, http.MethodPatch, "/api/v1/admin/sso/providers/"+createResp.Data.ID, admin.Data.AccessToken, map[string]any{"enabled": false}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH enabled:false: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var patchResp struct {
		Data struct {
			Enabled bool `json:"enabled"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &patchResp)
	if patchResp.Data.Enabled {
		t.Fatalf("PATCH response after disabling: enabled=true; want false: %s", w.Body.String())
	}
	// Observable effect: /start on the now-disabled provider must 404.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sso/sso-it-j/start?provider="+createResp.Data.ID, nil)
	sw := httptest.NewRecorder()
	r.ServeHTTP(sw, req)
	if sw.Code != http.StatusNotFound {
		t.Fatalf("GET /start after PATCH-disable: got %d, want 404; body=%s", sw.Code, sw.Body.String())
	}

	// Re-enable it.
	w = doAuthed(t, r, http.MethodPatch, "/api/v1/admin/sso/providers/"+createResp.Data.ID, admin.Data.AccessToken, map[string]any{"enabled": true}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH enabled:true: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// Observable effect: the repository's enabled-only lookup (what /start's resolveProvider
	// relies on) resolves the row again. Asserted directly against the repo — not through /start —
	// so this test never dials the fake https:// issuer over the network for real OIDC discovery.
	if _, err := auth.NewSSOProviderRepository(gdb).GetEnabledByID(context.Background(), tenantID, createResp.Data.ID); err != nil {
		t.Fatalf("GetEnabledByID after PATCH re-enable: %v; want resolvable", err)
	}

	// PATCH on an unknown id -> 404 (mirrors DeleteProvider's tenant-pinned not-found contract).
	w = doAuthed(t, r, http.MethodPatch, "/api/v1/admin/sso/providers/does-not-exist", admin.Data.AccessToken, map[string]any{"enabled": false}, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("PATCH unknown id: got %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestSSOIT_Callback_NoBindCookie_403Mismatch proves SSO-STATE-COOKIE (#8 post-review LOW-5,
// login-CSRF) through the REAL HTTP+MySQL+IdP stack: a completely VALID flow — genuine state
// from /start, correct code, id_token nonce matching the state — is still REFUSED when the
// /callback request does not carry the sso_state cookie the SAME browser session received at
// /start. This is exactly the attacker-initiated-flow shape: the attacker's own /start minted
// state+cookie land in the ATTACKER's browser; the victim tricked into opening the callback URL
// has no such cookie, so the victim can no longer be silently logged into the attacker's account.
func TestSSOIT_Callback_NoBindCookie_403Mismatch(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newSSOITRouter(t, gdb)
	idp := newFakeOIDCIdP(t)

	const tenantID = "5550e000-0000-0000-0000-0000000000m1"
	a4SeedTenant(t, gdb, tenantID, "sso-it-m", "SSO IT Tenant M")
	ssoITCleanup(t, gdb, tenantID)
	userID := a4SeedUser(t, gdb, tenantID, "mallory-victim@sso-it-m", false, nil)
	provider := seedSSOProvider(t, gdb, "6660e000-0000-0000-0000-000000000p12", tenantID, idp.server.URL, "client-12", false)
	if err := gdb.Create(&auth.SSOIdentity{
		ID: "7770e000-0000-0000-0000-000000000i12", TenantID: tenantID, ProviderID: provider.ID,
		UserID: userID, Subject: "sub-mallory", Email: "mallory-victim@sso-it-m",
	}).Error; err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	state, nonce, bindCookie := ssoStartAndExtract(t, r, "sso-it-m", provider.ID)
	idp.nextClaims = map[string]any{"sub": "sub-mallory", "email": "mallory-victim@sso-it-m", "nonce": nonce}

	// (a) NO cookie (the victim's browser) → 403 SSO_STATE_MISMATCH, no session.
	w, resp := ssoCallback(t, r, idp.acceptCode, state, "", nil)
	if w.Code != http.StatusForbidden || resp.Code != "SSO_STATE_MISMATCH" {
		t.Fatalf("cookieless callback: got %d/%s, want 403/SSO_STATE_MISMATCH; body=%s", w.Code, resp.Code, w.Body.String())
	}
	if resp.Data.AccessToken != "" {
		t.Fatal("cookieless callback must never issue tokens")
	}

	// (b) WRONG cookie value (a different flow's nonce) → same refusal.
	w, resp = ssoCallback(t, r, idp.acceptCode, state, "", &http.Cookie{Name: "sso_state", Value: "nonce-of-someone-elses-flow"})
	if w.Code != http.StatusForbidden || resp.Code != "SSO_STATE_MISMATCH" {
		t.Fatalf("wrong-cookie callback: got %d/%s, want 403/SSO_STATE_MISMATCH; body=%s", w.Code, resp.Code, w.Body.String())
	}

	// (c) The REAL cookie (the initiating browser) → the login completes — proving (a)/(b)
	// failed on the bind alone, not some other defect in this scenario's setup.
	w, resp = ssoCallback(t, r, idp.acceptCode, state, "", bindCookie)
	if w.Code != http.StatusOK || resp.Data.AccessToken == "" {
		t.Fatalf("bound callback: got %d, want 200 with tokens; body=%s", w.Code, w.Body.String())
	}
	if resp.Data.User.ID != userID {
		t.Fatalf("bound callback resolved wrong user: got %q, want %q", resp.Data.User.ID, userID)
	}
}

// --- SSO-MFA-POLICY (trust_idp_mfa, migration 000040) -----------------------------------------

// TestSSOIT_TrustIdpMfa_EnrolledUser_SkipsLocalMFA proves the whole policy end to end on ONE
// enrolled account against TWO providers (two real fake-IdP servers → two distinct issuers under
// the same tenant):
//   - the DEFAULT provider (trust_idp_mfa=0) returns the {mfaPending, pendingToken} shape — the
//     MFA-ENFORCE behavior is UNCHANGED where trust is off (the regression half of this test);
//   - the TRUSTED provider (trust_idp_mfa=1) logs the same enrolled user STRAIGHT in — full token
//     pair, no pending step — and the access token genuinely authorizes a protected call.
func TestSSOIT_TrustIdpMfa_EnrolledUser_SkipsLocalMFA(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newSSOITRouter(t, gdb)
	idpTrusted := newFakeOIDCIdP(t)
	idpDefault := newFakeOIDCIdP(t)

	const tenantID = "5550e000-0000-0000-0000-0000000000n1"
	const tenantSlug = "sso-it-n"
	a4SeedTenant(t, gdb, tenantID, tenantSlug, "SSO IT Tenant N")
	ssoITCleanup(t, gdb, tenantID) // registered FIRST → runs LAST (sweeps sso_* then users/tenant)
	mfaITCleanup(t, gdb, tenantID) // registered second → runs first (sweeps user_mfa/backup/pending)
	userID := a4SeedUser(t, gdb, tenantID, "nina@"+tenantSlug, false, nil)

	pTrusted := seedSSOProviderTrust(t, gdb, "6660e000-0000-0000-0000-000000000p13", tenantID, idpTrusted.server.URL, "client-13", false, true)
	pDefault := seedSSOProviderTrust(t, gdb, "6660e000-0000-0000-0000-000000000p14", tenantID, idpDefault.server.URL, "client-14", false, false)
	for _, link := range []struct{ id, providerID string }{
		{"7770e000-0000-0000-0000-000000000i13", pTrusted.ID},
		{"7770e000-0000-0000-0000-000000000i14", pDefault.ID},
	} {
		if err := gdb.Create(&auth.SSOIdentity{
			ID: link.id, TenantID: tenantID, ProviderID: link.providerID,
			UserID: userID, Subject: "sub-nina", Email: "nina@" + tenantSlug,
		}).Error; err != nil {
			t.Fatalf("seed identity %s: %v", link.id, err)
		}
	}

	// Enroll + activate LOCAL MFA through the real HTTP surface (the router has the MFA key).
	pre := itLogin(t, r, tenantSlug, "nina@"+tenantSlug, authPassword)
	if pre.Data.AccessToken == "" {
		t.Fatal("pre-enroll login failed")
	}
	mfaEnrollAndActivate(t, r, pre.Data.AccessToken)
	// Sanity: a password login is now two-step — the account is genuinely enrolled.
	mfaLoginExpectPending(t, r, tenantSlug, "nina@"+tenantSlug, authPassword)

	// (a) DEFAULT provider (trust off): SSO must return the SAME two-step pending shape.
	stateD, nonceD, cookieD := ssoStartAndExtract(t, r, tenantSlug, pDefault.ID)
	idpDefault.nextClaims = map[string]any{"sub": "sub-nina", "email": "nina@" + tenantSlug, "nonce": nonceD}
	wD, respD := ssoCallback(t, r, idpDefault.acceptCode, stateD, "", cookieD)
	if wD.Code != http.StatusOK {
		t.Fatalf("callback(default provider, enrolled): got %d, want 200; body=%s", wD.Code, wD.Body.String())
	}
	if !respD.Data.MFAPending || respD.Data.PendingToken == "" {
		t.Fatalf("callback(default provider, enrolled) = %s; want the mfaPending shape (trust-off unchanged)", wD.Body.String())
	}
	if respD.Data.AccessToken != "" || respD.Data.RefreshToken != "" {
		t.Fatalf("callback(default provider, enrolled) leaked token material before MFA verify: %s", wD.Body.String())
	}

	// (b) TRUSTED provider (trust_idp_mfa=1): the SAME enrolled user goes straight to a session.
	stateT, nonceT, cookieT := ssoStartAndExtract(t, r, tenantSlug, pTrusted.ID)
	idpTrusted.nextClaims = map[string]any{"sub": "sub-nina", "email": "nina@" + tenantSlug, "nonce": nonceT}
	wT, respT := ssoCallback(t, r, idpTrusted.acceptCode, stateT, "", cookieT)
	if wT.Code != http.StatusOK {
		t.Fatalf("callback(trusted provider, enrolled): got %d, want 200; body=%s", wT.Code, wT.Body.String())
	}
	if respT.Data.MFAPending || respT.Data.PendingToken != "" {
		t.Fatalf("callback(trusted provider, enrolled) = %s; want a DIRECT session (local MFA skipped)", wT.Body.String())
	}
	if respT.Data.AccessToken == "" || respT.Data.RefreshToken == "" || respT.Data.User.ID != userID {
		t.Fatalf("callback(trusted provider, enrolled) did not issue the expected session: %s", wT.Body.String())
	}

	// The trusted-path access token must genuinely authorize a protected call.
	meReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	meReq.Header.Set("Authorization", "Bearer "+respT.Data.AccessToken)
	meW := httptest.NewRecorder()
	r.ServeHTTP(meW, meReq)
	if meW.Code != http.StatusOK {
		t.Fatalf("GET /auth/me with trusted-SSO access token: got %d, want 200; body=%s", meW.Code, meW.Body.String())
	}
}

// TestSSOIT_AdminPatchTrustIdpMfa proves the ADMIN surface for the flag through the real
// HTTP+GORM+MySQL stack: create never sets it (even when a caller smuggles trustIdpMfa into the
// POST body it is ignored — the flag is settable ONLY via PATCH, as an explicit second decision),
// PATCH sets/clears it, and a partial PATCH of one flag leaves the other untouched IN THE
// DATABASE (fresh SELECT via the list endpoint, not just the in-memory echo).
func TestSSOIT_AdminPatchTrustIdpMfa(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newSSOITRouter(t, gdb)

	const tenantID = "5550e000-0000-0000-0000-0000000000o1"
	const tenantSlug = "sso-it-o"
	a4SeedTenant(t, gdb, tenantID, tenantSlug, "SSO IT Tenant O")
	ssoITCleanup(t, gdb, tenantID)
	a4SeedUser(t, gdb, tenantID, "admin@"+tenantSlug, true, nil)
	admin := itLogin(t, r, tenantSlug, "admin@"+tenantSlug, authPassword)
	if admin.Data.AccessToken == "" {
		t.Fatal("admin login failed")
	}

	// Create — with trustIdpMfa:true smuggled into the body. It is NOT part of the create
	// contract, so the created row must still be trust=false.
	createBody := map[string]any{
		"issuer": "https://accounts.example.com", "clientId": "client-15",
		"clientSecret": ssoITClientSecret, "trustIdpMfa": true,
	}
	w := doAuthed(t, r, http.MethodPost, "/api/v1/admin/sso/providers", admin.Data.AccessToken, createBody, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /admin/sso/providers: got %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var createResp struct {
		Data struct {
			ID          string `json:"id"`
			Enabled     bool   `json:"enabled"`
			TrustIdpMfa bool   `json:"trustIdpMfa"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &createResp)
	if createResp.Data.TrustIdpMfa {
		t.Fatalf("create response trustIdpMfa=true; want false (flag not settable at create): %s", w.Body.String())
	}

	// PATCH trustIdpMfa:true — response echoes it, enabled untouched.
	w = doAuthed(t, r, http.MethodPatch, "/api/v1/admin/sso/providers/"+createResp.Data.ID, admin.Data.AccessToken, map[string]any{"trustIdpMfa": true}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH trustIdpMfa:true: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var patchResp struct {
		Data struct {
			Enabled     bool `json:"enabled"`
			TrustIdpMfa bool `json:"trustIdpMfa"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &patchResp)
	if !patchResp.Data.TrustIdpMfa || !patchResp.Data.Enabled {
		t.Fatalf("PATCH trustIdpMfa:true response = %s; want trustIdpMfa=true enabled=true", w.Body.String())
	}

	// Partial update the OTHER way: PATCH enabled:false must leave trustIdpMfa=true.
	w = doAuthed(t, r, http.MethodPatch, "/api/v1/admin/sso/providers/"+createResp.Data.ID, admin.Data.AccessToken, map[string]any{"enabled": false}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH enabled:false: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &patchResp)
	if patchResp.Data.Enabled || !patchResp.Data.TrustIdpMfa {
		t.Fatalf("PATCH enabled:false response = %s; want enabled=false trustIdpMfa=true (partial update)", w.Body.String())
	}

	// Fresh SELECT through the list endpoint — both flags PERSISTED, not just echoed.
	w = doAuthed(t, r, http.MethodGet, "/api/v1/admin/sso/providers", admin.Data.AccessToken, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/sso/providers: got %d; body=%s", w.Code, w.Body.String())
	}
	var listResp struct {
		Data []struct {
			ID          string `json:"id"`
			Enabled     bool   `json:"enabled"`
			TrustIdpMfa bool   `json:"trustIdpMfa"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	found := false
	for _, p := range listResp.Data {
		if p.ID == createResp.Data.ID {
			found = true
			if p.Enabled || !p.TrustIdpMfa {
				t.Fatalf("persisted row = {enabled:%v trustIdpMfa:%v}; want {enabled:false trustIdpMfa:true}", p.Enabled, p.TrustIdpMfa)
			}
		}
	}
	if !found {
		t.Fatalf("created provider %q not found in list", createResp.Data.ID)
	}

	// PATCH trustIdpMfa:false — turning trust back OFF must be a real write, not a no-op.
	w = doAuthed(t, r, http.MethodPatch, "/api/v1/admin/sso/providers/"+createResp.Data.ID, admin.Data.AccessToken, map[string]any{"trustIdpMfa": false}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH trustIdpMfa:false: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &patchResp)
	if patchResp.Data.TrustIdpMfa {
		t.Fatalf("PATCH trustIdpMfa:false response = %s; want trustIdpMfa=false", w.Body.String())
	}

	// Empty body → 422 SSO_PROVIDER_FIELD_REQUIRED (at least one of the two flags required).
	w = doAuthed(t, r, http.MethodPatch, "/api/v1/admin/sso/providers/"+createResp.Data.ID, admin.Data.AccessToken, map[string]any{}, nil)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH {}: got %d, want 422; body=%s", w.Code, w.Body.String())
	}
	var errResp struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.Code != "SSO_PROVIDER_FIELD_REQUIRED" {
		t.Fatalf("error code = %q; want SSO_PROVIDER_FIELD_REQUIRED", errResp.Code)
	}
}

// TestSSOIT_Migration40_TrustIdpMfaColumn proves migration 000040 through the real
// golang-migrate chain (a4SetupMigratedDB runs ALL migrations): the column exists with the exact
// shape the model expects (tinyint(1), NOT NULL, DEFAULT '0'), and a row inserted WITHOUT
// mentioning it lands at 0 — the same NOT NULL DEFAULT 0 semantics MySQL applied to every row
// that pre-existed the ALTER (no provider becomes trusted by migrating).
func TestSSOIT_Migration40_TrustIdpMfaColumn(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)

	var col struct {
		ColumnType    string         `db:"column_type"`
		IsNullable    string         `db:"is_nullable"`
		ColumnDefault sql.NullString `db:"column_default"`
	}
	err := sx.Get(&col, `SELECT COLUMN_TYPE AS column_type, IS_NULLABLE AS is_nullable, COLUMN_DEFAULT AS column_default
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sso_providers' AND COLUMN_NAME = 'trust_idp_mfa'`)
	if err != nil {
		t.Fatalf("trust_idp_mfa column missing after migration 000040: %v", err)
	}
	if col.ColumnType != "tinyint(1)" || col.IsNullable != "NO" || !col.ColumnDefault.Valid || col.ColumnDefault.String != "0" {
		t.Fatalf("trust_idp_mfa shape = {type:%q nullable:%q default:%v}; want {tinyint(1), NO, '0'}", col.ColumnType, col.IsNullable, col.ColumnDefault)
	}

	const tenantID = "5550e000-0000-0000-0000-0000000000q1"
	a4SeedTenant(t, gdb, tenantID, "sso-it-q", "SSO IT Tenant Q")
	ssoITCleanup(t, gdb, tenantID)
	// Raw INSERT that never mentions trust_idp_mfa — the DB default must fill it with 0.
	if err := gdb.Exec(
		`INSERT INTO sso_providers (id, tenant_id, issuer, client_id, client_secret_enc, enabled, jit_provisioning)
		 VALUES (?, ?, ?, ?, ?, 1, 0)`,
		"6660e000-0000-0000-0000-000000000p16", tenantID, "https://legacy.example.com", "client-16", "sealed:x",
	).Error; err != nil {
		t.Fatalf("raw insert provider: %v", err)
	}
	var trust int
	if err := sx.Get(&trust, "SELECT trust_idp_mfa FROM sso_providers WHERE id = ?", "6660e000-0000-0000-0000-000000000p16"); err != nil {
		t.Fatalf("read trust_idp_mfa: %v", err)
	}
	if trust != 0 {
		t.Fatalf("trust_idp_mfa = %d for a row inserted without the column; want 0 (safe default)", trust)
	}
}
