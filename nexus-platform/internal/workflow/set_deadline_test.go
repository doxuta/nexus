package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/calendar"
)

// deadlineWF builds a set_deadline rule writing `due_date` at now + amount×unit.
func deadlineWF(amount int, unit string) Workflow {
	return Workflow{
		ID: "wf-deadline", TenantID: "t1", EntityName: "tk_tickets", Trigger: TriggerAfterCreate,
		Actions: []Action{{Type: ActionSetDeadline, Field: "due_date", Offset: &TimerOffset{Amount: amount, Unit: unit}}},
	}
}

func deadlineEvent(trigger Trigger) Event {
	return Event{
		TenantID: "t1", EntityName: "tk_tickets", RecordID: "rec-1", Trigger: trigger,
		Data: map[string]any{"priority": "Urgent"},
	}
}

// parseWritten reads back the value the action handed the mutator. The engine writes RFC3339 so
// the record write path (normalizeForStore → canonicalDatetimeUTC) can canonicalize it; anything
// else would be rejected by the datetime validator, which is the failure this asserts against.
func parseWritten(t *testing.T, v any) time.Time {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("set_deadline wrote %T (%v), want an RFC3339 string", v, v)
	}
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("set_deadline wrote %q, which the datetime validator rejects: %v", s, err)
	}
	return ts
}

// TestSetDeadline_WritesComputedInstant is the slice's core contract: an Urgent ticket created
// with an EMPTY due date comes out of the automation carrying now+4h.
func TestSetDeadline_WritesComputedInstant(t *testing.T) {
	mut := &fakeMutator{}
	before := time.Now().UTC()
	if _, err := runActions(context.Background(), deadlineWF(4, TimerUnitHours), deadlineEvent(TriggerAfterCreate), actionDeps{mut: mut}); err != nil {
		t.Fatalf("runActions: %v", err)
	}
	if len(mut.calls) != 1 {
		t.Fatalf("got %d writes, want 1", len(mut.calls))
	}
	c := mut.calls[0]
	if c.field != "due_date" || c.record != "rec-1" || c.entity != "tk_tickets" || c.tenant != "t1" {
		t.Fatalf("wrote to %s/%s/%s field %q, want t1/tk_tickets/rec-1 due_date", c.tenant, c.entity, c.record, c.field)
	}
	got := parseWritten(t, c.value)
	want := before.Add(4 * time.Hour)
	if d := got.Sub(want); d < -5*time.Second || d > 5*time.Second {
		t.Fatalf("deadline %s is %s away from now+4h (%s)", got.Format(time.RFC3339), d, want.Format(time.RFC3339))
	}
}

// TestSetDeadline_UnitsAgreeWithTimer pins the action's arithmetic to the date_offset trigger's:
// a deadline and a reminder must never disagree about what "+N unit" means.
func TestSetDeadline_UnitsAgreeWithTimer(t *testing.T) {
	for _, tc := range []struct {
		unit string
		want time.Duration
	}{
		{TimerUnitMinutes, 30 * time.Minute},
		{TimerUnitHours, 30 * time.Hour},
		{TimerUnitDays, 30 * 24 * time.Hour},
	} {
		t.Run(tc.unit, func(t *testing.T) {
			mut := &fakeMutator{}
			before := time.Now().UTC()
			if _, err := runActions(context.Background(), deadlineWF(30, tc.unit), deadlineEvent(TriggerAfterCreate), actionDeps{mut: mut}); err != nil {
				t.Fatalf("runActions: %v", err)
			}
			if len(mut.calls) != 1 {
				t.Fatalf("got %d writes, want 1", len(mut.calls))
			}
			if d := parseWritten(t, mut.calls[0].value).Sub(before.Add(tc.want)); d < -5*time.Second || d > 5*time.Second {
				t.Fatalf("unit %q landed %s off the expected +%s", tc.unit, d, tc.want)
			}
		})
	}
}

// TestSetDeadline_AfterDeleteNoOp: a deleted record has nothing to stamp — same skip field_update
// takes, and NOT an error (an errored action is recorded as a FAILED run).
func TestSetDeadline_AfterDeleteNoOp(t *testing.T) {
	mut := &fakeMutator{}
	results, err := runActions(context.Background(), deadlineWF(4, TimerUnitHours), deadlineEvent(TriggerAfterDelete), actionDeps{mut: mut})
	if err != nil {
		t.Fatalf("after_delete must be a silent no-op, got error: %v", err)
	}
	if len(mut.calls) != 0 {
		t.Fatalf("wrote %d times on a deleted record, want 0", len(mut.calls))
	}
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("want one OK result, got %+v", results)
	}
}

// TestSetDeadline_MissingOffsetErrors: a rule stored without an offset must FAIL loudly (a
// recorded FAILED run), never write "now" and look like it worked.
func TestSetDeadline_MissingOffsetErrors(t *testing.T) {
	mut := &fakeMutator{}
	wf := deadlineWF(4, TimerUnitHours)
	wf.Actions[0].Offset = nil
	if _, err := runActions(context.Background(), wf, deadlineEvent(TriggerAfterCreate), actionDeps{mut: mut}); err == nil {
		t.Fatal("want an error when the offset is missing, got nil")
	}
	if len(mut.calls) != 0 {
		t.Fatalf("wrote %d times with no offset configured, want 0", len(mut.calls))
	}
}

// TestSetDeadline_NoMutatorErrors: an unwired mutator errors fail-open instead of panicking.
func TestSetDeadline_NoMutatorErrors(t *testing.T) {
	if _, err := runActions(context.Background(), deadlineWF(4, TimerUnitHours), deadlineEvent(TriggerAfterCreate), actionDeps{}); err == nil {
		t.Fatal("want an error when no record mutator is wired, got nil")
	}
}

// TestSetDeadline_EmptyFieldErrors: no target field means there is nowhere to put the deadline.
func TestSetDeadline_EmptyFieldErrors(t *testing.T) {
	mut := &fakeMutator{}
	wf := deadlineWF(4, TimerUnitHours)
	wf.Actions[0].Field = "  "
	if _, err := runActions(context.Background(), wf, deadlineEvent(TriggerAfterCreate), actionDeps{mut: mut}); err == nil {
		t.Fatal("want an error when the target field is blank, got nil")
	}
	if len(mut.calls) != 0 {
		t.Fatalf("wrote %d times with a blank target, want 0", len(mut.calls))
	}
}

// TestSetDeadline_OverwritesExistingValue pins the OVERWRITE decision documented on
// ActionSetDeadline. An SLA is policy: a rule that quietly skipped records someone had already
// touched would be a deadline you cannot trust, and the way to exempt a record is the rule's own
// condition, not a hidden exception. Without this test that decision is only a comment, and
// narrowing it to "only when empty" would break nothing red.
func TestSetDeadline_OverwritesExistingValue(t *testing.T) {
	mut := &fakeMutator{}
	evt := deadlineEvent(TriggerAfterUpdate)
	evt.Data["due_date"] = "2026-01-01 09:00:00" // the agent already typed one
	before := time.Now().UTC()
	if _, err := runActions(context.Background(), deadlineWF(4, TimerUnitHours), evt, actionDeps{mut: mut}); err != nil {
		t.Fatalf("runActions: %v", err)
	}
	if len(mut.calls) != 1 {
		t.Fatalf("got %d writes over an existing deadline, want 1 (the rule must overwrite)", len(mut.calls))
	}
	if d := parseWritten(t, mut.calls[0].value).Sub(before.Add(4 * time.Hour)); d < -5*time.Second || d > 5*time.Second {
		t.Fatalf("overwrite landed %s off now+4h", d)
	}
}

// TestSetDeadline_UnknownUnitFailsClosed: an unrecognised unit must ERROR, not fall through to a
// zero time — writing 0001-01-01 would be a "deadline" in the year 1.
func TestSetDeadline_UnknownUnitFailsClosed(t *testing.T) {
	mut := &fakeMutator{}
	_, err := runActions(context.Background(), deadlineWF(4, "fortnights"), deadlineEvent(TriggerAfterCreate), actionDeps{mut: mut})
	if err == nil {
		t.Fatal("want an error for an unknown unit, got nil")
	}
	if len(mut.calls) != 0 {
		t.Fatalf("wrote %d times for an unknown unit, want 0", len(mut.calls))
	}
}

// -----------------------------------------------------------------------------------------------
// DAILY-QUEUE #10 — the calendar reaches the action.
// -----------------------------------------------------------------------------------------------

// fakeScheduleResolver stands in for the tenant's CALENDAR-1 store. sched is what it hands back;
// err is the store failure path. A (nil, nil) answer is the real "this tenant has no calendar"
// reply the SQL resolver gives, which is a DIFFERENT state from "nobody wired a resolver".
type fakeScheduleResolver struct {
	sched  *calendar.Schedule
	err    error
	calls  int
	lastTn string
	lastID string
}

func (f *fakeScheduleResolver) ResolveSchedule(_ context.Context, tenantID, calendarID string) (*calendar.Schedule, error) {
	f.calls++
	f.lastTn, f.lastID = tenantID, calendarID
	return f.sched, f.err
}

// TestSetDeadline_BusinessHoursUsesResolvedCalendar is the slice: a business_hours deadline is
// computed through the tenant's working schedule, not wall-clock. The expectation is DERIVED from
// the same schedule (the run's anchor is time.Now, which no test can pin) — what it proves is that
// the action consulted the calendar at all, which wall-clock arithmetic cannot fake outside
// working hours. The exact "Friday 15:00 ICT + 4bh = Monday 10:00" contract is pinned
// deterministically one layer down, in TestComputeFireAt.
func TestSetDeadline_BusinessHoursUsesResolvedCalendar(t *testing.T) {
	mut := &fakeMutator{}
	sched := mustSchedule(t)
	res := &fakeScheduleResolver{sched: sched}
	before := time.Now().UTC()
	if _, err := runActions(context.Background(), deadlineWF(4, TimerUnitBusinessHours), deadlineEvent(TriggerAfterCreate), actionDeps{mut: mut, sched: res}); err != nil {
		t.Fatalf("runActions: %v", err)
	}
	if res.calls != 1 || res.lastTn != "t1" {
		t.Fatalf("resolver called %d times for tenant %q, want 1 for t1", res.calls, res.lastTn)
	}
	if len(mut.calls) != 1 {
		t.Fatalf("got %d writes, want 1", len(mut.calls))
	}
	want, err := sched.AddBusinessDuration(before, 4*time.Hour)
	if err != nil {
		t.Fatalf("AddBusinessDuration: %v", err)
	}
	got := parseWritten(t, mut.calls[0].value)
	if d := got.Sub(want.UTC()); d < -5*time.Second || d > 5*time.Second {
		t.Fatalf("deadline %s is %s away from the calendar answer %s", got.Format(time.RFC3339), d, want.UTC().Format(time.RFC3339))
	}
}

// TestSetDeadline_BusinessHoursWithoutResolverErrors separates the two states that produce the
// SAME wall-clock number. "No resolver wired" is a WIRING BUG (the dispatcher was built without
// the calendar seam, or the scheduler snapshotted deps() too early) and must surface as a FAILED
// run. Silently computing wall-clock here is the exact lie the old hard rejection existed to stop,
// and every gate would stay green while it happened.
func TestSetDeadline_BusinessHoursWithoutResolverErrors(t *testing.T) {
	mut := &fakeMutator{}
	_, err := runActions(context.Background(), deadlineWF(4, TimerUnitBusinessHours), deadlineEvent(TriggerAfterCreate), actionDeps{mut: mut})
	if err == nil {
		t.Fatal("want an error when business_hours runs with no schedule resolver wired, got nil")
	}
	if len(mut.calls) != 0 {
		t.Fatalf("wrote %d times with no resolver; a wall-clock value here would be a silent lie", len(mut.calls))
	}
}

// TestSetDeadline_BusinessHoursNoCalendarFallsBackToWallClock is the OTHER side of that split: the
// resolver answered, and the answer is "this tenant has no calendar". That is the documented
// CALENDAR-1 fallback (specs/023 §1) the date_offset timer already takes, so the deadline still
// lands — as wall-clock — instead of the rule dying.
func TestSetDeadline_BusinessHoursNoCalendarFallsBackToWallClock(t *testing.T) {
	mut := &fakeMutator{}
	res := &fakeScheduleResolver{} // answered: no calendar
	before := time.Now().UTC()
	if _, err := runActions(context.Background(), deadlineWF(4, TimerUnitBusinessHours), deadlineEvent(TriggerAfterCreate), actionDeps{mut: mut, sched: res}); err != nil {
		t.Fatalf("runActions: %v", err)
	}
	if len(mut.calls) != 1 {
		t.Fatalf("got %d writes, want 1 (the fallback must still stamp a deadline)", len(mut.calls))
	}
	if d := parseWritten(t, mut.calls[0].value).Sub(before.Add(4 * time.Hour)); d < -5*time.Second || d > 5*time.Second {
		t.Fatalf("fallback landed %s off now+4h", d)
	}
}

// TestSetDeadline_BusinessHoursResolverErrorFailsClosed: a calendar store that is DOWN is not the
// same as a tenant with no calendar. Falling back to wall-clock on an infrastructure error would
// stamp a number nobody can distinguish from the real thing.
func TestSetDeadline_BusinessHoursResolverErrorFailsClosed(t *testing.T) {
	mut := &fakeMutator{}
	res := &fakeScheduleResolver{err: errors.New("calendar store unreachable")}
	_, err := runActions(context.Background(), deadlineWF(4, TimerUnitBusinessHours), deadlineEvent(TriggerAfterCreate), actionDeps{mut: mut, sched: res})
	if err == nil {
		t.Fatal("want an error when the calendar store fails, got nil")
	}
	if len(mut.calls) != 0 {
		t.Fatalf("wrote %d times after a calendar store failure, want 0", len(mut.calls))
	}
}

// TestSetDeadline_WallClockUnitsNeverTouchTheCalendar: minutes/hours/days must not pay for a
// calendar lookup they do not use — and must keep working on a tenant with no calendar at all.
func TestSetDeadline_WallClockUnitsNeverTouchTheCalendar(t *testing.T) {
	for _, unit := range []string{TimerUnitMinutes, TimerUnitHours, TimerUnitDays} {
		t.Run(unit, func(t *testing.T) {
			mut := &fakeMutator{}
			res := &fakeScheduleResolver{sched: mustSchedule(t)}
			if _, err := runActions(context.Background(), deadlineWF(2, unit), deadlineEvent(TriggerAfterCreate), actionDeps{mut: mut, sched: res}); err != nil {
				t.Fatalf("runActions: %v", err)
			}
			if res.calls != 0 {
				t.Fatalf("unit %q consulted the calendar %d times, want 0", unit, res.calls)
			}
		})
	}
}

// TestSetDeadline_WallClockFallbackIsVisibleInRunHistory (DAILY-QUEUE #82): the no-calendar
// fallback still stamps a deadline — that is the documented contract — but the run history must
// SAY it did. Before this, runActions recorded a bare {ok:true} and the dispatcher wrote the run as
// `success`, so the one signal an admin trusts (W6 run history) reported a working-hours deadline
// that was in fact wall-clock; the only trace was a WARN line in the server log.
//
// It is asserted on the MARSHALLED result because that JSON is exactly what recordRun writes into
// sys_workflow_runs.actions_result and what the run-history screen renders.
func TestSetDeadline_WallClockFallbackIsVisibleInRunHistory(t *testing.T) {
	run := func(t *testing.T, unit string, res ScheduleResolver) map[string]any {
		t.Helper()
		mut := &fakeMutator{}
		results, err := runActions(context.Background(), deadlineWF(4, unit), deadlineEvent(TriggerAfterCreate), actionDeps{mut: mut, sched: res})
		if err != nil {
			t.Fatalf("runActions: %v", err)
		}
		if len(results) != 1 || !results[0].OK || len(mut.calls) != 1 {
			t.Fatalf("results = %+v, writes = %d; want one OK action that stamped once (the fallback still stamps)", results, len(mut.calls))
		}
		b, err := json.Marshal(results[0])
		if err != nil {
			t.Fatalf("marshal ActionResult: %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("unmarshal ActionResult: %v", err)
		}
		return out
	}

	t.Run("no calendar: the recorded result says the deadline is wall-clock", func(t *testing.T) {
		got := run(t, TimerUnitBusinessHours, &fakeScheduleResolver{}) // answered: no calendar
		w, _ := got["warning"].(string)
		if !strings.Contains(w, "giờ treo tường") || !strings.Contains(w, "lịch làm việc") {
			t.Fatalf("actions_result = %v; want a warning naming the missing working calendar and the wall-clock fallback", got)
		}
	})
	t.Run("calendar resolved: no warning", func(t *testing.T) {
		if got := run(t, TimerUnitBusinessHours, &fakeScheduleResolver{sched: mustSchedule(t)}); got["warning"] != nil {
			t.Fatalf("actions_result = %v; a deadline computed through the calendar must not carry a fallback warning", got)
		}
	})
	t.Run("wall-clock unit: no warning", func(t *testing.T) {
		if got := run(t, TimerUnitHours, &fakeScheduleResolver{}); got["warning"] != nil {
			t.Fatalf("actions_result = %v; an `hours` deadline IS wall-clock by choice, not by fallback", got)
		}
	})
}
