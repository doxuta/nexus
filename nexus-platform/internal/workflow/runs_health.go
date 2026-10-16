package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RuleFailure is one rule's failed-run tally for the health screen (specs/066 L6).
type RuleFailure struct {
	WorkflowID string    `db:"workflow_id"`
	Name       string    `db:"name"`
	Count      int       `db:"n"`
	LastAt     time.Time `db:"last_at"`
}

// FailedRunsSince returns the tenant's failed-run total since `since` and its top `limit` rules by
// failures, names joined from the tenant's own sys_workflows. idx_wfr_tenant_started serves both.
func (r *SQLRepository) FailedRunsSince(ctx context.Context, tenantID string, since time.Time, limit int) (int, []RuleFailure, error) {
	var total int
	if err := r.db.GetContext(ctx, &total, `SELECT COUNT(*) FROM sys_workflow_runs
		WHERE tenant_id = ? AND started_at >= ? AND status = 'failed'`, tenantID, since); err != nil {
		return 0, nil, fmt.Errorf("workflow.SQLRepository.FailedRunsSince: count: %w", err)
	}
	var top []RuleFailure
	if err := r.db.SelectContext(ctx, &top, `
		SELECT f.workflow_id, COALESCE(w.name, '') AS name, f.n, f.last_at
		FROM (SELECT workflow_id, COUNT(*) AS n, MAX(started_at) AS last_at
		        FROM sys_workflow_runs
		        WHERE tenant_id = ? AND started_at >= ? AND status = 'failed'
		        GROUP BY workflow_id ORDER BY n DESC, last_at DESC LIMIT ?) f
		LEFT JOIN sys_workflows w ON w.id = f.workflow_id AND w.tenant_id = ?
		ORDER BY f.n DESC, f.last_at DESC`, tenantID, since, limit, tenantID); err != nil {
		return 0, nil, fmt.Errorf("workflow.SQLRepository.FailedRunsSince: %w", err)
	}
	return total, top, nil
}

// LastFailedRun returns the error text of the rule's newest failed run since `since` ("" when
// none). The text was masked when the run was recorded (#85).
func (r *SQLRepository) LastFailedRun(ctx context.Context, tenantID, workflowID string, since time.Time) (string, error) {
	var msg sql.NullString
	err := r.db.GetContext(ctx, &msg, `SELECT error FROM sys_workflow_runs
		WHERE tenant_id = ? AND workflow_id = ? AND started_at >= ? AND status = 'failed'
		ORDER BY started_at DESC, id DESC LIMIT 1`, tenantID, workflowID, since)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("workflow.SQLRepository.LastFailedRun: %w", err)
	}
	return msg.String, nil
}
