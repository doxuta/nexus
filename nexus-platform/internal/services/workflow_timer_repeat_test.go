package services

// ACTIVITY-CORE #3b — RECURRENCE. It composes on the shipped TIMER-1b primitives (a parked
// sys_jobs row at a computed instant) rather than adding a scheduler: when a wake-up fires, it
// parks the next one.
//
// The parts worth pinning are the ones that would silently break the chain or run away:
//   - the payload must DIFFER per occurrence, because the enqueue dedup matches payload equality
//     INCLUDING done jobs — an identical re-park would be dropped as a duplicate and the chain
//     would stop after the first fire, with nothing logged;
//   - each occurrence must be computed from the ANCHOR, not from "now" — otherwise a late worker
//     pickup or a retry drifts every subsequent reminder;
//   - the chain must END at Count, or one rule parks jobs forever.

import (
	"context"
	"encoding/json"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/workflow"
)

func repeatRuleRecord(t *testing.T, rep *workflow.TimerRepeat) *workflow.WorkflowRecord {
	t.Helper()
	rec := timerRuleRecord(t, true, nil)
	var cond workflow.Condition
	if err := json.Unmarshal(rec.Condition, &cond); err != nil {
		t.Fatalf("decode cond: %v", err)
	}
	cond.Timer.Repeat = rep
	raw, err := json.Marshal(cond)
	if err != nil {
		t.Fatalf("marshal cond: %v", err)
	}
	rec.Condition = raw
	return rec
}

func repeatJob(t *testing.T, occurrence int) jobs.Job {
	t.Helper()
	payload, err := json.Marshal(workflowTimerJobPayload{
		WorkflowID: "wf-1", EntityName: "deals", RecordID: "r1",
		DateValue: "2026-08-01", Occurrence: occurrence,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return jobs.Job{ID: "job-1", TenantID: "t1", Type: WorkflowTimerJobType, Payload: payload}
}

func repeatDeps(t *testing.T, rep *workflow.TimerRepeat) (*fakeTimerRules, *fakeTimerRecords) {
	t.Helper()
	return &fakeTimerRules{rows: map[string]*workflow.WorkflowRecord{"wf-1": repeatRuleRecord(t, rep)}},
		&fakeTimerRecords{rows: map[string]*dynrepo.Record{
			"r1": {ID: "r1", TenantID: "t1", Data: map[string]any{"close_date": "2026-08-01", "stage": "open"}},
		}}
}

func TestTimerRepeat_ParksTheNextOccurrenceWithADISTINCTPayload(t *testing.T) {
	rep := &workflow.TimerRepeat{Every: 1, Unit: workflow.RepeatUnitDays, Count: 3}
	rules, records := repeatDeps(t, rep)
	q := &fakeTimerQueue{}

	h := NewWorkflowTimerJobHandler(rules, records, timerEngine(&countingMutator{}), q, nil)
	if err := h(context.Background(), repeatJob(t, 0)); err != nil {
		t.Fatalf("fire: %v", err)
	}

	if len(q.jobs) != 1 {
		t.Fatalf("parked %d follow-ups; want 1 — the chain died after the first reminder", len(q.jobs))
	}
	var next workflowTimerJobPayload
	if err := json.Unmarshal(q.jobs[0].Payload, &next); err != nil {
		t.Fatalf("decode re-park: %v", err)
	}
	if next.Occurrence != 1 {
		t.Errorf("occurrence = %d; want 1 — an identical payload would be swallowed by the dedup probe", next.Occurrence)
	}
	if next.WorkflowID != "wf-1" || next.RecordID != "r1" || next.DateValue != "2026-08-01" {
		t.Errorf("re-park lost its identity: %+v", next)
	}
	if q.jobs[0].RunAt.IsZero() {
		t.Error("the follow-up has no RunAt — it would fire immediately")
	}
}

func TestTimerRepeat_EachOccurrenceIsComputedFromTheAnchor(t *testing.T) {
	rep := &workflow.TimerRepeat{Every: 2, Unit: workflow.RepeatUnitDays, Count: 5}
	rules, records := repeatDeps(t, rep)

	// Fire occurrence 0 and occurrence 2 independently; the gap between the instants they park must
	// be exactly two intervals, which only holds if both are derived from the anchor rather than
	// from when the handler happened to run.
	q0, q2 := &fakeTimerQueue{}, &fakeTimerQueue{}
	h0 := NewWorkflowTimerJobHandler(rules, records, timerEngine(&countingMutator{}), q0, nil)
	h2 := NewWorkflowTimerJobHandler(rules, records, timerEngine(&countingMutator{}), q2, nil)
	if err := h0(context.Background(), repeatJob(t, 0)); err != nil {
		t.Fatalf("fire 0: %v", err)
	}
	if err := h2(context.Background(), repeatJob(t, 2)); err != nil {
		t.Fatalf("fire 2: %v", err)
	}
	if len(q0.jobs) != 1 || len(q2.jobs) != 1 {
		t.Fatalf("parked %d and %d; want 1 each", len(q0.jobs), len(q2.jobs))
	}
	gap := q2.jobs[0].RunAt.Sub(q0.jobs[0].RunAt)
	want := workflow.RepeatOffsetFor(rep, 3) - workflow.RepeatOffsetFor(rep, 1)
	if gap != want {
		t.Errorf("gap between occurrence 3 and 1 = %v; want %v — instants are drifting off 'now'", gap, want)
	}
}

func TestTimerRepeat_ChainEndsAtCount(t *testing.T) {
	rep := &workflow.TimerRepeat{Every: 1, Unit: workflow.RepeatUnitDays, Count: 3}
	rules, records := repeatDeps(t, rep)
	q := &fakeTimerQueue{}

	h := NewWorkflowTimerJobHandler(rules, records, timerEngine(&countingMutator{}), q, nil)
	// Occurrence 2 is the LAST of three (0,1,2).
	if err := h(context.Background(), repeatJob(t, 2)); err != nil {
		t.Fatalf("fire: %v", err)
	}
	if len(q.jobs) != 0 {
		t.Fatalf("parked %d follow-ups after the final occurrence — a bounded recurrence must end itself", len(q.jobs))
	}
}

func TestTimerRepeat_AbsentRepeatStillFiresExactlyOnce(t *testing.T) {
	rules, records := repeatDeps(t, nil) // no recurrence — every pre-existing rule
	q := &fakeTimerQueue{}

	h := NewWorkflowTimerJobHandler(rules, records, timerEngine(&countingMutator{}), q, nil)
	if err := h(context.Background(), repeatJob(t, 0)); err != nil {
		t.Fatalf("fire: %v", err)
	}
	if len(q.jobs) != 0 {
		t.Errorf("a one-shot rule parked %d follow-ups", len(q.jobs))
	}
}

func TestValidTimerRepeat_RefusesTheRunawayShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		rep  *workflow.TimerRepeat
		ok   bool
	}{
		{"nil is one-shot", nil, true},
		{"valid", &workflow.TimerRepeat{Every: 1, Unit: "days", Count: 3}, true},
		{"weeks", &workflow.TimerRepeat{Every: 2, Unit: "weeks", Count: 4}, true},
		{"count 1 is not a recurrence", &workflow.TimerRepeat{Every: 1, Unit: "days", Count: 1}, false},
		{"unbounded count", &workflow.TimerRepeat{Every: 1, Unit: "days", Count: 100000}, false},
		{"minutes would be a polling loop", &workflow.TimerRepeat{Every: 5, Unit: "minutes", Count: 3}, false},
		{"zero interval", &workflow.TimerRepeat{Every: 0, Unit: "days", Count: 3}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := workflow.ValidTimerRepeat(tc.rep)
			if tc.ok != (err == nil) {
				t.Errorf("ValidTimerRepeat = %v; wantOK %v", err, tc.ok)
			}
		})
	}
}
