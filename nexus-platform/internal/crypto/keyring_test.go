package crypto

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"
)

func randKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

// TestAESGCM_SealAADRoundTrip pins the AAD primitive the named-credential vault seals with
// (specs/050 D-3): an envelope opens only under the SAME additional data, and the legacy
// Seal/Open (TOTP, SSO) stay byte-compatible — nil AAD is GCM's empty additional data.
func TestAESGCM_SealAADRoundTrip(t *testing.T) {
	s, err := New("v1", randKey(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	aad := []byte("tenant-a|cred-1|secret")
	env, err := s.SealAAD([]byte("partner-token"), aad)
	if err != nil {
		t.Fatalf("SealAAD: %v", err)
	}
	if !strings.HasPrefix(env, "v1:v1:") || strings.Contains(env, "partner-token") {
		t.Fatalf("envelope %q: want v1:<keyID>: prefix and no plaintext", env)
	}
	got, err := s.OpenAAD(env, aad)
	if err != nil || string(got) != "partner-token" {
		t.Fatalf("OpenAAD = %q, %v; want the plaintext back", got, err)
	}
	if _, err := s.Open(env); err == nil {
		t.Fatal("Open (no AAD) of an AAD-bound envelope succeeded; the context must be required")
	}

	legacy, err := s.Seal([]byte("totp-seed"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if got, err := s.OpenAAD(legacy, nil); err != nil || string(got) != "totp-seed" {
		t.Fatalf("OpenAAD(nil) of a legacy Seal envelope = %q, %v; want byte-compatible", got, err)
	}
}

// TestKeyring_SealWithActiveOpenWithRetired is master-key rotation (spec US7): after the active
// key moves to the retired list, rows sealed under it still open, and new seals use the new key.
func TestKeyring_SealWithActiveOpenWithRetired(t *testing.T) {
	k1, k2 := randKey(t), randKey(t)
	aad := []byte("tenant-a|cred-1|secret")

	before, err := NewKeyring("v1", k1, nil)
	if err != nil {
		t.Fatalf("NewKeyring(v1): %v", err)
	}
	old, err := before.SealAAD([]byte("s3cret"), aad)
	if err != nil {
		t.Fatalf("SealAAD: %v", err)
	}
	v1 := before.ActiveID()
	if id, err := KeyIDOf(old); err != nil || id != v1 || !strings.HasPrefix(v1, "v1.") {
		t.Fatalf("KeyIDOf(old) = %q, %v; want the active id %q (label v1 + fingerprint)", id, err, v1)
	}

	after, err := NewKeyring("v2", k2, map[string][]byte{"v1": k1})
	if err != nil {
		t.Fatalf("NewKeyring(v2, retired v1): %v", err)
	}
	v2 := after.ActiveID()
	if !strings.HasPrefix(v2, "v2.") || !after.Has(v1) || !after.Has(v2) {
		t.Fatalf("ActiveID=%q Has(%s)=%v Has(%s)=%v; want v2 active holding both", v2, v1, after.Has(v1), v2, after.Has(v2))
	}
	if got, err := after.OpenAAD(old, aad); err != nil || string(got) != "s3cret" {
		t.Fatalf("OpenAAD(old envelope) with v1 retired = %q, %v; want it to open", got, err)
	}
	fresh, err := after.SealAAD([]byte("s3cret"), aad)
	if err != nil {
		t.Fatalf("SealAAD: %v", err)
	}
	if id, _ := KeyIDOf(fresh); id != v2 {
		t.Fatalf("new envelope sealed under %q; want the active key %q", id, v2)
	}

	if _, err := NewKeyring("v1", k1, map[string][]byte{"v1": k2}); err == nil {
		t.Fatal("NewKeyring with a retired id equal to the active id succeeded; want an error")
	}
}

// TestKeyring_OpenRejectsWrongAAD: an envelope copied onto another row, tenant or slot must not
// open (D-3) — otherwise a bug or a hand-edited DB could send tenant A's secret to tenant B's host.
func TestKeyring_OpenRejectsWrongAAD(t *testing.T) {
	kr, err := NewKeyring("v1", randKey(t), nil)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	env, err := kr.SealAAD([]byte("s3cret"), []byte("tenant-a|cred-1|secret"))
	if err != nil {
		t.Fatalf("SealAAD: %v", err)
	}
	for _, aad := range [][]byte{
		[]byte("tenant-a|cred-2|secret"),      // copied onto another row
		[]byte("tenant-b|cred-1|secret"),      // copied into another tenant
		[]byte("tenant-a|cred-1|prev_secret"), // copied into another slot
		nil,                                   // opened without context
	} {
		if got, err := kr.OpenAAD(env, aad); err == nil {
			t.Fatalf("OpenAAD with AAD %q returned %q; want an authentication error", aad, got)
		}
	}
}

// TestKeyring_SameLabelDifferentKeyIsUnknown: staging and prod both keep the default label "v1"
// over DIFFERENT master keys. A prod dump restored into staging must read as "sealed under a key
// this server does not hold" (Has=false, ErrUnknownKeyID ⇒ status key_unavailable, run
// CREDENTIAL_KEY_UNAVAILABLE, Doctor C11, deploy precheck) — not as a held id that then fails GCM
// with a generic error while Status reports ready. The key id therefore carries a fingerprint of
// the key itself; the same label over the same key (restart, second replica) is the same id.
func TestKeyring_SameLabelDifferentKeyIsUnknown(t *testing.T) {
	prodKey := randKey(t)
	prod, err := NewKeyring("v1", prodKey, nil)
	if err != nil {
		t.Fatalf("NewKeyring(prod): %v", err)
	}
	staging, err := NewKeyring("v1", randKey(t), nil)
	if err != nil {
		t.Fatalf("NewKeyring(staging): %v", err)
	}
	aad := []byte("tenant-a|cred-1|secret")
	env, err := prod.SealAAD([]byte("s3cret"), aad)
	if err != nil {
		t.Fatalf("SealAAD: %v", err)
	}
	id, err := KeyIDOf(env)
	if err != nil {
		t.Fatalf("KeyIDOf: %v", err)
	}
	if staging.Has(id) {
		t.Fatalf("staging.Has(%q) = true for a different key under the same label; want false", id)
	}
	if _, err := staging.OpenAAD(env, aad); !errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("staging.OpenAAD(prod envelope) = %v; want ErrUnknownKeyID", err)
	}

	replica, err := NewKeyring("v1", prodKey, nil)
	if err != nil {
		t.Fatalf("NewKeyring(replica): %v", err)
	}
	if replica.ActiveID() != prod.ActiveID() || !replica.Has(id) {
		t.Fatalf("same label + same key: ActiveID %q vs %q, Has=%v; want one stable id", replica.ActiveID(), prod.ActiveID(), replica.Has(id))
	}
	if got, err := replica.OpenAAD(env, aad); err != nil || string(got) != "s3cret" {
		t.Fatalf("replica.OpenAAD = %q, %v; want the plaintext back", got, err)
	}
}

// TestKeyring_KeyIDFormula pins the id derivation, which tools outside Go recompute from the env
// (S7 deploy precheck, acceptance-named-credentials.py): "<label>.<first 8 hex of
// HMAC-SHA256(key, "nexus/crypto/keyring/key-id/v1")>". Vector cross-checked with
// `openssl dgst -sha256 -mac HMAC -macopt hexkey:000102…1f`.
func TestKeyring_KeyIDFormula(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	kr, err := NewKeyring("v1", key, map[string][]byte{"old": key})
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	if got := kr.ActiveID(); got != "v1.327c8670" {
		t.Fatalf("ActiveID() = %q; want v1.327c8670", got)
	}
	if !kr.Has("old.327c8670") || kr.Has("old") || kr.Has("v1") {
		t.Fatal("retired ids must carry the same fingerprint and bare labels must not match")
	}
}

// TestKeyring_OpenUnknownKeyID: a row sealed under a master key the server does not hold (DB
// restored into another environment) is reported as such — the resolver maps it to
// CREDENTIAL_KEY_UNAVAILABLE and sends nothing.
func TestKeyring_OpenUnknownKeyID(t *testing.T) {
	other, err := NewKeyring("v9", randKey(t), nil)
	if err != nil {
		t.Fatalf("NewKeyring(v9): %v", err)
	}
	env, err := other.SealAAD([]byte("s3cret"), []byte("aad"))
	if err != nil {
		t.Fatalf("SealAAD: %v", err)
	}
	kr, err := NewKeyring("v1", randKey(t), nil)
	if err != nil {
		t.Fatalf("NewKeyring(v1): %v", err)
	}
	if kr.Has("v9") {
		t.Fatal("Has(v9) = true for a keyring that never held v9")
	}
	if _, err := kr.OpenAAD(env, []byte("aad")); !errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("OpenAAD(envelope under v9) = %v; want ErrUnknownKeyID", err)
	}
	if _, err := KeyIDOf("not-an-envelope"); err == nil {
		t.Fatal("KeyIDOf(garbage) returned no error")
	}
	var nilRing *Keyring
	if nilRing.Has("v1") {
		t.Fatal("nil keyring Has(v1) = true; an unconfigured vault holds no key")
	}
}

// TestKeyring_NilIsNotConfigured: a nil *Keyring is the "vault inert" value main.go passes when
// CREDENTIAL_ENC_KEY is absent. Every method must answer it — Has already did; Seal/Open used to
// nil-deref (queue #222 LOW 1). Fail closed with ErrNotConfigured, never panic, never seal.
func TestKeyring_NilIsNotConfigured(t *testing.T) {
	var k *Keyring
	if got := k.ActiveID(); got != "" {
		t.Fatalf("nil.ActiveID() = %q; want empty", got)
	}
	if env, err := k.SealAAD([]byte("s"), []byte("aad")); !errors.Is(err, ErrNotConfigured) || env != "" {
		t.Fatalf("nil.SealAAD = %q, %v; want empty + ErrNotConfigured", env, err)
	}
	if pt, err := k.OpenAAD("v1:v1.00000000:abc", []byte("aad")); !errors.Is(err, ErrNotConfigured) || pt != nil {
		t.Fatalf("nil.OpenAAD = %q, %v; want nil + ErrNotConfigured", pt, err)
	}
}
