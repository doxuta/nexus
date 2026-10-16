package bpm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/workflow"
)

// Pause and resume of the SLA clock (specs/043 S13, FR-036). A process may carry options.pauseWhen,
// a condition on the RECORD ("the ticket waits for the customer"). Every after_update of the record
// asks its open instances to compare: while the condition holds the deadline is frozen — the working
// time still left is kept on the instance and the due date cleared (so no breach job or sweep can
// fire) — and when it stops holding the deadline is rebuilt from that remaining time. The seats stay
// actionable throughout: only the clock stops, not the work.

// syncClock brings the clock of inst in line with the record's current values. Fail-open by the
// caller: the record update is long committed.
func (r *Runner) syncClock(ctx context.Context, inst *Instance, data map[string]any) error {
	var pw *workflow.Condition
	if inst.Definition != nil && inst.Definition.Options != nil {
		pw = inst.Definition.Options.PauseWhen
	}
	if pw == nil || inst.Status != StatusPending {
		return nil
	}
	hold := workflow.Evaluate(pw, data)
	paused := inst.SLAPausedAt != nil
	now := r.now().UTC()
	switch {
	case hold && !paused:
		return r.pauseClock(ctx, inst, now)
	case !hold && paused:
		return r.resumeClock(ctx, inst, now)
	}
	return nil
}

func (r *Runner) pauseClock(ctx context.Context, inst *Instance, now time.Time) error {
	// Nothing to freeze: no deadline, one already missed (its breach is on its way), or a stuck step.
	if inst.SLADueAt == nil || inst.Escalated || inst.LastError != "" || !inst.SLADueAt.After(now) {
		return nil
	}
	basis := inst.CurrentSLABasis()
	remaining := inst.SLADueAt.Sub(now)
	if basis == SLABasisBusiness {
		m, ok := r.deadlines.(BusinessMeter)
		if !ok {
			return fmt.Errorf("bpm.Runner.pauseClock: calendar cannot measure working time")
		}
		d, found, err := m.BusinessBetween(ctx, inst.TenantID, inst.Definition.CalendarID(), now, *inst.SLADueAt)
		if err != nil {
			return fmt.Errorf("bpm.Runner.pauseClock: %w", err)
		}
		if found {
			remaining = d
		}
	}
	secs := int64(math.Ceil(remaining.Seconds()))
	return r.writeClock(ctx, inst, now, HistoryPaused, basis, nil, &InstanceTransition{
		Clock: &SLAClockWrite{PausedAt: &now, RemainingSeconds: &secs},
	})
}

func (r *Runner) resumeClock(ctx context.Context, inst *Instance, now time.Time) error {
	if inst.SLARemainingSeconds == nil {
		return nil
	}
	rem := time.Duration(*inst.SLARemainingSeconds) * time.Second
	basis := inst.CurrentSLABasis()
	due := now.Add(rem)
	if basis == SLABasisBusiness && r.deadlines != nil {
		d, b, err := r.deadlines.DueAt(ctx, inst.TenantID, inst.Definition.CalendarID(), now, rem)
		if err != nil {
			return fmt.Errorf("bpm.Runner.resumeClock: %w", err)
		}
		due, basis = d.UTC(), b
	}
	return r.writeClock(ctx, inst, now, HistoryResumed, basis, &due, &InstanceTransition{Clock: &SLAClockWrite{}})
}

// writeClock is the one guarded write of a pause / resume: same step, same holders, a fresh seq (so
// a stale breach job is skipped) and — when a deadline is set — a new bpm_sla_due job.
func (r *Runner) writeClock(ctx context.Context, inst *Instance, now time.Time, action string, basis SLABasis, due *time.Time, t *InstanceTransition) error {
	node, stepName := inst.Definition.StepRef(inst.CurrentStep)
	entry := HistoryEntry{Step: inst.CurrentStep, Action: action, Node: node, StepName: stepName,
		Version: inst.Definition.version(), SLABasis: basis, At: now}
	historyJSON, err := json.Marshal(append(inst.History, entry))
	if err != nil {
		return fmt.Errorf("bpm.Runner.writeClock: marshal history: %w", err)
	}
	t.CurrentStep, t.Status = inst.CurrentStep, StatusPending
	t.CurrentApproverType, t.CurrentApproverID = inst.CurrentApproverType, inst.CurrentApproverID
	t.SLADueAt, t.Escalated, t.LastError = due, inst.Escalated, inst.LastError
	if err := r.store.DecideTaskTx(ctx, inst.TenantID, inst.ID, TaskGuard{ExpectSeq: inst.Seq}, *t, historyJSON); err != nil {
		return txError("bpm.Runner.writeClock", err)
	}
	return nil
}

// signalPauses runs syncClock for the open instances of the updated record whose process carries a
// pauseWhen. The event's Data is the record AFTER the update. Fail-open, like signalWaits.
func (e *AsyncEngine) signalPauses(ctx context.Context, evt Event) {
	lister, ok := e.store.(RecordWaiters)
	syncer, ok2 := e.runner.(interface {
		syncClock(context.Context, *Instance, map[string]any) error
	})
	if !ok || !ok2 {
		return
	}
	open, err := lister.ListPendingByRecord(ctx, evt.TenantID, evt.EntityName, evt.RecordID)
	if err != nil {
		e.logger.Warn("bpm: listing the open approvals of an updated record failed — SLA clocks not synced",
			zap.String("tenant_id", evt.TenantID), zap.String("entity_name", evt.EntityName),
			zap.String("record_id", evt.RecordID), zap.Error(err))
		return
	}
	for i := range open {
		if err := syncer.syncClock(ctx, &open[i], evt.Data); err != nil && !errors.Is(err, ErrConcurrent) {
			e.logger.Warn("bpm: syncing an SLA clock failed",
				zap.String("tenant_id", open[i].TenantID), zap.String("instance_id", open[i].ID), zap.Error(err))
		}
	}
}
