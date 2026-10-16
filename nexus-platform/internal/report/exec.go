package report

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Execer is the one capability the *Exec statements need: satisfied by *sqlx.DB, *sqlx.Tx and *sql.Tx.
// The admin repository calls these on its pool; the draft layer's publish (specs/063-draft-publish
// S7-2) calls the SAME statements on its transaction, so there is one SQL text per write.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func nullableActor(s string) sql.NullString {
	if strings.TrimSpace(s) == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func affected(res sql.Result, op string) (int64, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("report.%s: rows affected: %w", op, err)
	}
	return n, nil
}

// InsertWithIDExec inserts a report under a caller-chosen id (a draft's id survives publish).
func InsertWithIDExec(ctx context.Context, ex Execer, id string, in CreateInput, now time.Time) error {
	createdBy := nullableActor(in.CreatedBy)
	const q = `INSERT INTO sys_reports
		(id, tenant_id, name, entity_name, type, config, is_active, created_by, updated_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := ex.ExecContext(ctx, q,
		id, in.TenantID, in.Name, in.EntityName, string(in.Type), string(in.Config),
		in.IsActive, createdBy, createdBy, now.UTC(), now.UTC(),
	); err != nil {
		return fmt.Errorf("report.InsertWithIDExec: %w", err)
	}
	return nil
}

// UpdateExec replaces the mutable columns of a live (non-deleted) report of the tenant.
// ErrReportNotFound when no live row matched.
func UpdateExec(ctx context.Context, ex Execer, tenantID, id string, in UpdateInput) error {
	const q = `UPDATE sys_reports
		SET name = ?, entity_name = ?, type = ?, config = ?, is_active = ?, updated_by = ?
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	res, err := ex.ExecContext(ctx, q,
		in.Name, in.EntityName, string(in.Type), string(in.Config), in.IsActive, nullableActor(in.UpdatedBy),
		tenantID, id)
	if err != nil {
		return fmt.Errorf("report.UpdateExec: %w", err)
	}
	n, err := affected(res, "UpdateExec")
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrReportNotFound
	}
	return nil
}

// SoftDeleteExec stamps deleted_at on a live report of the tenant. ErrReportNotFound when none matched.
func SoftDeleteExec(ctx context.Context, ex Execer, tenantID, id string, at time.Time) error {
	const q = `UPDATE sys_reports SET deleted_at = ? WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	res, err := ex.ExecContext(ctx, q, at.UTC(), tenantID, id)
	if err != nil {
		return fmt.Errorf("report.SoftDeleteExec: %w", err)
	}
	n, err := affected(res, "SoftDeleteExec")
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrReportNotFound
	}
	return nil
}

// RestoreExec brings a SOFT-DELETED report back under its own id with the given content (publish
// --force over a report the customer admin deleted). restored=false: no deleted row has that id.
func RestoreExec(ctx context.Context, ex Execer, tenantID, id string, in UpdateInput) (bool, error) {
	const q = `UPDATE sys_reports
		SET deleted_at = NULL, name = ?, entity_name = ?, type = ?, config = ?, is_active = ?, updated_by = ?
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NOT NULL`
	res, err := ex.ExecContext(ctx, q,
		in.Name, in.EntityName, string(in.Type), string(in.Config), in.IsActive, nullableActor(in.UpdatedBy),
		tenantID, id)
	if err != nil {
		return false, fmt.Errorf("report.RestoreExec: %w", err)
	}
	n, err := affected(res, "RestoreExec")
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
