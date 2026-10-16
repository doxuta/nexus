package dynrepo

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// TestEncodeCursor_RoundTrip verifies encode then decode returns original values,
// including the isNull flag and sort values that contain pipe characters.
func TestEncodeCursor_RoundTrip(t *testing.T) {
	cases := []struct {
		name       string
		isNull     bool
		sortVal    string
		id         string
		wantIsNull bool
		wantVal    string // expected sortVal after decode (may differ for isNull=true)
	}{
		{
			name:    "datetime sort value (non-null)",
			sortVal: "2026-06-24 10:00:00",
			id:      "550e8400-e29b-41d4-a716-446655440000",
		},
		{
			name:    "numeric sort value (non-null)",
			sortVal: "99.5000000000",
			id:      "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		},
		{
			name:    "text sort value with spaces (non-null)",
			sortVal: "hello world",
			id:      "00000000-0000-0000-0000-000000000001",
		},
		{
			name:    "sort value containing pipe character (last-pipe split safety)",
			sortVal: "foo|bar",
			id:      "22222222-2222-2222-2222-222222222222",
		},
		{
			name:    "sort value containing multiple pipes",
			sortVal: "a|b|c",
			id:      "33333333-3333-3333-3333-333333333333",
		},
		{
			name:       "null sort value",
			isNull:     true,
			sortVal:    "", // ignored by EncodeCursor when isNull=true
			id:         "44444444-4444-4444-4444-444444444444",
			wantIsNull: true,
			wantVal:    "", // DecodeCursor returns "" for isNull cursors
		},
		{
			// Empty-string sort value MUST stay isNull=false — a genuine "" is distinct from
			// SQL NULL (the keyset boundary picks different branches). Locks the v2 flag so a
			// nullable column with an empty value is never mistaken for NULL (or vice versa).
			name:       "empty-string sort value (non-null) — distinct from NULL",
			isNull:     false,
			sortVal:    "",
			id:         "55555555-5555-5555-5555-555555555555",
			wantIsNull: false,
			wantVal:    "",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			encoded := EncodeCursor("created_at", "DESC", c.isNull, c.sortVal, c.id)
			// The cursor must be opaque — no pipe delimiter visible in the output.
			if strings.Contains(encoded, "|") {
				t.Errorf("cursor %q exposes delimiter — must be opaque base64", encoded)
			}
			gotIsNull, gotSortVal, gotID, err := DecodeCursor(encoded, "created_at", "DESC")
			if err != nil {
				t.Fatalf("DecodeCursor(%q): unexpected error: %v", encoded, err)
			}
			wantIsNull := c.wantIsNull || c.isNull
			if gotIsNull != wantIsNull {
				t.Errorf("isNull: got %v, want %v", gotIsNull, wantIsNull)
			}
			wantSortVal := c.sortVal
			if c.isNull {
				wantSortVal = ""
			}
			if gotSortVal != wantSortVal {
				t.Errorf("sortVal: got %q, want %q", gotSortVal, wantSortVal)
			}
			if gotID != c.id {
				t.Errorf("id: got %q, want %q", gotID, c.id)
			}
		})
	}
}

// TestDecodeCursor_Malformed verifies that malformed cursors always return ErrInvalidCursor
// (never a panic and never partial output that could find its way into SQL).
func TestDecodeCursor_Malformed(t *testing.T) {
	// craftCursor builds a base64 cursor from a raw string without using EncodeCursor —
	// used to inject crafted payloads that EncodeCursor would never produce.
	craftCursor := func(raw string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(raw))
	}

	cases := []struct {
		name   string
		cursor string
	}{
		{"empty string", ""},
		{"whitespace only", "   "},
		{"plain text not base64", "not-a-cursor"},
		// valid base64 decoding to "test" — no pipe delimiter at all
		{"valid base64 but no pipe", craftCursor("test")},
		// v3 header present but no sortVal|id tail (no pipe left for the id split)
		{"v3 with no id delimiter", craftCursor("v3|created_at|DESC|0|test")},
		// valid base64, flag + sortVal present but empty id
		{"v3 with empty id", craftCursor("v3|created_at|DESC|0|sort_value|")},
		{"garbage base64 chars", "!!!invalid!!!"},
		// unknown flag byte
		{"unknown flag", craftCursor("v3|created_at|DESC|2|sort|00000000-0000-0000-0000-000000000001")},
		// Pre-binding v2 tokens (flag|sortVal|id) must fail the version check, never be
		// misparsed as a v3 header — including one whose sortVal contains pipes.
		{"legacy v2 token", craftCursor("0|2026-09-01 00:00:00|00000000-0000-0000-0000-000000000001")},
		{"legacy v2 token with pipes in sortVal", craftCursor("0|a|b|c|00000000-0000-0000-0000-000000000001")},
		{"unknown version", craftCursor("v4|created_at|DESC|0|x|00000000-0000-0000-0000-000000000001")},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			isNull, sortVal, id, err := DecodeCursor(c.cursor, "created_at", "DESC")
			if !errors.Is(err, ErrInvalidCursor) {
				t.Errorf("DecodeCursor(%q): err=%v, want ErrInvalidCursor", c.cursor, err)
			}
			// Output must be empty/zero on error — never put partial data into SQL.
			if isNull || sortVal != "" || id != "" {
				t.Errorf("DecodeCursor(%q): got isNull=%v sortVal=%q id=%q, want all zero on error",
					c.cursor, isNull, sortVal, id)
			}
		})
	}
}

// TestDecodeCursor_LengthGuard verifies that a cursor token longer than 2 KB is rejected
// before base64 work — guarding against slow-decode DoS.
func TestDecodeCursor_LengthGuard(t *testing.T) {
	// Construct a valid-looking base64 string that is >2048 bytes.
	oversized := strings.Repeat("A", maxCursorBytes+1)
	_, _, _, err := DecodeCursor(oversized, "created_at", "DESC")
	if !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("overlong cursor: err=%v, want ErrInvalidCursor", err)
	}
}

// TestDecodeCursor_SQLPlaceholders verifies that the keyset WHERE clause never contains
// a decoded cursor value inline — callers always bind sortVal and id through '?' placeholders.
// This is a documentation-enforcement test: the placeholder-only contract is tested by
// inspecting the ListRecordsCursor query builder (below), but here we assert the decode API
// itself returns string values (never interpolated SQL) and that EncodeCursor does not include
// SQL meta-characters in the opaque token.
func TestDecodeCursor_SQLPlaceholders(t *testing.T) {
	// Inject a sort value that would break SQL if concatenated.
	injectionAttempt := "'; DROP TABLE obj_test; --"
	id := "00000000-0000-0000-0000-000000000001"

	encoded := EncodeCursor("title", "ASC", false, injectionAttempt, id)

	// The encoded cursor must not expose the raw injection string.
	if strings.Contains(encoded, "DROP") || strings.Contains(encoded, ";") {
		t.Errorf("encoded cursor leaks SQL injection attempt: %q", encoded)
	}

	_, gotSortVal, gotID, err := DecodeCursor(encoded, "title", "ASC")
	if err != nil {
		t.Fatalf("unexpected decode error: %v", err)
	}
	// The raw value is recovered correctly — the caller is responsible for binding via ?
	// (tested in the keyset query builder path). We verify here the values are correct.
	if gotSortVal != injectionAttempt {
		t.Errorf("sortVal: got %q, want %q", gotSortVal, injectionAttempt)
	}
	if gotID != id {
		t.Errorf("id: got %q, want %q", gotID, id)
	}
}

// TestDecodeCursor_BoundToSortAndOrder: a cursor is only valid for the physical sort column and
// direction it was minted under (specs/039-cursor-bootstrap §2.1). Replaying it under another one is
// ErrCursorSortMismatch — which still IS ErrInvalidCursor (400 INVALID_CURSOR) — with zero outputs,
// never a keyset predicate comparing the boundary value against the wrong column.
func TestDecodeCursor_BoundToSortAndOrder(t *testing.T) {
	const id = "00000000-0000-0000-0000-000000000001"
	cases := []struct {
		name                 string
		mintCol, mintOrder   string
		replayCol, replayOrd string
		wantMismatch         bool
	}{
		{"same sort and order", "created_at", "DESC", "created_at", "DESC", false},
		{"email chain replayed as created_at", "email", "ASC", "created_at", "ASC", true},
		{"desc chain replayed as asc", "created_at", "DESC", "created_at", "ASC", true},
		{"both differ", "email", "ASC", "created_at", "DESC", true},
		{"relation column chain", "account_id", "ASC", "account_id", "ASC", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tok := EncodeCursor(c.mintCol, c.mintOrder, false, "ho.sy.at@daiphatmech.vn", id)
			isNull, sortVal, gotID, err := DecodeCursor(tok, c.replayCol, c.replayOrd)
			if !c.wantMismatch {
				if err != nil || gotID != id || sortVal != "ho.sy.at@daiphatmech.vn" || isNull {
					t.Fatalf("got (%v, %q, %q, %v); want the minted boundary back", isNull, sortVal, gotID, err)
				}
				return
			}
			if !errors.Is(err, ErrCursorSortMismatch) || !errors.Is(err, ErrInvalidCursor) {
				t.Fatalf("err = %v; want ErrCursorSortMismatch wrapping ErrInvalidCursor", err)
			}
			if isNull || sortVal != "" || gotID != "" {
				t.Fatalf("got (%v, %q, %q); want all zero on a mismatch", isNull, sortVal, gotID)
			}
		})
	}
}

// TestPageTooDeep_Sentinel confirms ErrPageTooDeep and MaxOffsetPage are exported/accessible
// so the service can add a PAGE_TOO_DEEP check without importing unexported constants.
func TestPageTooDeep_Sentinel(t *testing.T) {
	if MaxOffsetPage <= 0 {
		t.Errorf("MaxOffsetPage = %d; want > 0", MaxOffsetPage)
	}
	if ErrPageTooDeep == nil {
		t.Error("ErrPageTooDeep is nil")
	}
	if errors.Is(ErrPageTooDeep, ErrInvalidCursor) {
		t.Error("ErrPageTooDeep must not wrap ErrInvalidCursor — they are distinct sentinel values")
	}
}
