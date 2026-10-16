package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ScheduledWorkflow is one due scheduled rule the scheduler is about to consider. It carries the
// decoded condition/actions (the engine evaluates the condition against each record and runs the
// actions) plus the cadence fields the scheduler needs to claim a run idempotently.
type ScheduledWorkflow struct {
	Workflow
	IntervalSeconds int
	LastRunAt       *time.Time
}

// scheduledRow is the persisted projection for the scheduler's due-scan. It mirrors workflowRow
// but adds interval_seconds + last_run_at (the M2 cadence columns from migration 000019).
type scheduledRow struct {
	ID              string           `db:"id"`
	TenantID        string           `db:"tenant_id"`
	Name            string           `db:"name"`
	EntityName      string           `db:"entity_name"`
	Trigger         string           `db:"trigger_event"`
	Condition       *json.RawMessage `db:"condition"`
	Actions         json.RawMessage  `db:"actions"`
	IsActive        bool             `db:"is_active"`
	IntervalSeconds int              `db:"interval_seconds"`
	LastRunAt       *time.Time       `db:"last_run_at"`
}

// ListDueScheduled returns the ACTIVE, non-deleted, trigger='scheduled' rules across ALL tenants
// whose cadence is due at `now`. Due semantics depend on interval_seconds:
//   - interval_seconds = 0  → "fire once": due ONLY while last_run_at IS NULL (never run). After
//     it fires once, the claim stamps last_run_at and the rule is no longer due — so a second
//     scheduler pass (even microseconds later) does NOT re-fire it (no double-fire). This matches
//     the Salesforce one-time time-based action: a 0 cadence is a single fire, not a busy-loop.
//   - interval_seconds > 0  → "recurring": due when last_run_at IS NULL OR last_run_at is at least
//     interval_seconds in the past (last_run_at <= now - interval).
//
// Each candidate is then CLAIMED via ClaimScheduledRun (an atomic CAS) before its actions run, so
// two ticks never double-fire the same rule. The idx_wf_scheduled index
// (tenant_id, trigger_event, is_active, last_run_at) covers this scan.
//
// NOTE: this is the ONE place a query is intentionally NOT tenant-pinned — the background
// scheduler is a system principal that must sweep every tenant's due rules. Each loaded rule
// still carries its own TenantID, and every downstream action (record list, field_update,
// notification) re-pins tenant_id, so no cross-tenant leak is possible.
func (r *SQLRepository) ListDueScheduled(ctx context.Context, now time.Time) ([]ScheduledWorkflow, error) {
	const q = `SELECT id, tenant_id, name, entity_name, trigger_event, ` + "`condition`" + `, actions,
		is_active, interval_seconds, last_run_at
		FROM sys_workflows
		WHERE trigger_event = 'scheduled' AND is_active = 1 AND deleted_at IS NULL
		  AND (
		        last_run_at IS NULL
		     OR (interval_seconds > 0 AND last_run_at <= DATE_SUB(?, INTERVAL interval_seconds SECOND))
		      )
		ORDER BY tenant_id ASC, created_at ASC, id ASC`
	var rows []scheduledRow
	if err := r.db.SelectContext(ctx, &rows, q, now.UTC()); err != nil {
		return nil, fmt.Errorf("workflow.SQLRepository.ListDueScheduled: %w", err)
	}
	out := make([]ScheduledWorkflow, 0, len(rows))
	for i := range rows {
		sw, err := scheduledRowToWorkflow(&rows[i])
		if err != nil {
			// Skip a single corrupt row rather than blinding the scheduler to the rest.
			continue
		}
		out = append(out, sw)
	}
	return out, nil
}

// ClaimScheduledRun atomically claims the next run of a scheduled rule: it stamps last_run_at=now
// ONLY when the rule is still due. The due predicate MIRRORS ListDueScheduled exactly so a listed
// rule is claimable and a non-due rule is never claimed:
//   - interval_seconds = 0  → claimable only while last_run_at IS NULL (one-time fire).
//   - interval_seconds > 0  → claimable when last_run_at NULL or older than its interval.
//
// Returns true when THIS caller won the claim (RowsAffected==1) — meaning it owns this run and
// should fire the actions — and false when another tick already claimed it (CAS lost). This is the
// lock that makes the scheduler idempotent across concurrent ticks / multiple server instances
// (no double-fire).
func (r *SQLRepository) ClaimScheduledRun(ctx context.Context, tenantID, id string, now time.Time) (bool, error) {
	const q = `UPDATE sys_workflows
		SET last_run_at = ?
		WHERE id = ? AND tenant_id = ? AND trigger_event = 'scheduled' AND is_active = 1 AND deleted_at IS NULL
		  AND (
		        last_run_at IS NULL
		     OR (interval_seconds > 0 AND last_run_at <= DATE_SUB(?, INTERVAL interval_seconds SECOND))
		      )`
	nowUTC := now.UTC()
	res, err := r.db.ExecContext(ctx, q, nowUTC, id, tenantID, nowUTC)
	if err != nil {
		return false, fmt.Errorf("workflow.SQLRepository.ClaimScheduledRun: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("workflow.SQLRepository.ClaimScheduledRun: rows affected: %w", err)
	}
	return n == 1, nil
}

// scheduledRowToWorkflow decodes a scheduled projection into the domain ScheduledWorkflow.
func scheduledRowToWorkflow(row *scheduledRow) (ScheduledWorkflow, error) {
	wf := Workflow{
		ID:         row.ID,
		TenantID:   row.TenantID,
		Name:       row.Name,
		EntityName: row.EntityName,
		Trigger:    Trigger(row.Trigger),
		IsActive:   row.IsActive,
	}
	if row.Condition != nil && len(*row.Condition) > 0 && string(*row.Condition) != "null" {
		var c Condition
		if err := json.Unmarshal(*row.Condition, &c); err != nil {
			return ScheduledWorkflow{}, fmt.Errorf("decode condition: %w", err)
		}
		wf.Condition = &c
	}
	if len(row.Actions) > 0 && string(row.Actions) != "null" {
		if err := json.Unmarshal(row.Actions, &wf.Actions); err != nil {
			return ScheduledWorkflow{}, fmt.Errorf("decode actions: %w", err)
		}
	}
	return ScheduledWorkflow{
		Workflow:        wf,
		IntervalSeconds: row.IntervalSeconds,
		LastRunAt:       row.LastRunAt,
	}, nil
}
