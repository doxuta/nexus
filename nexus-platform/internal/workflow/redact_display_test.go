package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactActionsForDisplay (specs/067 T046): the form of a rule's actions the AI may read.
func TestRedactActionsForDisplay(t *testing.T) {
	in := json.RawMessage(`[{"type":"outbound_webhook","url":"https://u:p@api.telegram.org:8443/bot123:ABC/send?q=SENTINEL-Q","secret":"SENTINEL-KEY","headers":{"X-Api":"SENTINEL-HDR"},"path":"/bot123:ABC/","credentialId":"cred-1"},{"type":"field_update","field":"status","value":"x"}]`)
	orig := string(in)
	out := string(RedactActionsForDisplay(in))
	if string(in) != orig {
		t.Fatal("input mutated")
	}
	for _, leak := range []string{"SENTINEL-KEY", "SENTINEL-HDR", "bot123", "SENTINEL-Q", "u:p"} {
		if strings.Contains(out, leak) {
			t.Fatalf("leaked %q: %s", leak, out)
		}
	}
	var acts []map[string]any
	if err := json.Unmarshal([]byte(out), &acts); err != nil {
		t.Fatal(err)
	}
	w := acts[0]
	if w["url"] != "https://api.telegram.org:8443" || w["secret"] != "[đã che]" || w["path"] != "[đã che]" || w["credentialId"] != "cred-1" {
		t.Fatalf("webhook action wrong: %v", w)
	}
	if h := w["headers"].(map[string]any); h["X-Api"] != "[đã che]" {
		t.Fatalf("header value/name wrong: %v", h)
	}
	if acts[1]["field"] != "status" || acts[1]["value"] != "x" {
		t.Fatalf("non-webhook action changed: %v", acts[1])
	}
	if string(RedactActionsForDisplay(json.RawMessage(`garbage`))) != "[]" {
		t.Fatal("undecodable must become []")
	}
}
