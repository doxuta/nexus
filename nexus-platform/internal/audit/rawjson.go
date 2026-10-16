package audit

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// RawJSON is json.RawMessage that can scan a NULL-able MySQL JSON column (W7 audit-v2 fix).
//
// Why it exists: audit_log.diff is NULL for events recorded with a nil diff (e.g. a vertical-pack
// seed insert, N-N link rows). database/sql can convert SQL NULL into *[]byte but NOT into
// *json.RawMessage (a DIFFERENT named type with no sql.Scanner), so the first NULL-diff row on a
// page made the whole SELECT fail — observed live as `GET /audit-log?limit=N` returning 500
// AUDIT_QUERY_FAILED as soon as the page was deep enough to include such a row (the FE worked
// around it by hard-capping limit=10). Implementing sql.Scanner here makes NULL scan to nil.
//
// JSON contract is byte-identical to json.RawMessage: nil marshals as `null`, non-nil is
// forwarded verbatim (no re-marshalling of the stored diff).
type RawJSON []byte

// Scan implements sql.Scanner: NULL → nil, []byte/string → copied verbatim. The []byte case
// MUST copy — the driver may reuse its buffer for the next row.
func (r *RawJSON) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*r = nil
		return nil
	case []byte:
		*r = RawJSON(bytes.Clone(v))
		return nil
	case string:
		*r = RawJSON(v)
		return nil
	default:
		return fmt.Errorf("audit.RawJSON.Scan: unsupported source type %T", src)
	}
}

// Value implements driver.Valuer for symmetry (nil → SQL NULL). The write path (InsertEvent)
// does not use it today, but a round-trip-capable type avoids surprises in future callers.
func (r RawJSON) Value() (driver.Value, error) {
	if len(r) == 0 {
		return nil, nil
	}
	return []byte(r), nil
}

// MarshalJSON mirrors json.RawMessage: emit the raw bytes verbatim, `null` when empty — so the
// HTTP envelope is unchanged by the json.RawMessage → RawJSON migration.
func (r RawJSON) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return json.RawMessage(r).MarshalJSON()
}

// UnmarshalJSON mirrors json.RawMessage so tests/tools can decode an envelope back into AuditRow.
func (r *RawJSON) UnmarshalJSON(data []byte) error {
	if r == nil {
		return fmt.Errorf("audit.RawJSON.UnmarshalJSON: nil receiver")
	}
	*r = RawJSON(bytes.Clone(data))
	return nil
}
