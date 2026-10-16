package workflow

// W6 — run-history recording unit tests (fakes, no DB). The dispatcher must record EVERY
// (event × workflow) outcome — success AND failed AND skipped — through the RunRecorder, and a
// recorder failure must be fail-open (logged, never crashes the executor). The scheduler records
// one run per CLAIMED scheduled firing.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

type fakeRunRecorder struct {
	mu   sync.Mutex
	runs []RunInput
	err  error
}

func (r *fakeRunRecorder) InsertRun(_ context.Context, in RunInput) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.runs = append(r.runs, in)
	return nil
}

func (r *fakeRunRecorder) all() []RunInput {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RunInput, len(r.runs))
	copy(out, r.runs)
	return out
}

func matchingRule() Workflow {
	return Workflow{
		ID: "wf1", TenantID: "t1", EntityName: "deals", Trigger: TriggerAfterUpdate, IsActive: true,
		Condition: &Condition{Rules: []Rule{{Field: "status", Op: OpEq, Value: "won"}}},
		Actions:   []Action{{Type: ActionFieldUpdate, Field: "stage", Value: "closed"}},
	}
}

func TestDispatcher_RecordsSuccessRun(t *testing.T) {
	loader := &fakeLoader{workflows: []Workflow{matchingRule()}}
	rec := &fakeRunRecorder{}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 8, Workers: 1}, loader, &fakeMutator{}, nil, zap.NewNop()).
		WithRunRecorder(rec)

	d.Emit(context.Background(), Event{
		TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterUpdate,
		Data: map[string]any{"status": "won"},
	})
	drainDispatcher(t, d)

	runs := rec.all()
	if len(runs) != 1 {
		t.Fatalf("expected 1 recorded run, got %d", len(runs))
	}
	run := runs[0]
	if run.TenantID != "t1" || run.WorkflowID != "wf1" || run.RecordID != "r1" ||
		run.TriggerType != string(TriggerAfterUpdate) || run.Status != RunSuccess {
		t.Errorf("run = %+v; want t1/wf1/r1/after_update/success", run)
	}
	if run.Error != "" {
		t.Errorf("success run must carry no error, got %q", run.Error)
	}
	var results []ActionResult
	if err := json.Unmarshal(run.ActionsResult, &results); err != nil {
		t.Fatalf("actions_result not valid JSON: %v (%s)", err, run.ActionsResult)
	}
	if len(results) != 1 || !results[0].OK || results[0].Type != ActionFieldUpdate || results[0].Field != "stage" {
		t.Errorf("actions_result = %+v; want one ok field_update on stage", results)
	}
	if run.FinishedAt.Before(run.StartedAt) {
		t.Errorf("finished_at %v before started_at %v", run.FinishedAt, run.StartedAt)
	}
}

func TestDispatcher_RecordsSkippedRun_ConditionFalse(t *testing.T) {
	loader := &fakeLoader{workflows: []Workflow{matchingRule()}}
	rec := &fakeRunRecorder{}
	mut := &fakeMutator{}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 8, Workers: 1}, loader, mut, nil, zap.NewNop()).
		WithRunRecorder(rec)

	d.Emit(context.Background(), Event{
		TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterUpdate,
		Data: map[string]any{"status": "lost"}, // condition wants "won"
	})
	drainDispatcher(t, d)

	runs := rec.all()
	if len(runs) != 1 {
		t.Fatalf("a condition-false evaluation must still be recorded (skipped); got %d runs", len(runs))
	}
	if runs[0].Status != RunSkipped {
		t.Errorf("status = %s; want skipped", runs[0].Status)
	}
	if len(mut.calls) != 0 {
		t.Errorf("skipped rule must not run actions")
	}
}

func TestDispatcher_RecordsFailedRun_WithError(t *testing.T) {
	loader := &fakeLoader{workflows: []Workflow{matchingRule()}}
	rec := &fakeRunRecorder{}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 8, Workers: 1}, loader,
		&fakeMutator{err: errors.New("update failed")}, nil, zap.NewNop()).
		WithRunRecorder(rec)

	d.Emit(context.Background(), Event{
		TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterUpdate,
		Data: map[string]any{"status": "won"},
	})
	drainDispatcher(t, d)

	runs := rec.all()
	if len(runs) != 1 {
		t.Fatalf("expected 1 recorded run, got %d", len(runs))
	}
	run := runs[0]
	if run.Status != RunFailed {
		t.Fatalf("status = %s; want failed", run.Status)
	}
	if run.Error == "" {
		t.Errorf("failed run must carry the first action error")
	}
	var results []ActionResult
	if err := json.Unmarshal(run.ActionsResult, &results); err != nil {
		t.Fatalf("actions_result not valid JSON: %v", err)
	}
	if len(results) != 1 || results[0].OK || results[0].Error == "" {
		t.Errorf("actions_result = %+v; want one failed field_update with error text", results)
	}
}

func TestDispatcher_RecorderFailure_IsFailOpen(t *testing.T) {
	// A broken runs table must NEVER crash the executor or stop the actions: the action still
	// runs, the insert error is logged, the drain completes.
	loader := &fakeLoader{workflows: []Workflow{matchingRule()}}
	rec := &fakeRunRecorder{err: errors.New("runs table gone")}
	mut := &fakeMutator{}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 8, Workers: 1}, loader, mut, nil, zap.NewNop()).
		WithRunRecorder(rec)

	d.Emit(context.Background(), Event{
		TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterUpdate,
		Data: map[string]any{"status": "won"},
	})
	drainDispatcher(t, d) // must not hang or panic

	if len(mut.calls) != 1 {
		t.Errorf("action must still run when the recorder fails; got %d calls", len(mut.calls))
	}
}

func TestDispatcher_NilRecorder_NoOp(t *testing.T) {
	// No recorder wired (legacy tests / disabled) → engine behaves exactly as before.
	loader := &fakeLoader{workflows: []Workflow{matchingRule()}}
	mut := &fakeMutator{}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 8, Workers: 1}, loader, mut, nil, zap.NewNop())

	d.Emit(context.Background(), Event{
		TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: TriggerAfterUpdate,
		Data: map[string]any{"status": "won"},
	})
	drainDispatcher(t, d)
	if len(mut.calls) != 1 {
		t.Errorf("nil recorder must not affect execution; got %d calls", len(mut.calls))
	}
}

// ---- scheduler recording ----------------------------------------------------------------------

func TestScheduler_RecordsRunPerClaimedFiring(t *testing.T) {
	now := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	sw := ScheduledWorkflow{
		Workflow: Workflow{
			ID: "wf-s", TenantID: "t1", EntityName: "deals", Trigger: TriggerScheduled, IsActive: true,
			Condition: &Condition{Rules: []Rule{{Field: "status", Op: OpEq, Value: "stale"}}},
			Actions:   []Action{{Type: ActionInAppNotification, UserID: "u1", Message: "ping"}},
		},
	}
	loader := newFakeScheduledLoader(sw)
	lister := &fakeRecordLister{records: map[string][]ScheduledRecord{"deals": {
		{ID: "r1", Data: map[string]any{"status": "stale"}},
		{ID: "r2", Data: map[string]any{"status": "fresh"}},
	}}}
	rec := &fakeRunRecorder{}
	notif := &fakeNotif{}
	engine := NewAsyncDispatcher(DispatcherConfig{Buffer: 1, Workers: 1}, nil, nil, notif, zap.NewNop()).
		WithRunRecorder(rec)
	s := NewScheduler(SchedulerConfig{Interval: time.Minute}, loader, lister, engine, zap.NewNop())

	s.runDue(context.Background(), now)

	runs := rec.all()
	if len(runs) != 1 {
		t.Fatalf("expected 1 recorded scheduled run, got %d", len(runs))
	}
	run := runs[0]
	if run.WorkflowID != "wf-s" || run.TenantID != "t1" || run.TriggerType != string(TriggerScheduled) {
		t.Errorf("run = %+v; want wf-s/t1/scheduled", run)
	}
	if run.RecordID != "" {
		t.Errorf("scheduled run must carry no single record id, got %q", run.RecordID)
	}
	if run.Status != RunSuccess {
		t.Errorf("status = %s; want success (1 matched, action clean)", run.Status)
	}
	var summary map[string]int
	if err := json.Unmarshal(run.ActionsResult, &summary); err != nil {
		t.Fatalf("scheduled actions_result not valid JSON: %v", err)
	}
	if summary["scanned"] != 2 || summary["matched"] != 1 || summary["failed"] != 0 {
		t.Errorf("summary = %+v; want scanned=2 matched=1 failed=0", summary)
	}
}

func TestScheduler_RecordsSkippedWhenNothingMatches(t *testing.T) {
	now := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	sw := ScheduledWorkflow{
		Workflow: Workflow{
			ID: "wf-s", TenantID: "t1", EntityName: "deals", Trigger: TriggerScheduled, IsActive: true,
			Condition: &Condition{Rules: []Rule{{Field: "status", Op: OpEq, Value: "stale"}}},
			Actions:   []Action{{Type: ActionInAppNotification, UserID: "u1", Message: "ping"}},
		},
	}
	loader := newFakeScheduledLoader(sw)
	lister := &fakeRecordLister{records: map[string][]ScheduledRecord{"deals": {{ID: "r1", Data: map[string]any{"status": "fresh"}}}}}
	rec := &fakeRunRecorder{}
	engine := NewAsyncDispatcher(DispatcherConfig{Buffer: 1, Workers: 1}, nil, nil, &fakeNotif{}, zap.NewNop()).
		WithRunRecorder(rec)
	s := NewScheduler(SchedulerConfig{Interval: time.Minute}, loader, lister, engine, zap.NewNop())

	s.runDue(context.Background(), now)

	runs := rec.all()
	if len(runs) != 1 {
		t.Fatalf("expected 1 recorded run, got %d", len(runs))
	}
	if runs[0].Status != RunSkipped {
		t.Errorf("status = %s; want skipped (no record matched)", runs[0].Status)
	}
}
