package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/calendar"
	"nexus/platform/internal/workflow"
)

// The dry-run ("Chạy thử") is where an admin rehearses a rule before letting it touch real data.
// previewAction's default arm answers "action type không được hỗ trợ", which the builder renders as
// a red BLOCKED row — so an action that is not previewable reads as an action that does not work.
// create_record already paid this price once (see the comment on its arm). set_deadline must not
// repeat it, and the preview must name the CONCRETE instant: "it would set a deadline" is not an
// answer an admin can check, "it would set 2026-09-11T15:09:00Z" is.
func TestPreviewAction_SetDeadline(t *testing.T) {
	evt := workflow.Event{
		TenantID: "t1", EntityName: "tk_tickets", RecordID: "r1", Trigger: workflow.TriggerAfterCreate,
		Data: map[string]any{"priority": "Urgent", "due_date": nil},
	}
	act := workflow.Action{Type: workflow.ActionSetDeadline, Field: "due_date",
		Offset: &workflow.TimerOffset{Amount: 4, Unit: workflow.TimerUnitHours}}
	fieldTypes := map[string]string{"due_date": "datetime", "subject": "text", "calc": "formula"}

	t.Run("condition matched → previews the concrete instant", func(t *testing.T) {
		before := time.Now().UTC()
		detail, would := previewAction(context.Background(), "wf-1", act, evt, true, fieldTypes, nil, nil)
		if !would {
			t.Fatalf("wouldApply = false; want true (detail=%+v)", detail)
		}
		if detail["type"] != workflow.ActionSetDeadline || detail["field"] != "due_date" {
			t.Fatalf("detail = %+v; want type/field of the action", detail)
		}
		after, ok := detail["after"].(string)
		if !ok {
			t.Fatalf("detail[after] = %T (%v); want an RFC3339 string", detail["after"], detail["after"])
		}
		ts, err := time.Parse(time.RFC3339, after)
		if err != nil {
			t.Fatalf("detail[after] = %q is not RFC3339: %v", after, err)
		}
		if d := ts.Sub(before.Add(4 * time.Hour)); d < -5*time.Second || d > 5*time.Second {
			t.Fatalf("previewed deadline %s is %s off now+4h", after, d)
		}
		if detail["wouldApply"] != true {
			t.Fatalf("detail[wouldApply] = %v; want true", detail["wouldApply"])
		}
	})

	t.Run("condition did not match → no apply", func(t *testing.T) {
		detail, would := previewAction(context.Background(), "wf-1", act, evt, false, fieldTypes, nil, nil)
		if would || detail["wouldApply"] != false {
			t.Fatalf("wouldApply = %v / %v; want false (condition did not match)", would, detail["wouldApply"])
		}
	})

	t.Run("wrong field type is reported, not hidden", func(t *testing.T) {
		bad := act
		bad.Field = "subject"
		detail, would := previewAction(context.Background(), "wf-1", bad, evt, true, fieldTypes, nil, nil)
		if would {
			t.Fatalf("wouldApply = true for a text target; want false (detail=%+v)", detail)
		}
		if detail["error"] == nil {
			t.Fatalf("detail = %+v; want an error explaining the text target", detail)
		}
	})

	t.Run("computed target is reported", func(t *testing.T) {
		bad := act
		bad.Field = "calc"
		detail, would := previewAction(context.Background(), "wf-1", bad, evt, true, fieldTypes, nil, nil)
		if would || detail["error"] == nil {
			t.Fatalf("detail = %+v, would = %v; want a blocked preview naming the computed target", detail, would)
		}
	})

	t.Run("missing offset is reported", func(t *testing.T) {
		bad := act
		bad.Offset = nil
		detail, would := previewAction(context.Background(), "wf-1", bad, evt, true, fieldTypes, nil, nil)
		if would || detail["error"] == nil {
			t.Fatalf("detail = %+v, would = %v; want a blocked preview naming the missing offset", detail, would)
		}
	})
}

// The dry-run's verdict must match what the RUNNER would do, in every branch. Three cases where it
// previously said "sẽ chạy" for something that does nothing (or errors) — each one teaches an admin
// to trust a rehearsal that is wrong, which is worse than having no rehearsal.
func TestPreviewAction_SetDeadline_MatchesRunnerVerdict(t *testing.T) {
	fieldTypes := map[string]string{"due_date": "datetime"}
	act := workflow.Action{Type: workflow.ActionSetDeadline, Field: "due_date",
		Offset: &workflow.TimerOffset{Amount: 4, Unit: workflow.TimerUnitHours}}

	t.Run("after_delete: the runner no-ops, so the preview must not promise a stamp", func(t *testing.T) {
		evt := workflow.Event{TenantID: "t1", EntityName: "tk_tickets", RecordID: "r1",
			Trigger: workflow.TriggerAfterDelete, Data: map[string]any{}}
		detail, would := previewAction(context.Background(), "wf-1", act, evt, true, fieldTypes, nil, nil)
		if would || detail["wouldApply"] != false {
			t.Fatalf("wouldApply = %v; want false on after_delete (detail=%+v)", would, detail)
		}
		if detail["error"] == nil {
			t.Fatalf("detail = %+v; want an explanation that a deleted record has nothing to stamp", detail)
		}
	})

	t.Run("business_hours with no resolver wired: refuse rather than draw a wall-clock instant", func(t *testing.T) {
		// ComputeFireAt does NOT error on business_hours — it falls back to wall-clock when the
		// schedule is nil. So a preview that shrugged at a missing resolver would render a
		// plausible instant that the live run (which errors on the same missing wire) never
		// produces. This is the rehearsal half of runSetDeadline's "no resolver ≠ no calendar".
		bad := act
		bad.Offset = &workflow.TimerOffset{Amount: 4, Unit: workflow.TimerUnitBusinessHours}
		evt := workflow.Event{TenantID: "t1", EntityName: "tk_tickets", RecordID: "r1",
			Trigger: workflow.TriggerAfterCreate, Data: map[string]any{}}
		detail, would := previewAction(context.Background(), "wf-1", bad, evt, true, fieldTypes, nil, nil)
		if would || detail["wouldApply"] != false {
			t.Fatalf("wouldApply = %v for business_hours with no resolver; want false (detail=%+v)", would, detail)
		}
		if detail["after"] != nil {
			t.Fatalf("detail[after] = %v — previewing an instant the runner cannot produce is the lie this guards", detail["after"])
		}
	})

	t.Run("business_hours walks the tenant calendar, same as the live run", func(t *testing.T) {
		// The instant the rehearsal shows must be the one the firing writes. Deriving the
		// expectation from the same schedule is what makes this a real check: outside working
		// hours the calendar answer is DAYS away from the wall-clock one.
		bh := act
		bh.Offset = &workflow.TimerOffset{Amount: 4, Unit: workflow.TimerUnitBusinessHours}
		evt := workflow.Event{TenantID: "t1", EntityName: "tk_tickets", RecordID: "r1",
			Trigger: workflow.TriggerAfterCreate, Data: map[string]any{}}
		sched := previewTestSchedule(t)
		before := time.Now().UTC()
		detail, would := previewAction(context.Background(), "wf-1", bh, evt, true, fieldTypes, fixedScheduleResolver{sched: sched}, nil)
		if !would {
			t.Fatalf("wouldApply = false with a calendar wired (detail=%+v)", detail)
		}
		after, _ := detail["after"].(string)
		got, err := time.Parse(time.RFC3339, after)
		if err != nil {
			t.Fatalf("detail[after] = %q, not an RFC3339 instant: %v", after, err)
		}
		want, err := sched.AddBusinessDuration(before, 4*time.Hour)
		if err != nil {
			t.Fatalf("AddBusinessDuration: %v", err)
		}
		if d := got.Sub(want.UTC()); d < -5*time.Second || d > 5*time.Second {
			t.Fatalf("preview %s is %s away from the calendar answer %s", after, d, want.UTC().Format(time.RFC3339))
		}
	})

	t.Run("business_hours on a tenant with no calendar says so, and still previews wall-clock", func(t *testing.T) {
		// The documented CALENDAR-1 fallback. Showing the instant silently would leave an admin
		// believing working hours applied; the note is the only thing that distinguishes it.
		bh := act
		bh.Offset = &workflow.TimerOffset{Amount: 4, Unit: workflow.TimerUnitBusinessHours}
		evt := workflow.Event{TenantID: "t1", EntityName: "tk_tickets", RecordID: "r1",
			Trigger: workflow.TriggerAfterCreate, Data: map[string]any{}}
		before := time.Now().UTC()
		detail, would := previewAction(context.Background(), "wf-1", bh, evt, true, fieldTypes, fixedScheduleResolver{}, nil)
		if !would || detail["after"] == nil {
			t.Fatalf("detail = %+v, would = %v; want the wall-clock fallback instant", detail, would)
		}
		if detail["calendar"] == nil {
			t.Fatalf("detail = %+v; want a note that the tenant has no working calendar", detail)
		}
		got, err := time.Parse(time.RFC3339, detail["after"].(string))
		if err != nil {
			t.Fatalf("detail[after] not RFC3339: %v", err)
		}
		if d := got.Sub(before.Add(4 * time.Hour)); d < -5*time.Second || d > 5*time.Second {
			t.Fatalf("fallback preview landed %s off now+4h", d)
		}
	})

	t.Run("a failing calendar store is reported WITHOUT the driver's own words", func(t *testing.T) {
		// The trace detail is returned verbatim in the dry-run response body. Every other error in
		// this arm comes from ComputeFireAt, which is pure — the calendar lookup is the only I/O,
		// so it is the only way a DB-layer string could reach an API caller.
		bh := act
		bh.Offset = &workflow.TimerOffset{Amount: 4, Unit: workflow.TimerUnitBusinessHours}
		evt := workflow.Event{TenantID: "t1", EntityName: "tk_tickets", RecordID: "r1",
			Trigger: workflow.TriggerAfterCreate, Data: map[string]any{}}
		detail, would := previewAction(context.Background(), "wf-1", bh, evt, true, fieldTypes,
			failingScheduleResolver{msg: "calmeta.SQLRepository.GetDefault: Error 1146: Table 'nexus.sys_business_calendars' doesn't exist"}, nil)
		if would || detail["after"] != nil {
			t.Fatalf("wouldApply = %v, after = %v; a store failure must not preview an instant", would, detail["after"])
		}
		msg, _ := detail["error"].(string)
		for _, leak := range []string{"SQLRepository", "Error 1146", "sys_business_calendars", "nexus."} {
			if strings.Contains(msg, leak) {
				t.Fatalf("dry-run error leaks %q to the API caller: %q", leak, msg)
			}
		}
		if msg == "" {
			t.Fatal("a store failure must still SAY something — silence reads as success")
		}
	})

	t.Run("the normal case still previews an instant", func(t *testing.T) {
		evt := workflow.Event{TenantID: "t1", EntityName: "tk_tickets", RecordID: "r1",
			Trigger: workflow.TriggerAfterCreate, Data: map[string]any{}}
		detail, would := previewAction(context.Background(), "wf-1", act, evt, true, fieldTypes, nil, nil)
		if !would || detail["after"] == nil {
			t.Fatalf("detail = %+v, would = %v; want a concrete instant", detail, would)
		}
	})
}

// fixedScheduleResolver hands back one prepared schedule (or "this tenant has no calendar" when
// sched is nil) without touching a store.
type fixedScheduleResolver struct{ sched *calendar.Schedule }

func (f fixedScheduleResolver) ResolveSchedule(context.Context, string, string) (*calendar.Schedule, error) {
	return f.sched, nil
}

// previewTestSchedule is the Q9 default working week: Mon-Fri 08:00-17:00 Asia/Ho_Chi_Minh.
func previewTestSchedule(t *testing.T) *calendar.Schedule {
	t.Helper()
	day := []calendar.Interval{{Start: "08:00", End: "17:00"}}
	s, err := calendar.New(calendar.Config{Timezone: "Asia/Ho_Chi_Minh", Hours: map[string][]calendar.Interval{
		"mon": day, "tue": day, "wed": day, "thu": day, "fri": day,
	}})
	if err != nil {
		t.Fatalf("calendar.New: %v", err)
	}
	return s
}

// failingScheduleResolver stands in for a calendar store that is down.
type failingScheduleResolver struct{ msg string }

func (f failingScheduleResolver) ResolveSchedule(context.Context, string, string) (*calendar.Schedule, error) {
	return nil, errors.New(f.msg)
}
