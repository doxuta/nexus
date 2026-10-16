package workflow

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// elapsedWF builds a set_elapsed rule writing the business minutes a ticket took into
// `resolution_minutes` — the shape the DAILY-QUEUE #41 acceptance asks a tenant admin to build.
func elapsedWF() Workflow {
	return Workflow{
		ID: "wf-elapsed", TenantID: "t1", EntityName: "tk_tickets", Trigger: TriggerAfterUpdate,
		Actions: []Action{{Type: ActionSetElapsed, Field: "resolution_minutes"}},
	}
}

func elapsedEvent(trigger Trigger, createdAt time.Time) Event {
	return Event{
		TenantID: "t1", EntityName: "tk_tickets", RecordID: "rec-1", Trigger: trigger,
		Data: map[string]any{"status": "Resolved"}, CreatedAt: createdAt,
	}
}

// writtenMinutes reads back the value the action handed the mutator. It must be a float64: that
// is the only Go type the record validator's `number` arm accepts (checkType), so an int here is
// a rule that saves cleanly and fails on every single firing.
func writtenMinutes(t *testing.T, v any) float64 {
	t.Helper()
	n, ok := v.(float64)
	if !ok {
		t.Fatalf("set_elapsed wrote %T (%v), want float64 — the number validator rejects anything else", v, v)
	}
	if n != math.Trunc(n) || n < 0 {
		t.Fatalf("set_elapsed wrote %v, want a whole, non-negative number of minutes", n)
	}
	return n
}

// TestSetElapsed_WritesBusinessMinutesSinceCreation is the slice: a ticket opened a week ago and
// closed now is stamped with the WORKING minutes in between — nights and the weekend excluded —
// walked through the tenant's calendar. The expectation is derived from the same schedule and
// bracketed around the call (the end instant is time.Now, which no test can pin), and it is far
// enough from wall-clock (10080 min) that a wall-clock implementation cannot land inside it.
func TestSetElapsed_WritesBusinessMinutesSinceCreation(t *testing.T) {
	mut := &fakeMutator{}
	sched := mustSchedule(t)
	res := &fakeScheduleResolver{sched: sched}
	created := time.Now().UTC().Add(-7 * 24 * time.Hour)

	before := time.Now().UTC()
	results, err := runActions(context.Background(), elapsedWF(), elapsedEvent(TriggerAfterUpdate, created), actionDeps{mut: mut, sched: res})
	after := time.Now().UTC()
	if err != nil {
		t.Fatalf("runActions: %v", err)
	}
	if res.calls != 1 || res.lastTn != "t1" || res.lastID != "" {
		t.Fatalf("resolver called %d times for tenant %q calendar %q, want once for t1 with the default calendar", res.calls, res.lastTn, res.lastID)
	}
	if len(mut.calls) != 1 {
		t.Fatalf("got %d writes, want 1", len(mut.calls))
	}
	c := mut.calls[0]
	if c.field != "resolution_minutes" || c.record != "rec-1" || c.entity != "tk_tickets" || c.tenant != "t1" {
		t.Fatalf("wrote to %s/%s/%s field %q, want t1/tk_tickets/rec-1 resolution_minutes", c.tenant, c.entity, c.record, c.field)
	}
	got := writtenMinutes(t, c.value)
	lo := math.Floor(sched.BusinessDurationBetween(created, before).Minutes())
	hi := math.Floor(sched.BusinessDurationBetween(created, after).Minutes())
	if got < lo || got > hi {
		t.Fatalf("stamped %v business minutes, want within [%v, %v] (the calendar's answer)", got, lo, hi)
	}
	if wall := created.Sub(before).Abs().Minutes(); got > wall-3000 {
		t.Fatalf("stamped %v minutes against %v wall-clock minutes — nights and the weekend were counted", got, wall)
	}
	if results[0].Warning != "" {
		t.Fatalf("a calendar-backed stamp carries warning %q, want none", results[0].Warning)
	}
}

// TestSetElapsed_UnknownStartErrors: an event whose CreatedAt was never filled (a best-effort
// before-image read failed, or an emit site nobody threaded) must FAIL, not measure from year 1 —
// that would walk the calendar ten years and stamp a plausible-looking giant number.
func TestSetElapsed_UnknownStartErrors(t *testing.T) {
	mut := &fakeMutator{}
	res := &fakeScheduleResolver{sched: mustSchedule(t)}
	if _, err := runActions(context.Background(), elapsedWF(), elapsedEvent(TriggerAfterUpdate, time.Time{}), actionDeps{mut: mut, sched: res}); err == nil {
		t.Fatal("want an error when the record's creation instant is unknown, got nil")
	}
	if len(mut.calls) != 0 {
		t.Fatalf("wrote %d times with no start instant, want 0", len(mut.calls))
	}
}

// TestSetElapsed_AfterDeleteNoOp: a deleted record has nothing to stamp — the skip field_update and
// set_deadline take, and NOT an error.
func TestSetElapsed_AfterDeleteNoOp(t *testing.T) {
	mut := &fakeMutator{}
	res := &fakeScheduleResolver{sched: mustSchedule(t)}
	results, err := runActions(context.Background(), elapsedWF(), elapsedEvent(TriggerAfterDelete, time.Now().Add(-time.Hour)), actionDeps{mut: mut, sched: res})
	if err != nil {
		t.Fatalf("after_delete must be a silent no-op, got error: %v", err)
	}
	if len(mut.calls) != 0 || res.calls != 0 {
		t.Fatalf("after_delete wrote %d times / read the calendar %d times, want 0 / 0", len(mut.calls), res.calls)
	}
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("want one OK result, got %+v", results)
	}
}

// TestSetElapsed_ConfigAndWiringErrors: every half-configured state is a FAILED run, never a write.
func TestSetElapsed_ConfigAndWiringErrors(t *testing.T) {
	created := time.Now().UTC().Add(-2 * time.Hour)
	for _, tc := range []struct {
		name string
		wf   func() Workflow
		deps func(*fakeMutator) actionDeps
	}{
		{"no mutator", elapsedWF, func(*fakeMutator) actionDeps {
			return actionDeps{sched: &fakeScheduleResolver{sched: mustSchedule(t)}}
		}},
		{"blank target field", func() Workflow { w := elapsedWF(); w.Actions[0].Field = "  "; return w }, func(m *fakeMutator) actionDeps {
			return actionDeps{mut: m, sched: &fakeScheduleResolver{sched: mustSchedule(t)}}
		}},
		// No resolver wired is a WIRING bug, not "this tenant has no calendar": computing wall-clock
		// here is the lie #10 split the three states to prevent.
		{"no schedule resolver wired", elapsedWF, func(m *fakeMutator) actionDeps { return actionDeps{mut: m} }},
		// A calendar store that is DOWN must not masquerade as the no-calendar fallback: the stamped
		// number could never be told apart from a real one afterwards.
		{"calendar store failure", elapsedWF, func(m *fakeMutator) actionDeps {
			return actionDeps{mut: m, sched: &fakeScheduleResolver{err: errors.New("calendar store unreachable")}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mut := &fakeMutator{}
			if _, err := runActions(context.Background(), tc.wf(), elapsedEvent(TriggerAfterUpdate, created), tc.deps(mut)); err == nil {
				t.Fatal("want an error, got nil")
			}
			if len(mut.calls) != 0 {
				t.Fatalf("wrote %d times, want 0", len(mut.calls))
			}
		})
	}
}

// TestSetElapsed_NoCalendarFallsBackToWallClockAndSaysSo: the resolver answered "this tenant has
// no usable default calendar" — the documented CALENDAR-1 fallback. The number still lands (as
// wall-clock minutes), and the run history carries a warning saying so in the admin's words.
func TestSetElapsed_NoCalendarFallsBackToWallClockAndSaysSo(t *testing.T) {
	mut := &fakeMutator{}
	created := time.Now().UTC().Add(-3 * time.Hour)
	before := time.Now().UTC()
	results, err := runActions(context.Background(), elapsedWF(), elapsedEvent(TriggerAfterUpdate, created), actionDeps{mut: mut, sched: &fakeScheduleResolver{}})
	after := time.Now().UTC()
	if err != nil {
		t.Fatalf("runActions: %v", err)
	}
	if len(mut.calls) != 1 || len(results) != 1 || !results[0].OK {
		t.Fatalf("results = %+v, writes = %d; want one OK action that stamped once", results, len(mut.calls))
	}
	got := writtenMinutes(t, mut.calls[0].value)
	if lo, hi := math.Floor(before.Sub(created).Minutes()), math.Floor(after.Sub(created).Minutes()); got < lo || got > hi {
		t.Fatalf("fallback stamped %v minutes, want wall-clock within [%v, %v]", got, lo, hi)
	}
	w := results[0].Warning
	if !strings.Contains(w, "giờ treo tường") || !strings.Contains(w, "lịch làm việc") || strings.Contains(w, "hạn này") {
		t.Fatalf("warning = %q; want a sentence about THIS number being wall-clock (not the set_deadline wording)", w)
	}
}

// TestSetElapsed_FutureCreationClampsToZero: a creation instant after the fire instant (clock skew
// between the DB and the app) is zero elapsed time on BOTH paths, never a negative count.
func TestSetElapsed_FutureCreationClampsToZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  *fakeScheduleResolver
	}{
		{"calendar", &fakeScheduleResolver{sched: mustSchedule(t)}},
		{"wall-clock fallback", &fakeScheduleResolver{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mut := &fakeMutator{}
			if _, err := runActions(context.Background(), elapsedWF(), elapsedEvent(TriggerAfterUpdate, time.Now().Add(time.Hour)), actionDeps{mut: mut, sched: tc.res}); err != nil {
				t.Fatalf("runActions: %v", err)
			}
			if len(mut.calls) != 1 {
				t.Fatalf("got %d writes, want 1", len(mut.calls))
			}
			if got := writtenMinutes(t, mut.calls[0].value); got != 0 {
				t.Fatalf("stamped %v for a record created in the future, want 0", got)
			}
		})
	}
}

// TestSetElapsed_MeasuresToTheEventInstant: the clock stops when the save that closed the record
// COMMITTED (Event.OccurredAt, stamped by Emit / the scheduler / the timer handler), not when a
// dispatcher worker got round to the event. The buffer can hold an event for seconds under a mass
// edit, and "phiếu này mất bao lâu" must not include the automation's own queue wait. A ticket
// opened Monday 08:00 ICT and closed Tuesday 09:00 ICT took 600 working minutes (1500 wall-clock)
// however late the rule runs.
func TestSetElapsed_MeasuresToTheEventInstant(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	created := time.Date(2026, 9, 21, 8, 0, 0, 0, loc) // Monday 08:00 ICT
	closed := time.Date(2026, 9, 22, 9, 0, 0, 0, loc)  // Tuesday 09:00 ICT — the save that closed it
	for _, tc := range []struct {
		name string
		res  *fakeScheduleResolver
		want float64
	}{
		{"calendar", &fakeScheduleResolver{sched: mustSchedule(t)}, 600},
		{"wall-clock fallback", &fakeScheduleResolver{}, 1500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mut := &fakeMutator{}
			evt := elapsedEvent(TriggerAfterUpdate, created.UTC())
			evt.OccurredAt = closed.UTC()
			if _, err := runActions(context.Background(), elapsedWF(), evt, actionDeps{mut: mut, sched: tc.res}); err != nil {
				t.Fatalf("runActions: %v", err)
			}
			if len(mut.calls) != 1 {
				t.Fatalf("got %d writes, want 1", len(mut.calls))
			}
			if got := writtenMinutes(t, mut.calls[0].value); got != tc.want {
				t.Fatalf("stamped %v minutes, want %v — measured to the moment the rule ran, not the moment the record was closed", got, tc.want)
			}
		})
	}
}
