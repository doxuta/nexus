package metaversion

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Canonical re-encodes a JSON document with sorted object keys and no insignificant whitespace,
// numbers kept verbatim, so two renderings of the same document (MySQL's JSON column re-orders keys
// and re-spaces) hash alike. Empty input and JSON null canonicalise to "null".
func Canonical(raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("null"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("metaversion.Canonical: %w", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil { // map keys are sorted by encoding/json
		return nil, fmt.Errorf("metaversion.Canonical: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// DocHash is the sha256 (hex) of a document's canonical form: the draft's base_hash and the
// live-state comparison of a publish.
func DocHash(raw []byte) (string, error) {
	c, err := Canonical(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:]), nil
}

// ChangesetHash fingerprints the set of drafts the builder looked at: sha256 of the sorted
// "id|updated_at(ms)" lines. A publish or discard carrying a different value is refused
// (DRAFTS_CHANGED), so only the exact set that was seen is pushed.
func ChangesetHash(rows []DraftRow) string {
	lines := make([]string, len(rows))
	for i := range rows {
		lines[i] = rows[i].ID + "|" + rows[i].UpdatedAt.UTC().Format("2006-01-02T15:04:05.000")
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}
