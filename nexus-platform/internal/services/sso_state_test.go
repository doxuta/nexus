package services

import (
	"testing"
	"time"

	"nexus/platform/internal/crypto"
)

// newSSOTestSealer builds a real internal/crypto.AESGCM sealer (not the string-prefix
// fakeSealer used elsewhere in this package's MFA tests) because these tests specifically
// assert genuine AEAD tamper-detection — a fake Seal/Open round-trip that merely strips a
// prefix would never catch a flipped byte.
func newSSOTestSealer(t *testing.T) *crypto.AESGCM {
	t.Helper()
	key := make([]byte, 32) // deterministic all-zero key: fine for a unit test, never production
	sealer, err := crypto.New("v1", key)
	if err != nil {
		t.Fatalf("crypto.New: %v", err)
	}
	return sealer
}

func newSSOStateTestService(t *testing.T, now time.Time) *SSOService {
	t.Helper()
	svc := &SSOService{sealer: newSSOTestSealer(t)}
	svc.now = func() time.Time { return now }
	return svc
}

func TestSSOState_SealOpen_RoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	svc := newSSOStateTestService(t, now)

	st := ssoState{TenantID: "t-1", ProviderID: "p-1", Nonce: "nonce-abc", ExpiresAt: now.Add(ssoStateTTL)}
	sealed, err := svc.sealState(st)
	if err != nil {
		t.Fatalf("sealState: %v", err)
	}
	if sealed == "" {
		t.Fatal("sealState returned empty string")
	}

	got, err := svc.openState(sealed)
	if err != nil {
		t.Fatalf("openState: %v", err)
	}
	if got.TenantID != st.TenantID || got.ProviderID != st.ProviderID || got.Nonce != st.Nonce {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, st)
	}
}

func TestSSOState_Open_Tampered_Invalid(t *testing.T) {
	now := time.Now()
	svc := newSSOStateTestService(t, now)

	st := ssoState{TenantID: "t-1", ProviderID: "p-1", Nonce: "nonce-abc", ExpiresAt: now.Add(ssoStateTTL)}
	sealed, err := svc.sealState(st)
	if err != nil {
		t.Fatalf("sealState: %v", err)
	}

	// Flip a character in the MIDDLE of the opaque envelope. NOT the last one: base64's final
	// character often carries fewer than 6 meaningful bits, so a last-char swap between two
	// alphabet neighbours (the old 'a'↔'b') can decode to the *identical* ciphertext and GCM
	// happily opens it — the source of a long-lived CI flake. Every bit of a mid-string
	// character is load-bearing, so this swap ALWAYS changes the decoded bytes and GCM
	// authentication must reject it.
	mid := len(sealed) / 2
	repl := byte('A')
	if sealed[mid] == 'A' {
		repl = 'B'
	}
	tampered := sealed[:mid] + string(repl) + sealed[mid+1:]

	if _, err := svc.openState(tampered); err == nil {
		t.Fatal("openState(tampered) = nil error; want failure")
	}
}

func TestSSOState_Open_Expired_Invalid(t *testing.T) {
	now := time.Now()
	svc := newSSOStateTestService(t, now)

	// Sealed with an ExpiresAt already in the past relative to `now` (the state's own clock).
	st := ssoState{TenantID: "t-1", ProviderID: "p-1", Nonce: "nonce-abc", ExpiresAt: now.Add(-time.Minute)}
	sealed, err := svc.sealState(st)
	if err != nil {
		t.Fatalf("sealState: %v", err)
	}

	if _, err := svc.openState(sealed); err == nil {
		t.Fatal("openState(expired) = nil error; want failure")
	}
}

func TestSSOState_Open_MalformedEnvelope_Invalid(t *testing.T) {
	svc := newSSOStateTestService(t, time.Now())

	if _, err := svc.openState("not-a-sealed-envelope"); err == nil {
		t.Fatal("openState(garbage) = nil error; want failure")
	}
}

func TestSSOState_Open_NoSealer_Invalid(t *testing.T) {
	svc := &SSOService{now: time.Now} // sealer never wired (SSO disabled)
	if _, err := svc.openState("anything"); err == nil {
		t.Fatal("openState with no sealer = nil error; want failure")
	}
}
