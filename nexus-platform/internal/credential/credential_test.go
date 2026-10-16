package credential

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestValidateName(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"pay", true},
		{"webhook-pay-example-vn", true},
		{"zalo_oa", true},
		{"nc-nghiemthu-1727654400-abcdef12", true},
		{"a" + strings.Repeat("b", 63), true},
		{"a" + strings.Repeat("b", 64), false}, // 65 chars
		{"", false},
		{"1pay", false},   // must start with a letter
		{"Pay", false},    // lowercase only
		{"pay.vn", false}, // dot not allowed
		{"pay vn", false},
		{"-pay", false},
	} {
		err := ValidateName(tc.name)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateName(%q) = %v; want ok=%v", tc.name, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrNameInvalid) {
			t.Errorf("ValidateName(%q) error %v does not wrap ErrNameInvalid", tc.name, err)
		}
	}
}

func TestValidateBaseURL(t *testing.T) {
	for _, tc := range []struct {
		raw      string
		insecure bool // CREDENTIAL_ALLOW_INSECURE_HTTP
		ok       bool
	}{
		{"https://pay.example.vn/api", false, true},
		{"https://pay.example.vn:8443", false, true},
		{"HTTPS://Pay.Example.vn/api/", false, true},
		{"http://127.0.0.1:9000/hook", false, true}, // loopback
		{"http://localhost:9000", false, true},
		{"http://[::1]:9000", false, true},
		{"http://partner.internal/hook", false, false}, // plain http off loopback
		{"http://partner.internal/hook", true, true},   // dev flag
		{"https://user:pass@pay.example.vn", false, false},
		{"https://user@pay.example.vn", false, false},
		{"https://pay.example.vn/api?token=abc", false, false},
		{"https://pay.example.vn/api?", false, false},
		{"https://pay.example.vn/api#frag", false, false},
		{"https://pay.example.vn/api#", false, false},
		{"ftp://pay.example.vn", false, false},
		{"https://", false, false},
		{"https://:443/x", false, false},
		{"pay.example.vn/api", false, false},
		{"/api", false, false},
		{"https://pay.example.vn/a b", false, false},
		{"https://pay.example.vn/a\r\nX-Evil: 1", false, false},
		{"https://pay.example.vn/" + strings.Repeat("a", 500), false, false}, // > 500 chars
		{"", false, false},
	} {
		err := ValidateBaseURL(tc.raw, tc.insecure)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateBaseURL(%q, insecure=%v) = %v; want ok=%v", tc.raw, tc.insecure, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrBaseURLInvalid) {
			t.Errorf("ValidateBaseURL(%q) error %v does not wrap ErrBaseURLInvalid", tc.raw, err)
		}
	}
}

func TestValidateHeaderNameValue(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"Authorization", true},
		{"X-Api-Key", true},
		{"x_partner.id", true},
		{"Host", false},
		{"content-type", false}, // reserved, any case
		{"Content-Length", false},
		{"Transfer-Encoding", false},
		{"Connection", false},
		{"X-Nexus-Signature", false}, // X-Nexus-* is ours
		{"x-nexus-anything", false},
		{"", false},
		{"X Api", false},    // space is not a token char
		{"X-Api:", false},   // colon is not a token char
		{"X-Ápi", false},    // non-ASCII
		{"X-A\r\nB", false}, // header injection
		{strings.Repeat("a", 101), false},
	} {
		err := ValidateHeaderName(tc.name)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateHeaderName(%q) = %v; want ok=%v", tc.name, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrHeaderInvalid) {
			t.Errorf("ValidateHeaderName(%q) error %v does not wrap ErrHeaderInvalid", tc.name, err)
		}
	}

	for _, tc := range []struct {
		value string
		ok    bool
	}{
		{"Bearer abc.def-ghi", true},
		{"giá trị tiếng Việt", true},
		{"a\tb", true},
		{"abc\r\nX-Evil: 1", false},
		{"abc\nX-Evil: 1", false},
		{"abc\x00", false},
		{"abc\x7f", false},
	} {
		err := ValidateHeaderValue(tc.value)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateHeaderValue(%q) = %v; want ok=%v", tc.value, err, tc.ok)
		}
	}

	ok := map[string]string{"Authorization": "Bearer x", "X-Doi-Tac": "y"}
	if err := ValidateExtraHeaders(ok); err != nil {
		t.Errorf("ValidateExtraHeaders(two valid headers) = %v; want nil", err)
	}
	eleven := map[string]string{}
	for i := 0; i < 11; i++ {
		eleven["X-H"+string(rune('a'+i))] = "v"
	}
	big := map[string]string{"X-Big": strings.Repeat("v", 4097)}
	for label, h := range map[string]map[string]string{
		"11 headers":        eleven,
		"over 4KB":          big,
		"reserved name":     {"Host": "evil.example"},
		"CR/LF value":       {"X-A": "a\r\nX-B: b"},
		"case-folded twins": {"X-Api-Key": "a", "x-api-key": "b"},
	} {
		if err := ValidateExtraHeaders(h); !errors.Is(err, ErrHeaderInvalid) {
			t.Errorf("ValidateExtraHeaders(%s) = %v; want ErrHeaderInvalid", label, err)
		}
	}
}

func TestSameOrigin(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"https://pay.example.vn/api", "https://pay.example.vn/v2", true},
		{"https://pay.example.vn", "https://PAY.example.vn:443/x", true}, // default port, host case
		{"http://127.0.0.1/a", "http://127.0.0.1:80/b", true},
		{"https://pay.example.vn", "http://pay.example.vn", false},       // scheme
		{"https://pay.example.vn", "https://pay.example.vn:8443", false}, // port
		{"https://pay.example.vn", "https://evil.example", false},        // host
		{"https://pay.example.vn", "https://pay.example.vn.evil.example", false},
		{"https://pay.example.vn", "::not a url", false},
	} {
		if got := SameOrigin(tc.a, tc.b); got != tc.same {
			t.Errorf("SameOrigin(%q, %q) = %v; want %v", tc.a, tc.b, got, tc.same)
		}
	}
}

func TestStatusDerived(t *testing.T) {
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	env := func(s string) *string { return &s }
	future, past := now.Add(time.Hour), now.Add(-time.Hour)
	holds := func(id string) bool { return id == "v1" }

	for _, tc := range []struct {
		name   string
		c      Credential
		hasKey func(string) bool
		want   Status
	}{
		{"slot without secret", Credential{}, holds, StatusNeedsSecret},
		{"slot with sealed extra headers only", Credential{ExtraHeadersEnc: env("v1:v1:x"), KeyID: env("v1")}, holds, StatusNeedsSecret},
		{"sealed under a held key", Credential{SecretEnc: env("v1:v1:x"), KeyID: env("v1")}, holds, StatusReady},
		{"sealed under a key the server lacks", Credential{SecretEnc: env("v1:v0:x"), KeyID: env("v0")}, holds, StatusKeyUnavailable},
		{"vault not configured", Credential{SecretEnc: env("v1:v1:x"), KeyID: env("v1")}, nil, StatusKeyUnavailable},
		{"hmac in grace", Credential{SecretEnc: env("a"), PrevSecretEnc: env("b"), PrevExpiresAt: &future, KeyID: env("v1")}, holds, StatusGrace},
		{"grace expired", Credential{SecretEnc: env("a"), PrevSecretEnc: env("b"), PrevExpiresAt: &past, KeyID: env("v1")}, holds, StatusReady},
		{"key missing beats grace", Credential{SecretEnc: env("a"), PrevSecretEnc: env("b"), PrevExpiresAt: &future, KeyID: env("v0")}, holds, StatusKeyUnavailable},
	} {
		if got := tc.c.Status(now, tc.hasKey); got != tc.want {
			t.Errorf("%s: Status() = %q; want %q", tc.name, got, tc.want)
		}
	}
}

// TestCredential_JSONNeverCarriesSealedMaterial: the audit diff is built with json.Marshal
// (audit.StructToMap), so the sealed columns and the master-key id must be invisible to JSON
// (specs/050 plan §"Mô hình dữ liệu", threat "bí mật trong audit diff").
func TestCredential_JSONNeverCarriesSealedMaterial(t *testing.T) {
	s := func(v string) *string { return &v }
	c := Credential{
		ID: "cred-1", TenantID: "tenant-a", Name: "pay", AuthType: AuthHMAC,
		SecretEnc: s("v1:k1:SEALED-SECRET"), PrevSecretEnc: s("v1:k1:SEALED-PREV"),
		ExtraHeadersEnc: s("v1:k1:SEALED-HEADERS"), KeyID: s("k1"),
		ExtraHeaderNames: []string{"X-Doi-Tac"},
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, banned := range []string{"SEALED", "v1:", "k1", "tenant-a", "keyId", "secretEnc"} {
		if strings.Contains(string(b), banned) {
			t.Fatalf("JSON %s carries %q", b, banned)
		}
	}
	if !strings.Contains(string(b), `"extraHeaderNames":["X-Doi-Tac"]`) {
		t.Fatalf("JSON %s lost the header NAMES (they are the non-secret part)", b)
	}
}

func TestAAD(t *testing.T) {
	if got := string(AAD("t1", "c1", SlotSecret)); got != "t1|c1|secret" {
		t.Fatalf("AAD = %q; want t1|c1|secret", got)
	}
	if string(AAD("t1", "c1", SlotPrevSecret)) == string(AAD("t1", "c1", SlotExtraHeaders)) {
		t.Fatal("two slots share one AAD — an envelope could move between them")
	}
}
