package services

import (
	"regexp"
	"testing"
)

// TestCompileAnchoredPattern_Memoizes proves O5: the same raw pattern compiles ONCE and is
// reused (identical *regexp.Regexp pointer), so checkTextConstraints no longer recompiles per
// record. Distinct patterns get distinct instances.
func TestCompileAnchoredPattern_Memoizes(t *testing.T) {
	re1, err := compileAnchoredPattern(`[A-Z]+`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	re2, err := compileAnchoredPattern(`[A-Z]+`)
	if err != nil {
		t.Fatalf("compile (2nd): %v", err)
	}
	if re1 != re2 {
		t.Errorf("same pattern returned different *regexp.Regexp instances — cache miss (not compile-once)")
	}

	reOther, err := compileAnchoredPattern(`[0-9]+`)
	if err != nil {
		t.Fatalf("compile other: %v", err)
	}
	if reOther == re1 {
		t.Errorf("distinct patterns must not share a compiled instance")
	}
}

// TestCompileAnchoredPattern_FullMatchAnchoring verifies the "^(?:…)$" anchoring matches the
// field-def-time semantics: the whole string must match, not a substring.
func TestCompileAnchoredPattern_FullMatchAnchoring(t *testing.T) {
	re, err := compileAnchoredPattern(`[A-Z]+`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	cases := []struct {
		in   string
		want bool
	}{
		{"ABC", true},
		{"abc", false},  // lowercase: no match
		{"AB1", false},  // trailing digit breaks full match (anchoring works)
		{"1ABC", false}, // leading digit breaks full match
		{"", false},     // empty: + requires at least one
	}
	for _, c := range cases {
		if got := re.MatchString(c.in); got != c.want {
			t.Errorf("MatchString(%q) = %v; want %v", c.in, got, c.want)
		}
	}
}

// TestCompileAnchoredPattern_BadPatternErrors confirms an invalid regex surfaces an error
// (fail-closed at record-time) rather than being cached.
func TestCompileAnchoredPattern_BadPatternErrors(t *testing.T) {
	if _, err := compileAnchoredPattern(`[unterminated`); err == nil {
		t.Error("expected compile error for invalid pattern, got nil")
	}
	// A subsequent valid compile of a different pattern still works (bad one not poisoned-cached).
	if _, err := compileAnchoredPattern(`x{2}`); err != nil {
		t.Errorf("valid pattern after bad one: %v", err)
	}
	// Sanity: the regexp package itself agrees the bad pattern is invalid.
	if _, err := regexp.Compile(`^(?:[unterminated)$`); err == nil {
		t.Error("regexp.Compile unexpectedly accepted the malformed pattern")
	}
}
