package vertical_pack

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

var currencyCodeRe = regexp.MustCompile(`"currencyCode":"([^"]*)"`)

// DAILY-QUEUE #158/#439 — VN-only: no built-in pack may declare or seed a USD amount.
// The CRM pack used to declare USD on annual_revenue/amount/weighted_value while every other
// money field was VND, so a fresh tenant regenerated the mixed-currency rows Doctor D2 flags.
func TestBuiltinPacks_NoUSD(t *testing.T) {
	for _, slug := range BuiltinSlugs() {
		pack, ok := BuiltinPack(slug)
		if !ok {
			t.Fatalf("pack %q listed but not buildable", slug)
		}
		raw, err := json.Marshal(pack)
		if err != nil {
			t.Fatalf("marshal pack %q: %v", slug, err)
		}
		if strings.Contains(string(raw), `"USD"`) {
			t.Errorf("pack %q still mentions USD (fields or sample records) — VN-only, use VND", slug)
		}
		// VN-only: any currencyCode value (field option or seeded amount) must be exactly VND.
		for _, m := range currencyCodeRe.FindAllStringSubmatch(string(raw), -1) {
			if m[1] != "VND" {
				t.Errorf("pack %q has currencyCode %q — VN-only, use VND", slug, m[1])
			}
		}
	}
}
