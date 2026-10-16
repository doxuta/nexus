package services

// MFA-ENFORCE tests: losing MFA_SECRET_ENC_KEY must FAIL CLOSED for MFA-enrolled accounts at
// login (password AND SSO paths), never silently downgrade them to one-step login. The
// "key absent" server state is modeled exactly like production: the enrollment reader is wired
// (WithMFAEnrollmentReader — needs only the DB) while WithMFA is never called (no sealer).

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"nexus/platform/internal/auth"
)

// newEnforceFixture builds a production-shaped "key absent" AuthService: a bare service (no
// WithMFA — no sealer) with the standalone enrollment reader wired, plus a zap observer at
// Error level so the mandatory loud logging can be asserted. The seeded user is the same
// acme/alice/u-1 newTestService seeds.
func newEnforceFixture(t *testing.T, failOpen bool) (*AuthService, *fakeMFARepo, *observer.ObservedLogs, string) {
	t.Helper()
	svc, password := newTestService(t)
	core, logs := observer.New(zapcore.ErrorLevel)
	reader := newFakeMFARepo()
	svc.WithLogger(zap.New(core)).WithMFAEnrollmentReader(reader, failOpen)
	return svc, reader, logs, password
}

// enrollEnabledRow seeds an ENABLED user_mfa row directly (the account enrolled while the key
// was still present; the sealed secret is now undecryptable server-side, but the enabled flag
// — the only thing the login gate reads — is plain data).
func enrollEnabledRow(reader *fakeMFARepo, tenantID, userID string) {
	reader.rows[reader.key(tenantID, userID)] = &auth.UserMFA{
		ID: "mfa-" + userID, TenantID: tenantID, UserID: userID,
		SecretEnc: "sealed:unreadable-without-key", Enabled: true,
	}
}

// TestLogin_Enrolled_KeyAbsent_FailClosed is the core MFA-ENFORCE guarantee: correct password
// on an MFA-enrolled account while the server lacks the MFA key → the typed 503 sentinel — not
// a successful one-step login (the old silent downgrade), and not a 401 credential error (the
// password was right; the SERVER is what is broken).
func TestLogin_Enrolled_KeyAbsent_FailClosed(t *testing.T) {
	svc, reader, logs, password := newEnforceFixture(t, false)
	enrollEnabledRow(reader, "t-1", "u-1")

	out, err := svc.Login(context.Background(), LoginInput{Tenant: "acme", Email: "alice@acme.test", Password: password})
	if !errors.Is(err, ErrMFAEnrolledButUnavailable) {
		t.Fatalf("Login(enrolled, key absent) err = %v; want ErrMFAEnrolledButUnavailable", err)
	}
	if errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("Login(enrolled, key absent) collapsed to ErrInvalidCredentials — must stay a distinct 503-family sentinel")
	}
	if out != nil {
		t.Fatalf("Login(enrolled, key absent) output = %+v; want nil (no tokens, no pending shape)", out)
	}

	// The block must be LOUD: one zap.Error carrying the ids (never more PII than ids).
	entries := logs.FilterLevelExact(zapcore.ErrorLevel).All()
	if len(entries) != 1 {
		t.Fatalf("want exactly 1 error-level log for the fail-closed block, got %d: %+v", len(entries), entries)
	}
	fields := entries[0].ContextMap()
	if fields["tenant_id"] != "t-1" || fields["user_id"] != "u-1" {
		t.Errorf("fail-closed log fields = %v; want tenant_id=t-1 user_id=u-1", fields)
	}
}

// TestLogin_Enrolled_KeyAbsent_FailOpen proves the INFRA-EMERGENCY kill-switch: with
// MFA_FAIL_OPEN the enrolled user gets a NORMAL one-step login (full token pair + user), but
// the downgrade is logged at zap.Error on every occurrence.
func TestLogin_Enrolled_KeyAbsent_FailOpen(t *testing.T) {
	svc, reader, logs, password := newEnforceFixture(t, true)
	enrollEnabledRow(reader, "t-1", "u-1")

	out, err := svc.Login(context.Background(), LoginInput{Tenant: "acme", Email: "alice@acme.test", Password: password})
	if err != nil {
		t.Fatalf("Login(enrolled, key absent, fail-open): %v", err)
	}
	if out.MFAPending || out.PendingToken != "" {
		t.Fatalf("fail-open login must be one-step, got pending shape: %+v", out)
	}
	if out.AccessToken == "" || out.RefreshToken == "" || out.User == nil {
		t.Fatalf("fail-open login returned incomplete session: %+v", out)
	}

	entries := logs.FilterLevelExact(zapcore.ErrorLevel).All()
	if len(entries) != 1 {
		t.Fatalf("fail-open login must log exactly 1 error-level entry per login, got %d", len(entries))
	}
	fields := entries[0].ContextMap()
	if fields["tenant_id"] != "t-1" || fields["user_id"] != "u-1" {
		t.Errorf("fail-open log fields = %v; want tenant_id=t-1 user_id=u-1", fields)
	}

	// Every subsequent fail-open login logs AGAIN — the alarm must not be one-shot.
	if _, err := svc.Login(context.Background(), LoginInput{Tenant: "acme", Email: "alice@acme.test", Password: password}); err != nil {
		t.Fatalf("second fail-open login: %v", err)
	}
	if got := len(logs.FilterLevelExact(zapcore.ErrorLevel).All()); got != 2 {
		t.Fatalf("after 2 fail-open logins want 2 error logs, got %d", got)
	}
}

// TestLogin_NotEnrolled_KeyAbsent_ByteIdentical extends TestLogin_NonMFA_ResponseByteIdentical
// with the NEW production wiring (enrollment reader present, no WithMFA): a never-enrolled
// user's login response shape must be identical to the bare pre-MFA service's — the reader is
// a true no-op for non-enrolled accounts.
func TestLogin_NotEnrolled_KeyAbsent_ByteIdentical(t *testing.T) {
	ctx := context.Background()
	in := LoginInput{Tenant: "acme", Email: "alice@acme.test", Password: "s3cret-pw"}

	svcBare, _ := newTestService(t)
	outBare, err := svcBare.Login(ctx, in)
	if err != nil {
		t.Fatalf("Login (bare service): %v", err)
	}

	svcReader, _, logs, _ := newEnforceFixture(t, false) // reader wired, user NEVER enrolled
	outReader, err := svcReader.Login(ctx, in)
	if err != nil {
		t.Fatalf("Login (reader wired, not enrolled): %v", err)
	}
	if got := len(logs.All()); got != 0 {
		t.Errorf("non-enrolled login must not log anything at error level, got %d entries", got)
	}

	shapeOf := func(out *LoginOutput) string {
		raw, mErr := json.Marshal(out)
		if mErr != nil {
			t.Fatalf("marshal: %v", mErr)
		}
		var m map[string]any
		if uErr := json.Unmarshal(raw, &m); uErr != nil {
			t.Fatalf("unmarshal: %v", uErr)
		}
		delete(m, "accessToken") // freshly minted per call — always differs
		delete(m, "refreshToken")
		norm, mErr := json.Marshal(m)
		if mErr != nil {
			t.Fatalf("re-marshal: %v", mErr)
		}
		return string(norm)
	}
	if a, b := shapeOf(outBare), shapeOf(outReader); a != b {
		t.Fatalf("non-enrolled login shape diverged with reader wired:\n  bare:   %s\n  reader: %s", a, b)
	}
}

// TestLogin_Enrolled_KeyPresent_StillPending is the regression guard: when FULL MFA support is
// wired (key present), the always-wired enrollment reader must change nothing — an enrolled
// account still gets the two-step {mfaPending, pendingToken} shape, never the 503 sentinel.
func TestLogin_Enrolled_KeyPresent_StillPending(t *testing.T) {
	f := newMFAFixture(t)
	// Production wiring order: reader ALWAYS, WithMFA additionally when the key is present.
	f.svc.WithMFAEnrollmentReader(newFakeMFARepo(), false) // a DIFFERENT (empty) reader instance:
	// mfaRepo (full support) must win the source selection, or this enrolled user would look
	// non-enrolled through the empty standalone reader.
	f.enrollAndActivate(t)

	out, err := f.svc.Login(context.Background(), LoginInput{Tenant: "acme", Email: f.email, Password: f.password})
	if err != nil {
		t.Fatalf("Login(enrolled, full MFA wired): %v", err)
	}
	if !out.MFAPending || out.PendingToken == "" {
		t.Fatalf("Login(enrolled, full MFA wired) = %+v; want mfaPending shape", out)
	}
}

// --- SSO parity (found-identity path must obey the SAME gate) --------------------------------

// linkIdentity links (provider p, subject sub-alice) to u-bob and returns a valid sealed state
// + matching claims, mirroring the FIX 2 tests' setup.
func (f *ssoFixture) linkedCallbackArgs(t *testing.T) (code, state, browserNonce string) {
	t.Helper()
	p := f.seedProvider(t, false)
	f.identities.rows[f.identities.key(f.tenantID, p.ID, "sub-alice")] = &auth.SSOIdentity{
		ID: "id-1", TenantID: f.tenantID, ProviderID: p.ID, UserID: "u-bob", Subject: "sub-alice",
	}
	nonce := "nonce-1"
	state = f.sealedState(t, p.ID, nonce)
	f.oidc.claims = &oidcClaims{Subject: "sub-alice", Email: "bob@acme.test", Nonce: nonce}
	// SSO-STATE-COOKIE: HandleCallback now also takes the browser bind nonce (the sso_state
	// cookie value) — return the state's own nonce so these MFA-parity tests keep exercising
	// the gate, not the bind.
	return "auth-code", state, nonce
}

// TestSSOService_HandleCallback_Enrolled_KeyAbsent_FailClosed: without this arm, SSO login
// would be the exact bypass MFA-ENFORCE closes on the password path — an enrolled account's
// found-identity SSO login while the key is missing must surface the SAME 503 sentinel.
func TestSSOService_HandleCallback_Enrolled_KeyAbsent_FailClosed(t *testing.T) {
	f := newSSOFixture(t)
	reader := newFakeMFARepo()
	enrollEnabledRow(reader, f.tenantID, "u-bob")
	f.authSvc.WithMFAEnrollmentReader(reader, false) // reader only — no WithMFA (key absent)
	code, state, browserNonce := f.linkedCallbackArgs(t)

	out, err := f.svc.HandleCallback(context.Background(), code, state, browserNonce)
	if !errors.Is(err, ErrMFAEnrolledButUnavailable) {
		t.Fatalf("HandleCallback(enrolled, key absent) err = %v; want ErrMFAEnrolledButUnavailable", err)
	}
	if out != nil {
		t.Fatalf("HandleCallback(enrolled, key absent) output = %+v; want nil", out)
	}
}

// TestSSOService_HandleCallback_Enrolled_KeyAbsent_FailOpen: the emergency flag is honored on
// the SSO path too — a direct session is issued (and the shared gate logs the downgrade).
func TestSSOService_HandleCallback_Enrolled_KeyAbsent_FailOpen(t *testing.T) {
	f := newSSOFixture(t)
	core, logs := observer.New(zapcore.ErrorLevel)
	f.authSvc.WithLogger(zap.New(core))
	reader := newFakeMFARepo()
	enrollEnabledRow(reader, f.tenantID, "u-bob")
	f.authSvc.WithMFAEnrollmentReader(reader, true)
	code, state, browserNonce := f.linkedCallbackArgs(t)

	out, err := f.svc.HandleCallback(context.Background(), code, state, browserNonce)
	if err != nil {
		t.Fatalf("HandleCallback(enrolled, key absent, fail-open): %v", err)
	}
	if out.MFAPending || out.AccessToken == "" || out.RefreshToken == "" {
		t.Fatalf("fail-open SSO login must issue a direct session: %+v", out)
	}
	if got := len(logs.FilterLevelExact(zapcore.ErrorLevel).All()); got != 1 {
		t.Fatalf("fail-open SSO login must log exactly 1 error-level entry, got %d", got)
	}
}

// TestSSOService_HandleCallback_NotEnrolled_KeyAbsent_IssuesSessionDirectly: the reader alone
// (key absent) must not disturb a non-enrolled account's SSO login.
func TestSSOService_HandleCallback_NotEnrolled_KeyAbsent_IssuesSessionDirectly(t *testing.T) {
	f := newSSOFixture(t)
	f.authSvc.WithMFAEnrollmentReader(newFakeMFARepo(), false) // wired, u-bob never enrolled
	code, state, browserNonce := f.linkedCallbackArgs(t)

	out, err := f.svc.HandleCallback(context.Background(), code, state, browserNonce)
	if err != nil {
		t.Fatalf("HandleCallback(not enrolled, key absent): %v", err)
	}
	if out.MFAPending || out.AccessToken == "" || out.RefreshToken == "" {
		t.Fatalf("HandleCallback(not enrolled) must issue a direct session: %+v", out)
	}
}

// TestSSOService_HandleCallback_Enrolled_KeyPresent_StillPending is the SSO regression twin of
// TestLogin_Enrolled_KeyPresent_StillPending: full support wired (via the existing enableMFA
// helper) plus the standalone reader → still the pending shape, never the 503 sentinel.
func TestSSOService_HandleCallback_Enrolled_KeyPresent_StillPending(t *testing.T) {
	f := newSSOFixture(t)
	f.enableMFA(t, "u-bob")                                    // full WithMFA wiring, enrolled row
	f.authSvc.WithMFAEnrollmentReader(newFakeMFARepo(), false) // empty standalone reader must not mask it
	code, state, browserNonce := f.linkedCallbackArgs(t)

	out, err := f.svc.HandleCallback(context.Background(), code, state, browserNonce)
	if err != nil {
		t.Fatalf("HandleCallback(enrolled, key present): %v", err)
	}
	if !out.MFAPending || out.PendingToken == "" {
		t.Fatalf("HandleCallback(enrolled, key present) = %+v; want mfaPending shape", out)
	}
}
