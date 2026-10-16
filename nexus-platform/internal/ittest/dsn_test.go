package ittest

import "testing"

func TestWithFoundRows(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"no params", "root:@tcp(h:3306)/db", "root:@tcp(h:3306)/db?clientFoundRows=true"},
		{"params", "root:@tcp(h:3306)/db?parseTime=true", "root:@tcp(h:3306)/db?parseTime=true&clientFoundRows=true"},
		{"already true", "root:@tcp(h:3306)/db?clientFoundRows=true&parseTime=true", "root:@tcp(h:3306)/db?clientFoundRows=true&parseTime=true"},
		{"explicit false is overridden by the last value", "root:@tcp(h:3306)/db?clientFoundRows=false", "root:@tcp(h:3306)/db?clientFoundRows=false&clientFoundRows=true"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := withFoundRows(tc.in); got != tc.want {
				t.Fatalf("withFoundRows(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWithUTC(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"no params", "root:@tcp(h:3306)/db", "root:@tcp(h:3306)/db?loc=UTC"},
		{"local is overridden by the last value", "root:@tcp(h:3306)/db?parseTime=true&loc=Local", "root:@tcp(h:3306)/db?parseTime=true&loc=Local&loc=UTC"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := withUTC(tc.in); got != tc.want {
				t.Fatalf("withUTC(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}
