package services

// SSO-FE-REDIRECT tests: StartHandoff runs the FULL callback verification pipeline and persists
// ONLY a hashed, short-lived, single-use handoff row (no session minted yet); ExchangeHandoff
// redeems that row atomically and completes the login through the SAME completeSSOLogin the
// direct JSON path uses (local-MFA gate + trust_idp_mfa honor + re-checks that the user is still
// active and the provider still enabled). Every "this code buys nothing" outcome folds into the
// one opaque auth.ErrSSOHandoffInvalid.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"nexus/platform/internal/auth"
)

// fakeSSOHandoffRepo is an in-memory stand-in for auth.SSOHandoffTokenRepository with the SAME
// semantics: FindByHash is tenant-agnostic and answers auth.ErrSSOHandoffInvalid on a miss (no
// used_at/expires_at filtering — that is ConsumeByHash's job); ConsumeByHash is the atomic
// single-winner conditional claim on (tenant_id, token_hash, used_at IS NULL, expires_at > now).
// The clock is injectable so the expiry test can move time past the TTL.
type fakeSSOHandoffRepo struct {
	mu   sync.Mutex
	rows map[string]*auth.SSOHandoffToken // keyed by token_hash (UNIQUE in the real table)
	now  func() time.Time
}

func newFakeSSOHandoffRepo() *fakeSSOHandoffRepo {
	return &fakeSSOHandoffRepo{rows: map[string]*auth.SSOHandoffToken{}, now: time.Now}
}

func (f *fakeSSOHandoffRepo) Store(_ context.Context, t *auth.SSOHandoffToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *t
	f.rows[t.TokenHash] = &cp
	return nil
}

func (f *fakeSSOHandoffRepo) FindByHash(_ context.Context, tokenHash string) (*auth.SSOHandoffToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[tokenHash]
	if !ok {
		return nil, auth.ErrSSOHandoffInvalid
	}
	cp := *row
	return &cp, nil
}

func (f *fakeSSOHandoffRepo) ConsumeByHash(_ context.Context, tenantID, tokenHash string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[tokenHash]
	if !ok || row.TenantID != tenantID || row.UsedAt != nil || !row.ExpiresAt.After(f.now()) {
		return false, nil
	}
	used := f.now()
	row.UsedAt = &used
	return true, nil
}

// count returns how many handoff rows exist — for the "no row persisted on failure" assertion.
func (f *fakeSSOHandoffRepo) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// newHandoffFixture wires the shared SSO fixture with an in-memory handoff store (mirrors
// main.go's unconditional WithHandoffStore call).
func newHandoffFixture(t *testing.T) (*ssoFixture, *fakeSSOHandoffRepo) {
	t.Helper()
	f := newSSOFixture(t)
	h := newFakeSSOHandoffRepo()
	f.svc.WithHandoffStore(h)
	return f, h
}

// handoffCallbackArgs mirrors the linked-identity happy-path setup of the HandleCallback tests:
// seed an enabled trust=false provider, link sub-alice→u-bob under it, and return ready-to-use
// (code, state, browserNonce) args for StartHandoff.
func handoffCallbackArgs(t *testing.T, f *ssoFixture) (code, state, browserNonce string) {
	t.Helper()
	p := f.seedProvider(t, false)
	f.identities.rows[f.identities.key(f.tenantID, p.ID, "sub-alice")] = &auth.SSOIdentity{
		ID: "id-1", TenantID: f.tenantID, ProviderID: p.ID, UserID: "u-bob", Subject: "sub-alice",
	}
	nonce := "nonce-1"
	state = f.sealedState(t, p.ID, nonce)
	f.oidc.claims = &oidcClaims{Subject: "sub-alice", Email: "bob@acme.test", Nonce: nonce}
	return "auth-code", state, nonce
}

// --- StartHandoff ------------------------------------------------------------------------------

// TestSSOService_StartHandoff_StoresHashOnly_NoSessionMinted pins the redirect mode's core
// storage contract: the returned raw code is NEVER persisted (only its SHA-256 hash is), the row
// carries exactly the identity resolveSSOUser resolved, ExpiresAt is now+120s — and, crucially,
// NO session exists yet (the fixture's refresh-token store stays empty): the tokens are minted at
// exchange time, so they can never appear in the redirect URL / browser history / access logs.
func TestSSOService_StartHandoff_StoresHashOnly_NoSessionMinted(t *testing.T) {
	f, h := newHandoffFixture(t)
	code, state, browserNonce := handoffCallbackArgs(t, f)
	fixed := time.Now()
	f.svc.now = func() time.Time { return fixed } // pin the clock for an exact ExpiresAt assert

	raw, err := f.svc.StartHandoff(context.Background(), code, state, browserNonce)
	if err != nil {
		t.Fatalf("StartHandoff: %v", err)
	}
	if raw == "" {
		t.Fatal("StartHandoff returned an empty raw code")
	}

	if h.count() != 1 {
		t.Fatalf("handoff rows = %d; want exactly 1", h.count())
	}
	if _, ok := h.rows[raw]; ok {
		t.Fatal("the RAW handoff code was persisted as the key — only its hash may be stored")
	}
	row, ok := h.rows[auth.HashRefreshToken(raw)]
	if !ok {
		t.Fatalf("no row stored under auth.HashRefreshToken(raw); rows = %+v", h.rows)
	}
	if row.TokenHash != auth.HashRefreshToken(raw) || row.TokenHash == raw {
		t.Fatalf("row.TokenHash = %q; want the SHA-256 hex of the raw code, never the raw itself", row.TokenHash)
	}
	if row.TenantID != f.tenantID || row.UserID != "u-bob" || row.ProviderID != "p-1" {
		t.Fatalf("row identity = {tenant:%q user:%q provider:%q}; want {t-1 u-bob p-1}", row.TenantID, row.UserID, row.ProviderID)
	}
	if !row.ExpiresAt.Equal(fixed.Add(ssoHandoffTTL)) {
		t.Fatalf("row.ExpiresAt = %v; want now+%v (%v)", row.ExpiresAt, ssoHandoffTTL, fixed.Add(ssoHandoffTTL))
	}
	if row.UsedAt != nil {
		t.Fatalf("fresh row already has UsedAt = %v; want nil", row.UsedAt)
	}
	// No session minted at /callback time: the shared AuthService's refresh store must be empty.
	rr := f.authSvc.refresh.(*fakeRefreshRepo)
	rr.mu.Lock()
	stored := len(rr.rows)
	rr.mu.Unlock()
	if stored != 0 {
		t.Fatalf("StartHandoff minted %d refresh token(s); the session must only be minted at ExchangeHandoff", stored)
	}
}

// TestSSOService_StartHandoff_Unavailable_WithoutStore: without WithHandoffStore (main.go not
// wiring the repo) the redirect mode is inert — ErrSSOUnavailable, mirroring the sealer gating.
func TestSSOService_StartHandoff_Unavailable_WithoutStore(t *testing.T) {
	f := newSSOFixture(t) // no WithHandoffStore
	code, state, browserNonce := handoffCallbackArgs(t, f)

	_, err := f.svc.StartHandoff(context.Background(), code, state, browserNonce)
	if !errors.Is(err, ErrSSOUnavailable) {
		t.Fatalf("StartHandoff(no store) error = %v; want ErrSSOUnavailable", err)
	}
}

// TestSSOService_StartHandoff_ResolveFails_PropagatesAndStoresNothing proves StartHandoff runs
// the SAME verification pipeline as the direct path (resolveSSOUser) BEFORE anything persists: a
// failing verification (blank state here → ErrSSOStateInvalid, exactly as HandleCallback) must
// surface the ORIGINAL classified error and leave zero handoff rows behind.
func TestSSOService_StartHandoff_ResolveFails_PropagatesAndStoresNothing(t *testing.T) {
	f, h := newHandoffFixture(t)
	f.seedProvider(t, false)

	_, err := f.svc.StartHandoff(context.Background(), "auth-code", "", "nonce-1")
	if !errors.Is(err, ErrSSOStateInvalid) {
		t.Fatalf("StartHandoff(blank state) error = %v; want ErrSSOStateInvalid (same classification as HandleCallback)", err)
	}
	if h.count() != 0 {
		t.Fatalf("a failed verification persisted %d handoff row(s); want 0", h.count())
	}
}

// --- ExchangeHandoff ---------------------------------------------------------------------------

// TestSSOService_ExchangeHandoff_Happy_MintsSessionLikeLogin: redeeming a fresh code completes
// the login with the exact password-login LoginOutput shape — token pair + user, no mfaPending.
func TestSSOService_ExchangeHandoff_Happy_MintsSessionLikeLogin(t *testing.T) {
	f, _ := newHandoffFixture(t)
	code, state, browserNonce := handoffCallbackArgs(t, f)
	raw, err := f.svc.StartHandoff(context.Background(), code, state, browserNonce)
	if err != nil {
		t.Fatalf("StartHandoff: %v", err)
	}

	out, err := f.svc.ExchangeHandoff(context.Background(), raw)
	if err != nil {
		t.Fatalf("ExchangeHandoff: %v", err)
	}
	if out.AccessToken == "" || out.RefreshToken == "" {
		t.Fatalf("ExchangeHandoff did not issue a token pair: %+v", out)
	}
	if out.MFAPending || out.PendingToken != "" {
		t.Fatalf("ExchangeHandoff(no MFA) = %+v; want a direct session, not the mfaPending shape", out)
	}
	if out.User == nil || out.User.ID != "u-bob" || out.User.TenantID != f.tenantID {
		t.Fatalf("ExchangeHandoff issued a session for the wrong user: %+v", out.User)
	}
}

// TestSSOService_ExchangeHandoff_SecondUse_Invalid pins single-use: the second redeem of the SAME
// code loses the atomic conditional claim and folds into the opaque auth.ErrSSOHandoffInvalid.
func TestSSOService_ExchangeHandoff_SecondUse_Invalid(t *testing.T) {
	f, _ := newHandoffFixture(t)
	code, state, browserNonce := handoffCallbackArgs(t, f)
	raw, err := f.svc.StartHandoff(context.Background(), code, state, browserNonce)
	if err != nil {
		t.Fatalf("StartHandoff: %v", err)
	}
	if _, err := f.svc.ExchangeHandoff(context.Background(), raw); err != nil {
		t.Fatalf("first ExchangeHandoff: %v", err)
	}

	_, err = f.svc.ExchangeHandoff(context.Background(), raw)
	if !errors.Is(err, auth.ErrSSOHandoffInvalid) {
		t.Fatalf("second ExchangeHandoff error = %v; want auth.ErrSSOHandoffInvalid (single-use)", err)
	}
}

// TestSSOService_ExchangeHandoff_Expired_Invalid: a code redeemed past ssoHandoffTTL buys nothing.
// The service and repo share one injectable clock; time moves 1s past the TTL between the store
// and the exchange, so the conditional claim's expires_at > now arm refuses it.
func TestSSOService_ExchangeHandoff_Expired_Invalid(t *testing.T) {
	f, h := newHandoffFixture(t)
	code, state, browserNonce := handoffCallbackArgs(t, f)
	base := time.Now()
	current := base
	f.svc.now = func() time.Time { return current }
	h.now = func() time.Time { return current }

	raw, err := f.svc.StartHandoff(context.Background(), code, state, browserNonce)
	if err != nil {
		t.Fatalf("StartHandoff: %v", err)
	}
	current = base.Add(ssoHandoffTTL + time.Second) // past the 120s window

	_, err = f.svc.ExchangeHandoff(context.Background(), raw)
	if !errors.Is(err, auth.ErrSSOHandoffInvalid) {
		t.Fatalf("ExchangeHandoff(expired) error = %v; want auth.ErrSSOHandoffInvalid", err)
	}
}

// TestSSOService_ExchangeHandoff_UnknownOrEmptyCode_Invalid: probing with garbage, an empty
// string, or whitespace reveals nothing — every miss is the same opaque sentinel.
func TestSSOService_ExchangeHandoff_UnknownOrEmptyCode_Invalid(t *testing.T) {
	f, _ := newHandoffFixture(t)
	for _, rawCode := range []string{"", "   ", "no-such-code"} {
		_, err := f.svc.ExchangeHandoff(context.Background(), rawCode)
		if !errors.Is(err, auth.ErrSSOHandoffInvalid) {
			t.Fatalf("ExchangeHandoff(%q) error = %v; want auth.ErrSSOHandoffInvalid", rawCode, err)
		}
	}
}

// TestSSOService_ExchangeHandoff_MFAEnrolled_ReturnsPending proves the redirect mode CANNOT
// bypass the local-MFA gate: an enrolled account gets the same {mfaPending, pendingToken}
// two-step shape as the direct path (completeSSOLogin is shared by construction) — no token pair.
func TestSSOService_ExchangeHandoff_MFAEnrolled_ReturnsPending(t *testing.T) {
	f, _ := newHandoffFixture(t)
	f.enableMFA(t, "u-bob")
	code, state, browserNonce := handoffCallbackArgs(t, f)
	raw, err := f.svc.StartHandoff(context.Background(), code, state, browserNonce)
	if err != nil {
		t.Fatalf("StartHandoff: %v", err)
	}

	out, err := f.svc.ExchangeHandoff(context.Background(), raw)
	if err != nil {
		t.Fatalf("ExchangeHandoff(MFA enrolled): %v", err)
	}
	if !out.MFAPending || out.PendingToken == "" {
		t.Fatalf("ExchangeHandoff(MFA enrolled) = %+v; want the mfaPending shape", out)
	}
	if out.AccessToken != "" || out.RefreshToken != "" {
		t.Fatalf("ExchangeHandoff(MFA enrolled) must not mint a token pair before MFA verify: %+v", out)
	}
}

// TestSSOService_ExchangeHandoff_TrustIdpMfa_SkipsLocalGate: a trust_idp_mfa=1 provider skips the
// local gate on the exchange path too (SSO-MFA-POLICY parity with HandleCallback) — an enrolled
// account still gets a direct token pair because the admin delegated MFA to the IdP.
func TestSSOService_ExchangeHandoff_TrustIdpMfa_SkipsLocalGate(t *testing.T) {
	f, _ := newHandoffFixture(t)
	f.enableMFA(t, "u-bob")
	code, state, browserNonce := f.trustCallbackArgs(t, true) // trust=on provider, sub-alice→u-bob
	raw, err := f.svc.StartHandoff(context.Background(), code, state, browserNonce)
	if err != nil {
		t.Fatalf("StartHandoff: %v", err)
	}

	out, err := f.svc.ExchangeHandoff(context.Background(), raw)
	if err != nil {
		t.Fatalf("ExchangeHandoff(trust=on, enrolled): %v", err)
	}
	if out.MFAPending || out.PendingToken != "" {
		t.Fatalf("ExchangeHandoff(trust=on, enrolled) = %+v; want a direct session, not mfaPending", out)
	}
	if out.AccessToken == "" || out.RefreshToken == "" || out.User == nil || out.User.ID != "u-bob" {
		t.Fatalf("ExchangeHandoff(trust=on, enrolled) did not issue the expected session: %+v", out)
	}
}

// TestSSOService_ExchangeHandoff_ProviderDisabledMidWindow_Invalid: an admin disabling the
// provider INSIDE the 120s window must kill the pending handoff — mirrors the /start→/callback
// re-resolve discipline, folded into the opaque sentinel (no "provider exists but disabled" leak).
func TestSSOService_ExchangeHandoff_ProviderDisabledMidWindow_Invalid(t *testing.T) {
	f, _ := newHandoffFixture(t)
	code, state, browserNonce := handoffCallbackArgs(t, f)
	raw, err := f.svc.StartHandoff(context.Background(), code, state, browserNonce)
	if err != nil {
		t.Fatalf("StartHandoff: %v", err)
	}
	f.providers.rows["p-1"].Enabled = false // admin revokes the provider mid-window

	_, err = f.svc.ExchangeHandoff(context.Background(), raw)
	if !errors.Is(err, auth.ErrSSOHandoffInvalid) {
		t.Fatalf("ExchangeHandoff(provider disabled mid-window) error = %v; want auth.ErrSSOHandoffInvalid", err)
	}
}

// TestSSOService_ExchangeHandoff_UserDisabledMidWindow_Invalid: the user being disabled between
// /callback and /exchange must also kill the handoff — the exchange re-loads the user and
// requires it still active, exactly like the direct path's orphaned-link discipline.
func TestSSOService_ExchangeHandoff_UserDisabledMidWindow_Invalid(t *testing.T) {
	f, _ := newHandoffFixture(t)
	code, state, browserNonce := handoffCallbackArgs(t, f)
	raw, err := f.svc.StartHandoff(context.Background(), code, state, browserNonce)
	if err != nil {
		t.Fatalf("StartHandoff: %v", err)
	}
	f.users.users["t-1\x00bob@acme.test"].Status = "disabled"

	_, err = f.svc.ExchangeHandoff(context.Background(), raw)
	if !errors.Is(err, auth.ErrSSOHandoffInvalid) {
		t.Fatalf("ExchangeHandoff(user disabled mid-window) error = %v; want auth.ErrSSOHandoffInvalid", err)
	}
}

// TestSSOService_ExchangeHandoff_Unavailable_WithoutStore mirrors StartHandoff's gating on the
// exchange side: no store wired → ErrSSOUnavailable, never a nil-deref.
func TestSSOService_ExchangeHandoff_Unavailable_WithoutStore(t *testing.T) {
	f := newSSOFixture(t) // no WithHandoffStore
	_, err := f.svc.ExchangeHandoff(context.Background(), "any-code")
	if !errors.Is(err, ErrSSOUnavailable) {
		t.Fatalf("ExchangeHandoff(no store) error = %v; want ErrSSOUnavailable", err)
	}
}
