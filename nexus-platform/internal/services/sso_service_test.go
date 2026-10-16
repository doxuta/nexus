package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/auth"
)

// --- SSO test doubles ----------------------------------------------------------------------

type fakeSSOProviderRepo struct {
	mu   sync.Mutex
	rows map[string]*auth.SSOProvider // keyed by id
}

func newFakeSSOProviderRepo() *fakeSSOProviderRepo {
	return &fakeSSOProviderRepo{rows: map[string]*auth.SSOProvider{}}
}

func (f *fakeSSOProviderRepo) Create(_ context.Context, p *auth.SSOProvider) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.TenantID == p.TenantID && r.Issuer == p.Issuer {
			return auth.ErrSSOProviderAlreadyExists
		}
	}
	cp := *p
	f.rows[p.ID] = &cp
	return nil
}

func (f *fakeSSOProviderRepo) List(_ context.Context, tenantID string) ([]*auth.SSOProvider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*auth.SSOProvider
	for _, r := range f.rows {
		if r.TenantID == tenantID {
			cp := *r
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeSSOProviderRepo) GetByID(_ context.Context, tenantID, id string) (*auth.SSOProvider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return nil, auth.ErrSSOProviderNotFound
	}
	cp := *r
	return &cp, nil
}

func (f *fakeSSOProviderRepo) GetEnabledByID(ctx context.Context, tenantID, id string) (*auth.SSOProvider, error) {
	p, err := f.GetByID(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if !p.Enabled {
		return nil, auth.ErrSSOProviderNotFound
	}
	return p, nil
}

func (f *fakeSSOProviderRepo) GetEnabledByIssuer(_ context.Context, tenantID, issuer string) (*auth.SSOProvider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.TenantID == tenantID && r.Issuer == issuer && r.Enabled {
			cp := *r
			return &cp, nil
		}
	}
	return nil, auth.ErrSSOProviderNotFound
}

func (f *fakeSSOProviderRepo) SetEnabled(_ context.Context, tenantID, id string, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return auth.ErrSSOProviderNotFound
	}
	r.Enabled = enabled
	return nil
}

func (f *fakeSSOProviderRepo) SetTrustIdpMfa(_ context.Context, tenantID, id string, trust bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return auth.ErrSSOProviderNotFound
	}
	r.TrustIdpMfa = trust
	return nil
}

func (f *fakeSSOProviderRepo) Delete(_ context.Context, tenantID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return auth.ErrSSOProviderNotFound
	}
	delete(f.rows, id)
	return nil
}

// fakeSSOIdentityRepo is wired against the SAME fakeUserRepo instance the AuthService fixture
// uses, so CreateUserWithIdentity's atomicity (../specs/004-sso-oidc/spec.md §"#8", mirrors
// ActivateWithStepAndBackupCodes) can be proven: when failCreateIdentity is set, the user must
// NOT be visible in userRepo afterward.
type fakeSSOIdentityRepo struct {
	mu                 sync.Mutex
	rows               map[string]*auth.SSOIdentity // keyed by tenantID+providerID+subject
	users              *fakeUserRepo
	failCreateIdentity bool
}

func newFakeSSOIdentityRepo(users *fakeUserRepo) *fakeSSOIdentityRepo {
	return &fakeSSOIdentityRepo{rows: map[string]*auth.SSOIdentity{}, users: users}
}

func (f *fakeSSOIdentityRepo) key(tenantID, providerID, subject string) string {
	return tenantID + "\x00" + providerID + "\x00" + subject
}

func (f *fakeSSOIdentityRepo) FindBySubject(_ context.Context, tenantID, providerID, subject string) (*auth.SSOIdentity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[f.key(tenantID, providerID, subject)]
	if !ok {
		return nil, auth.ErrSSOIdentityNotFound
	}
	cp := *row
	return &cp, nil
}

func (f *fakeSSOIdentityRepo) CreateUserWithIdentity(_ context.Context, u *auth.User, identity *auth.SSOIdentity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.users.users {
		if existing.TenantID == u.TenantID && existing.Email == u.Email {
			return auth.ErrUserAlreadyExists
		}
	}
	if f.failCreateIdentity {
		// Simulate the identity INSERT failing inside the (fake) transaction — the user row
		// must never become visible to the rest of the fixture.
		return fmt.Errorf("fakeSSOIdentityRepo: simulated identity insert failure")
	}
	cpUser := *u
	if f.users.users == nil {
		f.users.users = map[string]*auth.User{}
	}
	f.users.users[u.TenantID+"\x00"+u.Email] = &cpUser
	if f.users.byID == nil {
		f.users.byID = map[string]*auth.User{}
	}
	f.users.byID[u.TenantID+"\x00"+u.ID] = &cpUser
	cpIdentity := *identity
	f.rows[f.key(identity.TenantID, identity.ProviderID, identity.Subject)] = &cpIdentity
	return nil
}

// fakeOIDCClient is a controllable stand-in for the real go-oidc-backed client: unit tests set
// claims/errors directly rather than running a real discovery+JWKS+token exchange (that is what
// the httptest-stubbed integration suite proves instead).
type fakeOIDCClient struct {
	authCodeURL     string
	authCodeErr     error
	claims          *oidcClaims
	exchangeErr     error
	lastNonce       string
	lastRedirectURI string
	lastCode        string
}

func (f *fakeOIDCClient) AuthCodeURL(_ context.Context, _ *auth.SSOProvider, _, redirectURI, state, nonce string) (string, error) {
	f.lastNonce = nonce
	f.lastRedirectURI = redirectURI
	if f.authCodeErr != nil {
		return "", f.authCodeErr
	}
	if f.authCodeURL != "" {
		return f.authCodeURL, nil
	}
	return "https://idp.example.com/authorize?state=" + state, nil
}

func (f *fakeOIDCClient) Exchange(_ context.Context, _ *auth.SSOProvider, _, redirectURI, code string) (*oidcClaims, error) {
	f.lastRedirectURI = redirectURI
	f.lastCode = code
	if f.exchangeErr != nil {
		return nil, f.exchangeErr
	}
	return f.claims, nil
}

// --- fixture ---------------------------------------------------------------------------------

// ssoFixture bundles an SSOService wired over fakes, plus the AuthService/userRepo it shares,
// with tenant "acme" (id t-1) seeded and one pre-existing LOCAL (non-SSO) active user
// "bob@acme.test" for the deny-link tests.
type ssoFixture struct {
	svc        *SSOService
	authSvc    *AuthService
	users      *fakeUserRepo
	providers  *fakeSSOProviderRepo
	identities *fakeSSOIdentityRepo
	oidc       *fakeOIDCClient
	tenantID   string
}

func newSSOFixture(t *testing.T) *ssoFixture {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret-pw"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt hash: %v", err)
	}
	users := &fakeUserRepo{
		tenants: map[string]*auth.Tenant{
			"acme": {ID: "t-1", Slug: "acme", Name: "Acme", Status: "active"},
		},
		users: map[string]*auth.User{
			"t-1\x00bob@acme.test": {
				ID: "u-bob", TenantID: "t-1", Email: "bob@acme.test",
				PasswordHash: string(hash), Status: statusActive, IsAdmin: false,
			},
		},
		byID: map[string]*auth.User{},
	}
	users.byID["t-1\x00u-bob"] = users.users["t-1\x00bob@acme.test"]

	tokens, err := auth.NewTokenService("super-secret-test-key-0123456789", 15*60*time.Second, 7*24*60*60*time.Second)
	if err != nil {
		t.Fatalf("token service: %v", err)
	}
	authSvc := NewAuthService(users, newFakeRefreshRepo(), tokens)

	providers := newFakeSSOProviderRepo()
	identities := newFakeSSOIdentityRepo(users)
	oidcClient := &fakeOIDCClient{}

	svc := NewSSOService(authSvc, providers, identities).WithSealer(fakeSealer{}, "https://api.example.com/api/v1")
	svc.oidc = oidcClient

	return &ssoFixture{
		svc: svc, authSvc: authSvc, users: users, providers: providers,
		identities: identities, oidc: oidcClient, tenantID: "t-1",
	}
}

// seedProvider creates and returns an enabled provider row directly in the fake repo (bypassing
// CreateProvider's https-issuer validation, mirroring how the integration suite seeds providers
// pointing at a plain-http httptest server).
func (f *ssoFixture) seedProvider(t *testing.T, jit bool) *auth.SSOProvider {
	t.Helper()
	p := &auth.SSOProvider{
		ID: "p-1", TenantID: f.tenantID, Issuer: "https://idp.example.com",
		ClientID: "client-1", ClientSecretEnc: "sealed:shh", Enabled: true, JITProvisioning: jit,
	}
	if err := f.providers.Create(context.Background(), p); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	return p
}

// sealedState builds a valid, non-expired sealed state string for (tenantID, providerID, nonce).
func (f *ssoFixture) sealedState(t *testing.T, providerID, nonce string) string {
	t.Helper()
	st := ssoState{TenantID: f.tenantID, ProviderID: providerID, Nonce: nonce, ExpiresAt: time.Now().Add(ssoStateTTL)}
	sealed, err := f.svc.sealState(st)
	if err != nil {
		t.Fatalf("sealState: %v", err)
	}
	return sealed
}

// --- admin CRUD tests --------------------------------------------------------------------

func TestSSOService_CreateProvider_SealsSecret(t *testing.T) {
	f := newSSOFixture(t)
	p, err := f.svc.CreateProvider(context.Background(), f.tenantID, CreateSSOProviderInput{
		Issuer: "https://idp.example.com", ClientID: "client-1", ClientSecret: "top-secret",
	})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if p.ClientSecretEnc == "" || p.ClientSecretEnc == "top-secret" {
		t.Fatalf("ClientSecretEnc = %q; want sealed (non-empty, not plaintext)", p.ClientSecretEnc)
	}
}

func TestSSOService_CreateProvider_RejectsNonHTTPSIssuer(t *testing.T) {
	f := newSSOFixture(t)
	_, err := f.svc.CreateProvider(context.Background(), f.tenantID, CreateSSOProviderInput{
		Issuer: "http://idp.example.com", ClientID: "client-1", ClientSecret: "s",
	})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("CreateProvider(http issuer) error = %v; want *ValidationError", err)
	}
}

func TestSSOService_CreateProvider_RejectsMissingFields(t *testing.T) {
	f := newSSOFixture(t)
	tests := []CreateSSOProviderInput{
		{Issuer: "", ClientID: "c", ClientSecret: "s"},
		{Issuer: "https://idp.example.com", ClientID: "", ClientSecret: "s"},
		{Issuer: "https://idp.example.com", ClientID: "c", ClientSecret: ""},
	}
	for _, in := range tests {
		if _, err := f.svc.CreateProvider(context.Background(), f.tenantID, in); err == nil {
			t.Fatalf("CreateProvider(%+v) = nil error; want validation error", in)
		}
	}
}

func TestSSOService_CreateProvider_Unavailable_WhenSealerNil(t *testing.T) {
	f := newSSOFixture(t)
	f.svc.sealer = nil // simulate SSO_SECRET_ENC_KEY absent
	_, err := f.svc.CreateProvider(context.Background(), f.tenantID, CreateSSOProviderInput{
		Issuer: "https://idp.example.com", ClientID: "c", ClientSecret: "s",
	})
	if !errors.Is(err, ErrSSOUnavailable) {
		t.Fatalf("CreateProvider with no sealer error = %v; want ErrSSOUnavailable", err)
	}
}

func TestSSOService_ListProviders_ScopedToTenant(t *testing.T) {
	f := newSSOFixture(t)
	f.seedProvider(t, false)
	// A provider in a DIFFERENT tenant must never leak into this tenant's list.
	other := &auth.SSOProvider{ID: "p-2", TenantID: "t-2", Issuer: "https://other.example.com", ClientID: "c2", ClientSecretEnc: "sealed:x", Enabled: true}
	if err := f.providers.Create(context.Background(), other); err != nil {
		t.Fatalf("seed other-tenant provider: %v", err)
	}
	list, err := f.svc.ListProviders(context.Background(), f.tenantID)
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if len(list) != 1 || list[0].ID != "p-1" {
		t.Fatalf("ListProviders = %+v; want exactly [p-1]", list)
	}
}

func TestSSOService_DeleteProvider_NotFound(t *testing.T) {
	f := newSSOFixture(t)
	err := f.svc.DeleteProvider(context.Background(), f.tenantID, "does-not-exist")
	if !errors.Is(err, auth.ErrSSOProviderNotFound) {
		t.Fatalf("DeleteProvider(unknown) error = %v; want auth.ErrSSOProviderNotFound", err)
	}
}

// --- StartLogin tests --------------------------------------------------------------------

func TestSSOService_StartLogin_Unavailable_WhenDisabled(t *testing.T) {
	f := newSSOFixture(t)
	f.svc.sealer = nil
	_, _, err := f.svc.StartLogin(context.Background(), "acme", "p-1")
	if !errors.Is(err, ErrSSOUnavailable) {
		t.Fatalf("StartLogin with SSO disabled error = %v; want ErrSSOUnavailable", err)
	}
}

func TestSSOService_StartLogin_UnknownTenant(t *testing.T) {
	f := newSSOFixture(t)
	f.seedProvider(t, false)
	_, _, err := f.svc.StartLogin(context.Background(), "no-such-tenant", "p-1")
	if !errors.Is(err, auth.ErrSSOProviderNotFound) {
		t.Fatalf("StartLogin(unknown tenant) error = %v; want auth.ErrSSOProviderNotFound (anti-enumeration fold)", err)
	}
}

func TestSSOService_StartLogin_UnknownProvider(t *testing.T) {
	f := newSSOFixture(t)
	_, _, err := f.svc.StartLogin(context.Background(), "acme", "no-such-provider")
	if !errors.Is(err, auth.ErrSSOProviderNotFound) {
		t.Fatalf("StartLogin(unknown provider) error = %v; want auth.ErrSSOProviderNotFound", err)
	}
}

func TestSSOService_StartLogin_BuildsRedirectAndSealedState(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, false)
	f.oidc.authCodeURL = "https://idp.example.com/authorize?mock=1"

	redirectURL, nonce, err := f.svc.StartLogin(context.Background(), "acme", p.ID)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if redirectURL != f.oidc.authCodeURL {
		t.Fatalf("redirectURL = %q; want %q", redirectURL, f.oidc.authCodeURL)
	}
	if f.oidc.lastRedirectURI != "https://api.example.com/api/v1/auth/sso/callback" {
		t.Fatalf("redirect_uri = %q; want backend callback URL built from SSO_REDIRECT_BASE", f.oidc.lastRedirectURI)
	}
	if f.oidc.lastNonce == "" {
		t.Fatal("nonce passed to AuthCodeURL is empty")
	}
	// SSO-STATE-COOKIE: the nonce StartLogin RETURNS (what the handler puts in the sso_state
	// cookie) must be the SAME one passed to the IdP (and sealed into the state) — otherwise the
	// cookie could never satisfy HandleCallback's bind check.
	if nonce != f.oidc.lastNonce {
		t.Fatalf("StartLogin returned nonce %q; want the one passed to AuthCodeURL %q", nonce, f.oidc.lastNonce)
	}
}

// enableMFA wires MFA support onto the fixture's shared AuthService (mirrors newMFAFixture's
// WithMFA call, auth_mfa_test.go) and seeds an ENABLED user_mfa row for userID, so a subsequent
// SSO HandleCallback for that user must be gated the same way Login is (reviewer FIX 2).
func (f *ssoFixture) enableMFA(t *testing.T, userID string) *fakeMFARepo {
	t.Helper()
	mfaRepo := newFakeMFARepo()
	mfaRepo.rows[mfaRepo.key(f.tenantID, userID)] = &auth.UserMFA{
		ID: "mfa-" + userID, TenantID: f.tenantID, UserID: userID, SecretEnc: "sealed:totp-secret", Enabled: true,
	}
	f.authSvc.WithMFA(mfaRepo, newFakeBackupRepo(), newFakePendingRepo(), fakeSealer{})
	return mfaRepo
}

// TestSSOService_StartLogin_DiscoveryFailed_ClassifiedDistinctly proves FIX 6: a discovery
// failure at /start (typo'd/unreachable/malformed issuer) is classified into
// ErrSSOProviderDiscoveryFailed, not left to fall through to a generic wrapped error (which the
// handler would otherwise map to a plain 500 SSO_OPERATION_FAILED, indistinguishable from an
// internal fault).
func TestSSOService_StartLogin_DiscoveryFailed_ClassifiedDistinctly(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, false)
	f.oidc.authCodeErr = fmt.Errorf("%w: simulated unreachable issuer", errOIDCDiscoveryFailed)

	_, _, err := f.svc.StartLogin(context.Background(), "acme", p.ID)
	if !errors.Is(err, ErrSSOProviderDiscoveryFailed) {
		t.Fatalf("StartLogin(discovery failure) error = %v; want ErrSSOProviderDiscoveryFailed", err)
	}
}

// --- HandleCallback tests ------------------------------------------------------------------

func TestSSOService_HandleCallback_LinkedIdentity_IssuesSession(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, false)
	// Pre-link an identity to an existing active user.
	f.identities.rows[f.identities.key(f.tenantID, p.ID, "sub-alice")] = &auth.SSOIdentity{
		ID: "id-1", TenantID: f.tenantID, ProviderID: p.ID, UserID: "u-bob", Subject: "sub-alice",
	}
	nonce := "nonce-1"
	state := f.sealedState(t, p.ID, nonce)
	f.oidc.claims = &oidcClaims{Subject: "sub-alice", Email: "bob@acme.test", Nonce: nonce}

	out, err := f.svc.HandleCallback(context.Background(), "auth-code", state, nonce)
	if err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	if out.AccessToken == "" || out.RefreshToken == "" {
		t.Fatalf("HandleCallback did not issue a token pair: %+v", out)
	}
	if out.User == nil || out.User.ID != "u-bob" {
		t.Fatalf("HandleCallback issued session for wrong user: %+v", out.User)
	}
}

func TestSSOService_HandleCallback_TenantIgnoresCallbackQuery(t *testing.T) {
	// HandleCallback's signature takes only (code, state) — there is structurally no tenant
	// parameter it could read from a query string. This test documents/pins that contract: the
	// ONLY source of tenant is the signed state, proven by a successful callback using a state
	// sealed for tenant "t-1" resolving strictly within that tenant.
	f := newSSOFixture(t)
	p := f.seedProvider(t, false)
	f.identities.rows[f.identities.key(f.tenantID, p.ID, "sub-alice")] = &auth.SSOIdentity{
		ID: "id-1", TenantID: f.tenantID, ProviderID: p.ID, UserID: "u-bob", Subject: "sub-alice",
	}
	nonce := "nonce-1"
	state := f.sealedState(t, p.ID, nonce)
	f.oidc.claims = &oidcClaims{Subject: "sub-alice", Email: "bob@acme.test", Nonce: nonce}

	out, err := f.svc.HandleCallback(context.Background(), "auth-code", state, nonce)
	if err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	if out.User.TenantID != f.tenantID {
		t.Fatalf("session tenant = %q; want %q (from state, never a query param)", out.User.TenantID, f.tenantID)
	}
}

func TestSSOService_HandleCallback_UnknownSubject_EmailMatchesLocalUser_DenyLink(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, false)
	nonce := "nonce-1"
	state := f.sealedState(t, p.ID, nonce)
	// bob@acme.test already exists locally (seeded in the fixture) but has NEVER linked this sub.
	f.oidc.claims = &oidcClaims{Subject: "sub-new", Email: "bob@acme.test", Nonce: nonce}

	_, err := f.svc.HandleCallback(context.Background(), "auth-code", state, nonce)
	if !errors.Is(err, ErrSSOAccountLinkDenied) {
		t.Fatalf("HandleCallback(email matches unlinked local user) error = %v; want ErrSSOAccountLinkDenied", err)
	}
}

func TestSSOService_HandleCallback_UnknownSubject_NoLocalMatch_JITOff_NotProvisioned(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, false) // jit=false
	nonce := "nonce-1"
	state := f.sealedState(t, p.ID, nonce)
	f.oidc.claims = &oidcClaims{Subject: "sub-new", Email: "carol@acme.test", Nonce: nonce}

	_, err := f.svc.HandleCallback(context.Background(), "auth-code", state, nonce)
	if !errors.Is(err, ErrSSOUserNotProvisioned) {
		t.Fatalf("HandleCallback(JIT off, no local match) error = %v; want ErrSSOUserNotProvisioned", err)
	}
}

func TestSSOService_HandleCallback_UnknownSubject_NoLocalMatch_JITOn_CreatesUserAndSession(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, true) // jit=true
	nonce := "nonce-1"
	state := f.sealedState(t, p.ID, nonce)
	f.oidc.claims = &oidcClaims{Subject: "sub-new", Email: "carol@acme.test", EmailVerified: true, Nonce: nonce}

	out, err := f.svc.HandleCallback(context.Background(), "auth-code", state, nonce)
	if err != nil {
		t.Fatalf("HandleCallback(JIT on): %v", err)
	}
	if out.User == nil || out.User.Email != "carol@acme.test" {
		t.Fatalf("HandleCallback(JIT on) unexpected user: %+v", out.User)
	}
	if out.User.IsAdmin {
		t.Fatal("JIT-provisioned user must default to lowest privilege (IsAdmin=false)")
	}
	// A second login with the same subject must now resolve via the identity, not re-JIT.
	identity, err := f.identities.FindBySubject(context.Background(), f.tenantID, p.ID, "sub-new")
	if err != nil {
		t.Fatalf("expected identity row to be created: %v", err)
	}
	if identity.UserID != out.User.ID {
		t.Fatalf("identity.UserID = %q; want %q", identity.UserID, out.User.ID)
	}
}

// TestSSOService_HandleCallback_JITOn_EmailNotVerified_NotProvisioned proves FIX 4 (LOW): the
// `email_verified` claim is decoded (oidc_client.go) but was never actually enforced. An IdP is
// free to assert ANY string in the `email` claim regardless of whether that mailbox is verified —
// trusting it as authoritative to CREATE a brand-new local account under that email (and thereby
// grant it access to whatever pre-existing data/invites are addressed to that email string) is
// unsafe when the IdP has not vouched the mailbox is actually owned by the authenticating subject.
// Deliberately gated ONLY on the JIT-create branch (not on the deny-link email-match lookup
// above it) — an unverified email used to DENY a login is safe (it only ever makes a login MORE
// restrictive), used to silently CREATE an account under that address is not.
func TestSSOService_HandleCallback_JITOn_EmailNotVerified_NotProvisioned(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, true) // jit=true
	nonce := "nonce-1"
	state := f.sealedState(t, p.ID, nonce)
	f.oidc.claims = &oidcClaims{Subject: "sub-unverified", Email: "frank@acme.test", EmailVerified: false, Nonce: nonce}

	_, err := f.svc.HandleCallback(context.Background(), "auth-code", state, nonce)
	if !errors.Is(err, ErrSSOUserNotProvisioned) {
		t.Fatalf("HandleCallback(JIT on, email_verified=false) error = %v; want ErrSSOUserNotProvisioned", err)
	}
	if _, ok := f.users.users["t-1\x00frank@acme.test"]; ok {
		t.Fatal("HandleCallback(JIT on, email_verified=false) must not create a local user")
	}
}

func TestSSOService_HandleCallback_JITAtomicity_IdentityInsertFails_NoOrphanUser(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, true)
	f.identities.failCreateIdentity = true
	nonce := "nonce-1"
	state := f.sealedState(t, p.ID, nonce)
	f.oidc.claims = &oidcClaims{Subject: "sub-new", Email: "dave@acme.test", EmailVerified: true, Nonce: nonce}

	_, err := f.svc.HandleCallback(context.Background(), "auth-code", state, nonce)
	if err == nil {
		t.Fatal("HandleCallback with simulated identity-insert failure = nil error; want failure")
	}
	if _, ok := f.users.users["t-1\x00dave@acme.test"]; ok {
		t.Fatal("a failed JIT provisioning left an orphan user row — CreateUserWithIdentity must be atomic")
	}
}

func TestSSOService_HandleCallback_NonceMismatch_Invalid(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, false)
	state := f.sealedState(t, p.ID, "nonce-expected")
	f.oidc.claims = &oidcClaims{Subject: "sub-alice", Email: "bob@acme.test", Nonce: "nonce-DIFFERENT"}

	// browserNonce matches the STATE's nonce (the cookie bind passes) — what mismatches here is
	// the ID TOKEN's nonce claim, so the failure must classify as ErrSSOIDTokenInvalid.
	_, err := f.svc.HandleCallback(context.Background(), "auth-code", state, "nonce-expected")
	if !errors.Is(err, ErrSSOIDTokenInvalid) {
		t.Fatalf("HandleCallback(nonce mismatch) error = %v; want ErrSSOIDTokenInvalid", err)
	}
}

func TestSSOService_HandleCallback_EmptySubject_Invalid(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, false)
	nonce := "nonce-1"
	state := f.sealedState(t, p.ID, nonce)
	f.oidc.claims = &oidcClaims{Subject: "", Email: "bob@acme.test", Nonce: nonce}

	_, err := f.svc.HandleCallback(context.Background(), "auth-code", state, nonce)
	if !errors.Is(err, ErrSSOIDTokenInvalid) {
		t.Fatalf("HandleCallback(empty sub) error = %v; want ErrSSOIDTokenInvalid", err)
	}
}

func TestSSOService_HandleCallback_EmptyEmail_UnknownSubject_NotProvisioned(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, true) // even with JIT on, a blank email must not silently provision.
	nonce := "nonce-1"
	state := f.sealedState(t, p.ID, nonce)
	f.oidc.claims = &oidcClaims{Subject: "sub-new", Email: "", Nonce: nonce}

	_, err := f.svc.HandleCallback(context.Background(), "auth-code", state, nonce)
	if !errors.Is(err, ErrSSOUserNotProvisioned) {
		t.Fatalf("HandleCallback(blank email) error = %v; want ErrSSOUserNotProvisioned", err)
	}
}

func TestSSOService_HandleCallback_OrphanedIdentity_UserInactive_FailsClean(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, false)
	f.identities.rows[f.identities.key(f.tenantID, p.ID, "sub-alice")] = &auth.SSOIdentity{
		ID: "id-1", TenantID: f.tenantID, ProviderID: p.ID, UserID: "u-bob", Subject: "sub-alice",
	}
	// Simulate the linked user having been disabled/soft-deleted after the identity was linked.
	f.users.users["t-1\x00bob@acme.test"].Status = "disabled"
	nonce := "nonce-1"
	state := f.sealedState(t, p.ID, nonce)
	f.oidc.claims = &oidcClaims{Subject: "sub-alice", Email: "bob@acme.test", Nonce: nonce}

	_, err := f.svc.HandleCallback(context.Background(), "auth-code", state, nonce)
	if !errors.Is(err, ErrSSOUserNotProvisioned) {
		t.Fatalf("HandleCallback(orphaned identity, inactive user) error = %v; want ErrSSOUserNotProvisioned (fail clean, not 500)", err)
	}
}

func TestSSOService_HandleCallback_StateTampered_Invalid(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, false)
	state := f.sealedState(t, p.ID, "nonce-1")
	tampered := state[:len(state)-1] + "Z"

	_, err := f.svc.HandleCallback(context.Background(), "auth-code", tampered, "nonce-1")
	if !errors.Is(err, ErrSSOStateInvalid) {
		t.Fatalf("HandleCallback(tampered state) error = %v; want ErrSSOStateInvalid", err)
	}
}

func TestSSOService_HandleCallback_ProviderNotFoundOrDisabled(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, false)
	state := f.sealedState(t, p.ID, "nonce-1")
	// Disable the provider AFTER /start issued the state, simulating an admin revoking it
	// mid-flow — /callback must re-check, not trust whatever /start saw.
	f.providers.rows[p.ID].Enabled = false

	_, err := f.svc.HandleCallback(context.Background(), "auth-code", state, "nonce-1")
	if !errors.Is(err, auth.ErrSSOProviderNotFound) {
		t.Fatalf("HandleCallback(provider disabled mid-flow) error = %v; want auth.ErrSSOProviderNotFound", err)
	}
}

func TestSSOService_HandleCallback_ExchangeFailed(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, false)
	state := f.sealedState(t, p.ID, "nonce-1")
	f.oidc.exchangeErr = errOIDCExchangeFailed

	_, err := f.svc.HandleCallback(context.Background(), "bad-code", state, "nonce-1")
	if !errors.Is(err, ErrSSOExchangeFailed) {
		t.Fatalf("HandleCallback(exchange failure) error = %v; want ErrSSOExchangeFailed", err)
	}
}

func TestSSOService_HandleCallback_IDTokenInvalid(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, false)
	state := f.sealedState(t, p.ID, "nonce-1")
	f.oidc.exchangeErr = errOIDCIDTokenInvalid

	_, err := f.svc.HandleCallback(context.Background(), "code", state, "nonce-1")
	if !errors.Is(err, ErrSSOIDTokenInvalid) {
		t.Fatalf("HandleCallback(id_token verify failure) error = %v; want ErrSSOIDTokenInvalid", err)
	}
}

func TestSSOService_HandleCallback_Unavailable_WhenDisabled(t *testing.T) {
	f := newSSOFixture(t)
	f.svc.sealer = nil
	_, err := f.svc.HandleCallback(context.Background(), "code", "state", "any-nonce")
	if !errors.Is(err, ErrSSOUnavailable) {
		t.Fatalf("HandleCallback with SSO disabled error = %v; want ErrSSOUnavailable", err)
	}
}

func TestSSOService_HandleCallback_CrossTenant_IdentityScopedToOwnTenant(t *testing.T) {
	// Two tenants each register their OWN provider row against the SAME issuer (allowed: the
	// UNIQUE constraint is (tenant_id, issuer)). The same IdP-asserted `sub` under tenant A's
	// provider must never resolve to a session in tenant B.
	f := newSSOFixture(t)
	pA := f.seedProvider(t, false)
	f.users.tenants["beta"] = &auth.Tenant{ID: "t-2", Slug: "beta", Name: "Beta", Status: "active"}
	pB := &auth.SSOProvider{ID: "p-2", TenantID: "t-2", Issuer: pA.Issuer, ClientID: "client-2", ClientSecretEnc: "sealed:shh", Enabled: true}
	if err := f.providers.Create(context.Background(), pB); err != nil {
		t.Fatalf("seed tenant B provider: %v", err)
	}
	// Link "sub-shared" under tenant A only.
	f.identities.rows[f.identities.key("t-1", pA.ID, "sub-shared")] = &auth.SSOIdentity{
		ID: "id-a", TenantID: "t-1", ProviderID: pA.ID, UserID: "u-bob", Subject: "sub-shared",
	}

	// A callback whose STATE points at tenant B's provider must NOT see tenant A's identity —
	// FindBySubject is scoped by (tenant, provider_id, subject), and provider_id differs.
	nonce := "nonce-1"
	stB := ssoState{TenantID: "t-2", ProviderID: pB.ID, Nonce: nonce, ExpiresAt: time.Now().Add(ssoStateTTL)}
	sealedB, err := f.svc.sealState(stB)
	if err != nil {
		t.Fatalf("sealState: %v", err)
	}
	f.oidc.claims = &oidcClaims{Subject: "sub-shared", Email: "someone@beta.test", Nonce: nonce}

	_, err = f.svc.HandleCallback(context.Background(), "code", sealedB, nonce)
	// No identity under (t-2, p-2, sub-shared) and no local user "someone@beta.test" in t-2,
	// JIT is off on pB -> not-provisioned. The key assertion is that this does NOT succeed as
	// tenant A's user u-bob.
	if !errors.Is(err, ErrSSOUserNotProvisioned) {
		t.Fatalf("cross-tenant callback error = %v; want ErrSSOUserNotProvisioned (never tenant A's identity)", err)
	}
}

// --- FIX 2: SSO must not bypass local MFA (../specs/004-sso-oidc/spec.md §"#8" Founder decision #4,
// api-conventions.md §3c.8 item 4 revised) ---------------------------------------------------

// TestSSOService_HandleCallback_LinkedIdentity_MFAEnabled_ReturnsMFAPending proves the fix: a
// found-identity SSO login for an account with LOCAL MFA enabled must NOT mint a token pair
// straight away — it must return the SAME two-step "pending" shape Login returns
// (LoginOutput{MFAPending:true, PendingToken:...}), forcing the caller through
// POST /auth/mfa/verify exactly like a password login would. Before this fix, SSO unconditionally
// called issueSessionForSubject, skipping the local second factor entirely — a JIT-then-enroll
// (or any found-identity) login was a working MFA bypass.
func TestSSOService_HandleCallback_LinkedIdentity_MFAEnabled_ReturnsMFAPending(t *testing.T) {
	f := newSSOFixture(t)
	f.enableMFA(t, "u-bob")
	p := f.seedProvider(t, false)
	f.identities.rows[f.identities.key(f.tenantID, p.ID, "sub-alice")] = &auth.SSOIdentity{
		ID: "id-1", TenantID: f.tenantID, ProviderID: p.ID, UserID: "u-bob", Subject: "sub-alice",
	}
	nonce := "nonce-1"
	state := f.sealedState(t, p.ID, nonce)
	f.oidc.claims = &oidcClaims{Subject: "sub-alice", Email: "bob@acme.test", Nonce: nonce}

	out, err := f.svc.HandleCallback(context.Background(), "auth-code", state, nonce)
	if err != nil {
		t.Fatalf("HandleCallback(MFA enabled): %v", err)
	}
	if !out.MFAPending {
		t.Fatalf("HandleCallback(MFA enabled) = %+v; want MFAPending=true", out)
	}
	if out.PendingToken == "" {
		t.Fatal("HandleCallback(MFA enabled): PendingToken is empty")
	}
	if out.AccessToken != "" || out.RefreshToken != "" {
		t.Fatalf("HandleCallback(MFA enabled) must not mint a token pair before MFA verify: %+v", out)
	}
}

// TestSSOService_HandleCallback_LinkedIdentity_MFADisabled_IssuesSessionDirectly proves the
// non-MFA path stays exactly as before once MFA support is wired but this particular account
// never enrolled (mfaRepo != nil, but Get returns ErrMFANotEnrolled) — SSO must still mint a
// token pair directly, mirroring Login's own byte-identical-when-no-MFA discipline.
func TestSSOService_HandleCallback_LinkedIdentity_MFADisabled_IssuesSessionDirectly(t *testing.T) {
	f := newSSOFixture(t)
	// Wire MFA support onto the AuthService (mfaRepo != nil) but do NOT enroll u-bob — the Get
	// call inside HandleCallback's MFA check must see ErrMFANotEnrolled and fall through.
	f.authSvc.WithMFA(newFakeMFARepo(), newFakeBackupRepo(), newFakePendingRepo(), fakeSealer{})
	p := f.seedProvider(t, false)
	f.identities.rows[f.identities.key(f.tenantID, p.ID, "sub-alice")] = &auth.SSOIdentity{
		ID: "id-1", TenantID: f.tenantID, ProviderID: p.ID, UserID: "u-bob", Subject: "sub-alice",
	}
	nonce := "nonce-1"
	state := f.sealedState(t, p.ID, nonce)
	f.oidc.claims = &oidcClaims{Subject: "sub-alice", Email: "bob@acme.test", Nonce: nonce}

	out, err := f.svc.HandleCallback(context.Background(), "auth-code", state, nonce)
	if err != nil {
		t.Fatalf("HandleCallback(MFA wired, not enrolled): %v", err)
	}
	if out.MFAPending {
		t.Fatalf("HandleCallback(not enrolled) = %+v; want a direct session, not MFAPending", out)
	}
	if out.AccessToken == "" || out.RefreshToken == "" {
		t.Fatalf("HandleCallback(not enrolled) did not issue a token pair: %+v", out)
	}
}

// TestSSOService_HandleCallback_JIT_NewUser_NeverHasMFA_IssuesSessionDirectly proves the JIT
// branch is unaffected: a brand-new JIT-provisioned user cannot possibly have a pre-existing
// user_mfa row, so it must never be routed through the MFA-pending check (which only applies to
// the found-identity branch — a freshly minted user_id trivially has no MFA enrollment yet).
func TestSSOService_HandleCallback_JIT_NewUser_NeverHasMFA_IssuesSessionDirectly(t *testing.T) {
	f := newSSOFixture(t)
	f.authSvc.WithMFA(newFakeMFARepo(), newFakeBackupRepo(), newFakePendingRepo(), fakeSealer{})
	p := f.seedProvider(t, true) // jit=true
	nonce := "nonce-1"
	state := f.sealedState(t, p.ID, nonce)
	f.oidc.claims = &oidcClaims{Subject: "sub-new-mfa", Email: "erin@acme.test", EmailVerified: true, Nonce: nonce}

	out, err := f.svc.HandleCallback(context.Background(), "auth-code", state, nonce)
	if err != nil {
		t.Fatalf("HandleCallback(JIT, MFA wired): %v", err)
	}
	if out.MFAPending {
		t.Fatalf("HandleCallback(JIT) = %+v; want a direct session for a brand-new user", out)
	}
	if out.AccessToken == "" {
		t.Fatalf("HandleCallback(JIT) did not issue a token pair: %+v", out)
	}
}

// --- SSO-STATE-COOKIE: browser-session bind (#8 post-review LOW-5, login-CSRF) ----------------

// TestSSOService_HandleCallback_BrowserNonceMissing_StateMismatch proves the bind's missing-cookie
// arm: a perfectly valid sealed state presented WITHOUT the sso_state cookie (browserNonce == "")
// is refused with ErrSSOStateMismatch — the exact shape of a victim opening an attacker-initiated
// callback URL in a browser that never ran /start.
func TestSSOService_HandleCallback_BrowserNonceMissing_StateMismatch(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, false)
	nonce := "nonce-1"
	state := f.sealedState(t, p.ID, nonce)
	f.oidc.claims = &oidcClaims{Subject: "sub-alice", Email: "bob@acme.test", Nonce: nonce}

	_, err := f.svc.HandleCallback(context.Background(), "auth-code", state, "")
	if !errors.Is(err, ErrSSOStateMismatch) {
		t.Fatalf("HandleCallback(no bind cookie) error = %v; want ErrSSOStateMismatch", err)
	}
}

// TestSSOService_HandleCallback_BrowserNonceWrong_StateMismatch proves the mismatch arm AND that
// the bind is checked BEFORE the code exchange: the authorization code must never be redeemed for
// a callback this browser did not initiate (redeeming it would consume the one-shot code at the
// IdP even though we reject the login).
func TestSSOService_HandleCallback_BrowserNonceWrong_StateMismatch(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, false)
	state := f.sealedState(t, p.ID, "nonce-victim")
	f.oidc.claims = &oidcClaims{Subject: "sub-alice", Email: "bob@acme.test", Nonce: "nonce-victim"}

	_, err := f.svc.HandleCallback(context.Background(), "auth-code", state, "nonce-from-another-flow")
	if !errors.Is(err, ErrSSOStateMismatch) {
		t.Fatalf("HandleCallback(wrong bind cookie) error = %v; want ErrSSOStateMismatch", err)
	}
	if f.oidc.lastCode != "" {
		t.Fatalf("HandleCallback(wrong bind cookie) redeemed the authorization code %q; bind must reject before exchange", f.oidc.lastCode)
	}
}

// TestSSOService_StartLogin_NonceRoundTripsThroughStateAndBind proves the full bind contract in
// one place: the nonce StartLogin returns opens the door that HandleCallback's constant-time
// compare guards — seal at /start, match at /callback, same value end to end.
func TestSSOService_StartLogin_NonceRoundTripsThroughStateAndBind(t *testing.T) {
	f := newSSOFixture(t)
	p := f.seedProvider(t, false)
	f.identities.rows[f.identities.key(f.tenantID, p.ID, "sub-alice")] = &auth.SSOIdentity{
		ID: "id-1", TenantID: f.tenantID, ProviderID: p.ID, UserID: "u-bob", Subject: "sub-alice",
	}

	redirectURL, nonce, err := f.svc.StartLogin(context.Background(), "acme", p.ID)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	// The fake AuthCodeURL embeds the sealed state via RAW string concatenation (no URL escaping
	// — fakeSealer's envelope is plaintext JSON whose `+` timezone offset would be mangled by a
	// Query().Get round-trip), so extract it the same raw way. The REAL oauth2-built URL's
	// escape/unescape round-trip is covered by the integration suite.
	state := strings.TrimPrefix(redirectURL, "https://idp.example.com/authorize?state=")
	if state == "" {
		t.Fatal("authorize URL carries no state param")
	}
	f.oidc.claims = &oidcClaims{Subject: "sub-alice", Email: "bob@acme.test", Nonce: nonce}

	out, err := f.svc.HandleCallback(context.Background(), "auth-code", state, nonce)
	if err != nil {
		t.Fatalf("HandleCallback(bound nonce from StartLogin): %v", err)
	}
	if out.AccessToken == "" || out.User == nil || out.User.ID != "u-bob" {
		t.Fatalf("bound round-trip did not issue the expected session: %+v", out)
	}
}
