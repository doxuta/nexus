package dynrepo

import (
	"context"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"
)

// USER-FIELD slice (../specs/017-user-field-type/spec.md): the two batch-query primitives a `user` field
// type needs against the FIXED `users` system table. Unlike every other repository method in
// this package, these do NOT go through ddl.SanitizeIdentifier + an `obj_<entity>` table name —
// `users` is a hardcoded, constant table (never client- or tenant-supplied), so there is no
// identifier to sanitize; only VALUES (tenant_id, ids) travel as `?` placeholders. This mirrors
// the existing precedent of dynrepo reaching a fixed sys_* table by name when a dynamic-record
// feature needs it (sequence.go → sys_sequences, pivot.go → sys_unique_index) — not a new
// architectural pattern.

// BatchActiveUserExists reports, for a set of ids, which are LIVE, ACTIVE users of the given
// tenant, in ONE query (RM-11-style batching — no N+1). It backs the `user` field's write-time
// existence probe (services.checkUserTargets): a record write referencing a `user` field must
// point at a real, non-disabled user in the SAME tenant. A disabled user (status != 'active') or
// a soft-deleted one (deleted_at NOT NULL) is deliberately excluded — assigning NEW work to
// either is never valid, mirroring how BatchRecordExists excludes soft-deleted obj_* rows.
// Absent ids (unknown / another tenant's / disabled / deleted) are simply missing from the
// result map — the caller collapses every such case into ONE anti-enumeration verdict.
func (r *Repository) BatchActiveUserExists(ctx context.Context, tenantID string, ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return map[string]bool{}, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	stmt := fmt.Sprintf(
		"SELECT `id` FROM `users` WHERE `tenant_id` = ? AND `id` IN (%s) AND `status` = 'active' AND `deleted_at` IS NULL",
		placeholders)
	args := make([]any, 0, 1+len(ids))
	args = append(args, tenantID)
	for _, id := range ids {
		args = append(args, id)
	}
	var found []string
	if err := r.db.SelectContext(ctx, &found, stmt, args...); err != nil {
		return nil, fmt.Errorf("dynrepo.BatchActiveUserExists: %w", err)
	}
	result := make(map[string]bool, len(found))
	for _, id := range found {
		result[id] = true
	}
	return result, nil
}

// UserLoginMatch is an ACTIVE user found by what a person writes in a CSV cell: Email and Name are
// the stored values, trimmed and lower-cased, ready to compare with the lower-cased cell.
type UserLoginMatch struct {
	ID    string `db:"id"`
	Email string `db:"e"`
	Name  string `db:"n"`
}

// BatchActiveUsersByLogin finds the ACTIVE users of the tenant whose e-mail OR full name equals one
// of values (queue #357, CSV import of a `user` field: "an@example.com" / "Nguyễn Văn An" instead of a
// UUID). values are lower-cased + trimmed by the caller; the comparison is case-insensitive but
// ACCENT-SENSITIVE (utf8mb4_bin on the lowered column — the unicode_ci default would fold "Nguyen"
// into "Nguyễn" and pick the wrong person silently). Chunked like the dedup probe; tenant-scoped; the
// same active + not-deleted predicate as BatchActiveUserExists. A name can repeat, so every match is
// returned and the caller decides what an ambiguous cell means.
func (r *Repository) BatchActiveUsersByLogin(ctx context.Context, tenantID string, values []string) ([]UserLoginMatch, error) {
	var out []UserLoginMatch
	for start := 0; start < len(values); start += DedupChunkSize {
		end := min(start+DedupChunkSize, len(values))
		chunk := make([]any, 0, end-start)
		for _, v := range values[start:end] {
			chunk = append(chunk, v)
		}
		const emailExpr = "LOWER(TRIM(`email`)) COLLATE utf8mb4_bin"
		const nameExpr = "LOWER(TRIM(COALESCE(`full_name`, ''))) COLLATE utf8mb4_bin"
		query, args, err := sqlx.In(
			"SELECT `id`, "+emailExpr+" AS `e`, "+nameExpr+" AS `n` FROM `users` "+
				"WHERE `tenant_id` = ? AND `status` = 'active' AND `deleted_at` IS NULL "+
				"AND ("+emailExpr+" IN (?) OR "+nameExpr+" IN (?))",
			tenantID, chunk, chunk)
		if err != nil {
			return nil, fmt.Errorf("dynrepo.BatchActiveUsersByLogin: build IN query: %w", err)
		}
		var rows []UserLoginMatch
		if err := r.db.SelectContext(ctx, &rows, r.db.Rebind(query), args...); err != nil {
			return nil, fmt.Errorf("dynrepo.BatchActiveUsersByLogin: select: %w", err)
		}
		out = append(out, rows...)
	}
	return out, nil
}

// UserLabelResult is one {id, name, email} triple returned by BatchFetchUserLabels.
type UserLabelResult struct {
	ID    string
	Name  string
	Email string
}

// BatchFetchUserLabels resolves a set of user ids to their display name (full_name, blank when
// unset) + email, in ONE query — the `user` field's ?expand= primitive (mirrors
// BatchFetchLabels, which targets an obj_* table's JSON `data`; this targets `users`' physical
// columns instead). Deliberately does NOT filter status='active': a record legitimately
// assigned to a user who was LATER disabled must still display who that was (history should not
// go blank) — only a soft-deleted (permanently removed) user stops resolving. Unknown ids
// (absent / another tenant's / soft-deleted) are simply missing from the result — the caller
// leaves the raw id untouched for those, mirroring BatchFetchLabels.
func (r *Repository) BatchFetchUserLabels(ctx context.Context, tenantID string, ids []string) ([]UserLabelResult, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	anyIDs := make([]any, len(ids))
	for i, id := range ids {
		anyIDs[i] = id
	}
	baseQuery := "SELECT `id`, COALESCE(`full_name`, '') AS `name`, `email` AS `email` " +
		"FROM `users` WHERE `tenant_id` = ? AND `id` IN (?) AND `deleted_at` IS NULL"
	query, args, err := sqlx.In(baseQuery, tenantID, anyIDs)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.BatchFetchUserLabels: build IN query: %w", err)
	}
	query = r.db.Rebind(query)

	type row struct {
		ID    string `db:"id"`
		Name  string `db:"name"`
		Email string `db:"email"`
	}
	var rows []row
	if err := r.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, fmt.Errorf("dynrepo.BatchFetchUserLabels: select: %w", err)
	}
	results := make([]UserLabelResult, 0, len(rows))
	for _, rr := range rows {
		results = append(results, UserLabelResult{ID: rr.ID, Name: rr.Name, Email: rr.Email})
	}
	return results, nil
}
