package workflow

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// DAILY-QUEUE #445: one scheduled firing over N matching records on a tenant with no calendar
// logs the wall-clock fallback ONCE (the run summary already carries warned/warnings), while the
// event path — one record per event — keeps one line per event.
func TestResolveBusinessSchedule_FallbackWarnLoggedOncePerFiring(t *testing.T) {
	deadline := Action{Type: ActionSetDeadline, Field: "due_date", Offset: &TimerOffset{Amount: 4, Unit: TimerUnitBusinessHours}}
	recs := make([]ScheduledRecord, 100)
	for i := range recs {
		recs[i] = ScheduledRecord{ID: string(rune('a' + i%26)), Data: map[string]any{}, CreatedAt: time.Now().UTC().Add(-time.Hour)}
	}
	wf := Workflow{ID: "wf1", TenantID: "t1", EntityName: "tk_tickets", Trigger: TriggerScheduled, IsActive: true, Actions: []Action{deadline}}
	fallbackLines := func(logs *observer.ObservedLogs) int {
		return logs.FilterMessage(wallClockFallbackLogMsg).Len()
	}

	t.Run("scheduled firing over 100 records: one line", func(t *testing.T) {
		core, logs := observer.New(zap.WarnLevel)
		s := newScheduler(newFakeScheduledLoader(ScheduledWorkflow{Workflow: wf}),
			&fakeRecordLister{records: map[string][]ScheduledRecord{"tk_tickets": recs}},
			actionDeps{mut: &fakeMutator{}, sched: &fakeScheduleResolver{}, logger: zap.New(core)})
		s.runDue(context.Background(), time.Now())
		if got := fallbackLines(logs); got != 1 {
			t.Fatalf("fallback WARN lines = %d; want 1 for the whole firing", got)
		}
	})

	t.Run("event path: one line per event", func(t *testing.T) {
		core, logs := observer.New(zap.WarnLevel)
		deps := actionDeps{mut: &fakeMutator{}, sched: &fakeScheduleResolver{}, logger: zap.New(core)}
		for i := 0; i < 3; i++ {
			evt := Event{TenantID: "t1", EntityName: "tk_tickets", RecordID: "r", Data: map[string]any{}, CreatedAt: time.Now().UTC()}
			if _, err := runActions(context.Background(), wf, evt, deps); err != nil {
				t.Fatalf("runActions: %v", err)
			}
		}
		if got := fallbackLines(logs); got != 3 {
			t.Fatalf("fallback WARN lines = %d; want 3 (one per event)", got)
		}
	})
}
