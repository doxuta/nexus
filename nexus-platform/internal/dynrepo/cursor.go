package dynrepo

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidCursor is returned when a cursor string cannot be decoded (malformed base64,
// wrong number of segments, or payload fails basic sanity). The handler maps this to HTTP
// 400 so the client knows the cursor is bad (not a server error).
var ErrInvalidCursor = errors.New("invalid cursor")

// ErrPageTooDeep is returned when an offset-mode page number exceeds MaxOffsetPage.
// The handler maps this to HTTP 422 PAGE_TOO_DEEP so the client switches to cursor mode.
var ErrPageTooDeep = errors.New("page number too deep (use cursor pagination)")

// MaxOffsetPage is the maximum page number allowed in offset mode. Deeper than this page
// forces the client to use cursor mode: OFFSET = MaxOffsetPage*100 = 100 000 rows scanned,
// which is an O(n) full-table scan. Cap at 1000 per the D2 spec.
const MaxOffsetPage = 1000

// ErrCursorSortMismatch is returned when a well-formed cursor was minted under a different sort
// column or direction than the request replaying it. It wraps ErrInvalidCursor (same HTTP 400
// INVALID_CURSOR) so existing errors.Is(err, ErrInvalidCursor) checks keep matching; callers may
// test for it to name the mistake. Without the binding the keyset predicate compared the boundary
// value against the wrong column — and when MySQL coerced it to that column's type (an email or
// uuid against a TIMESTAMP, warning 1292, not an error) no row matched: 200, empty page,
// nextCursor null, which a sync job reads as "done" (specs/039-cursor-bootstrap §2.1).
var ErrCursorSortMismatch = fmt.Errorf("cursor minted for another sort column or order: %w", ErrInvalidCursor)

// DefaultCursorSort is the keyset sort column when the request sends no ?sort= (same default as
// offset). Shared with the service so the cursor binding compares against the column the keyset
// actually ran on.
const DefaultCursorSort = "created_at"

// maxCursorBytes is the length guard for DecodeCursor. A realistic cursor is
// "v3" + sortCol(<=26) + order(4) + flag(1) + datetime(19) + uuid(36) + 5 '|' = ~95 bytes →
// ~125 base64 chars. We allow 2 KB as an extremely generous ceiling (covers any realistic
// sort-value including long text fields) and reject larger tokens before doing base64 work,
// guarding against slow-decode DoS.
const maxCursorBytes = 2048

// cursorVersion tags the payload layout. v1/v2 tokens (no binding) fail the version check with
// ErrInvalidCursor instead of being misparsed.
const cursorVersion = "v3"

// cursorDelimiter is the single-byte separator used inside the decoded cursor payload.
// Format (v3, sort-bound, NULLS-LAST aware):
//
//	v3 | <sortCol> | <order> | <isNull> | <sortVal> | <id>
//
// <sortCol> is the PHYSICAL sort column the keyset ran on (a sanitized identifier, never "|"),
// <order> is "ASC" or "DESC", <isNull> is "0" (non-null) or "1" (null sort value), <sortVal> is the
// MySQL-native text of the sort column (may contain "|"), and <id> is a 36-char UUID (never "|").
// The first four fields are split from the LEFT and <id> from the RIGHT, so <sortVal> captures any
// "|" in the sort value.
const cursorDelimiter = "|"

// EncodeCursor encodes a boundary row's sort-column value and record id into an opaque
// base64url cursor token, bound to the physical sort column and direction ("ASC"/"DESC") the page
// was read with — DecodeCursor refuses the token under any other sort/order.
//
// isNullSort must be true when the sort column is SQL NULL for that row (CAST(col AS CHAR)
// scans as nil []byte in Go). When true, sortVal is ignored; the cursor encodes a flag so
// the next page can reconstruct the correct NULL-tail predicate without confusion with a
// genuine empty-string sort value.
func EncodeCursor(sortCol, order string, isNullSort bool, sortVal, id string) string {
	flag := "0"
	val := sortVal
	if isNullSort {
		flag = "1"
		val = "" // normalize: NULL cursors don't carry a sort value
	}
	raw := strings.Join([]string{cursorVersion, sortCol, order, flag, val, id}, cursorDelimiter)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor reverses EncodeCursor for a request that sorts by sortCol in order ("ASC"/"DESC").
// Returns isNull, sortVal, id; ErrInvalidCursor on any malformed input (tokens longer than 2 KB are
// rejected before base64 work) and ErrCursorSortMismatch when the token was minted under another
// sort column or direction.
//
// The isNull return indicates whether the boundary row's sort column was SQL NULL. When true,
// sortVal is always "" (discard it; the caller uses a NULL-specific keyset predicate).
func DecodeCursor(cursor, sortCol, order string) (isNull bool, sortVal, id string, err error) {
	if strings.TrimSpace(cursor) == "" {
		return false, "", "", fmt.Errorf("dynrepo.DecodeCursor: %w", ErrInvalidCursor)
	}
	if len(cursor) > maxCursorBytes {
		return false, "", "", fmt.Errorf("dynrepo.DecodeCursor: token too long: %w", ErrInvalidCursor)
	}
	b, decErr := base64.RawURLEncoding.DecodeString(cursor)
	if decErr != nil {
		return false, "", "", fmt.Errorf("dynrepo.DecodeCursor: base64: %w", ErrInvalidCursor)
	}

	// version | sortCol | order | flag | "<sortVal>|<id>" — none of the first four contain '|'.
	parts := strings.SplitN(string(b), cursorDelimiter, 5)
	if len(parts) != 5 || parts[0] != cursorVersion {
		return false, "", "", fmt.Errorf("dynrepo.DecodeCursor: not a %s cursor: %w", cursorVersion, ErrInvalidCursor)
	}
	if parts[1] != sortCol || parts[2] != order {
		return false, "", "", fmt.Errorf("dynrepo.DecodeCursor: minted for %s %s, request is %s %s: %w",
			parts[1], parts[2], sortCol, order, ErrCursorSortMismatch)
	}
	switch parts[3] {
	case "0":
		isNull = false
	case "1":
		isNull = true
	default:
		return false, "", "", fmt.Errorf("dynrepo.DecodeCursor: unknown flag %q: %w", parts[3], ErrInvalidCursor)
	}

	// Split from the RIGHT to get the id (UUID, never contains '|').
	rest := parts[4]
	lastPipe := strings.LastIndex(rest, cursorDelimiter)
	if lastPipe < 0 {
		return false, "", "", fmt.Errorf("dynrepo.DecodeCursor: missing id delimiter: %w", ErrInvalidCursor)
	}
	id = rest[lastPipe+1:]
	sortVal = rest[:lastPipe]

	if id == "" {
		return false, "", "", fmt.Errorf("dynrepo.DecodeCursor: empty id: %w", ErrInvalidCursor)
	}
	// isNull cursors carry an empty sortVal (EncodeCursor normalizes them); the caller ignores
	// sortVal when isNull=true anyway, so a non-empty one is not rejected here.
	return isNull, sortVal, id, nil
}
