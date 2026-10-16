package workflow

import (
	"bytes"
	"encoding/json"
	neturl "net/url"
	"regexp"
	"strings"
)

// RedactedMarker stands in for every secret value the admin API may not show (specs/050 S3a,
// FR-025). The save path refuses a url/path still carrying it (WEBHOOK_REDACTED_VALUE): the old
// Studio builder sends back what it read verbatim, and without that refusal one "Cập nhật" would
// replace a real token with the mask. One constant for both sides so they cannot drift.
const RedactedMarker = "••••"

// redactedMarkerEncoded is the mask as a client that re-serialises a URL (new URL(...).toString(),
// a query-string builder) sends it back: percent-encoded, lower-cased for comparison.
var redactedMarkerEncoded = strings.ToLower(neturl.QueryEscape(RedactedMarker))

// HasRedactedMarker reports whether a url/path still carries the read-side mask — literally or
// percent-encoded. The save path refuses such a value (WEBHOOK_REDACTED_VALUE); checking only the
// literal lets a re-serialising client overwrite the real token with the encoded mask unnoticed.
func HasRedactedMarker(s string) bool {
	return strings.Contains(s, RedactedMarker) || strings.Contains(strings.ToLower(s), redactedMarkerEncoded)
}

// redactedMarkerEncodedRE matches the percent-encoded mask in either hex case.
var redactedMarkerEncodedRE = regexp.MustCompile("(?i)" + regexp.QuoteMeta(neturl.QueryEscape(RedactedMarker)))

// NormalizeRedactedMarker rewrites the percent-encoded mask (any hex case) to the literal one, so a
// path a re-serialising client sent back compares with RedactTarget's output.
func NormalizeRedactedMarker(s string) string {
	return redactedMarkerEncodedRE.ReplaceAllLiteralString(s, RedactedMarker)
}

// RedactActionsJSON returns a rule's stored actions as the admin API may show them (specs/050 S3a,
// D-11): `secret` dropped, every `headers` value masked, and in `url`/`path` the whole userinfo and
// every non-empty query value masked, the query names kept so the builder can still tell the
// parameters apart. A token in the path itself is NOT masked (S5 url_token; spec Nợ 9). Rows saved
// before S3a are the only ones that still carry an inline key; the vault (S2b) is where keys live now.
//
// It is for the HTTP boundary only (admin_workflow_handler List/Get/Create/Update). The engine, the
// timer / durable-webhook job handlers (repo.Get), the dry-run and the pack export read the stored
// row — masking there would send "••••" as a real header.
//
// Actions with nothing to hide come back as the stored bytes (the S2a 409 guard matches the old
// builder's resave by exact URL). A row that does not decode as a list of objects is replaced by
// `[]` rather than echoed: it may hold a secret this function cannot find.
func RedactActionsJSON(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // a field_update value like 12345678901234567 must survive the round trip
	var acts []map[string]any
	if err := dec.Decode(&acts); err != nil {
		return json.RawMessage("[]")
	}
	changed := false
	// ponytail: keyed on the field names, not on type=outbound_webhook — only webhook actions carry
	// them, and a row with a mistyped type would otherwise keep its key in plain sight.
	// Keys are matched without regard to case: encoding/json fills Action.Secret from "Secret" or
	// "SECRET" too, so the engine would send what a case-sensitive mask left readable (a row edited
	// by hand; every writer of this codebase emits lower case).
	// strings.EqualFold, not ToLower: it folds the way encoding/json does (U+017F "ſ" matches "s").
	for _, a := range acts {
		for k, v := range a { // deleting / assigning the current key while ranging is defined
			switch {
			case strings.EqualFold(k, "secret"):
				delete(a, k)
				changed = true
			case strings.EqualFold(k, "headers"):
				if m, isMap := v.(map[string]any); isMap {
					for name := range m {
						m[name] = RedactedMarker
					}
				} else {
					delete(a, k)
				}
				changed = true
			case strings.EqualFold(k, "url") || strings.EqualFold(k, "path"):
				if s, ok := v.(string); ok {
					if r := RedactTarget(s); r != s {
						a[k] = r
						changed = true
					}
				}
			}
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(acts)
	if err != nil {
		return json.RawMessage("[]")
	}
	return out
}

// RedactTarget masks the credential-bearing parts of a url or a vault path — inside
// RedactActionsJSON, and on its own where a response names a target outside the actions JSON (the
// dry-run of a url action). It works on the text, in the order net/url parses it (fragment, then
// query, then the authority's LAST '@'), so what is masked is exactly what the sender would put in
// userinfo/query — and a value with nothing to hide comes back unchanged byte for byte. A value
// net/url cannot parse is masked whole.
func RedactTarget(v string) string {
	u, err := neturl.Parse(v)
	if err != nil {
		return RedactedMarker
	}
	head, frag, hasFrag := strings.Cut(v, "#")
	head, query, hasQuery := strings.Cut(head, "?")
	if u.User != nil {
		// Where net/url starts the authority: right after "scheme://" (never a "://" further in the
		// path), or after a bare leading "//".
		start := -1
		if u.Scheme != "" {
			start = len(u.Scheme) + len("://")
		} else if strings.HasPrefix(head, "//") {
			start = len("//")
		}
		if start >= 0 && start <= len(head) {
			authority, _, _ := strings.Cut(head[start:], "/")
			if at := strings.LastIndex(authority, "@"); at >= 0 {
				head = head[:start] + redactUserinfo(authority[:at]) + head[start+at:]
			}
		}
	}
	if hasQuery {
		head += "?" + redactQuery(query)
	}
	if hasFrag {
		head += "#" + frag
	}
	return head
}

// redactUserinfo masks the whole userinfo: the name is as often the key as the password is —
// `sk_live_…@` and Stripe's `sk_live_…:@` (net/http sends both as the same Basic header), Freshdesk's
// `KEY:X` — and a masked userinfo cannot round-trip anyway (the save path refuses "••••"). Only an
// empty one (`@`, `:@`) hides nothing and stays.
func redactUserinfo(ui string) string {
	if strings.Trim(ui, ":") == "" {
		return ui
	}
	return RedactedMarker
}

// redactQuery masks every non-empty value of a raw query, keeping the names. A segment with no '='
// may itself be the token (?<key>), so it is masked whole.
func redactQuery(q string) string {
	parts := strings.Split(q, "&")
	for i, p := range parts {
		if p == "" {
			continue
		}
		name, val, hasEq := strings.Cut(p, "=")
		switch {
		case !hasEq:
			parts[i] = RedactedMarker
		case val != "":
			parts[i] = name + "=" + RedactedMarker
		}
	}
	return strings.Join(parts, "&")
}
