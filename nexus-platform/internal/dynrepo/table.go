package dynrepo

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// TableFor returns the sanitized physical obj_* table name for an entity — the exported form
// of tableFor for the service layer. REPORT-JOIN builds JoinSpec.RelatedTable with it so the
// joined table name derives from the ONE name rule the store itself uses (never a
// hand-concatenated "obj_"+x in the service). The identifier is re-sanitized again inside
// JoinSpec.joinSQL (defense-in-depth).
func TableFor(entity string) (string, error) { return tableFor(entity) }

// CountLive counts the tenant's live (not binned) rows of one entity's obj_* table — the record
// total of the "Mức dùng" screen (specs/066 L5). The table comes from tableFor, never raw input;
// tenant_id is pinned; the predicate walks the prefix of idx_created_at (tenant_id, deleted_at, …).
// A table that does not exist surfaces as a wrapped MySQL 1146 (the caller skips the entity).
func CountLive(ctx context.Context, db sqlx.QueryerContext, entity, tenantID string) (int64, error) {
	table, err := tableFor(entity)
	if err != nil {
		return 0, fmt.Errorf("dynrepo.CountLive: %w", err)
	}
	var n int64
	if err := sqlx.GetContext(ctx, db, &n,
		"SELECT COUNT(*) FROM `"+table+"` WHERE tenant_id = ? AND deleted_at IS NULL", tenantID); err != nil {
		return 0, fmt.Errorf("dynrepo.CountLive: %w", err)
	}
	return n, nil
}
