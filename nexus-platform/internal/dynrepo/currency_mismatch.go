// currency_mismatch.go — the one read behind the Doctor's D2 check (queue #63,
// ../specs/008-nexus-doctor/spec.md §2.2): live rows whose stored currency code differs from the
// field's configured options.currencyCode. Such rows predate the Q7 write guard (queue #7) or come
// from a change of the field's configured code made before queue #157 guarded it
// (CountCurrencyCodeRows below); every money reader (aggregate, rollup, report) sums their
// `.amount` and labels the total with the configured code.
package dynrepo

import (
	"context"
	"fmt"
	"strings"

	"nexus/platform/internal/ddl"
)

// currencyCodePath builds the JSON expression of a currency value's code. `field` is a top-level
// field name or "composite.sub" for a currency sub-field of a composite (#440); each dotted part
// goes through SanitizeIdentifier, so the path carries no client input.
func currencyCodePath(field string) (string, error) {
	parts := strings.Split(field, ".")
	if len(parts) > 2 {
		return "", fmt.Errorf("field %q: at most composite.sub", field)
	}
	path := "$"
	for _, p := range parts {
		col, err := ddl.SanitizeIdentifier(p)
		if err != nil {
			return "", fmt.Errorf("field %q: %w", field, err)
		}
		path += "." + col
	}
	return "JSON_EXTRACT(`data`, '" + path + ".currencyCode')", nil
}

// CurrencyCodeMismatch is one live record whose stored currency code is not the configured one.
type CurrencyCodeMismatch struct {
	ID   string `db:"id"`
	Code string `db:"code"`
}

// ListCurrencyCodeMismatches returns up to limit live rows of (entity, tenant) whose currency
// field carries a non-empty STRING code other than want (compared after TRIM, the same rule as
// the write guard's strings.TrimSpace). A value without a code, or with an empty one, is not a
// relabel candidate and is skipped. Read-only; tenant-pinned; soft-deleted rows excluded.
//
// ponytail: one tenant-scoped scan of the JSON column per currency field — no index can serve a
// predicate on a nested JSON key. Acceptable for an on-demand admin scan (Doctor only, never a
// request path); a generated column is the upgrade path if a tenant's table ever makes it slow.
func (r *Repository) ListCurrencyCodeMismatches(ctx context.Context, entity, tenantID, field, want string, limit int) ([]CurrencyCodeMismatch, error) {
	table, err := tableFor(entity)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.ListCurrencyCodeMismatches: %w", err)
	}
	codePath, err := currencyCodePath(field)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.ListCurrencyCodeMismatches: %w", err)
	}
	// Every value travels as a `?` placeholder; the path is built from sanitized identifiers.
	stmt := fmt.Sprintf(
		"SELECT `id`, JSON_UNQUOTE(%s) AS `code` FROM `%s` "+
			"WHERE `tenant_id` = ? AND `deleted_at` IS NULL AND JSON_TYPE(%s) = 'STRING' "+
			"AND TRIM(JSON_UNQUOTE(%s)) NOT IN ('', ?) ORDER BY `id` LIMIT ?",
		codePath, table, codePath, codePath)
	var rows []CurrencyCodeMismatch
	if err := r.db.SelectContext(ctx, &rows, stmt, tenantID, want, limit); err != nil {
		return nil, fmt.Errorf("dynrepo.ListCurrencyCodeMismatches: %w", err)
	}
	return rows, nil
}

// CountCurrencyCodeRows counts the rows of (entity, tenant) whose currency field stores the code
// `code` (compared after TRIM, like ListCurrencyCodeMismatches), split into live rows and rows in
// the recycle bin. It backs the queue #157 guard in FieldService.EditField: these are exactly the
// rows a change of the field's configured currencyCode away from `code` would relabel — same
// amount, new currency, nobody told. Recycle-bin rows count too: POST /:entity/:id/restore brings a
// soft-deleted row back unchanged (RecordService.Restore re-checks unique values, not money), so a
// row trashed before the change would come back relabelled. Only a purge removes it for good.
// Read-only; tenant-pinned.
//
// ponytail: one tenant-scoped scan of the JSON column, same shape as ListCurrencyCodeMismatches —
// it runs only when an admin changes a field's configured currency (rare, never a record path).
func (r *Repository) CountCurrencyCodeRows(ctx context.Context, entity, tenantID, field, code string) (live, trashed int64, err error) {
	table, err := tableFor(entity)
	if err != nil {
		return 0, 0, fmt.Errorf("dynrepo.CountCurrencyCodeRows: %w", err)
	}
	codePath, err := currencyCodePath(field)
	if err != nil {
		return 0, 0, fmt.Errorf("dynrepo.CountCurrencyCodeRows: %w", err)
	}
	// Tenant and code travel as `?` placeholders; the path is built from sanitized identifiers.
	stmt := fmt.Sprintf(
		"SELECT CAST(COALESCE(SUM(`deleted_at` IS NULL), 0) AS SIGNED) AS `live`, "+
			"CAST(COALESCE(SUM(`deleted_at` IS NOT NULL), 0) AS SIGNED) AS `trashed` FROM `%s` "+
			"WHERE `tenant_id` = ? AND JSON_TYPE(%s) = 'STRING' AND TRIM(JSON_UNQUOTE(%s)) = ?",
		table, codePath, codePath)
	var got struct {
		Live    int64 `db:"live"`
		Trashed int64 `db:"trashed"`
	}
	if err := r.db.GetContext(ctx, &got, stmt, tenantID, code); err != nil {
		return 0, 0, fmt.Errorf("dynrepo.CountCurrencyCodeRows: %w", err)
	}
	return got.Live, got.Trashed, nil
}
