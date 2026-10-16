package crypto

import "testing"

// TestKeyIDFor_GoldenVector pins the key-id fingerprint to the exact value deploy/nexus/scripts/
// admin-precheck.sh recomputes from .env.prod with openssl (specs/050 S7 D-15). The expected hex was
// produced by the shell recipe, not by this code:
//
//	printf 'nexus/crypto/keyring/key-id/v1' | openssl dgst -sha256 -mac HMAC -macopt hexkey:<key hex>
//
// If keyIDFor or keyIDContext changes, the precheck silently disagrees with the server — change both.
func TestKeyIDFor_GoldenVector(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i) // base64 AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=
	}
	if got, want := keyIDFor("v1", key), "v1.327c8670"; got != want {
		t.Fatalf("keyIDFor = %q, want %q (the precheck script computes the same id with openssl)", got, want)
	}
}
