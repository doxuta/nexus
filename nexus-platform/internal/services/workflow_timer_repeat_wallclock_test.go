package services

import (
	"context"
	"encoding/json"
	"testing"

	"nexus/platform/internal/workflow"
)

// DAILY-QUEUE #447: occurrence 2..N of a business_hours rule is recomputed with sched=nil (the
// handler has no calendar read), so its instant is wall-clock even when occurrence 1 used a
// calendar. The payload must carry that fact so the run it becomes says so.
func TestTimerRepeat_BusinessHoursRepeatIsMarkedWallClock(t *testing.T) {
	rep := &workflow.TimerRepeat{Every: 1, Unit: workflow.RepeatUnitDays, Count: 3}
	for _, tc := range []struct {
		name string
		unit string
		want bool
	}{
		{"business_hours re-park is wall-clock", workflow.TimerUnitBusinessHours, true},
		{"days re-park stays unflagged", workflow.TimerUnitDays, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rules, records := repeatDeps(t, rep)
			rec := rules.rows["wf-1"]
			var cond workflow.Condition
			if err := json.Unmarshal(rec.Condition, &cond); err != nil {
				t.Fatalf("decode cond: %v", err)
			}
			cond.Timer.Offset.Unit = tc.unit
			cond.Timer.Direction = workflow.TimerDirectionAfter
			raw, err := json.Marshal(cond)
			if err != nil {
				t.Fatalf("marshal cond: %v", err)
			}
			rec.Condition = raw

			q := &fakeTimerQueue{}
			h := NewWorkflowTimerJobHandler(rules, records, timerEngine(&countingMutator{}), q, nil)
			// repeatJob has WallClock=false: the first occurrence had a calendar.
			if err := h(context.Background(), repeatJob(t, 0)); err != nil {
				t.Fatalf("fire: %v", err)
			}
			if len(q.jobs) != 1 {
				t.Fatalf("parked %d follow-ups; want 1", len(q.jobs))
			}
			var next workflowTimerJobPayload
			if err := json.Unmarshal(q.jobs[0].Payload, &next); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if next.RepeatWallClock != tc.want {
				t.Errorf("RepeatWallClock = %v; want %v", next.RepeatWallClock, tc.want)
			}
		})
	}
}
