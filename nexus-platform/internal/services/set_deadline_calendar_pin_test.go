package services

// DAILY-QUEUE #79 (`set-deadline-calendar-pin`) — the config half: a set_deadline may name the
// working calendar its business_hours offset walks (`calendarId`), and the save refuses a pin that
// cannot work — the same two refusals the date_offset timer's pin has had since TIMER-1
// (WORKFLOW_TIMER_CALENDAR_INVALID / _NOT_FOUND), under set_deadline's own codes and pinned to the
// ACTION ROW (`details.field = "actions[i].calendarId"`): a rule can carry several set_deadline
// actions, and "calendarId not found" in a footer does not say which one to fix.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/calendar"
	"nexus/platform/internal/workflow"
)

func TestWorkflowService_SetDeadlineCalendarPin(t *testing.T) {
	officeWeek := previewTestSchedule(t)
	// The set_deadline sits at index 1 on purpose: the pointer must name the action's own row,
	// not "the first action".
	actions := func(unit, calendarID string) []WorkflowActionInput {
		return []WorkflowActionInput{
			{Type: "field_update", Field: "stage", Value: "sla"},
			{Type: "set_deadline", Field: "follow_up_at",
				Offset: &WorkflowTimerOffsetInput{Amount: 12, Unit: unit}, CalendarID: calendarID},
		}
	}
	newSvc := func(res workflow.ScheduleResolver) (*WorkflowService, *fakeWFRepo) {
		repo := newFakeWFRepo()
		return NewWorkflowService(repo).WithEntityChecker(timerCatalog()).WithScheduleResolver(res).
			WithCalendarChecker(&fakeCalendarChecker{known: map[string]string{"cal-1": "t1", "cal-other": "t2"}}), repo
	}
	storedAction := func(t *testing.T, repo *fakeWFRepo) map[string]any {
		t.Helper()
		if len(repo.created) != 1 {
			t.Fatalf("expected 1 create, got %d", len(repo.created))
		}
		var stored []map[string]any
		if err := json.Unmarshal(repo.created[0].Actions, &stored); err != nil {
			t.Fatalf("actions JSON: %v", err)
		}
		return stored[1]
	}

	t.Run("a business_hours deadline pinned to a live calendar saves the pin", func(t *testing.T) {
		res := &recordingScheduleResolver{sched: officeWeek}
		svc, repo := newSvc(res)
		if _, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
			Name: "r", EntityName: "deals", Trigger: "after_create",
			Actions: actions(workflow.TimerUnitBusinessHours, " cal-1 "),
		}); err != nil {
			t.Fatalf("CreateWorkflow: %v", err)
		}
		if got := storedAction(t, repo)["calendarId"]; got != "cal-1" {
			t.Fatalf("stored calendarId = %v, want the trimmed pin cal-1", got)
		}
		// #80's reach check must walk the calendar the run will walk — the pin, not the default.
		if len(res.ids) != 1 || res.ids[0] != "cal-1" {
			t.Fatalf("reach check asked for calendars %q, want exactly [cal-1]", res.ids)
		}
	})

	t.Run("no pin is stored absent — a rule saved before #79 and one saved after are byte-identical", func(t *testing.T) {
		svc, repo := newSvc(&recordingScheduleResolver{sched: officeWeek})
		if _, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
			Name: "r", EntityName: "deals", Trigger: "after_create",
			Actions: actions(workflow.TimerUnitBusinessHours, "   "),
		}); err != nil {
			t.Fatalf("CreateWorkflow: %v", err)
		}
		if _, present := storedAction(t, repo)["calendarId"]; present {
			t.Fatalf("stored action %v carries calendarId; a blank pin must be stored absent", storedAction(t, repo))
		}
	})

	refusals := []struct {
		name     string
		unit     string
		pin      string
		wantCode string
	}{
		{"a pin on a wall-clock unit walks no calendar — refused", workflow.TimerUnitHours, "cal-1", "SET_DEADLINE_CALENDAR_INVALID"},
		{"an unknown calendar is refused", workflow.TimerUnitBusinessHours, "cal-ghost", "SET_DEADLINE_CALENDAR_NOT_FOUND"},
		{"another tenant's calendar is refused (no cross-tenant pin)", workflow.TimerUnitBusinessHours, "cal-other", "SET_DEADLINE_CALENDAR_NOT_FOUND"},
	}
	for _, tt := range refusals {
		t.Run(tt.name, func(t *testing.T) {
			svc, repo := newSvc(&recordingScheduleResolver{sched: officeWeek})
			_, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
				Name: "r", EntityName: "deals", Trigger: "after_create", Actions: actions(tt.unit, tt.pin),
			})
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != tt.wantCode {
				t.Fatalf("err = %v, want code %s", err, tt.wantCode)
			}
			if ve.Field != "actions[1].calendarId" || ve.Details["field"] != "actions[1].calendarId" {
				t.Fatalf("field = %q / details = %v; want the pointer actions[1].calendarId", ve.Field, ve.Details)
			}
			if len(repo.created) != 0 {
				t.Fatalf("a refused rule was persisted: %+v", repo.created)
			}
		})
	}

	t.Run("update runs the same existence check as create", func(t *testing.T) {
		svc, _ := newSvc(&recordingScheduleResolver{sched: officeWeek})
		rec, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
			Name: "r", EntityName: "deals", Trigger: "after_create", Actions: actions(workflow.TimerUnitBusinessHours, "cal-1"),
		})
		if err != nil {
			t.Fatalf("CreateWorkflow: %v", err)
		}
		_, err = svc.UpdateWorkflow(context.Background(), "t1", rec.ID, UpdateWorkflowInput{
			Name: "r", EntityName: "deals", Trigger: "after_create", Actions: actions(workflow.TimerUnitBusinessHours, "cal-ghost"),
		})
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "SET_DEADLINE_CALENDAR_NOT_FOUND" {
			t.Fatalf("UpdateWorkflow err = %v, want SET_DEADLINE_CALENDAR_NOT_FOUND", err)
		}
	})

	// B6 parity: the config-time validator the AI Builder and pack install share refuses the
	// DB-free half with the same code the apply path uses.
	t.Run("ValidateWorkflowConfig refuses a pin on a wall-clock unit with the same code", func(t *testing.T) {
		err := ValidateWorkflowConfig("deals", "after_create", nil, actions(workflow.TimerUnitDays, "cal-1"),
			map[string]string{"stage": "text", "follow_up_at": "datetime"})
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "SET_DEADLINE_CALENDAR_INVALID" {
			t.Fatalf("ValidateWorkflowConfig err = %v, want SET_DEADLINE_CALENDAR_INVALID", err)
		}
	})
}

// byIDScheduleResolver answers per calendar id ("" = the tenant default) and records the ids asked.
type byIDScheduleResolver struct {
	byID map[string]*calendar.Schedule
	ids  []string
}

func (r *byIDScheduleResolver) ResolveSchedule(_ context.Context, _ string, calendarID string) (*calendar.Schedule, error) {
	r.ids = append(r.ids, calendarID)
	return r.byID[calendarID], nil
}

// The dry run must rehearse the calendar the run walks. Previewing a pinned deadline against the
// default would show an admin one instant and stamp another — the trap #10 already fell into once.
func TestPreviewAction_SetDeadlinePinnedCalendar(t *testing.T) {
	evt := workflow.Event{TenantID: "t1", EntityName: "tk_tickets", RecordID: "r1",
		Trigger: workflow.TriggerAfterCreate, Data: map[string]any{}}
	fieldTypes := map[string]string{"due_date": "datetime"}
	act := workflow.Action{Type: workflow.ActionSetDeadline, Field: "due_date", CalendarID: "cal-247",
		Offset: &workflow.TimerOffset{Amount: 12, Unit: workflow.TimerUnitBusinessHours}}

	t.Run("previews through the pinned calendar and names it", func(t *testing.T) {
		res := &byIDScheduleResolver{byID: map[string]*calendar.Schedule{"": previewTestSchedule(t), "cal-247": alwaysOpenSchedule(t)}}
		detail, would := previewAction(context.Background(), "wf-1", act, evt, true, fieldTypes, res, nil)
		if !would {
			t.Fatalf("wouldApply = false (detail=%+v)", detail)
		}
		if len(res.ids) != 1 || res.ids[0] != "cal-247" {
			t.Fatalf("preview asked for calendars %q, want exactly [cal-247]", res.ids)
		}
		if detail["calendarId"] != "cal-247" {
			t.Fatalf("detail[calendarId] = %v; the trace must say which calendar it walked", detail["calendarId"])
		}
	})

	t.Run("a pin that no longer resolves is named as the pin, not as a missing default", func(t *testing.T) {
		res := &byIDScheduleResolver{byID: map[string]*calendar.Schedule{"": previewTestSchedule(t)}}
		detail, would := previewAction(context.Background(), "wf-1", act, evt, true, fieldTypes, res, nil)
		if !would || detail["after"] == nil {
			t.Fatalf("detail = %+v; want the wall-clock fallback instant, as the run stamps", detail)
		}
		note, _ := detail["calendar"].(string)
		if !strings.Contains(note, "ghim") {
			t.Fatalf("detail[calendar] = %q; want a note that the PINNED calendar is unusable", note)
		}
	})
}
