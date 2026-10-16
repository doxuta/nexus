package appmeta

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// The *Exec statements below are the ONE SQL text of every app / app-nav write. The admin
// repository runs them on its own transaction; the draft layer's publish (specs/063-draft-publish
// S7-4) runs the SAME statements on its publish transaction, so a published app is byte-for-byte
// what a direct admin write would have produced. All take an ExecerContext (a *sqlx.Tx / *sqlx.DB).

// ClearDefaultExec drops the is_default flag on every live app of the tenant except exceptID
// ("" = no exception) — the "≤1 default per tenant" rule.
func ClearDefaultExec(ctx context.Context, ex ExecerContext, tenantID, exceptID string) error {
	const q = `UPDATE sys_apps SET is_default = 0
		WHERE tenant_id = ? AND is_default = 1 AND id <> ? AND deleted_at IS NULL`
	if _, err := ex.ExecContext(ctx, q, tenantID, exceptID); err != nil {
		return fmt.Errorf("appmeta.ClearDefaultExec: %w", err)
	}
	return nil
}

// UpdateExec applies the mutable fields (and, when in.HomeDashboardID is set, the home-page
// pointer) to a live, tenant-scoped app. ErrAppNotFound when no live row matched. It does NOT
// clear other apps' default flag — callers that set IsDefault call ClearDefaultExec first.
func UpdateExec(ctx context.Context, ex ExecerContext, tenantID, id string, in UpdateInput) error {
	const q = `UPDATE sys_apps
		SET label = ?, description = ?, icon = ?, color = ?, is_default = ?, sort_order = ?, updated_by = ?
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	res, err := ex.ExecContext(ctx, q,
		in.Label, nullable(in.Description), nullable(in.Icon), nullable(in.Color),
		in.IsDefault, in.SortOrder, nullable(in.UpdatedBy),
		tenantID, id,
	)
	if err != nil {
		return fmt.Errorf("appmeta.UpdateExec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("appmeta.UpdateExec: rows affected: %w", err)
	}
	if n == 0 {
		return ErrAppNotFound
	}
	return ApplyOptionsExec(ctx, ex, tenantID, id, in)
}

// ApplyOptionsExec writes the options keys of an UpdateInput (home dashboard, home queues, default
// roles). UpdateExec calls it after the columns; a restore calls it alone, because RestoreExec has
// just written the columns and an UPDATE that changes nothing reports 0 rows (no clientFoundRows).
func ApplyOptionsExec(ctx context.Context, ex ExecerContext, tenantID, id string, in UpdateInput) error {
	if in.HomeDashboardID != nil {
		// JSON_SET/JSON_REMOVE keep any other options keys; the value travels as a `?`.
		hq := `UPDATE sys_apps SET options = JSON_SET(COALESCE(options, JSON_OBJECT()), '$.homeDashboardId', ?)
			WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
		args := []any{*in.HomeDashboardID, tenantID, id}
		if *in.HomeDashboardID == "" {
			hq = `UPDATE sys_apps SET options = JSON_REMOVE(COALESCE(options, JSON_OBJECT()), '$.homeDashboardId')
				WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
			args = []any{tenantID, id}
		}
		if _, err := ex.ExecContext(ctx, hq, args...); err != nil {
			return fmt.Errorf("appmeta.ApplyOptionsExec: home dashboard: %w", err)
		}
	}
	for _, o := range []struct {
		key string
		ids *[]string
	}{{"homeQueueIds", in.HomeQueueIDs}, {"defaultRoleIds", in.DefaultRoleIDs}} {
		if o.ids == nil {
			continue
		}
		if err := setOptionList(ctx, ex, tenantID, id, o.key, *o.ids); err != nil {
			return fmt.Errorf("appmeta.ApplyOptionsExec: %w", err)
		}
	}
	return nil
}

// setOptionList writes (or, when empty, removes) one JSON-array key of sys_apps.options. key is a
// package constant (never caller input); the array travels as a `?` cast to JSON.
func setOptionList(ctx context.Context, ex ExecerContext, tenantID, id, key string, ids []string) error {
	if len(ids) == 0 {
		_, err := ex.ExecContext(ctx, `UPDATE sys_apps SET options = JSON_REMOVE(COALESCE(options, JSON_OBJECT()), '$.`+key+`')
			WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, tenantID, id)
		if err != nil {
			return fmt.Errorf("option %s: %w", key, err)
		}
		return nil
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("option %s: %w", key, err)
	}
	if _, err := ex.ExecContext(ctx, `UPDATE sys_apps SET options = JSON_SET(COALESCE(options, JSON_OBJECT()), '$.`+key+`', CAST(? AS JSON))
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, string(b), tenantID, id); err != nil {
		return fmt.Errorf("option %s: %w", key, err)
	}
	return nil
}

// ReplaceNavExec swaps the app's entire nav for items (delete-then-insert, sort_order = slice index).
func ReplaceNavExec(ctx context.Context, ex ExecerContext, tenantID, appID string, items []NavItemInput, now time.Time) error {
	const del = `DELETE FROM sys_app_nav_items WHERE tenant_id = ? AND app_id = ?`
	if _, err := ex.ExecContext(ctx, del, tenantID, appID); err != nil {
		return fmt.Errorf("appmeta.ReplaceNavExec: clear: %w", err)
	}
	const ins = `INSERT INTO sys_app_nav_items
		(id, tenant_id, app_id, item_type, ref_id, label, sort_order, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	for i, it := range items {
		if _, err := ex.ExecContext(ctx, ins,
			uuid.NewString(), tenantID, appID, it.ItemType, it.RefID, nullable(it.Label), i, now.UTC(),
		); err != nil {
			return fmt.Errorf("appmeta.ReplaceNavExec: insert item %d: %w", i, err)
		}
	}
	return nil
}

// RestoreExec brings a SOFT-DELETED app back under its own id with the given slug and content
// (publish --force over an app the customer admin deleted). restored=false: no deleted row has
// that id. A slug taken by another live app surfaces as the MySQL duplicate error.
func RestoreExec(ctx context.Context, ex ExecerContext, tenantID, id, name string, in UpdateInput) (bool, error) {
	const q = `UPDATE sys_apps
		SET deleted_at = NULL, name = ?, label = ?, description = ?, icon = ?, color = ?, is_default = ?, sort_order = ?, updated_by = ?
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NOT NULL`
	res, err := ex.ExecContext(ctx, q, name, in.Label, nullable(in.Description), nullable(in.Icon), nullable(in.Color),
		in.IsDefault, in.SortOrder, nullable(in.UpdatedBy), tenantID, id)
	if err != nil {
		return false, fmt.Errorf("appmeta.RestoreExec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("appmeta.RestoreExec: rows affected: %w", err)
	}
	return n > 0, nil
}

// SoftDeleteExec soft-deletes a live app (tombstone-renaming its slug so uq_sys_apps_tenant_name frees
// the name) and hard-deletes its nav rows: the ONE statement pair of the admin DELETE and of a rollback
// that removes an app created after the target version. ErrAppNotFound when no live row matched.
func SoftDeleteExec(ctx context.Context, ex ExecerContext, tenantID, id string, at time.Time) error {
	const q = `UPDATE sys_apps
		SET deleted_at = ?, name = CONCAT(name, '__deleted_', LEFT(id, 8))
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	res, err := ex.ExecContext(ctx, q, at, tenantID, id)
	if err != nil {
		return fmt.Errorf("appmeta.SoftDeleteExec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("appmeta.SoftDeleteExec: rows affected: %w", err)
	}
	if n == 0 {
		return ErrAppNotFound
	}
	const purge = `DELETE FROM sys_app_nav_items WHERE tenant_id = ? AND app_id = ?`
	if _, err := ex.ExecContext(ctx, purge, tenantID, id); err != nil {
		return fmt.Errorf("appmeta.SoftDeleteExec: purge nav: %w", err)
	}
	return nil
}
