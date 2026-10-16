package services

// SSO-MFA-POLICY tests: the per-provider trust_idp_mfa opt-in (migration 000040) lets a
// FOUND-IDENTITY SSO login skip the local MFA gate (mfaLoginGate) — blanket trust in the IdP's
// own MFA, no per-login AMR/acr inspection (documented assumption, api-conventions.md §3c.8).
// The load-bearing property proven here is the DEFAULT's safety: with the flag OFF (the zero
// value every pre-existing and newly created provider has), NOTHING changes — enrolled accounts
// still get {mfaPending, pendingToken}, and the MFA-ENFORCE key-absent fail-closed 503 still
// fires. Only an explicitly trusted provider skips the gate.

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/auth"
)

// seedTrustProvider creates and returns an enabled provider row with an explicit TrustIdpMfa
// value directly in the fake repo (mirrors ssoFixture.seedProvider, which always leaves the flag
// at its zero value / false).
func (f *ssoFixture) seedTrustProvider(t *testing.T, jit, trust bool) *auth.SSOProvider {
	t.Helper()
	p := &auth.SSOProvider{
		ID: "p-trust", TenantID: f.tenantID, Issuer: "https://idp-trust.example.com",
		ClientID: "client-t", ClientSecretEnc: "sealed:shh", Enabled: true,
		JITProvisioning: jit, TrustIdpMfa: trust,
	}
	if err := f.providers.Create(context.Background(), p); err != nil {
		t.Fatalf("seed trust provider: %v", err)
	}
	return p
}

// trustCallbackArgs links sub-alice→u-bob under a provider with the given TrustIdpMfa value and
// returns ready-to-use HandleCallback args (mirrors linkedCallbackArgs, which only ever seeds a
// trust=false provider).
func (f *ssoFixture) trustCallbackArgs(t *testing.T, trust bool) (code, state, browserNonce string) {
	t.Helper()
	p := f.seedTrustProvider(t, false, trust)
	f.identities.rows[f.identities.key(f.tenantID, p.ID, "sub-alice")] = &auth.SSOIdentity{
		ID: "id-1", TenantID: f.tenantID, ProviderID: p.ID, UserID: "u-bob", Subject: "sub-alice",
	}
	nonce := "nonce-1"
	state = f.sealedState(t, p.ID, nonce)
	f.oidc.claims = &oidcClaims{Subject: "sub-alice", Email: "bob@acme.test", Nonce: nonce}
	return "auth-code", state, nonce
}

// TestSSOService_HandleCallback_TrustOn_Enrolled_SkipsLocalMFA is the slice's core behavior: a
// found-identity login through a trust_idp_mfa=1 provider for an account that IS enrolled in
// local MFA gets a DIRECT session — no {mfaPending, pendingToken} detour — because the admin
// explicitly declared the IdP's own MFA sufficient.
func TestSSOService_HandleCallback_TrustOn_Enrolled_SkipsLocalMFA(t *testing.T) {
	f := newSSOFixture(t)
	f.enableMFA(t, "u-bob") // full WithMFA wiring + ENABLED user_mfa row for u-bob
	code, state, browserNonce := f.trustCallbackArgs(t, true)

	out, err := f.svc.HandleCallback(context.Background(), code, state, browserNonce)
	if err != nil {
		t.Fatalf("HandleCallback(trust=on, enrolled): %v", err)
	}
	if out.MFAPending || out.PendingToken != "" {
		t.Fatalf("HandleCallback(trust=on, enrolled) = %+v; want a direct session, not the mfaPending shape", out)
	}
	if out.AccessToken == "" || out.RefreshToken == "" || out.User == nil || out.User.ID != "u-bob" {
		t.Fatalf("HandleCallback(trust=on, enrolled) did not issue the expected session: %+v", out)
	}
}

// TestSSOService_HandleCallback_TrustOn_NotEnrolled_DirectSession: trust=on for a NEVER-enrolled
// account behaves exactly like before this slice — direct session (the flag can only ever remove
// the local second step, never add one).
func TestSSOService_HandleCallback_TrustOn_NotEnrolled_DirectSession(t *testing.T) {
	f := newSSOFixture(t)
	f.authSvc.WithMFA(newFakeMFARepo(), newFakeBackupRepo(), newFakePendingRepo(), fakeSealer{}) // wired, u-bob NOT enrolled
	code, state, browserNonce := f.trustCallbackArgs(t, true)

	out, err := f.svc.HandleCallback(context.Background(), code, state, browserNonce)
	if err != nil {
		t.Fatalf("HandleCallback(trust=on, not enrolled): %v", err)
	}
	if out.MFAPending || out.AccessToken == "" || out.RefreshToken == "" {
		t.Fatalf("HandleCallback(trust=on, not enrolled) must issue a direct session: %+v", out)
	}
}

// TestSSOService_HandleCallback_TrustOff_Enrolled_StillPending pins the DEFAULT's safety with the
// flag EXPLICITLY false (not merely zero-valued): an enrolled account through a trust=off
// provider still gets the two-step {mfaPending, pendingToken} shape — SSO-MFA-POLICY must not
// have weakened the FIX 2 / MFA-ENFORCE behavior for untrusted providers.
func TestSSOService_HandleCallback_TrustOff_Enrolled_StillPending(t *testing.T) {
	f := newSSOFixture(t)
	f.enableMFA(t, "u-bob")
	code, state, browserNonce := f.trustCallbackArgs(t, false)

	out, err := f.svc.HandleCallback(context.Background(), code, state, browserNonce)
	if err != nil {
		t.Fatalf("HandleCallback(trust=off, enrolled): %v", err)
	}
	if !out.MFAPending || out.PendingToken == "" {
		t.Fatalf("HandleCallback(trust=off, enrolled) = %+v; want the mfaPending shape (default unchanged)", out)
	}
	if out.AccessToken != "" || out.RefreshToken != "" {
		t.Fatalf("HandleCallback(trust=off, enrolled) must not mint a token pair before MFA verify: %+v", out)
	}
}

// TestSSOService_HandleCallback_TrustOff_Enrolled_KeyAbsent_StillFailClosed is the MFA-ENFORCE
// regression demanded by this slice's brief: with trust OFF, an enrolled account on a server
// whose MFA key is ABSENT (enrollment reader wired, WithMFA never called — production "key lost"
// shape) must STILL be refused with the 503-family sentinel. The trust flag being false must
// leave the fail-closed guarantee byte-identical.
func TestSSOService_HandleCallback_TrustOff_Enrolled_KeyAbsent_StillFailClosed(t *testing.T) {
	f := newSSOFixture(t)
	reader := newFakeMFARepo()
	enrollEnabledRow(reader, f.tenantID, "u-bob")
	f.authSvc.WithMFAEnrollmentReader(reader, false) // reader only — no WithMFA (key absent)
	code, state, browserNonce := f.trustCallbackArgs(t, false)

	out, err := f.svc.HandleCallback(context.Background(), code, state, browserNonce)
	if !errors.Is(err, ErrMFAEnrolledButUnavailable) {
		t.Fatalf("HandleCallback(trust=off, enrolled, key absent) err = %v; want ErrMFAEnrolledButUnavailable (fail-closed unchanged)", err)
	}
	if out != nil {
		t.Fatalf("HandleCallback(trust=off, enrolled, key absent) output = %+v; want nil", out)
	}
}

// TestSSOService_HandleCallback_TrustOn_Enrolled_KeyAbsent_DirectSession documents the flag's
// full consequence (deliberate, part of what the admin accepts by flipping it): with trust ON the
// local gate never runs, so the key-absent server state is irrelevant on this path — the login
// completes directly instead of 503ing. This is NOT a hole in MFA-ENFORCE: there is no local
// second factor being silently skipped by a DEGRADED server here; the admin explicitly delegated
// the second factor to the IdP for this provider.
func TestSSOService_HandleCallback_TrustOn_Enrolled_KeyAbsent_DirectSession(t *testing.T) {
	f := newSSOFixture(t)
	reader := newFakeMFARepo()
	enrollEnabledRow(reader, f.tenantID, "u-bob")
	f.authSvc.WithMFAEnrollmentReader(reader, false) // key absent
	code, state, browserNonce := f.trustCallbackArgs(t, true)

	out, err := f.svc.HandleCallback(context.Background(), code, state, browserNonce)
	if err != nil {
		t.Fatalf("HandleCallback(trust=on, enrolled, key absent): %v", err)
	}
	if out.MFAPending || out.AccessToken == "" || out.RefreshToken == "" {
		t.Fatalf("HandleCallback(trust=on, enrolled, key absent) must issue a direct session: %+v", out)
	}
}

// --- UpdateProviderPolicy (admin partial update) ----------------------------------------------

// TestSSOService_UpdateProviderPolicy_PartialUpdate proves the nil-leaves-unchanged contract in
// both directions plus the both-set case and the tenant-pinned not-found.
func TestSSOService_UpdateProviderPolicy_PartialUpdate(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	ctx := context.Background()

	t.Run("trust only — enabled untouched", func(t *testing.T) {
		f := newSSOFixture(t)
		p := f.seedProvider(t, false) // enabled=true, trust=false
		got, err := f.svc.UpdateProviderPolicy(ctx, f.tenantID, p.ID, nil, boolPtr(true))
		if err != nil {
			t.Fatalf("UpdateProviderPolicy(trust only): %v", err)
		}
		if !got.TrustIdpMfa || !got.Enabled {
			t.Fatalf("UpdateProviderPolicy(trust only) = {enabled:%v trust:%v}; want {enabled:true trust:true}", got.Enabled, got.TrustIdpMfa)
		}
	})

	t.Run("enabled only — trust untouched", func(t *testing.T) {
		f := newSSOFixture(t)
		p := f.seedTrustProvider(t, false, true) // enabled=true, trust=true
		got, err := f.svc.UpdateProviderPolicy(ctx, f.tenantID, p.ID, boolPtr(false), nil)
		if err != nil {
			t.Fatalf("UpdateProviderPolicy(enabled only): %v", err)
		}
		if got.Enabled || !got.TrustIdpMfa {
			t.Fatalf("UpdateProviderPolicy(enabled only) = {enabled:%v trust:%v}; want {enabled:false trust:true}", got.Enabled, got.TrustIdpMfa)
		}
	})

	t.Run("both set", func(t *testing.T) {
		f := newSSOFixture(t)
		p := f.seedProvider(t, false)
		got, err := f.svc.UpdateProviderPolicy(ctx, f.tenantID, p.ID, boolPtr(false), boolPtr(true))
		if err != nil {
			t.Fatalf("UpdateProviderPolicy(both): %v", err)
		}
		if got.Enabled || !got.TrustIdpMfa {
			t.Fatalf("UpdateProviderPolicy(both) = {enabled:%v trust:%v}; want {enabled:false trust:true}", got.Enabled, got.TrustIdpMfa)
		}
	})

	t.Run("trust back OFF is not a no-op", func(t *testing.T) {
		f := newSSOFixture(t)
		p := f.seedTrustProvider(t, false, true)
		got, err := f.svc.UpdateProviderPolicy(ctx, f.tenantID, p.ID, nil, boolPtr(false))
		if err != nil {
			t.Fatalf("UpdateProviderPolicy(trust off): %v", err)
		}
		if got.TrustIdpMfa {
			t.Fatal("UpdateProviderPolicy(trust off) left trustIdpMfa=true — the OFF write was silently skipped")
		}
	})

	t.Run("unknown id — not found", func(t *testing.T) {
		f := newSSOFixture(t)
		if _, err := f.svc.UpdateProviderPolicy(ctx, f.tenantID, "does-not-exist", nil, boolPtr(true)); !errors.Is(err, auth.ErrSSOProviderNotFound) {
			t.Fatalf("UpdateProviderPolicy(unknown id) err = %v; want auth.ErrSSOProviderNotFound", err)
		}
	})

	t.Run("other tenant's provider — not found", func(t *testing.T) {
		f := newSSOFixture(t)
		other := &auth.SSOProvider{ID: "p-other", TenantID: "t-2", Issuer: "https://other.example.com", ClientID: "c", ClientSecretEnc: "sealed:x", Enabled: true}
		if err := f.providers.Create(ctx, other); err != nil {
			t.Fatalf("seed other-tenant provider: %v", err)
		}
		if _, err := f.svc.UpdateProviderPolicy(ctx, f.tenantID, other.ID, nil, boolPtr(true)); !errors.Is(err, auth.ErrSSOProviderNotFound) {
			t.Fatalf("UpdateProviderPolicy(cross-tenant) err = %v; want auth.ErrSSOProviderNotFound", err)
		}
	})
}

// TestSSOService_CreateProvider_TrustDefaultsFalse pins the create-path contract: trustIdpMfa is
// NOT part of CreateSSOProviderInput — a freshly created provider is ALWAYS trust=false and an
// admin must flip it via the PATCH as an explicit second decision.
func TestSSOService_CreateProvider_TrustDefaultsFalse(t *testing.T) {
	f := newSSOFixture(t)
	p, err := f.svc.CreateProvider(context.Background(), f.tenantID, CreateSSOProviderInput{
		Issuer: "https://idp.example.com", ClientID: "client-1", ClientSecret: "top-secret",
	})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if p.TrustIdpMfa {
		t.Fatal("CreateProvider returned trustIdpMfa=true; a new provider must default to false (enforce local MFA)")
	}
	stored, err := f.providers.GetByID(context.Background(), f.tenantID, p.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.TrustIdpMfa {
		t.Fatal("stored provider has trustIdpMfa=true; create path must persist false")
	}
}
