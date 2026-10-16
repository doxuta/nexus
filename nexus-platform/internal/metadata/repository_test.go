package metadata

// Pure-function unit tests for the tombstone helpers (no DB) — table-driven per project
// convention. The DB-backed effects of tombstoning (name/join_table_name actually freed in
// MySQL, transaction atomicity) are covered by the integration tests in
// internal/services/relation_lifecycle_integration_test.go.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTombstoneSuffix(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{"uuid truncated to 8 chars", "11111111-2222-3333-4444-555555555555", "11111111"},
		{"short id kept as-is", "abc", "abc"},
		{"exactly 8 chars kept as-is", "12345678", "12345678"},
		{"empty id", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tombstoneSuffix(tt.id); got != tt.want {
				t.Errorf("tombstoneSuffix(%q) = %q; want %q", tt.id, got, tt.want)
			}
		})
	}
}

func TestTombstoneName(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		suffix string
	}{
		{"short ASCII name fits untouched", "accounts", 100, "11111111"},
		{"name at exactly the limit", strings.Repeat("a", 100-len("__deleted_11111111")), 100, "11111111"},
		{
			"long ASCII name truncated",
			strings.Repeat("a", 200),
			100, "11111111",
		},
		{
			// W2 review fix #2: a long multibyte (Vietnamese) name whose byte length
			// exceeds maxLen must NOT be sliced mid-rune. Each 'ệ'/'ố' etc. below is a
			// 3-byte UTF-8 sequence, so a byte-index slice at an arbitrary boundary
			// (the pre-fix `name[:keep]`) would very likely land inside one.
			"long Vietnamese name (multibyte) truncated safely",
			strings.Repeat("Quan hệ khách hàng và đơn hàng liên kết ", 6), // 240 chars, ~280+ bytes
			150, "abcdef12",
		},
		{
			"multibyte name where keepBytes lands exactly mid-rune byte-wise",
			// "Tê" = 'T' (1 byte) + 'ê' (2 bytes) repeated — engineered so a naive
			// byte-slice at various offsets is very likely to cut 'ê' in half.
			strings.Repeat("Tê", 80), // 240 bytes
			130, "deadbeef",
		},
		{"empty name", "", 100, "11111111"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tombstoneName(tt.input, tt.maxLen, tt.suffix)

			if !utf8.ValidString(got) {
				t.Fatalf("tombstoneName(%q, %d, %q) = %q; NOT valid UTF-8 (mid-rune cut)",
					tt.input, tt.maxLen, tt.suffix, got)
			}
			if len(got) > tt.maxLen {
				t.Errorf("tombstoneName(%q, %d, %q) = %q; byte length %d exceeds maxLen %d",
					tt.input, tt.maxLen, tt.suffix, got, len(got), tt.maxLen)
			}
			if !strings.HasSuffix(got, "__deleted_"+tt.suffix) {
				t.Errorf("tombstoneName(%q, %d, %q) = %q; must end with __deleted_%s (suffix never truncated)",
					tt.input, tt.maxLen, tt.suffix, got, tt.suffix)
			}
			// The original name portion (everything before "__deleted_<suffix>") must be a
			// prefix of the original input, whole-rune — never a fragment with a broken
			// trailing rune.
			namePart := strings.TrimSuffix(got, "__deleted_"+tt.suffix)
			if !strings.HasPrefix(tt.input, namePart) {
				t.Errorf("tombstoneName(%q, %d, %q): name portion %q is not a whole-rune prefix of input",
					tt.input, tt.maxLen, tt.suffix, namePart)
			}
		})
	}
}

// TestTombstoneName_NeverExceedsMaxLenAcrossManyOffsets is a focused regression proof for
// the byte-vs-rune bug: for every possible maxLen in a range that forces truncation deep
// inside multibyte runes, the result must stay valid UTF-8 and within budget. Sweeping the
// budget catches the "landed exactly mid-rune" case regardless of exact byte offsets.
func TestTombstoneName_NeverExceedsMaxLenAcrossManyOffsets(t *testing.T) {
	name := strings.Repeat("Quan hệ khách hàng ưu tiên đơn hàng liên kết nhiều tầng ", 4)
	suffix := "1a2b3c4d"
	for maxLen := 20; maxLen <= 220; maxLen++ {
		got := tombstoneName(name, maxLen, suffix)
		if !utf8.ValidString(got) {
			t.Fatalf("maxLen=%d: tombstoneName = %q; invalid UTF-8", maxLen, got)
		}
		if len(got) > maxLen {
			t.Fatalf("maxLen=%d: tombstoneName byte length %d exceeds maxLen", maxLen, len(got))
		}
	}
}
