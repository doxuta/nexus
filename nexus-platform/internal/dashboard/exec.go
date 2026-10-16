package dashboard

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"nexus/platform/internal/appmeta"
)

// Execer is the one capability the *Exec statements need: satisfied by *sqlx.DB, *sqlx.Tx and *sql.Tx.
// The admin repository calls these on its pool; the draft layer's publish (specs/063-draft-publish
// S7-2) calls the SAME statements on its transaction, so there is one SQL text per write.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func nullIfBlank(s string) sql.NullString {
	if strings.TrimSpace(s) == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// InsertWithIDExec inserts a dashboard under a caller-chosen id (a draft's id survives publish).
func InsertWithIDExec(ctx context.Context, ex Execer, id string, in CreateInput, now time.Time) error {
	const q = `INSERT INTO sys_dashboards
		(id, tenant_id, name, description, widgets, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := ex.ExecContext(ctx, q,
		id, in.TenantID, in.Name, nullIfBlank(in.Description), string(in.Widgets), nullIfBlank(in.CreatedBy), now.UTC(), now.UTC(),
	); err != nil {
		return fmt.Errorf("dashboard.InsertWithIDExec: %w", err)
	}
	return nil
}

// UpdateExec replaces the mutable columns of a live dashboard. ErrDashboardNotFound when none matched.
func UpdateExec(ctx context.Context, ex Execer, tenantID, id string, in UpdateInput) error {
	const q = `UPDATE sys_dashboards
		SET name = ?, description = ?, widgets = ?
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	res, err := ex.ExecContext(ctx, q, in.Name, nullIfBlank(in.Description), string(in.Widgets), tenantID, id)
	if err != nil {
		return fmt.Errorf("dashboard.UpdateExec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("dashboard.UpdateExec: rows affected: %w", err)
	}
	if n == 0 {
		return ErrDashboardNotFound
	}
	return nil
}

// SoftDeleteExec stamps deleted_at on a live dashboard AND removes every app-nav row / home-page
// pointer that references it (appmeta.PurgeNavForRef, INV-4) on the SAME executor — so on a tx the
// cascade is atomic with the delete. ErrDashboardNotFound when no live row matched (nothing purged).
func SoftDeleteExec(ctx context.Context, ex Execer, tenantID, id string, at time.Time) error {
	const q = `UPDATE sys_dashboards SET deleted_at = ? WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	res, err := ex.ExecContext(ctx, q, at.UTC(), tenantID, id)
	if err != nil {
		return fmt.Errorf("dashboard.SoftDeleteExec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("dashboard.SoftDeleteExec: rows affected: %w", err)
	}
	if n == 0 {
		return ErrDashboardNotFound
	}
	if err := appmeta.PurgeNavForRef(ctx, ex, tenantID, appmeta.ItemTypeDashboard, id); err != nil {
		return fmt.Errorf("dashboard.SoftDeleteExec: %w", err)
	}
	return nil
}

// RestoreExec brings a SOFT-DELETED dashboard back under its own id with the given content
// (publish --force over a dashboard the customer admin deleted). restored=false: no deleted row has that id.
func RestoreExec(ctx context.Context, ex Execer, tenantID, id string, in UpdateInput) (bool, error) {
	const q = `UPDATE sys_dashboards
		SET deleted_at = NULL, name = ?, description = ?, widgets = ?
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NOT NULL`
	res, err := ex.ExecContext(ctx, q, in.Name, nullIfBlank(in.Description), string(in.Widgets), tenantID, id)
	if err != nil {
		return false, fmt.Errorf("dashboard.RestoreExec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("dashboard.RestoreExec: rows affected: %w", err)
	}
	return n > 0, nil
}
