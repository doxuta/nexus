package services

import "testing"

// TestIsBlankValue: a required field is unfilled when absent, null, blank text, an empty list, or a
// composite whose parts are all blank; zero and false are answers.
func TestIsBlankValue(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want bool
	}{
		{"absent", nil, true},
		{"empty string", "", true},
		{"spaces", "  \t", true},
		{"text", "x", false},
		{"zero", 0, false},
		{"zero float", float64(0), false},
		{"false", false, false},
		{"empty list", []any{}, true},
		{"list", []any{"a"}, false},
		{"empty composite", map[string]any{}, true},
		{"composite of blanks", map[string]any{"amount": nil, "currencyCode": " "}, true},
		{"composite with a part", map[string]any{"amount": float64(1500000), "currencyCode": "VND"}, false},
		{"composite with a zero amount", map[string]any{"amount": float64(0)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBlankValue(tc.v); got != tc.want {
				t.Fatalf("isBlankValue(%#v) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}
