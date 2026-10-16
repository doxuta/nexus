package workflow

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Execer is the one capability the *Exec statements need: it is satisfied by *sqlx.DB, *sqlx.Tx and
// *sql.Tx alike. The admin repository calls these on its pool; the draft layer's publish
// (specs/063-draft-publish) calls the SAME statements on its transaction, so there is exactly one
// SQL text per write and the two paths cannot drift.
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
		return 0, fmt.Errorf("workflow.%s: rows affected: %w", op, err)
	}
	return n, nil
}

// InsertWithIDExec inserts a workflow under a caller-chosen id (a draft's id survives publish).
func InsertWithIDExec(ctx context.Context, ex Execer, id string, in CreateInput, now time.Time) error {
	var cond any
	if len(in.Condition) > 0 {
		cond = string(in.Condition)
	}
	createdBy := nullableActor(in.CreatedBy)
	const q = `INSERT INTO sys_workflows
		(id, tenant_id, name, entity_name, trigger_event, ` + "`condition`" + `, actions, is_active, interval_seconds, created_by, updated_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := ex.ExecContext(ctx, q,
		id, in.TenantID, in.Name, in.EntityName, string(in.Trigger), cond, string(in.Actions),
		in.IsActive, in.IntervalSeconds, createdBy, createdBy, now.UTC(), now.UTC(),
	); err != nil {
		return fmt.Errorf("workflow.InsertWithIDExec: %w", err)
	}
	return nil
}

// UpdateExec replaces the mutable columns of a live (non-deleted) workflow of the tenant.
// ErrWorkflowNotFound when no live row matched.
func UpdateExec(ctx context.Context, ex Execer, tenantID, id string, in UpdateInput) error {
	var cond any
	if len(in.Condition) > 0 {
		cond = string(in.Condition)
	}
	const q = `UPDATE sys_workflows
		SET name = ?, entity_name = ?, trigger_event = ?, ` + "`condition`" + ` = ?, actions = ?, is_active = ?, interval_seconds = ?, updated_by = ?
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	res, err := ex.ExecContext(ctx, q,
		in.Name, in.EntityName, string(in.Trigger), cond, string(in.Actions), in.IsActive, in.IntervalSeconds, nullableActor(in.UpdatedBy),
		tenantID, id)
	if err != nil {
		return fmt.Errorf("workflow.UpdateExec: %w", err)
	}
	n, err := affected(res, "UpdateExec")
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrWorkflowNotFound
	}
	return nil
}

// SoftDeleteExec stamps deleted_at on a live workflow of the tenant. ErrWorkflowNotFound when none matched.
func SoftDeleteExec(ctx context.Context, ex Execer, tenantID, id string, at time.Time) error {
	const q = `UPDATE sys_workflows SET deleted_at = ? WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	res, err := ex.ExecContext(ctx, q, at.UTC(), tenantID, id)
	if err != nil {
		return fmt.Errorf("workflow.SoftDeleteExec: %w", err)
	}
	n, err := affected(res, "SoftDeleteExec")
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrWorkflowNotFound
	}
	return nil
}

// RestoreExec brings a SOFT-DELETED workflow of the tenant back under its own id with the given
// content (publish --force over a rule the customer admin deleted). restored=false: no deleted row
// has that id (the caller then inserts).
func RestoreExec(ctx context.Context, ex Execer, tenantID, id string, in UpdateInput) (bool, error) {
	var cond any
	if len(in.Condition) > 0 {
		cond = string(in.Condition)
	}
	const q = `UPDATE sys_workflows
		SET deleted_at = NULL, name = ?, entity_name = ?, trigger_event = ?, ` + "`condition`" + ` = ?, actions = ?, is_active = ?, interval_seconds = ?, updated_by = ?
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NOT NULL`
	res, err := ex.ExecContext(ctx, q,
		in.Name, in.EntityName, string(in.Trigger), cond, string(in.Actions), in.IsActive, in.IntervalSeconds, nullableActor(in.UpdatedBy),
		tenantID, id)
	if err != nil {
		return false, fmt.Errorf("workflow.RestoreExec: %w", err)
	}
	n, err := affected(res, "RestoreExec")
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetInactiveExec switches a live workflow OFF and touches nothing else ("Tắt ngay cho khách",
// specs/063 FR-017). ErrWorkflowNotFound when no live row matched.
func SetInactiveExec(ctx context.Context, ex Execer, tenantID, id, updatedBy string) error {
	const q = `UPDATE sys_workflows SET is_active = 0, updated_by = ? WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	res, err := ex.ExecContext(ctx, q, nullableActor(updatedBy), tenantID, id)
	if err != nil {
		return fmt.Errorf("workflow.SetInactiveExec: %w", err)
	}
	n, err := affected(res, "SetInactiveExec")
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrWorkflowNotFound
	}
	return nil
}
