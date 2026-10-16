package dynrepo

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/ddl"
)

// LabelResult is one {id, label} pair returned by BatchFetchLabels.
type LabelResult struct {
	ID    string
	Label string
}

// BatchFetchLabels resolves a set of record IDs in a target entity to their label values
// in a SINGLE query (no N+1). The label value comes from the JSON `data` column via a
// JSON_EXTRACT path (e.g. `$.name`) — it is NOT a physical column — so `labelField` is a
// logical field name, not a column name. Both the entity name and labelField are sanitized
// through ddl.SanitizeIdentifier before being inlined into the query; record IDs and the
// tenant_id always travel as `?` placeholders (sqlx.In expands the IN clause).
//
// Caller rules (RM-07 §depth):
//   - Only called for relation fields (depth = 1); this method does NOT recurse.
//   - `ids` must be non-empty (the caller guards this).
//   - Unknown IDs (deleted, cross-tenant, simply absent) are silently omitted from the
//     result — the caller substitutes a null/missing label for unmapped ids.
func (r *Repository) BatchFetchLabels(ctx context.Context, targetEntity, tenantID, labelField string, ids []string) ([]LabelResult, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	// Sanitize the entity name and label field as identifiers before inlining.
	entityName, err := ddl.SanitizeIdentifier(targetEntity)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.BatchFetchLabels: entity %q: %w", targetEntity, err)
	}
	lf, err := ddl.SanitizeIdentifier(labelField)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.BatchFetchLabels: labelField %q: %w", labelField, err)
	}

	table := "obj_" + entityName
	// Build the parameterized IN clause via sqlx.In so each id is a `?` placeholder.
	// The slice is converted to []any so sqlx.In accepts it.
	anyIDs := make([]any, len(ids))
	for i, id := range ids {
		anyIDs[i] = id
	}

	// Inline-safe: table and JSON path use sanitized identifiers; values are placeholders.
	baseQuery := fmt.Sprintf(
		"SELECT `id`, COALESCE(JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.%s')), '') AS `label` "+
			"FROM `%s` WHERE `tenant_id` = ? AND `id` IN (?) AND `deleted_at` IS NULL",
		lf, table,
	)
	// sqlx.In rewrites the `?` IN placeholder to the correct number of `?` args.
	query, args, err := sqlx.In(baseQuery, tenantID, anyIDs)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.BatchFetchLabels: build IN query: %w", err)
	}
	// Rebind for the MySQL driver (replaces `?` with `?` — a no-op for MySQL, but correct
	// practise so the method works with any sqlx-compatible driver).
	query = r.db.Rebind(query)

	type row struct {
		ID    string `db:"id"`
		Label string `db:"label"`
	}
	var rows []row
	if err := r.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, fmt.Errorf("dynrepo.BatchFetchLabels: select: %w", err)
	}

	results := make([]LabelResult, 0, len(rows))
	for _, r := range rows {
		results = append(results, LabelResult{ID: r.ID, Label: r.Label})
	}
	return results, nil
}

// FieldValueResult is one {id, value} pair returned by BatchFetchField. Value carries the
// field's value with its native JSON type preserved (string/float64/bool/map/slice/nil) —
// UNLIKE LabelResult.Label (BatchFetchLabels), which stringifies via JSON_UNQUOTE. A lookup
// field (ENG-COMPUTED #2) mirrors a sibling field of ANY type (currency, number, composite,
// picklist, ...), so coercing everything to a string would corrupt non-text source fields
// (e.g. a currency {amount,currencyCode} object read back as the Go string
// "map[amount:...]"). Value is nil when the source row's field is JSON null or the key is
// absent from `data`.
type FieldValueResult struct {
	ID    string
	Value any
}

// BatchFetchField resolves a set of record IDs in a target entity to the value of ONE field
// (sourceField) in a SINGLE query (no N+1) — the read-time lookup primitive (ENG-COMPUTED #2).
// It generalizes BatchFetchLabels: same tenant-scoped IN-list shape, but returns the field's
// raw JSON value (type-preserving) instead of a stringified label. sourceField is a logical
// field name (a JSON `data` key), not a physical column — both targetEntity and sourceField
// are sanitized through ddl.SanitizeIdentifier before being inlined; record IDs and tenant_id
// always travel as `?` placeholders (sqlx.In expands the IN clause).
//
// Caller rules (mirrors BatchFetchLabels):
//   - Only called for a lookup field's `via` sibling relation target; depth-capped to 1 by
//     the caller (record_expand.go) — this method does not recurse.
//   - `ids` must be non-empty (the caller guards this).
//   - Unknown IDs (soft-deleted, cross-tenant, simply absent) are silently omitted from the
//     result — the caller substitutes null for unmapped ids (RECORD deleted-source edge case).
func (r *Repository) BatchFetchField(ctx context.Context, targetEntity, tenantID, sourceField string, ids []string) ([]FieldValueResult, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	entityName, err := ddl.SanitizeIdentifier(targetEntity)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.BatchFetchField: entity %q: %w", targetEntity, err)
	}
	sf, err := ddl.SanitizeIdentifier(sourceField)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.BatchFetchField: sourceField %q: %w", sourceField, err)
	}

	table := "obj_" + entityName
	anyIDs := make([]any, len(ids))
	for i, id := range ids {
		anyIDs[i] = id
	}

	// JSON_EXTRACT (WITHOUT JSON_UNQUOTE) preserves the value's native JSON encoding — a
	// string stays a quoted JSON string, a number stays a JSON number, an object/array stays
	// structured — so the Go json.Unmarshal below reconstructs the original Go type
	// (string/float64/bool/map[string]any/[]any/nil) exactly as ValidateRecord/checkType
	// would have stored it. This is the key difference from BatchFetchLabels, which
	// deliberately flattens everything to a display string via JSON_UNQUOTE.
	baseQuery := fmt.Sprintf(
		"SELECT `id`, JSON_EXTRACT(`data`, '$.%s') AS `value` "+
			"FROM `%s` WHERE `tenant_id` = ? AND `id` IN (?) AND `deleted_at` IS NULL",
		sf, table,
	)
	query, args, err := sqlx.In(baseQuery, tenantID, anyIDs)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.BatchFetchField: build IN query: %w", err)
	}
	query = r.db.Rebind(query)

	// Value is []byte, not json.RawMessage: JSON_EXTRACT yields SQL NULL when the related record
	// never had sourceField, and database/sql can store NULL into *[]byte (→ nil, read as a null
	// lookup below) but NOT into the named type *json.RawMessage — that scan error used to 500 the
	// whole record read.
	type row struct {
		ID    string `db:"id"`
		Value []byte `db:"value"`
	}
	var rows []row
	if err := r.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, fmt.Errorf("dynrepo.BatchFetchField: select: %w", err)
	}

	results := make([]FieldValueResult, 0, len(rows))
	for _, rr := range rows {
		var val any
		if len(rr.Value) > 0 {
			if err := json.Unmarshal(rr.Value, &val); err != nil {
				return nil, fmt.Errorf("dynrepo.BatchFetchField: decode value for id %q: %w", rr.ID, err)
			}
		}
		results = append(results, FieldValueResult{ID: rr.ID, Value: val})
	}
	return results, nil
}
