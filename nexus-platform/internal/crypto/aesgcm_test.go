package crypto

import (
	"bytes"
	"strings"
	"testing"
)

func mustKey(t *testing.T, seed byte) []byte {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = seed + byte(i)
	}
	return key
}

func TestNew_RejectsBadKeySize(t *testing.T) {
	tests := []struct {
		name string
		key  []byte
	}{
		{"empty", nil},
		{"too short", make([]byte, 16)},
		{"too long", make([]byte, 33)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New("v1", tt.key); err == nil {
				t.Fatalf("New() with %d-byte key: want error, got nil", len(tt.key))
			}
		})
	}
}

func TestNew_RejectsEmptyKeyID(t *testing.T) {
	if _, err := New("", mustKey(t, 1)); err == nil {
		t.Fatal("New() with empty keyID: want error, got nil")
	}
}

func TestSealOpen_RoundTrip(t *testing.T) {
	c, err := New("v1", mustKey(t, 1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plaintext := []byte("JBSWY3DPEHPK3PXP-super-secret-totp-seed")

	sealed, err := c.Seal(plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if sealed == "" {
		t.Fatal("Seal returned empty string")
	}

	got, err := c.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("Open() = %q; want %q", got, plaintext)
	}
}

func TestSeal_FormatIsVersionedAndSelfDescribing(t *testing.T) {
	c, err := New("v1", mustKey(t, 1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sealed, err := c.Seal([]byte("hello"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	parts := strings.SplitN(sealed, ":", 3)
	if len(parts) != 3 {
		t.Fatalf("sealed format = %q; want 3 colon-separated parts", sealed)
	}
	if parts[0] != "v1" {
		t.Errorf("version = %q; want v1", parts[0])
	}
	if parts[1] != "v1" { // keyID happens to also be "v1" in this test — check it echoes the keyID param
		t.Errorf("keyID segment = %q; want v1 (the configured keyID)", parts[1])
	}
}

func TestSeal_NonceIsRandomPerCall(t *testing.T) {
	c, err := New("v1", mustKey(t, 1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a, err := c.Seal([]byte("same plaintext"))
	if err != nil {
		t.Fatalf("Seal #1: %v", err)
	}
	b, err := c.Seal([]byte("same plaintext"))
	if err != nil {
		t.Fatalf("Seal #2: %v", err)
	}
	if a == b {
		t.Fatal("two Seal() calls on identical plaintext produced identical ciphertext — nonce is not random")
	}
}

func TestOpen_TamperDetection(t *testing.T) {
	c, err := New("v1", mustKey(t, 1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sealed, err := c.Seal([]byte("do-not-tamper"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Flip one byte deep in the base64 payload segment (not the version/keyID prefix).
	parts := strings.SplitN(sealed, ":", 3)
	payload := []byte(parts[2])
	// Flip a character in the middle of the payload to a different valid base64 char.
	mid := len(payload) / 2
	orig := payload[mid]
	replacement := byte('A')
	if orig == replacement {
		replacement = 'B'
	}
	payload[mid] = replacement
	tampered := parts[0] + ":" + parts[1] + ":" + string(payload)

	if _, err := c.Open(tampered); err == nil {
		t.Fatal("Open() on tampered ciphertext: want error, got nil")
	}
}

func TestOpen_WrongKey(t *testing.T) {
	sealer, err := New("v1", mustKey(t, 1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sealed, err := sealer.Seal([]byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Different key bytes, SAME keyID (simulates a corrupted/rotated-wrong key under the
	// same label) — Open must fail GCM authentication, not just a keyID mismatch check.
	wrongKeySameID, err := New("v1", mustKey(t, 99))
	if err != nil {
		t.Fatalf("New (wrong key): %v", err)
	}
	if _, err := wrongKeySameID.Open(sealed); err == nil {
		t.Fatal("Open() with wrong key (same keyID): want error, got nil")
	}
}

func TestOpen_KeyIDMismatch(t *testing.T) {
	sealer, err := New("v1", mustKey(t, 1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sealed, err := sealer.Seal([]byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	other, err := New("v2", mustKey(t, 1))
	if err != nil {
		t.Fatalf("New (v2): %v", err)
	}
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("Open() with mismatched keyID: want error, got nil")
	}
}

func TestOpen_MalformedEnvelope(t *testing.T) {
	c, err := New("v1", mustKey(t, 1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tests := []string{
		"",
		"not-a-sealed-value",
		"v1:v1",                // missing payload segment
		"v2:v1:AAAA",           // wrong version
		"v1:v1:not-base64!!!",  // invalid base64
		"v1:v1:" + shortB64(t), // valid base64 but too short for a nonce
	}
	for _, sealed := range tests {
		if _, err := c.Open(sealed); err == nil {
			t.Errorf("Open(%q): want error, got nil", sealed)
		}
	}
}

func shortB64(t *testing.T) string {
	t.Helper()
	return "QQ" // base64 for a single byte, shorter than any GCM nonce size
}
