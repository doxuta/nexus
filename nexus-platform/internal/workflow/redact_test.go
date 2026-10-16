package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactActionsJSON (specs/050 S3a T056): the admin API's copy of a rule's actions carries no
// secret — the signing key is dropped, every header value is masked, and in a url/path the whole
// userinfo and every query value are masked while the query names stay readable. Anything with nothing
// to hide comes back byte for byte (the S2a 409 guard matches the builder's resave by exact URL).
func TestRedactActionsJSON(t *testing.T) {
	const m = RedactedMarker
	cases := []struct {
		name  string
		in    string
		want  []map[string]any // decoded output; nil ⇒ compare bytes with wantRaw
		raw   string           // exact output bytes when set
		leaks []string         // substrings that must not survive
	}{
		{name: "secret dropped, header values masked",
			in: `[{"type":"outbound_webhook","url":"https://erp.example.vn/hook","secret":"s3cr3t-key","headers":{"Authorization":"Bearer tok-123","X-Doi-Tac":"p-9"}}]`,
			want: []map[string]any{{"type": "outbound_webhook", "url": "https://erp.example.vn/hook",
				"headers": map[string]any{"Authorization": m, "X-Doi-Tac": m}}},
			leaks: []string{"s3cr3t-key", "tok-123", "p-9", `"secret"`}},
		{name: "url: the whole userinfo and every query value",
			in:    `[{"type":"outbound_webhook","url":"https://shop:pa55@h.example.vn/x?token=a&src=b"}]`,
			want:  []map[string]any{{"type": "outbound_webhook", "url": "https://" + m + "@h.example.vn/x?token=" + m + "&src=" + m}},
			leaks: []string{"shop", "pa55", "token=a", "src=b"}},
		{name: "url: a lone userinfo is the credential itself",
			in:    `[{"type":"outbound_webhook","url":"https://sk_live_abc@api.example.com/v1/charges"}]`,
			want:  []map[string]any{{"type": "outbound_webhook", "url": "https://" + m + "@api.example.com/v1/charges"}},
			leaks: []string{"sk_live_abc"}},
		// Stripe's documented form (`-u sk_live_…:`): net/http sends the same Basic header as the lone
		// name above, so the name is the key here too.
		{name: "url: a name with an empty password is the credential itself",
			in:    `[{"type":"outbound_webhook","url":"https://sk_live_abc:@api.stripe.com/v1/charges"}]`,
			want:  []map[string]any{{"type": "outbound_webhook", "url": "https://" + m + "@api.stripe.com/v1/charges"}},
			leaks: []string{"sk_live_abc"}},
		// Freshdesk's form: the key is the name, the password a dummy "X".
		{name: "url: a key in the name with a dummy password",
			in:    `[{"type":"outbound_webhook","url":"https://fd_key_123:X@acme.freshdesk.com/api/v2/tickets"}]`,
			want:  []map[string]any{{"type": "outbound_webhook", "url": "https://" + m + "@acme.freshdesk.com/api/v2/tickets"}},
			leaks: []string{"fd_key_123"}},
		{name: "url: an empty userinfo hides nothing and stays byte for byte",
			in:  `[{"type":"outbound_webhook","url":"https://:@h.example.vn/hook"}]`,
			raw: `[{"type":"outbound_webhook","url":"https://:@h.example.vn/hook"}]`},
		{name: "url: a bare query segment may be the token; an empty value hides nothing",
			in:    `[{"type":"outbound_webhook","url":"https://h.example.vn/x?abc123def&flag=&k=v"}]`,
			want:  []map[string]any{{"type": "outbound_webhook", "url": "https://h.example.vn/x?" + m + "&flag=&k=" + m}},
			leaks: []string{"abc123def", "k=v"}},
		{name: "vault path: query values masked, names kept",
			in:    `[{"type":"outbound_webhook","credentialId":"cred-1","path":"/x?key=a"}]`,
			want:  []map[string]any{{"type": "outbound_webhook", "credentialId": "cred-1", "path": "/x?key=" + m}},
			leaks: []string{"key=a"}},
		// encoding/json matches a key to its struct field without regard to case, so the engine SENDS
		// the "Secret"/"Headers"/"URL" of a hand-edited row — the read mask must find them the same way.
		{name: "keys in another case are still the key (the engine reads them case-insensitively)",
			in: `[{"Type":"outbound_webhook","URL":"https://u:p@h.example.vn/x?token=a","Secret":"s3cr3t-key","HEADERS":{"Authorization":"Bearer tok-123"}}]`,
			want: []map[string]any{{"Type": "outbound_webhook", "URL": "https://" + m + "@h.example.vn/x?token=" + m,
				"HEADERS": map[string]any{"Authorization": m}}},
			leaks: []string{"s3cr3t-key", "tok-123", "u:p", "token=a"}},
		// net/url reads "//u:p@host" (no scheme) as userinfo too; masking looked for "://" only.
		{name: "url: a scheme-relative form keeps no userinfo either",
			in:    `[{"type":"outbound_webhook","url":"//u:p@h.example.vn/x"}]`,
			want:  []map[string]any{{"type": "outbound_webhook", "url": "//" + m + "@h.example.vn/x"}},
			leaks: []string{"u:p"}},
		// encoding/json folds case with its own table (U+017F "ſ" folds to s): strings.ToLower does not,
		// so a lower-casing compare left these two keys readable although the engine reads them.
		{name: "keys that only fold to secret / headers (U+017F) are the key too",
			in: "[{\"type\":\"outbound_webhook\",\"\u017fecret\":\"s3cr3t-key\",\"url\":\"https://h.example.vn/x\",\"header\u017f\":{\"Authorization\":\"tok-123\"}}]",
			want: []map[string]any{{"type": "outbound_webhook", "url": "https://h.example.vn/x",
				"header\u017f": map[string]any{"Authorization": m}}},
			leaks: []string{"s3cr3t-key", "tok-123"}},
		// A "://" later in the path is not where the authority starts: the scheme-relative form still is.
		{name: "url: scheme-relative userinfo with a later :// in the path",
			in:    `[{"type":"outbound_webhook","url":"//u:p@h.example.vn/a://b"}]`,
			want:  []map[string]any{{"type": "outbound_webhook", "url": "//" + m + "@h.example.vn/a://b"}},
			leaks: []string{"u:p"}},
		{name: "an unparseable url is masked whole",
			in:    `[{"type":"outbound_webhook","url":"https://h.example.vn/%zz?token=abc"}]`,
			want:  []map[string]any{{"type": "outbound_webhook", "url": m}},
			leaks: []string{"abc"}},
		{name: "nothing to hide: the stored bytes come back as they are",
			in:  `[{"type":"outbound_webhook","url":"https://erp.example.vn/hook"},{"type":"field_update","field":"stage","value":"won"}]`,
			raw: `[{"type":"outbound_webhook","url":"https://erp.example.vn/hook"},{"type":"field_update","field":"stage","value":"won"}]`},
		{name: "other actions untouched, big numbers kept exact",
			in: `[{"type":"field_update","field":"amount","value":12345678901234567},{"type":"outbound_webhook","url":"https://h.example.vn/x","secret":"k"}]`,
			want: []map[string]any{{"type": "field_update", "field": "amount", "value": json.Number("12345678901234567")},
				{"type": "outbound_webhook", "url": "https://h.example.vn/x"}}},
		{name: "a row the engine cannot decode fails closed",
			in: `{"type":"outbound_webhook","secret":"s3cr3t-key"}`, raw: `[]`, leaks: []string{"s3cr3t-key"}},
		{name: "broken JSON fails closed",
			in: `[{"type":"outbound_webhook","secret":"s3cr3t-key"`, raw: `[]`, leaks: []string{"s3cr3t-key"}},
		{name: "null stays null", in: `null`, raw: `null`},
		{name: "empty stays empty", in: ``, raw: ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := RedactActionsJSON(json.RawMessage(tc.in))
			for _, l := range tc.leaks {
				if strings.Contains(string(out), l) {
					t.Fatalf("redacted copy still carries %q: %s", l, out)
				}
			}
			if tc.want == nil {
				if string(out) != tc.raw {
					t.Fatalf("out = %s, want %s", out, tc.raw)
				}
				return
			}
			dec := json.NewDecoder(strings.NewReader(string(out)))
			dec.UseNumber()
			var got []map[string]any
			if err := dec.Decode(&got); err != nil {
				t.Fatalf("decode %s: %v", out, err)
			}
			if gb, wb := mustJSON(t, got), mustJSON(t, tc.want); gb != wb {
				t.Fatalf("out = %s\nwant %s", gb, wb)
			}
		})
	}

	t.Run("the input is not modified", func(t *testing.T) {
		in := json.RawMessage(`[{"type":"outbound_webhook","url":"https://u:p@h.example.vn/x","secret":"k","headers":{"A":"b"}}]`)
		before := string(in)
		_ = RedactActionsJSON(in)
		if string(in) != before {
			t.Fatalf("input changed: %s", in)
		}
	})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
