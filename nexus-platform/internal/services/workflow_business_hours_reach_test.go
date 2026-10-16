package services

// DAILY-QUEUE #80 (`timer-amount-bound-calendar-aware`) — a business_hours offset the tenant's
// working calendar cannot reach must be refused when the rule is SAVED, not discovered as a FAILED
// run every time it fires.
//
// The bound that existed (workflow.maxTimerAmount = 100000) is a WALL-CLOCK ceiling: 100000 hours
// is ~11.4 years of calendar time. A business hour is scarcer than that — a Mon-Fri 08:00-17:00
// week holds 45 of them — so on that calendar anything from ~23500 business hours up walks past
// calendar.maxWalkDays (~10 years) and ComputeFireAt answers `no working time within 3660 days` on
// EVERY firing, while the save path said nothing. The same hole sits under both consumers of the
// unit: the date_offset trigger and the set_deadline action, so both are pinned here.
//
// What the cases prove, beyond "big number rejected":
//   - the ceiling is DERIVED FROM THE CALENDAR, not a new flat constant: the same 32000 that is
//     unreachable on a 45-hour week is accepted on a 24/7 calendar;
//   - a tenant with NO calendar keeps the documented wall-clock fallback (specs/023 §1), which never
//     walks the calendar and so never hits the ceiling — its bound stays the wall-clock one;
//   - wall-clock units never consult the calendar at all;
//   - a calendar store that is DOWN is an error, not a silent pass.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/calendar"
	"nexus/platform/internal/workflow"
)

// recordingScheduleResolver hands back a fixed schedule and remembers which calendar id it was
// asked for, so a pinned timer can be shown to consult ITS calendar rather than the default.
type recordingScheduleResolver struct {
	sched *calendar.Schedule
	calls int
	ids   []string
}

func (r *recordingScheduleResolver) ResolveSchedule(_ context.Context, _ string, calendarID string) (*calendar.Schedule, error) {
	r.calls++
	r.ids = append(r.ids, calendarID)
	return r.sched, nil
}

// alwaysOpenSchedule is a 24/7 calendar: 168 business hours a week.
func alwaysOpenSchedule(t *testing.T) *calendar.Schedule {
	t.Helper()
	day := []calendar.Interval{{Start: "00:00", End: "24:00"}}
	s, err := calendar.New(calendar.Config{Timezone: "Asia/Ho_Chi_Minh", Hours: map[string][]calendar.Interval{
		"mon": day, "tue": day, "wed": day, "thu": day, "fri": day, "sat": day, "sun": day,
	}})
	if err != nil {
		t.Fatalf("calendar.New: %v", err)
	}
	return s
}

func TestWorkflowService_BusinessHoursAmountMustFitTheCalendar(t *testing.T) {
	officeWeek := previewTestSchedule(t) // Mon-Fri 08:00-17:00 = 45 business hours a week

	bhTimer := func(amount int, calendarID string) *WorkflowTimerInput {
		return &WorkflowTimerInput{
			DateField:  "follow_up_at",
			Offset:     WorkflowTimerOffsetInput{Amount: amount, Unit: workflow.TimerUnitBusinessHours},
			Direction:  workflow.TimerDirectionAfter,
			CalendarID: calendarID,
		}
	}
	notify := []WorkflowActionInput{{Type: "field_update", Field: "stage", Value: "reminded"}}
	deadline := func(amount int, unit string) []WorkflowActionInput {
		return []WorkflowActionInput{{Type: "set_deadline", Field: "follow_up_at",
			Offset: &WorkflowTimerOffsetInput{Amount: amount, Unit: unit}}}
	}

	tests := []struct {
		name     string
		trigger  string
		timer    *WorkflowTimerInput
		actions  []WorkflowActionInput
		sched    *calendar.Schedule // nil = the tenant has no calendar (wall-clock fallback)
		wantCode string             // "" = accepted
	}{
		// date_offset trigger — the hole that existed before set_deadline did.
		{"timer: 32000 business hours cannot be reached on a 45-hour week",
			"date_offset", bhTimer(32000, ""), notify, officeWeek, "WORKFLOW_TIMER_OFFSET_INVALID"},
		{"timer: 4 business hours is an ordinary SLA",
			"date_offset", bhTimer(4, ""), notify, officeWeek, ""},
		{"timer: 23000 business hours still fits inside the walk on a 45-hour week",
			"date_offset", bhTimer(23000, ""), notify, officeWeek, ""},
		{"timer: the same 32000 IS reachable on a 24/7 calendar — the ceiling is the calendar's, not a constant",
			"date_offset", bhTimer(32000, ""), notify, alwaysOpenSchedule(t), ""},
		{"timer: no calendar → wall-clock fallback, bounded by the wall-clock ceiling only",
			"date_offset", bhTimer(32000, ""), notify, nil, ""},

		// set_deadline action — the second consumer #10 opened the same hole to.
		{"set_deadline: 32000 business hours cannot be reached on a 45-hour week",
			"after_create", nil, deadline(32000, workflow.TimerUnitBusinessHours), officeWeek, "SET_DEADLINE_OFFSET_INVALID"},
		{"set_deadline: 40 business hours (the Ticketing standard tier) is fine",
			"after_create", nil, deadline(40, workflow.TimerUnitBusinessHours), officeWeek, ""},
		{"set_deadline: no calendar → wall-clock fallback, accepted",
			"after_create", nil, deadline(32000, workflow.TimerUnitBusinessHours), nil, ""},
		{"set_deadline: 32000 wall-clock hours never touches the calendar",
			"after_create", nil, deadline(32000, workflow.TimerUnitHours), officeWeek, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := &recordingScheduleResolver{sched: tt.sched}
			svc := NewWorkflowService(newFakeWFRepo()).WithEntityChecker(timerCatalog()).WithScheduleResolver(res)
			_, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
				Name: "r", EntityName: "deals", Trigger: tt.trigger, Timer: tt.timer, Actions: tt.actions,
			})
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("want accept, got %v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != tt.wantCode {
				t.Fatalf("err = %v, want code %s", err, tt.wantCode)
			}
			// The admin has to be able to fix the rule from the message alone: it must say the
			// calendar is what limits the amount, and how much working time a week that calendar has.
			if !strings.Contains(ve.Msg, "lịch làm việc") || !strings.Contains(ve.Msg, "45") {
				t.Errorf("message %q does not name the calendar and its 45 hours a week", ve.Msg)
			}
		})
	}

	t.Run("wall-clock units never consult the calendar", func(t *testing.T) {
		res := &recordingScheduleResolver{sched: officeWeek}
		svc := NewWorkflowService(newFakeWFRepo()).WithEntityChecker(timerCatalog()).WithScheduleResolver(res)
		if _, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
			Name: "r", EntityName: "deals", Trigger: "after_create", Actions: deadline(3, workflow.TimerUnitDays),
		}); err != nil {
			t.Fatalf("CreateWorkflow: %v", err)
		}
		if res.calls != 0 {
			t.Fatalf("a days offset consulted the calendar %d times, want 0", res.calls)
		}
	})

	t.Run("a pinned timer is checked against ITS calendar, not the default", func(t *testing.T) {
		res := &recordingScheduleResolver{sched: officeWeek}
		svc := NewWorkflowService(newFakeWFRepo()).WithEntityChecker(timerCatalog()).WithScheduleResolver(res).
			WithCalendarChecker(&fakeCalendarChecker{known: map[string]string{"cal-1": "t1"}})
		if _, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
			Name: "r", EntityName: "deals", Trigger: "date_offset", Timer: bhTimer(4, "cal-1"), Actions: notify,
		}); err != nil {
			t.Fatalf("CreateWorkflow: %v", err)
		}
		if len(res.ids) != 1 || res.ids[0] != "cal-1" {
			t.Fatalf("resolver asked for calendars %v, want exactly [cal-1]", res.ids)
		}
	})

	t.Run("update runs the same check as create", func(t *testing.T) {
		repo := newFakeWFRepo()
		svc := NewWorkflowService(repo).WithEntityChecker(timerCatalog()).
			WithScheduleResolver(&recordingScheduleResolver{sched: officeWeek})
		rec, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
			Name: "r", EntityName: "deals", Trigger: "after_create", Actions: deadline(40, workflow.TimerUnitBusinessHours),
		})
		if err != nil {
			t.Fatalf("CreateWorkflow: %v", err)
		}
		_, err = svc.UpdateWorkflow(context.Background(), "t1", rec.ID, UpdateWorkflowInput{
			Name: "r", EntityName: "deals", Trigger: "after_create", Actions: deadline(32000, workflow.TimerUnitBusinessHours),
		})
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "SET_DEADLINE_OFFSET_INVALID" {
			t.Fatalf("UpdateWorkflow err = %v, want SET_DEADLINE_OFFSET_INVALID", err)
		}
	})

	t.Run("a calendar store that is down is an error, not a pass", func(t *testing.T) {
		svc := NewWorkflowService(newFakeWFRepo()).WithEntityChecker(timerCatalog()).
			WithScheduleResolver(failingScheduleResolver{msg: "calendar store unreachable"})
		_, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
			Name: "r", EntityName: "deals", Trigger: "after_create", Actions: deadline(4, workflow.TimerUnitBusinessHours),
		})
		if err == nil {
			t.Fatal("want an error when the calendar cannot be read at save time, got nil")
		}
		var ve *ValidationError
		if errors.As(err, &ve) {
			t.Fatalf("a store failure surfaced as validation %s — it would read as the admin's mistake", ve.Code)
		}
	})
}
