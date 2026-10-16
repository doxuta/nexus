package workflow

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

// specs/066 L1a: the recipient mask is what lands in sys_email_log — it must never carry an address.
func TestMaskRecipients(t *testing.T) {
	long := strings.Repeat("a", 300) + "@x.vn"
	tests := []struct {
		name, in, want string
	}{
		{"one address", "lan.nguyen@gmail.com", "l***@gmail.com"},
		{"comma list", "a@x.vn, b@y.vn", "a***@x.vn, b***@y.vn"},
		{"semicolon list", "a@x.vn;b@y.vn", "a***@x.vn, b***@y.vn"},
		{"no at sign", "not-an-address", "***"},
		{"empty", "", ""},
		{"blank parts dropped", " ; ", ""},
		{"unicode first rune", "Điệp@gmail.com", "Đ***@gmail.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := MaskRecipients(tc.in); got != tc.want {
				t.Errorf("MaskRecipients(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	t.Run("cut at 255 bytes on a rune boundary", func(t *testing.T) {
		many := strings.TrimSuffix(strings.Repeat("Đ@gmail.com,", 40), ",")
		got := MaskRecipients(many)
		if len(got) > 255 || !utf8.ValidString(got) {
			t.Errorf("len=%d valid=%v, want <=255 and valid UTF-8", len(got), utf8.ValidString(got))
		}
		if strings.Contains(MaskRecipients(long), "aaaa") {
			t.Errorf("a long local part must be reduced to its first rune")
		}
	})
}

func TestEmailActionHash(t *testing.T) {
	base := Action{Type: ActionEmailAlert, To: "{{email}}", Subject: "s", Body: "b"}
	h := EmailActionHash(base)
	if len(h) != 64 || h != EmailActionHash(base) {
		t.Fatalf("hash must be 64 hex chars and stable, got %q", h)
	}
	for name, mut := range map[string]Action{
		"to": {To: "x", Subject: "s", Body: "b"}, "subject": {To: "{{email}}", Subject: "x", Body: "b"},
		"body": {To: "{{email}}", Subject: "s", Body: "x"},
	} {
		if EmailActionHash(mut) == h {
			t.Errorf("changing %s must change the hash", name)
		}
	}
	// "a"+"bc" vs "ab"+"c": the separator keeps field boundaries apart.
	if EmailActionHash(Action{To: "a", Subject: "bc"}) == EmailActionHash(Action{To: "ab", Subject: "c"}) {
		t.Errorf("field boundaries must not collide")
	}
}

func TestEmailOriginContext(t *testing.T) {
	if _, ok := EmailOriginFrom(context.Background()); ok {
		t.Fatalf("empty ctx must report no origin")
	}
	want := EmailOrigin{TenantID: "t1", WorkflowID: "w1", ActionIndex: 2, LogID: "l1"}
	got, ok := EmailOriginFrom(WithEmailOrigin(context.Background(), want))
	if !ok || got != want {
		t.Errorf("round trip = %+v ok=%v, want %+v", got, ok, want)
	}
}
