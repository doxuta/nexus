package ai

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestAutoTitle covers the pure title-derivation logic (spec §11: rune-truncate the first user
// message, NO LLM call). The DB-backed scoping/ownership behavior is proven separately by the
// integration test (real MySQL) — this asserts only the string transform.
func TestAutoTitle(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "short message passes through unchanged",
			in:   "Tôi cần quản lý bệnh nhân",
			want: "Tôi cần quản lý bệnh nhân",
		},
		{
			name: "whitespace and newlines collapse to single spaces",
			in:   "  Tôi làm\n\tphòng   khám  ",
			want: "Tôi làm phòng khám",
		},
		{
			name: "empty message yields the stable default",
			in:   "   \n\t ",
			want: "Phiên mới",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AutoTitle(tt.in); got != tt.want {
				t.Fatalf("AutoTitle(%q) = %q; want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestAutoTitle_RuneTruncate proves a long, multibyte (Vietnamese) message is cut on a RUNE
// boundary to ~60 runes with an ellipsis — never mid-rune (which would corrupt the UTF-8) and never
// counted in bytes (which would truncate far too early for Vietnamese).
func TestAutoTitle_RuneTruncate(t *testing.T) {
	// 100 Vietnamese runes (each multi-byte in UTF-8) with no spaces to collapse.
	long := strings.Repeat("ệ", 100)
	got := AutoTitle(long)

	if !utf8.ValidString(got) {
		t.Fatalf("AutoTitle produced invalid UTF-8 (cut mid-rune): %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("AutoTitle(long) = %q; want a trailing ellipsis marking truncation", got)
	}
	// autoTitleRunes runes total: (autoTitleRunes-1) content runes + the ellipsis.
	if n := utf8.RuneCountInString(got); n != autoTitleRunes {
		t.Fatalf("AutoTitle(long) rune count = %d; want %d (%d content + ellipsis)", n, autoTitleRunes, autoTitleRunes-1)
	}
}

// TestClampRunes guards the VARCHAR(200) backstop AppendTurn relies on when creating a session: a
// title longer than the column is cut on a rune boundary, never mid-rune.
func TestClampRunes(t *testing.T) {
	over := strings.Repeat("ữ", maxTitleRunes+50)
	got := clampRunes(over, maxTitleRunes)
	if utf8.RuneCountInString(got) != maxTitleRunes {
		t.Fatalf("clampRunes rune count = %d; want %d", utf8.RuneCountInString(got), maxTitleRunes)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("clampRunes produced invalid UTF-8: %q", got)
	}
	if clampRunes("short", maxTitleRunes) != "short" {
		t.Fatal("clampRunes must pass a short string through unchanged")
	}
}

// Issue #42: an MCP thread replays only the proposals the builder has not settled, in thread order;
// a turn without a draft, or whose draft does not decode, adds nothing.
func TestPendingProposals_SkipsSettledCards(t *testing.T) {
	msgs := []Message{
		{Seq: 0, Role: "user", Content: "[MCP] Kết nối agent bên ngoài"},
		{Seq: 1, Role: "assistant", DraftJSON: json.RawMessage(`[{"kind":"field","field":{"entity":"a","name":"x"}},{"kind":"field","field":{"entity":"a","name":"y"}}]`), Settled: []int{0}},
		{Seq: 3, Role: "assistant", DraftJSON: json.RawMessage(`not json`)},
		{Seq: 5, Role: "assistant", DraftJSON: json.RawMessage(`[{"kind":"entity","entity":{"name":"b"}}]`), Settled: []int{0}},
		{Seq: 7, Role: "assistant", DraftJSON: json.RawMessage(`[{"kind":"entity","entity":{"name":"c"}}]`)},
	}
	var got []string
	for _, it := range PendingProposals(msgs) {
		switch {
		case it.Field != nil:
			got = append(got, it.Field.Name)
		case it.Entity != nil:
			got = append(got, it.Entity.Name)
		}
	}
	if want := []string{"y", "c"}; !slices.Equal(got, want) {
		t.Fatalf("pending = %v; want %v (settled cards left out, thread order kept)", got, want)
	}
}
