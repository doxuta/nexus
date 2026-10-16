package services

import (
	"context"
	"encoding/json"
	"testing"
)

// TestLogin_NonMFA_ResponseByteIdentical is the regression guard demanded by
// ../specs/003-mfa-totp/spec.md §"#7" MUST-FIX ("land behind a test proving the non-MFA path is
// byte-identical"). It is deliberately green BOTH before and after ENG-AUTH2 #7 landed (unlike
// the RED-first MFA-behavior tests in auth_mfa_test.go): it pins the exact JSON shape of a
// successful login for an account that has never touched MFA, so a future edit to LoginOutput
// cannot silently leak a new key (or an empty one) into the response every existing client
// already parses.
//
// Two services are compared: one built exactly like every pre-#7 test (newTestService, no
// WithMFA call at all — mfaRepo stays nil) and one built WITH MFA wired via WithMFA but for a
// user who has NEVER enrolled (mfaRepo.Get returns auth.ErrMFANotEnrolled). Both must produce
// byte-identical JSON, proving the MFA branch inside Login is a true no-op for a non-enrolled
// account regardless of whether MFA support happens to be configured on the server.
func TestLogin_NonMFA_ResponseByteIdentical(t *testing.T) {
	ctx := context.Background()
	in := LoginInput{Tenant: "acme", Email: "alice@acme.test", Password: "s3cret-pw"}

	svcNoMFA, _ := newTestService(t)
	outNoMFA, err := svcNoMFA.Login(ctx, in)
	if err != nil {
		t.Fatalf("Login (mfa never wired): %v", err)
	}

	f := newMFAFixture(t) // MFA IS wired, but the user has never enrolled
	outMFAWiredNotEnrolled, err := f.svc.Login(ctx, LoginInput{Tenant: "acme", Email: f.email, Password: f.password})
	if err != nil {
		t.Fatalf("Login (mfa wired, not enrolled): %v", err)
	}

	// Structural assertions (independent of JSON key order — belt-and-suspenders).
	for name, out := range map[string]*LoginOutput{"mfa-never-wired": outNoMFA, "mfa-wired-not-enrolled": outMFAWiredNotEnrolled} {
		if out.MFAPending {
			t.Errorf("%s: MFAPending = true; want false for a non-MFA login", name)
		}
		if out.PendingToken != "" {
			t.Errorf("%s: PendingToken = %q; want empty for a non-MFA login", name, out.PendingToken)
		}
		if out.AccessToken == "" || out.RefreshToken == "" || out.User == nil {
			t.Errorf("%s: incomplete token pair: %+v", name, out)
		}
	}

	// Byte-identical JSON SHAPE (keys present/absent) — normalize the two variable secrets
	// (tokens are freshly minted per call, so they will always differ) before comparing.
	shapeOf := func(t *testing.T, out *LoginOutput) map[string]any {
		t.Helper()
		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		delete(m, "accessToken")
		delete(m, "refreshToken")
		return m
	}
	shapeA := shapeOf(t, outNoMFA)
	shapeB := shapeOf(t, outMFAWiredNotEnrolled)

	rawA, _ := json.Marshal(shapeA)
	rawB, _ := json.Marshal(shapeB)
	if string(rawA) != string(rawB) {
		t.Fatalf("non-MFA login JSON shape diverged:\n  mfa-never-wired:        %s\n  mfa-wired-not-enrolled: %s", rawA, rawB)
	}

	// Pin the EXACT key set expected on a non-MFA success response — a future accidental
	// omitempty removal or a new field without omitempty would fail this immediately.
	wantKeys := map[string]bool{"user": true} // accessToken/refreshToken deleted above; mfaPending/pendingToken must be ABSENT
	for k := range shapeA {
		if !wantKeys[k] {
			t.Errorf("unexpected key %q present in non-MFA login response shape: %v", k, shapeA)
		}
	}
	for k := range wantKeys {
		if _, ok := shapeA[k]; !ok {
			t.Errorf("expected key %q missing from non-MFA login response shape: %v", k, shapeA)
		}
	}
}

// TestLogin_MFAPending_ResponseShape_OmitsTokenAndUserKeys is the companion token-leak guard:
// the pending response must contain ONLY mfaPending+pendingToken — accessToken, refreshToken,
// and user must be ABSENT (not merely empty-valued) from the marshaled JSON, so a client that
// naively checks `if (resp.accessToken)` cannot be fooled by a stray empty string and, more
// importantly, no partial token material ever appears on the wire alongside mfaPending:true.
func TestLogin_MFAPending_ResponseShape_OmitsTokenAndUserKeys(t *testing.T) {
	f := newMFAFixture(t)
	f.enrollAndActivate(t)

	out, err := f.svc.Login(context.Background(), LoginInput{Tenant: "acme", Email: f.email, Password: f.password})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !out.MFAPending {
		t.Fatal("MFAPending = false; want true")
	}

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, forbidden := range []string{"accessToken", "refreshToken", "user"} {
		if _, present := m[forbidden]; present {
			t.Errorf("key %q present in mfaPending response (want ABSENT): %s", forbidden, raw)
		}
	}
	if _, ok := m["mfaPending"]; !ok {
		t.Errorf("key %q missing from mfaPending response: %s", "mfaPending", raw)
	}
	if _, ok := m["pendingToken"]; !ok {
		t.Errorf("key %q missing from mfaPending response: %s", "pendingToken", raw)
	}
	if len(m) != 2 {
		t.Errorf("mfaPending response has %d keys, want exactly 2 (mfaPending,pendingToken): %s", len(m), raw)
	}
}

// TestLogin_MFANotEnrolled_DoesNotConsultMFARepoBeforePasswordCheck is a placement guard for
// ../specs/003-mfa-totp/spec.md §"#7" (US1: nhánh MFA chỉ chạy sau password): the MFA branch must run strictly AFTER the
// password has verified and the account is confirmed active — never before — so a wrong
// password against an MFA-enabled account still returns the ordinary generic credential
// failure (never mfaPending, which would leak "this account has MFA" ahead of a real auth
// failure).
func TestLogin_WrongPassword_OnMFAEnabledAccount_StaysGenericCredentialError(t *testing.T) {
	f := newMFAFixture(t)
	f.enrollAndActivate(t)

	_, err := f.svc.Login(context.Background(), LoginInput{Tenant: "acme", Email: f.email, Password: "totally-wrong"})
	if err == nil {
		t.Fatal("Login with wrong password on an MFA-enabled account: want error, got nil")
	}
	if err != ErrInvalidCredentials {
		t.Fatalf("Login with wrong password on an MFA-enabled account: err = %v; want ErrInvalidCredentials (must not leak mfaPending)", err)
	}
}
