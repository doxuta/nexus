// CONG-TIN-HIEU unit tests — the pure receive-path helpers (../specs/015-inbound-hooks/spec.md §8): the
// dot-path extractor matrix and the constant-time HMAC verifier. The config-validate matrix +
// end-to-end upsert run against real MySQL in inbound_hook_integration_test.go.
package services

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestExtractPath(t *testing.T) {
	var payload any
	if err := json.Unmarshal([]byte(`{
		"data": {"amount": 150000, "desc": "CK DH-0042", "ok": true, "nil": null},
		"items": [{"sku": "A1"}, {"sku": "B2"}],
		"top": "x"
	}`), &payload); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		path string
		want any
		ok   bool
	}{
		{"top", "x", true},
		{"data.amount", float64(150000), true},
		{"data.desc", "CK DH-0042", true},
		{"data.ok", true, true},
		{"data.nil", nil, true}, // an explicit null RESOLVES (value nil) — path exists
		{"items.0.sku", "A1", true},
		{"items.1.sku", "B2", true},
		{"items.2.sku", nil, false}, // index out of range
		{"items.x.sku", nil, false}, // non-numeric index into array
		{"data.missing", nil, false},
		{"top.deeper", nil, false}, // descending into a scalar
		{"", nil, false},           // empty path = one empty segment → no such key
	}
	for _, tt := range tests {
		got, ok := extractPath(payload, tt.path)
		if ok != tt.ok || (ok && !jsonScalarEq(got, tt.want)) {
			t.Errorf("extractPath(%q) = (%v, %v), want (%v, %v)", tt.path, got, ok, tt.want, tt.ok)
		}
	}
}

func jsonScalarEq(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a == b
}

func TestVerifyHookSignature(t *testing.T) {
	secret := "whk_deadbeef"
	body := []byte(`{"a":1}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	good := hex.EncodeToString(mac.Sum(nil))

	if !verifyHookSignature(secret, good, body) {
		t.Fatal("valid signature rejected")
	}
	if !verifyHookSignature(secret, "  "+good+" ", body) {
		t.Fatal("whitespace-padded signature must be accepted (trimmed)")
	}
	if verifyHookSignature(secret, good, []byte(`{"a":2}`)) {
		t.Fatal("signature over different body accepted")
	}
	if verifyHookSignature("other-secret", good, body) {
		t.Fatal("signature with wrong secret accepted")
	}
	if verifyHookSignature(secret, "", body) {
		t.Fatal("empty signature accepted")
	}
	if verifyHookSignature(secret, "not-hex!", body) {
		t.Fatal("non-hex signature accepted")
	}
}

func TestMatchValueString(t *testing.T) {
	// Review W1: %v on float64 gave "1.5e+06" — plain decimal formatting is the contract.
	if v, ok := matchValueString(float64(1500000)); !ok || v != "1500000" {
		t.Fatalf("large float = (%q, %v)", v, ok)
	}
	if v, ok := matchValueString(float64(12.5)); !ok || v != "12.5" {
		t.Fatalf("decimal = (%q, %v)", v, ok)
	}
	if v, ok := matchValueString("DH-0042"); !ok || v != "DH-0042" {
		t.Fatalf("string = (%q, %v)", v, ok)
	}
	for _, bad := range []any{"", true, nil, map[string]any{}, []any{1}} {
		if _, ok := matchValueString(bad); ok {
			t.Fatalf("%T must not form a match key", bad)
		}
	}
}

func TestTruncateUTF8(t *testing.T) {
	s := "tiền tệ Việt Nam" // multi-byte runes
	for max := 0; max <= len(s); max++ {
		got := truncateUTF8(s, max)
		if len(got) > max {
			t.Fatalf("max %d: len %d", max, len(got))
		}
		if !utf8ValidString(got) {
			t.Fatalf("max %d: invalid UTF-8 %q", max, got)
		}
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}

func TestMintHookSecret(t *testing.T) {
	a, err := mintHookSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := mintHookSecret()
	if len(a) != 4+48 || a[:4] != "whk_" {
		t.Fatalf("secret shape = %q", a)
	}
	if a == b {
		t.Fatal("two mints returned the same secret")
	}
}
