package services

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/workflow"
)

// TestPreviewAction_SetElapsed: the dry-run must name the CONCRETE number the live run would write
// ("sẽ ghi 1234 phút"), computed by the same BusinessDurationBetween over the same resolved
// calendar — and its verdict must match runSetElapsed's in every branch, or "Chạy thử" teaches an
// admin to trust a rehearsal the firing then contradicts. Without an arm, the default answer is
// "action type không được hỗ trợ": a red BLOCKED row for an action that works (the create_record
// trap).
func TestPreviewAction_SetElapsed(t *testing.T) {
	created := time.Now().UTC().Add(-7 * 24 * time.Hour)
	fieldTypes := map[string]string{"resolution_minutes": "number", "subject": "text", "calc": "formula"}
	act := workflow.Action{Type: workflow.ActionSetElapsed, Field: "resolution_minutes"}
	evt := func(trigger workflow.Trigger, createdAt time.Time) workflow.Event {
		return workflow.Event{TenantID: "t1", EntityName: "tk_tickets", RecordID: "r1", Trigger: trigger,
			Data: map[string]any{"status": "Resolved", "resolution_minutes": nil}, CreatedAt: createdAt}
	}

	t.Run("calendar wired → previews the working minutes the run would write", func(t *testing.T) {
		sched := previewTestSchedule(t)
		before := time.Now().UTC()
		detail, would := previewAction(context.Background(), "wf-1", act, evt(workflow.TriggerAfterUpdate, created), true, fieldTypes, fixedScheduleResolver{sched: sched}, nil)
		after := time.Now().UTC()
		if !would || detail["wouldApply"] != true {
			t.Fatalf("wouldApply = %v (detail=%+v); want true", would, detail)
		}
		got, ok := detail["after"].(float64)
		if !ok {
			t.Fatalf("detail[after] = %T (%v); want the float64 minutes the run writes", detail["after"], detail["after"])
		}
		lo := math.Floor(sched.BusinessDurationBetween(created, before).Minutes())
		hi := math.Floor(sched.BusinessDurationBetween(created, after).Minutes())
		if got < lo || got > hi {
			t.Fatalf("previewed %v minutes, want the calendar answer within [%v, %v]", got, lo, hi)
		}
		if detail["from"] != created.Format(time.RFC3339) || detail["calendar"] != nil {
			t.Fatalf("detail = %+v; want from = the creation instant and no fallback note", detail)
		}
	})

	t.Run("no calendar → wall-clock minutes AND a note saying so", func(t *testing.T) {
		detail, would := previewAction(context.Background(), "wf-1", act, evt(workflow.TriggerAfterUpdate, created), true, fieldTypes, fixedScheduleResolver{}, nil)
		got, _ := detail["after"].(float64)
		if !would || got < 7*24*60-1 || detail["calendar"] == nil {
			t.Fatalf("detail = %+v, would = %v; want ~10080 wall-clock minutes plus a calendar note", detail, would)
		}
	})

	for _, tc := range []struct {
		name     string
		trigger  workflow.Trigger
		created  time.Time
		sched    workflow.ScheduleResolver
		field    string
		matched  bool
		wantText string // substring the explanation must carry ("" = only check the verdict)
	}{
		{"condition did not match", workflow.TriggerAfterUpdate, created, fixedScheduleResolver{sched: previewTestSchedule(t)}, "resolution_minutes", false, ""},
		{"after_delete: the runner no-ops", workflow.TriggerAfterDelete, created, fixedScheduleResolver{sched: previewTestSchedule(t)}, "resolution_minutes", true, "after_delete"},
		{"unknown creation instant: the runner errors", workflow.TriggerAfterUpdate, time.Time{}, fixedScheduleResolver{sched: previewTestSchedule(t)}, "resolution_minutes", true, "tạo"},
		{"no resolver wired: the runner errors", workflow.TriggerAfterUpdate, created, nil, "resolution_minutes", true, "lịch"},
		{"text target: the runner's write is rejected", workflow.TriggerAfterUpdate, created, fixedScheduleResolver{sched: previewTestSchedule(t)}, "subject", true, "SET_ELAPSED_FIELD_TYPE_INVALID"},
		{"computed target is readonly", workflow.TriggerAfterUpdate, created, fixedScheduleResolver{sched: previewTestSchedule(t)}, "calc", true, "computed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := act
			a.Field = tc.field
			detail, would := previewAction(context.Background(), "wf-1", a, evt(tc.trigger, tc.created), tc.matched, fieldTypes, tc.sched, nil)
			if would || detail["wouldApply"] != false {
				t.Fatalf("wouldApply = %v (detail=%+v); want false", would, detail)
			}
			if msg, _ := detail["error"].(string); tc.wantText != "" && !strings.Contains(msg, tc.wantText) {
				t.Fatalf("detail[error] = %q; want it to mention %q", msg, tc.wantText)
			}
		})
	}

	t.Run("a failing calendar store is reported WITHOUT the driver's own words", func(t *testing.T) {
		detail, would := previewAction(context.Background(), "wf-1", act, evt(workflow.TriggerAfterUpdate, created), true, fieldTypes,
			failingScheduleResolver{msg: "calmeta.SQLRepository.GetDefault: Error 1146: Table 'nexus.sys_business_calendars' doesn't exist"}, nil)
		if would || detail["after"] != nil {
			t.Fatalf("wouldApply = %v, after = %v; a store failure must not preview a number", would, detail["after"])
		}
		msg, _ := detail["error"].(string)
		if msg == "" || strings.Contains(msg, "SQLRepository") || strings.Contains(msg, "1146") {
			t.Fatalf("dry-run error = %q; want a fixed sentence, never the driver's", msg)
		}
	})
}
