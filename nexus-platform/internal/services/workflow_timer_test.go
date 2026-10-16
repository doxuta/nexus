package services

// TIMER-1 unit tests — date_offset config validation (validate == apply parity), the
// WorkflowTimerScheduler producer (fire_at math + dedup + wall-clock fallback) and the
// workflow_timer job handler (fire-time re-evaluation). All fakes, no DB.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nexus/platform/internal/calmeta"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// timerCatalog builds a catalog whose "deals" entity carries date/datetime/text fields.
func timerCatalog() *fakeWFCatalog {
	return &fakeWFCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-t1", TenantID: "t1", Name: "deals"},
		fields: []metadata.FieldDefinition{
			{ID: "f1", EntityID: "ent-t1", Name: "close_date", FieldType: "date"},
			{ID: "f2", EntityID: "ent-t1", Name: "follow_up_at", FieldType: "datetime"},
			{ID: "f3", EntityID: "ent-t1", Name: "stage", FieldType: "text"},
		},
	}
}

// timerInput returns a valid baseline timer input tests mutate per case.
func timerInput() *WorkflowTimerInput {
	return &WorkflowTimerInput{
		DateField: "close_date",
		Offset:    WorkflowTimerOffsetInput{Amount: 3, Unit: "days"},
		Direction: "before",
	}
}

// fakeCalendarChecker satisfies workflowCalendarChecker with a fixed id set (tenant-pinned).
type fakeCalendarChecker struct {
	known map[string]string // id → tenant
}

func (f *fakeCalendarChecker) GetByID(_ context.Context, tenantID, id string) (*calmeta.BusinessCalendar, error) {
	if owner, ok := f.known[id]; ok && owner == tenantID {
		return &calmeta.BusinessCalendar{ID: id, TenantID: tenantID}, nil
	}
	return nil, calmeta.ErrCalendarNotFound
}

func TestWorkflowService_DateOffsetValidation(t *testing.T) {
	baseActions := []WorkflowActionInput{{Type: "field_update", Field: "stage", Value: "reminded"}}

	tests := []struct {
		name     string
		trigger  string
		mutate   func(in *WorkflowTimerInput) *WorkflowTimerInput
		wantCode string // "" = accepted
	}{
		{name: "valid days-before on date field", trigger: "date_offset",
			mutate: func(in *WorkflowTimerInput) *WorkflowTimerInput { return in }},
		{name: "valid business_hours after on datetime field", trigger: "date_offset",
			mutate: func(in *WorkflowTimerInput) *WorkflowTimerInput {
				in.DateField, in.Offset, in.Direction = "follow_up_at", WorkflowTimerOffsetInput{Amount: 4, Unit: "business_hours"}, "after"
				return in
			}},
		{name: "valid created_at anchor", trigger: "date_offset",
			mutate: func(in *WorkflowTimerInput) *WorkflowTimerInput { in.DateField = "created_at"; return in }},
		{name: "valid pinned calendar with business_hours", trigger: "date_offset",
			mutate: func(in *WorkflowTimerInput) *WorkflowTimerInput {
				in.Offset, in.Direction, in.CalendarID = WorkflowTimerOffsetInput{Amount: 2, Unit: "business_hours"}, "after", "cal-1"
				return in
			}},
		{name: "missing timer on date_offset", trigger: "date_offset",
			mutate: func(*WorkflowTimerInput) *WorkflowTimerInput { return nil }, wantCode: "WORKFLOW_TIMER_REQUIRED"},
		{name: "timer on non-timer trigger rejected", trigger: "after_create",
			mutate: func(in *WorkflowTimerInput) *WorkflowTimerInput { return in }, wantCode: "WORKFLOW_TIMER_INVALID"},
		{name: "dateField not a date type", trigger: "date_offset",
			mutate:   func(in *WorkflowTimerInput) *WorkflowTimerInput { in.DateField = "stage"; return in },
			wantCode: "WORKFLOW_TIMER_FIELD_INVALID"},
		{name: "dateField unknown", trigger: "date_offset",
			mutate:   func(in *WorkflowTimerInput) *WorkflowTimerInput { in.DateField = "nope"; return in },
			wantCode: "WORKFLOW_TIMER_FIELD_INVALID"},
		// #287: "unchanged for N days" — updated_at is a system anchor like created_at.
		{name: "valid updated_at anchor (inactivity)", trigger: "date_offset",
			mutate: func(in *WorkflowTimerInput) *WorkflowTimerInput {
				in.DateField, in.Offset, in.Direction = "updated_at", WorkflowTimerOffsetInput{Amount: 14, Unit: "days"}, "after"
				return in
			}},
		{name: "other system column still refused", trigger: "date_offset",
			mutate:   func(in *WorkflowTimerInput) *WorkflowTimerInput { in.DateField = "deleted_at"; return in },
			wantCode: "WORKFLOW_TIMER_FIELD_INVALID"},
		{name: "zero amount", trigger: "date_offset",
			mutate:   func(in *WorkflowTimerInput) *WorkflowTimerInput { in.Offset.Amount = 0; return in },
			wantCode: "WORKFLOW_TIMER_OFFSET_INVALID"},
		{name: "unknown unit", trigger: "date_offset",
			mutate:   func(in *WorkflowTimerInput) *WorkflowTimerInput { in.Offset.Unit = "weeks"; return in },
			wantCode: "WORKFLOW_TIMER_UNIT_INVALID"},
		{name: "unknown direction", trigger: "date_offset",
			mutate:   func(in *WorkflowTimerInput) *WorkflowTimerInput { in.Direction = "sideways"; return in },
			wantCode: "WORKFLOW_TIMER_DIRECTION_INVALID"},
		{name: "business_hours before rejected (v1)", trigger: "date_offset",
			mutate: func(in *WorkflowTimerInput) *WorkflowTimerInput {
				in.Offset.Unit, in.Direction = "business_hours", "before"
				return in
			},
			wantCode: "WORKFLOW_TIMER_DIRECTION_INVALID"},
		{name: "updated_at anchor before rejected", trigger: "date_offset",
			mutate: func(in *WorkflowTimerInput) *WorkflowTimerInput {
				in.DateField, in.Direction = "updated_at", "before"
				return in
			},
			wantCode: "WORKFLOW_TIMER_DIRECTION_INVALID"},
		{name: "calendarId without business_hours rejected", trigger: "date_offset",
			mutate:   func(in *WorkflowTimerInput) *WorkflowTimerInput { in.CalendarID = "cal-1"; return in },
			wantCode: "WORKFLOW_TIMER_CALENDAR_INVALID"},
		{name: "unknown calendarId rejected", trigger: "date_offset",
			mutate: func(in *WorkflowTimerInput) *WorkflowTimerInput {
				in.Offset, in.Direction, in.CalendarID = WorkflowTimerOffsetInput{Amount: 1, Unit: "business_hours"}, "after", "cal-ghost"
				return in
			},
			wantCode: "WORKFLOW_TIMER_CALENDAR_NOT_FOUND"},
		{name: "cross-tenant calendarId rejected", trigger: "date_offset",
			mutate: func(in *WorkflowTimerInput) *WorkflowTimerInput {
				in.Offset, in.Direction, in.CalendarID = WorkflowTimerOffsetInput{Amount: 1, Unit: "business_hours"}, "after", "cal-other"
				return in
			},
			wantCode: "WORKFLOW_TIMER_CALENDAR_NOT_FOUND"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeWFRepo()
			svc := NewWorkflowService(repo).WithEntityChecker(timerCatalog()).
				WithCalendarChecker(&fakeCalendarChecker{known: map[string]string{
					"cal-1": "t1", "cal-other": "t2",
				}})
			timer := tt.mutate(timerInput())
			_, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
				Name: "r", EntityName: "deals", Trigger: tt.trigger, Timer: timer, Actions: baseActions,
			})
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("want accept, got %v", err)
				}
				// The persisted condition JSON must round-trip the timer (parked-job consumers
				// decode it from this exact blob).
				if len(repo.created) != 1 {
					t.Fatalf("expected 1 create, got %d", len(repo.created))
				}
				var cond workflow.Condition
				if err := json.Unmarshal(repo.created[0].Condition, &cond); err != nil {
					t.Fatalf("condition JSON: %v", err)
				}
				if cond.Timer == nil || cond.Timer.DateField != timer.DateField {
					t.Fatalf("persisted timer = %+v, want dateField %q", cond.Timer, timer.DateField)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != tt.wantCode {
				t.Fatalf("err = %v, want code %s", err, tt.wantCode)
			}
		})
	}
}

// TestValidateWorkflowConfig_TimerParity proves the exported config-time validator and the apply
// path reject a bad timer with the SAME code (one buildWorkflowRule body — B6 parity).
func TestValidateWorkflowConfig_TimerParity(t *testing.T) {
	fieldTypes := map[string]string{"close_date": "date", "stage": "text"}
	cond := &WorkflowConditionInput{Timer: &WorkflowTimerInput{
		DateField: "stage", Offset: WorkflowTimerOffsetInput{Amount: 1, Unit: "days"}, Direction: "before",
	}}
	actions := []WorkflowActionInput{{Type: "field_update", Field: "stage", Value: "x"}}

	err := ValidateWorkflowConfig("deals", "date_offset", cond, actions, fieldTypes)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "WORKFLOW_TIMER_FIELD_INVALID" {
		t.Fatalf("ValidateWorkflowConfig err = %v, want WORKFLOW_TIMER_FIELD_INVALID", err)
	}

	svc := NewWorkflowService(newFakeWFRepo()).WithEntityChecker(timerCatalog())
	_, aerr := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
		Name: "r", EntityName: "deals", Trigger: "date_offset",
		Timer:   cond.Timer,
		Actions: actions,
	})
	var ave *ValidationError
	if !errors.As(aerr, &ave) || ave.Code != ve.Code {
		t.Fatalf("apply path code = %v, want %s (parity)", aerr, ve.Code)
	}
}

// ---- producer (WorkflowTimerScheduler) ---------------------------------------------------------

// fakeTimerQueue records enqueued jobs and answers the dedup probe from what it holds.
type fakeTimerQueue struct {
	mu       sync.Mutex
	jobs     []jobs.Job
	probeErr error
}

func (f *fakeTimerQueue) Enqueue(_ context.Context, j jobs.Job) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j.Status = jobs.StatusQueued // mirror SQLStore.Enqueue, which parks rows as queued
	f.jobs = append(f.jobs, j)
	return fmt.Sprintf("job-%d", len(f.jobs)), nil
}

// HasJobWithPayload mirrors SQLStore: a payload is "present" only while a matching job is still
// queued, running, or DONE. A failed job (retries exhausted) does not block a re-park.
func (f *fakeTimerQueue) HasJobWithPayload(_ context.Context, jobType, tenantID string, payload []byte) (bool, error) {
	if f.probeErr != nil {
		return false, f.probeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, j := range f.jobs {
		if j.Type != jobType || j.TenantID != tenantID || string(j.Payload) != string(payload) {
			continue
		}
		switch j.Status {
		case jobs.StatusQueued, jobs.StatusRunning, jobs.StatusDone:
			return true, nil
		}
	}
	return false, nil
}

// markStatus flips the status of every job carrying the given payload — lets a test simulate a
// parked wake-up that has since fired (done) or exhausted its retries (failed).
func (f *fakeTimerQueue) markStatus(payload []byte, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.jobs {
		if string(f.jobs[i].Payload) == string(payload) {
			f.jobs[i].Status = status
		}
	}
}

// fakeTimerCalendars serves one default calendar row (24/7 UTC unless hours overridden).
type fakeTimerCalendars struct {
	def *calmeta.BusinessCalendar
}

func (f *fakeTimerCalendars) GetByID(_ context.Context, tenantID, id string) (*calmeta.BusinessCalendar, error) {
	if f.def != nil && f.def.ID == id && f.def.TenantID == tenantID {
		return f.def, nil
	}
	return nil, calmeta.ErrCalendarNotFound
}

func (f *fakeTimerCalendars) GetDefault(_ context.Context, tenantID string) (*calmeta.BusinessCalendar, error) {
	if f.def != nil && f.def.TenantID == tenantID {
		return f.def, nil
	}
	return nil, calmeta.ErrCalendarNotFound
}

// fakeTimerRecords serves one record per id.
type fakeTimerRecords struct {
	rows map[string]*dynrepo.Record
}

func (f *fakeTimerRecords) GetRecord(_ context.Context, _, tenantID, id string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return nil, dynrepo.ErrRecordNotFound
	}
	return r, nil
}

// timerWorkflow builds a date_offset workflow with the given timer config.
func timerWorkflow(cfg workflow.TimerConfig) workflow.Workflow {
	return workflow.Workflow{
		ID: "wf-1", TenantID: "t1", Name: "remind", EntityName: "deals",
		Trigger:   workflow.TriggerDateOffset,
		Condition: &workflow.Condition{Timer: &cfg},
		Actions:   []workflow.Action{{Type: workflow.ActionFieldUpdate, Field: "stage", Value: "reminded"}},
		IsActive:  true,
	}
}

func TestWorkflowTimerScheduler_ScheduleTimer(t *testing.T) {
	// Business calendar: mon–fri 09:00–17:00 UTC.
	weekHours := `{"mon":[{"start":"09:00","end":"17:00"}],"tue":[{"start":"09:00","end":"17:00"}],
		"wed":[{"start":"09:00","end":"17:00"}],"thu":[{"start":"09:00","end":"17:00"}],
		"fri":[{"start":"09:00","end":"17:00"}]}`
	defCal := &calmeta.BusinessCalendar{
		ID: "cal-1", TenantID: "t1", Timezone: "UTC",
		Hours: json.RawMessage(weekHours), IsDefault: true,
	}

	evt := func(data map[string]any) workflow.Event {
		return workflow.Event{
			TenantID: "t1", EntityName: "deals", RecordID: "r1",
			Trigger: workflow.TriggerAfterCreate, Data: data,
		}
	}
	// recs seeds the record reader with r1 carrying data. The row on disk is the anchor's source of
	// truth (the event snapshot predates the same dispatch's actions — DAILY-QUEUE #6), so a case
	// that expects a wake-up must put the date on the RECORD, not only on the event.
	recs := func(data map[string]any) *fakeTimerRecords {
		return &fakeTimerRecords{rows: map[string]*dynrepo.Record{
			"r1": {ID: "r1", TenantID: "t1", Data: data},
		}}
	}

	t.Run("wall-clock days-before enqueues at date minus offset", func(t *testing.T) {
		q := &fakeTimerQueue{}
		s := NewWorkflowTimerScheduler(q, &fakeTimerCalendars{def: defCal}, recs(map[string]any{"close_date": "2026-08-01"}))
		wf := timerWorkflow(workflow.TimerConfig{
			DateField: "close_date", Offset: workflow.TimerOffset{Amount: 3, Unit: "days"}, Direction: "before",
		})
		if err := s.ScheduleTimer(context.Background(), wf, evt(map[string]any{"close_date": "2026-08-01"})); err != nil {
			t.Fatalf("ScheduleTimer: %v", err)
		}
		if len(q.jobs) != 1 {
			t.Fatalf("jobs = %d, want 1", len(q.jobs))
		}
		want := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC) // 2026-08-01 00:00 UTC − 72h
		if !q.jobs[0].RunAt.Equal(want) {
			t.Fatalf("RunAt = %v, want %v", q.jobs[0].RunAt, want)
		}
		if q.jobs[0].TenantID != "t1" || q.jobs[0].Type != WorkflowTimerJobType {
			t.Fatalf("job identity wrong: %+v", q.jobs[0])
		}
		var p workflowTimerJobPayload
		if err := json.Unmarshal(q.jobs[0].Payload, &p); err != nil || p.WorkflowID != "wf-1" || p.RecordID != "r1" || p.DateValue != "2026-08-01" {
			t.Fatalf("payload = %+v (err %v)", p, err)
		}
	})

	t.Run("business_hours consults the default calendar", func(t *testing.T) {
		q := &fakeTimerQueue{}
		s := NewWorkflowTimerScheduler(q, &fakeTimerCalendars{def: defCal}, recs(map[string]any{"follow_up_at": "2026-07-10T15:00:00Z"}))
		wf := timerWorkflow(workflow.TimerConfig{
			DateField: "follow_up_at", Offset: workflow.TimerOffset{Amount: 4, Unit: "business_hours"}, Direction: "after",
		})
		// Friday 2026-07-10 15:00 UTC + 4 business hours (09–17): 2h Friday + 2h Monday → Monday 11:00 UTC.
		if err := s.ScheduleTimer(context.Background(), wf, evt(map[string]any{"follow_up_at": "2026-07-10T15:00:00Z"})); err != nil {
			t.Fatalf("ScheduleTimer: %v", err)
		}
		want := time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC)
		if len(q.jobs) != 1 || !q.jobs[0].RunAt.Equal(want) {
			t.Fatalf("jobs=%d RunAt=%v, want 1 @ %v", len(q.jobs), q.jobs[0].RunAt, want)
		}
	})

	t.Run("business_hours with no calendar falls back to wall-clock", func(t *testing.T) {
		q := &fakeTimerQueue{}
		s := NewWorkflowTimerScheduler(q, &fakeTimerCalendars{}, recs(map[string]any{"follow_up_at": "2026-07-10T15:00:00Z"})) // no default calendar
		wf := timerWorkflow(workflow.TimerConfig{
			DateField: "follow_up_at", Offset: workflow.TimerOffset{Amount: 4, Unit: "business_hours"}, Direction: "after",
		})
		if err := s.ScheduleTimer(context.Background(), wf, evt(map[string]any{"follow_up_at": "2026-07-10T15:00:00Z"})); err != nil {
			t.Fatalf("ScheduleTimer: %v", err)
		}
		want := time.Date(2026, 7, 10, 19, 0, 0, 0, time.UTC) // plain +4h
		if len(q.jobs) != 1 || !q.jobs[0].RunAt.Equal(want) {
			t.Fatalf("jobs=%d RunAt=%v, want 1 @ %v (wall-clock fallback)", len(q.jobs), q.jobs[0].RunAt, want)
		}
		// DAILY-QUEUE #105 (b): the fallback is written INTO the wake-up, so the run it becomes can say so.
		var p workflowTimerJobPayload
		if err := json.Unmarshal(q.jobs[0].Payload, &p); err != nil || !p.WallClock {
			t.Fatalf("payload = %s; want wallClock:true on a business_hours wake-up parked without a calendar", q.jobs[0].Payload)
		}
	})

	// #105 (b): only the fallback is flagged — a calendar-computed or plain-hours wake-up keeps the
	// exact payload it always had (older parked jobs and the dedup probe depend on that shape).
	t.Run("wallClock is set only on the business_hours fallback", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			cals *fakeTimerCalendars
			unit string
		}{
			{"business_hours with a calendar", &fakeTimerCalendars{def: defCal}, "business_hours"},
			{"plain hours, no calendar", &fakeTimerCalendars{}, "hours"},
		} {
			q := &fakeTimerQueue{}
			s := NewWorkflowTimerScheduler(q, tc.cals, recs(map[string]any{"follow_up_at": "2026-07-10T15:00:00Z"}))
			wf := timerWorkflow(workflow.TimerConfig{
				DateField: "follow_up_at", Offset: workflow.TimerOffset{Amount: 4, Unit: tc.unit}, Direction: "after",
			})
			if err := s.ScheduleTimer(context.Background(), wf, evt(map[string]any{"follow_up_at": "2026-07-10T15:00:00Z"})); err != nil {
				t.Fatalf("%s: ScheduleTimer: %v", tc.name, err)
			}
			if len(q.jobs) != 1 || strings.Contains(string(q.jobs[0].Payload), "wallClock") {
				t.Fatalf("%s: payload = %s; want no wallClock key", tc.name, q.jobs[0].Payload)
			}
		}
	})

	// #105 (b): the flag is part of the payload, and the payload is the dedup key. A calendar added
	// (or removed) between two edits that leave the date alone must NOT park a second wake-up for
	// the same (rule, record, date) — that would fire the reminder twice.
	t.Run("a calendar appearing or vanishing between edits does not park a second wake-up", func(t *testing.T) {
		data := map[string]any{"follow_up_at": "2026-07-10T15:00:00Z"}
		wf := timerWorkflow(workflow.TimerConfig{
			DateField: "follow_up_at", Offset: workflow.TimerOffset{Amount: 4, Unit: "business_hours"}, Direction: "after",
		})
		for _, order := range [][2]*fakeTimerCalendars{{{}, {def: defCal}}, {{def: defCal}, {}}} {
			q := &fakeTimerQueue{}
			rows := recs(data)
			for _, cals := range order {
				if err := NewWorkflowTimerScheduler(q, cals, rows).ScheduleTimer(context.Background(), wf, evt(data)); err != nil {
					t.Fatalf("ScheduleTimer: %v", err)
				}
			}
			if len(q.jobs) != 1 {
				t.Fatalf("jobs = %d, want 1 — the same wake-up must not be parked twice when only the calendar changed", len(q.jobs))
			}
		}
	})

	t.Run("date field unset enqueues nothing", func(t *testing.T) {
		q := &fakeTimerQueue{}
		s := NewWorkflowTimerScheduler(q, &fakeTimerCalendars{def: defCal}, recs(map[string]any{"stage": "new"}))
		wf := timerWorkflow(workflow.TimerConfig{
			DateField: "close_date", Offset: workflow.TimerOffset{Amount: 3, Unit: "days"}, Direction: "before",
		})
		if err := s.ScheduleTimer(context.Background(), wf, evt(map[string]any{"stage": "new"})); err != nil {
			t.Fatalf("ScheduleTimer: %v", err)
		}
		if len(q.jobs) != 0 {
			t.Fatalf("jobs = %d, want 0 (unset date)", len(q.jobs))
		}
	})

	t.Run("identical payload is deduped", func(t *testing.T) {
		q := &fakeTimerQueue{}
		data := map[string]any{"close_date": "2026-08-01"}
		rows := recs(data)
		s := NewWorkflowTimerScheduler(q, &fakeTimerCalendars{def: defCal}, rows)
		wf := timerWorkflow(workflow.TimerConfig{
			DateField: "close_date", Offset: workflow.TimerOffset{Amount: 3, Unit: "days"}, Direction: "before",
		})
		for i := 0; i < 3; i++ { // create + two unrelated updates
			if err := s.ScheduleTimer(context.Background(), wf, evt(data)); err != nil {
				t.Fatalf("ScheduleTimer #%d: %v", i, err)
			}
		}
		if len(q.jobs) != 1 {
			t.Fatalf("jobs = %d, want 1 (dedup)", len(q.jobs))
		}
		// A MOVED date is a new payload → a second wake-up parks (the stale one skips at fire).
		moved := map[string]any{"close_date": "2026-09-01"}
		rows.rows["r1"].Data = moved
		if err := s.ScheduleTimer(context.Background(), wf, evt(moved)); err != nil {
			t.Fatalf("ScheduleTimer moved: %v", err)
		}
		if len(q.jobs) != 2 {
			t.Fatalf("jobs = %d, want 2 (moved date parks a replacement)", len(q.jobs))
		}
	})

	t.Run("created_at anchor reads the record", func(t *testing.T) {
		created := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
		q := &fakeTimerQueue{}
		recs := &fakeTimerRecords{rows: map[string]*dynrepo.Record{
			"r1": {ID: "r1", TenantID: "t1", CreatedAt: created, Data: map[string]any{}},
		}}
		s := NewWorkflowTimerScheduler(q, &fakeTimerCalendars{}, recs)
		wf := timerWorkflow(workflow.TimerConfig{
			DateField: "created_at", Offset: workflow.TimerOffset{Amount: 1, Unit: "hours"}, Direction: "after",
		})
		if err := s.ScheduleTimer(context.Background(), wf, evt(nil)); err != nil {
			t.Fatalf("ScheduleTimer: %v", err)
		}
		if len(q.jobs) != 1 || !q.jobs[0].RunAt.Equal(created.Add(time.Hour)) {
			t.Fatalf("jobs=%d, want 1 @ created_at+1h", len(q.jobs))
		}
	})

	// #287: an edit moves updated_at, so the wake-up parked for the old stamp is stale (the handler
	// skips it) and a fresh one is parked N days after the NEW stamp. Same stamp twice = one job.
	t.Run("updated_at anchor: each edit re-parks N days later", func(t *testing.T) {
		first := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
		q := &fakeTimerQueue{}
		recs := &fakeTimerRecords{rows: map[string]*dynrepo.Record{
			"r1": {ID: "r1", TenantID: "t1", UpdatedAt: first, Data: map[string]any{}},
		}}
		s := NewWorkflowTimerScheduler(q, &fakeTimerCalendars{}, recs)
		wf := timerWorkflow(workflow.TimerConfig{
			DateField: "updated_at", Offset: workflow.TimerOffset{Amount: 14, Unit: "days"}, Direction: "after",
		})
		for i := 0; i < 2; i++ { // same stamp twice → dedup
			if err := s.ScheduleTimer(context.Background(), wf, evt(nil)); err != nil {
				t.Fatalf("ScheduleTimer: %v", err)
			}
		}
		if len(q.jobs) != 1 || !q.jobs[0].RunAt.Equal(first.AddDate(0, 0, 14)) {
			t.Fatalf("jobs=%d, want 1 @ updated_at+14d", len(q.jobs))
		}
		second := first.Add(48 * time.Hour)
		recs.rows["r1"].UpdatedAt = second // an edit
		if err := s.ScheduleTimer(context.Background(), wf, evt(nil)); err != nil {
			t.Fatalf("ScheduleTimer: %v", err)
		}
		if len(q.jobs) != 2 || !q.jobs[1].RunAt.Equal(second.AddDate(0, 0, 14)) {
			t.Fatalf("jobs=%d, want a second wake-up @ new updated_at+14d", len(q.jobs))
		}
	})

	t.Run("dedup probe error propagates", func(t *testing.T) {
		q := &fakeTimerQueue{probeErr: errors.New("db down")}
		s := NewWorkflowTimerScheduler(q, &fakeTimerCalendars{}, recs(map[string]any{"close_date": "2026-08-01"}))
		wf := timerWorkflow(workflow.TimerConfig{
			DateField: "close_date", Offset: workflow.TimerOffset{Amount: 3, Unit: "days"}, Direction: "before",
		})
		err := s.ScheduleTimer(context.Background(), wf, evt(map[string]any{"close_date": "2026-08-01"}))
		if err == nil {
			t.Fatal("want error from dedup probe")
		}
	})

	// HIGH re-fire regression: once a wake-up has FIRED (job done), a later unrelated update that
	// re-derives the SAME payload must NOT park a replacement — else the action runs again on every
	// post-deadline edit. The old probe only matched queued rows, so a done job looked absent.
	t.Run("update after job fired does not re-park", func(t *testing.T) {
		q := &fakeTimerQueue{}
		data := map[string]any{"close_date": "2026-08-01"}
		s := NewWorkflowTimerScheduler(q, &fakeTimerCalendars{def: defCal}, recs(data))
		wf := timerWorkflow(workflow.TimerConfig{
			DateField: "close_date", Offset: workflow.TimerOffset{Amount: 3, Unit: "days"}, Direction: "before",
		})
		if err := s.ScheduleTimer(context.Background(), wf, evt(data)); err != nil {
			t.Fatalf("ScheduleTimer create: %v", err)
		}
		payload := q.jobs[0].Payload
		q.markStatus(payload, jobs.StatusDone) // the wake-up fired

		if err := s.ScheduleTimer(context.Background(), wf, evt(data)); err != nil {
			t.Fatalf("ScheduleTimer post-fire update: %v", err)
		}
		if len(q.jobs) != 1 {
			t.Fatalf("jobs = %d, want 1 (done job blocks re-park — no re-fire)", len(q.jobs))
		}

		// Contrast: a FAILED wake-up (retries exhausted) is not terminal-success, so re-park is allowed.
		q.markStatus(payload, jobs.StatusFailed)
		if err := s.ScheduleTimer(context.Background(), wf, evt(data)); err != nil {
			t.Fatalf("ScheduleTimer after failure: %v", err)
		}
		if len(q.jobs) != 2 {
			t.Fatalf("jobs = %d, want 2 (failed job allows a fresh park)", len(q.jobs))
		}
	})

	// DAILY-QUEUE #6 (timer-anchor-current-record): the after-commit event snapshot predates the
	// SAME dispatch's action loop, so a field_update that just wrote the date field (a machine-set
	// due_date) is invisible in evt.Data. The anchor must come from the record on disk.
	t.Run("anchor is the CURRENT record, not the event snapshot", func(t *testing.T) {
		q := &fakeTimerQueue{}
		// Event snapshot: no date (the ticket had none when the user saved). Record: the date the
		// after_update rule's field_update wrote in the same dispatch, in the persisted space-form.
		s := NewWorkflowTimerScheduler(q, &fakeTimerCalendars{}, recs(map[string]any{"follow_up_at": "2026-07-10 08:00:00"}))
		wf := timerWorkflow(workflow.TimerConfig{
			DateField: "follow_up_at", Offset: workflow.TimerOffset{Amount: 2, Unit: "hours"}, Direction: "before",
		})
		if err := s.ScheduleTimer(context.Background(), wf, evt(map[string]any{"stage": "Pending"})); err != nil {
			t.Fatalf("ScheduleTimer: %v", err)
		}
		want := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
		if len(q.jobs) != 1 || !q.jobs[0].RunAt.Equal(want) {
			t.Fatalf("jobs=%d, want 1 @ %v (anchor from the record the action just wrote)", len(q.jobs), want)
		}
		var p workflowTimerJobPayload
		if err := json.Unmarshal(q.jobs[0].Payload, &p); err != nil || p.DateValue != "2026-07-10 08:00:00" {
			t.Fatalf("payload = %+v (err %v), want dateValue from the record", p, err)
		}
	})

	t.Run("stale snapshot date loses to the record", func(t *testing.T) {
		// The event carries the OLD date (pre-action image); the record holds the moved one.
		q := &fakeTimerQueue{}
		s := NewWorkflowTimerScheduler(q, &fakeTimerCalendars{}, recs(map[string]any{"close_date": "2026-09-01"}))
		wf := timerWorkflow(workflow.TimerConfig{
			DateField: "close_date", Offset: workflow.TimerOffset{Amount: 1, Unit: "days"}, Direction: "before",
		})
		if err := s.ScheduleTimer(context.Background(), wf, evt(map[string]any{"close_date": "2026-08-01"})); err != nil {
			t.Fatalf("ScheduleTimer: %v", err)
		}
		want := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
		if len(q.jobs) != 1 || !q.jobs[0].RunAt.Equal(want) {
			t.Fatalf("jobs=%d, want 1 @ %v (record wins over the snapshot)", len(q.jobs), want)
		}
	})

	t.Run("record gone before park parks nothing, no error", func(t *testing.T) {
		q := &fakeTimerQueue{}
		s := NewWorkflowTimerScheduler(q, &fakeTimerCalendars{}, &fakeTimerRecords{}) // reader wired, row deleted
		wf := timerWorkflow(workflow.TimerConfig{
			DateField: "close_date", Offset: workflow.TimerOffset{Amount: 1, Unit: "days"}, Direction: "before",
		})
		if err := s.ScheduleTimer(context.Background(), wf, evt(map[string]any{"close_date": "2026-08-01"})); err != nil {
			t.Fatalf("ScheduleTimer: %v", err)
		}
		if len(q.jobs) != 0 {
			t.Fatalf("jobs = %d, want 0 (raced delete)", len(q.jobs))
		}
	})

	t.Run("no record reader falls back to the event snapshot", func(t *testing.T) {
		q := &fakeTimerQueue{}
		s := NewWorkflowTimerScheduler(q, &fakeTimerCalendars{}, nil)
		wf := timerWorkflow(workflow.TimerConfig{
			DateField: "close_date", Offset: workflow.TimerOffset{Amount: 1, Unit: "days"}, Direction: "before",
		})
		if err := s.ScheduleTimer(context.Background(), wf, evt(map[string]any{"close_date": "2026-08-01"})); err != nil {
			t.Fatalf("ScheduleTimer: %v", err)
		}
		want := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
		if len(q.jobs) != 1 || !q.jobs[0].RunAt.Equal(want) {
			t.Fatalf("jobs=%d, want 1 @ %v (snapshot fallback without a reader)", len(q.jobs), want)
		}
	})

	t.Run("no record reader with created_at is an error", func(t *testing.T) {
		q := &fakeTimerQueue{}
		s := NewWorkflowTimerScheduler(q, &fakeTimerCalendars{}, nil)
		wf := timerWorkflow(workflow.TimerConfig{
			DateField: "created_at", Offset: workflow.TimerOffset{Amount: 1, Unit: "hours"}, Direction: "after",
		})
		if err := s.ScheduleTimer(context.Background(), wf, evt(nil)); err == nil {
			t.Fatal("want error: created_at needs the record and no reader is wired")
		}
		if len(q.jobs) != 0 {
			t.Fatalf("jobs = %d, want 0", len(q.jobs))
		}
	})
}

// ---- consumer (workflow_timer job handler) -----------------------------------------------------

// fakeTimerRules serves workflow rows by id (tenant-pinned).
type fakeTimerRules struct {
	rows map[string]*workflow.WorkflowRecord
	err  error
}

func (f *fakeTimerRules) Get(_ context.Context, tenantID, id string) (*workflow.WorkflowRecord, error) {
	if f.err != nil {
		return nil, f.err
	}
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return nil, workflow.ErrWorkflowNotFound
	}
	return r, nil
}

// countingMutator counts field_update executions (implements workflow.RecordMutator).
type countingMutator struct{ n atomic.Int64 }

func (m *countingMutator) UpdateField(context.Context, string, string, string, string, string, any) error {
	m.n.Add(1)
	return nil
}

// timerRuleRecord persists a date_offset rule row the way the repo would (condition JSON with
// timer + optional rules; actions JSON).
func timerRuleRecord(t *testing.T, active bool, rules []workflow.Rule) *workflow.WorkflowRecord {
	t.Helper()
	cond := workflow.Condition{
		Match: "and",
		Rules: rules,
		Timer: &workflow.TimerConfig{
			DateField: "close_date", Offset: workflow.TimerOffset{Amount: 3, Unit: "days"}, Direction: "before",
		},
	}
	condJSON, err := json.Marshal(cond)
	if err != nil {
		t.Fatalf("marshal cond: %v", err)
	}
	actionsJSON, err := json.Marshal([]workflow.Action{{Type: workflow.ActionFieldUpdate, Field: "stage", Value: "reminded"}})
	if err != nil {
		t.Fatalf("marshal actions: %v", err)
	}
	return &workflow.WorkflowRecord{
		ID: "wf-1", TenantID: "t1", Name: "remind", EntityName: "deals",
		Trigger: string(workflow.TriggerDateOffset), Condition: condJSON, Actions: actionsJSON,
		IsActive: active,
	}
}

// timerJob builds the parked job the producer would have enqueued.
func timerJob(t *testing.T, dateValue string) jobs.Job {
	t.Helper()
	payload, err := json.Marshal(workflowTimerJobPayload{
		WorkflowID: "wf-1", EntityName: "deals", RecordID: "r1", DateValue: dateValue,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return jobs.Job{ID: "job-1", TenantID: "t1", Type: WorkflowTimerJobType, Payload: payload}
}

// timerEngine builds a real dispatcher over a counting mutator — FireWorkflow then exercises the
// EXACT shared per-rule body (evaluate → actions) the async path runs.
func timerEngine(mut *countingMutator) *workflow.AsyncDispatcher {
	return workflow.NewAsyncDispatcher(workflow.DispatcherConfig{Buffer: 1, Workers: 1}, nil, mut, nil, nil)
}

func TestWorkflowTimerJobHandler(t *testing.T) {
	liveRecord := &dynrepo.Record{
		ID: "r1", TenantID: "t1", CreatedBy: "owner-1",
		Data: map[string]any{"close_date": "2026-08-01", "stage": "new"},
	}

	tests := []struct {
		name      string
		rules     *fakeTimerRules
		records   *fakeTimerRecords
		job       func(t *testing.T) jobs.Job
		wantErr   bool
		wantFires int64
	}{
		{
			name:      "condition holds → actions run",
			rules:     &fakeTimerRules{rows: map[string]*workflow.WorkflowRecord{"wf-1": timerRuleRecord(t, true, nil)}},
			records:   &fakeTimerRecords{rows: map[string]*dynrepo.Record{"r1": liveRecord}},
			job:       func(t *testing.T) jobs.Job { return timerJob(t, "2026-08-01") },
			wantFires: 1,
		},
		{
			name: "condition fails → evaluated but no action",
			rules: &fakeTimerRules{rows: map[string]*workflow.WorkflowRecord{
				"wf-1": timerRuleRecord(t, true, []workflow.Rule{{Field: "stage", Op: "eq", Value: "won"}}),
			}},
			records:   &fakeTimerRecords{rows: map[string]*dynrepo.Record{"r1": liveRecord}},
			job:       func(t *testing.T) jobs.Job { return timerJob(t, "2026-08-01") },
			wantFires: 0,
		},
		{
			name:      "record deleted → no-op success",
			rules:     &fakeTimerRules{rows: map[string]*workflow.WorkflowRecord{"wf-1": timerRuleRecord(t, true, nil)}},
			records:   &fakeTimerRecords{rows: map[string]*dynrepo.Record{}},
			job:       func(t *testing.T) jobs.Job { return timerJob(t, "2026-08-01") },
			wantFires: 0,
		},
		{
			name:      "rule deleted → no-op success",
			rules:     &fakeTimerRules{rows: map[string]*workflow.WorkflowRecord{}},
			records:   &fakeTimerRecords{rows: map[string]*dynrepo.Record{"r1": liveRecord}},
			job:       func(t *testing.T) jobs.Job { return timerJob(t, "2026-08-01") },
			wantFires: 0,
		},
		{
			name:      "rule deactivated → no-op success",
			rules:     &fakeTimerRules{rows: map[string]*workflow.WorkflowRecord{"wf-1": timerRuleRecord(t, false, nil)}},
			records:   &fakeTimerRecords{rows: map[string]*dynrepo.Record{"r1": liveRecord}},
			job:       func(t *testing.T) jobs.Job { return timerJob(t, "2026-08-01") },
			wantFires: 0,
		},
		{
			name:      "date moved → skip (replacement wake-up owns the new instant)",
			rules:     &fakeTimerRules{rows: map[string]*workflow.WorkflowRecord{"wf-1": timerRuleRecord(t, true, nil)}},
			records:   &fakeTimerRecords{rows: map[string]*dynrepo.Record{"r1": liveRecord}},
			job:       func(t *testing.T) jobs.Job { return timerJob(t, "2026-07-15") }, // parked from an older date
			wantFires: 0,
		},
		{
			name:    "malformed payload → error (burns attempts, lands failed)",
			rules:   &fakeTimerRules{rows: map[string]*workflow.WorkflowRecord{}},
			records: &fakeTimerRecords{},
			job: func(t *testing.T) jobs.Job {
				return jobs.Job{ID: "job-x", TenantID: "t1", Type: WorkflowTimerJobType, Payload: json.RawMessage(`{"workflowId":`)}
			},
			wantErr: true,
		},
		{
			name:    "rule load infra error → error (queue retries)",
			rules:   &fakeTimerRules{err: errors.New("db down")},
			records: &fakeTimerRecords{},
			job:     func(t *testing.T) jobs.Job { return timerJob(t, "2026-08-01") },
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mut := &countingMutator{}
			h := NewWorkflowTimerJobHandler(tt.rules, tt.records, timerEngine(mut), nil, nil)
			err := h(context.Background(), tt.job(t))
			if tt.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got := mut.n.Load(); got != tt.wantFires {
				t.Fatalf("field_update ran %d times, want %d", got, tt.wantFires)
			}
		})
	}
}

// DAILY-QUEUE #287: "no change for 14 days → remind". The wake-up fires only while the record still
// carries the updated_at stamp it was parked for AND still matches the rule's condition; an edit in
// between (new stamp) or a record that moved on (condition false) makes it a silent no-op.
func TestWorkflowTimerJobHandler_UpdatedAtInactivity(t *testing.T) {
	stamp := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	_, stampCanonical, _ := workflow.ParseTimerDate(stamp)
	rule := func(rules []workflow.Rule) *fakeTimerRules {
		rec := timerRuleRecord(t, true, rules)
		cond := workflow.Condition{Match: "and", Rules: rules, Timer: &workflow.TimerConfig{
			DateField: "updated_at", Offset: workflow.TimerOffset{Amount: 14, Unit: "days"}, Direction: "after",
		}}
		raw, err := json.Marshal(cond)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rec.Condition = raw
		return &fakeTimerRules{rows: map[string]*workflow.WorkflowRecord{"wf-1": rec}}
	}
	tests := []struct {
		name      string
		updatedAt time.Time
		rules     []workflow.Rule
		wantFires int64
	}{
		{name: "untouched since park → fires once", updatedAt: stamp, wantFires: 1},
		{name: "edited since park → skipped", updatedAt: stamp.Add(time.Hour), wantFires: 0},
		{name: "untouched but no longer matches the condition → skipped", updatedAt: stamp,
			rules: []workflow.Rule{{Field: "stage", Op: "eq", Value: "won"}}, wantFires: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recs := &fakeTimerRecords{rows: map[string]*dynrepo.Record{
				"r1": {ID: "r1", TenantID: "t1", UpdatedAt: tt.updatedAt, Data: map[string]any{"stage": "new"}},
			}}
			mut := &countingMutator{}
			h := NewWorkflowTimerJobHandler(rule(tt.rules), recs, timerEngine(mut), nil, nil)
			if err := h(context.Background(), timerJob(t, stampCanonical)); err != nil {
				t.Fatalf("handler: %v", err)
			}
			if got := mut.n.Load(); got != tt.wantFires {
				t.Fatalf("field_update ran %d times, want %d", got, tt.wantFires)
			}
		})
	}
}

// DAILY-QUEUE #105 (b): a business_hours wake-up parked on the wall-clock fallback leaves a trace
// the admin can read — the run the wake-up becomes carries the warning on every action it ran
// (the whole run happened at a wall-clock instant). A normal wake-up's run stays clean.
func TestWorkflowTimerJobHandler_WallClockWakeUpSaysSoInRunHistory(t *testing.T) {
	var repeatWallClock bool
	liveRecord := &dynrepo.Record{ID: "r1", TenantID: "t1", Data: map[string]any{"close_date": "2026-08-01", "stage": "new"}}
	job := func(t *testing.T, wallClock bool, calendarID string) (jobs.Job, *fakeTimerRules) {
		t.Helper()
		rule := timerRuleRecord(t, true, nil)
		var cond workflow.Condition
		if err := json.Unmarshal(rule.Condition, &cond); err != nil {
			t.Fatalf("condition: %v", err)
		}
		cond.Timer.Offset = workflow.TimerOffset{Amount: 4, Unit: workflow.TimerUnitBusinessHours}
		cond.Timer.Direction = workflow.TimerDirectionAfter
		cond.Timer.CalendarID = calendarID
		raw, err := json.Marshal(cond)
		if err != nil {
			t.Fatalf("condition: %v", err)
		}
		rule.Condition = raw
		payload, err := json.Marshal(workflowTimerJobPayload{
			WorkflowID: "wf-1", EntityName: "deals", RecordID: "r1", DateValue: "2026-08-01", WallClock: wallClock, RepeatWallClock: repeatWallClock,
		})
		if err != nil {
			t.Fatalf("payload: %v", err)
		}
		return jobs.Job{ID: "job-1", TenantID: "t1", Type: WorkflowTimerJobType, Payload: payload},
			&fakeTimerRules{rows: map[string]*workflow.WorkflowRecord{"wf-1": rule}}
	}
	fire := func(t *testing.T, j jobs.Job, rules *fakeTimerRules) []workflow.ActionResult {
		t.Helper()
		log := &runLog{}
		h := NewWorkflowTimerJobHandler(rules, &fakeTimerRecords{rows: map[string]*dynrepo.Record{"r1": liveRecord}},
			timerEngine(&countingMutator{}).WithRunRecorder(log), nil, nil)
		if err := h(context.Background(), j); err != nil {
			t.Fatalf("handler: %v", err)
		}
		if len(log.runs) != 1 || log.runs[0].Status != workflow.RunSuccess {
			t.Fatalf("runs = %+v; want one success", log.runs)
		}
		var results []workflow.ActionResult
		if err := json.Unmarshal(log.runs[0].ActionsResult, &results); err != nil || len(results) != 1 {
			t.Fatalf("actions_result = %s (err %v)", log.runs[0].ActionsResult, err)
		}
		return results
	}

	t.Run("parked on the default-calendar fallback", func(t *testing.T) {
		j, rules := job(t, true, "")
		if got := fire(t, j, rules)[0].Warning; got != workflow.TimerWallClockWarning {
			t.Fatalf("warning = %q; want %q", got, workflow.TimerWallClockWarning)
		}
	})
	t.Run("parked while its pinned calendar was gone", func(t *testing.T) {
		j, rules := job(t, true, "cal-gone")
		if got := fire(t, j, rules)[0].Warning; got != workflow.TimerPinnedCalendarWarning {
			t.Fatalf("warning = %q; want %q", got, workflow.TimerPinnedCalendarWarning)
		}
	})
	t.Run("parked on a calendar: no warning", func(t *testing.T) {
		j, rules := job(t, false, "")
		if got := fire(t, j, rules)[0].Warning; got != "" {
			t.Fatalf("warning = %q; want none", got)
		}
	})
	// #447: a repeat re-park is wall-clock by design; neither "no default calendar" nor "pinned
	// calendar gone" is true for it, so it gets its own sentence.
	t.Run("repeat re-park: its own truthful note", func(t *testing.T) {
		repeatWallClock = true
		defer func() { repeatWallClock = false }()
		for _, cal := range []string{"", "cal-pinned"} {
			j, rules := job(t, false, cal)
			if got := fire(t, j, rules)[0].Warning; got != workflow.TimerRepeatWallClockWarning {
				t.Fatalf("calendar %q: warning = %q; want %q", cal, got, workflow.TimerRepeatWallClockWarning)
			}
		}
	})
}

// TestWorkflowTimer_DatetimeScheduleFireRoundTrip proves the datetime-field regression fix end to
// end. A datetime value persists as the MySQL space-form ("2006-01-02 15:04:05", UTC — see
// record_service.normalizeForStore/canonicalDatetimeUTC), so BOTH the scheduler (evt.Data) and the
// fire-time handler (record re-read) see that exact string. Before the ParseTimerDate space-form
// fix the producer parked NOTHING (parse failed → no wake-up) and, even if a job existed, the
// handler skipped it as "date moved" (parse failed → ok=false). Now: the job parks, and the SAME
// parked job driven straight into the handler FIRES — parked DateValue == re-parsed current value.
func TestWorkflowTimer_DatetimeScheduleFireRoundTrip(t *testing.T) {
	const dueDate = "2026-07-10 08:00:00" // persisted space-form, in the PAST

	// --- schedule side: the producer must PARK a wake-up (the schedule-time half of the bug) ---
	// The record on disk is what BOTH halves read: the producer's anchor read-back and the
	// handler's fire-time premise check — the same persisted space-form string.
	records := &fakeTimerRecords{rows: map[string]*dynrepo.Record{
		"r1": {ID: "r1", TenantID: "t1", CreatedBy: "owner-1", Data: map[string]any{"follow_up_at": dueDate, "stage": "new"}},
	}}
	q := &fakeTimerQueue{}
	s := NewWorkflowTimerScheduler(q, &fakeTimerCalendars{}, records)
	wf := timerWorkflow(workflow.TimerConfig{
		DateField: "follow_up_at", Offset: workflow.TimerOffset{Amount: 1, Unit: "hours"}, Direction: "after",
	})
	evt := workflow.Event{
		TenantID: "t1", EntityName: "deals", RecordID: "r1",
		Trigger: workflow.TriggerAfterCreate, Data: map[string]any{"follow_up_at": dueDate},
	}
	if err := s.ScheduleTimer(context.Background(), wf, evt); err != nil {
		t.Fatalf("ScheduleTimer: %v", err)
	}
	if len(q.jobs) != 1 {
		t.Fatalf("jobs = %d, want 1 (datetime space-form must park a wake-up)", len(q.jobs))
	}
	var p workflowTimerJobPayload
	if err := json.Unmarshal(q.jobs[0].Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.DateValue != dueDate {
		t.Fatalf("parked DateValue = %q, want %q (raw persisted space-form)", p.DateValue, dueDate)
	}

	// --- fire side: the handler re-reads the SAME space-form and must NOT skip as "date moved" ---
	condJSON, err := json.Marshal(workflow.Condition{
		Match: "and",
		Timer: &workflow.TimerConfig{
			DateField: "follow_up_at", Offset: workflow.TimerOffset{Amount: 1, Unit: "hours"}, Direction: "after",
		},
	})
	if err != nil {
		t.Fatalf("marshal cond: %v", err)
	}
	actionsJSON, err := json.Marshal([]workflow.Action{{Type: workflow.ActionFieldUpdate, Field: "stage", Value: "reminded"}})
	if err != nil {
		t.Fatalf("marshal actions: %v", err)
	}
	rules := &fakeTimerRules{rows: map[string]*workflow.WorkflowRecord{"wf-1": {
		ID: "wf-1", TenantID: "t1", Name: "remind", EntityName: "deals",
		Trigger: string(workflow.TriggerDateOffset), Condition: condJSON, Actions: actionsJSON, IsActive: true,
	}}}
	mut := &countingMutator{}
	h := NewWorkflowTimerJobHandler(rules, records, timerEngine(mut), nil, nil)
	// Drive the EXACT parked job into the handler — schedule↔fire consistency by construction.
	if err := h(context.Background(), q.jobs[0]); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if mut.n.Load() != 1 {
		t.Fatalf("fires = %d, want 1 (datetime timer must FIRE, not skip as date-moved)", mut.n.Load())
	}
}

// TestWorkflowTimerJobHandler_Race drives the handler concurrently (the queue is at-least-once;
// a reaped job can re-run while its sibling wake-up fires) — the -race gate proves the handler
// and the shared dispatcher body are data-race free.
func TestWorkflowTimerJobHandler_Race(t *testing.T) {
	rules := &fakeTimerRules{rows: map[string]*workflow.WorkflowRecord{"wf-1": timerRuleRecord(t, true, nil)}}
	records := &fakeTimerRecords{rows: map[string]*dynrepo.Record{
		"r1": {ID: "r1", TenantID: "t1", CreatedBy: "owner-1", Data: map[string]any{"close_date": "2026-08-01"}},
	}}
	mut := &countingMutator{}
	h := NewWorkflowTimerJobHandler(rules, records, timerEngine(mut), nil, nil)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h(context.Background(), timerJob(t, "2026-08-01")); err != nil {
				t.Errorf("handler: %v", err)
			}
		}()
	}
	wg.Wait()
	if mut.n.Load() != 16 {
		t.Fatalf("fires = %d, want 16 (every wake-up fired; dedup lives at enqueue)", mut.n.Load())
	}
}

// SetOwner satisfies workflow.RecordMutator — assign now grants ownership alongside the field
// write. This timer test drives field_update only, so it is a no-op.
func (m *countingMutator) SetOwner(_ context.Context, _, _, _, _, _ string) error { return nil }

// RelatedEntity satisfies workflow.RecordMutator (specs/058); the timer tests never follow a relation.
func (m *countingMutator) RelatedEntity(_ context.Context, _, _, _ string) (string, error) {
	return "", nil
}

func (m *countingMutator) ReadForUpdate(context.Context, string, string, string) (map[string]any, int, error) {
	return map[string]any{}, 0, nil
}

func (m *countingMutator) UpdateFieldsIfVersion(context.Context, string, string, string, string, map[string]any, int) error {
	m.n.Add(1)
	return nil
}

// snapshotClockRecords records the instant the handler reads the record, then holds for a few ms
// (the read itself plus the re-park before the fire).
type snapshotClockRecords struct {
	fakeTimerRecords
	readAt time.Time
}

func (f *snapshotClockRecords) GetRecord(ctx context.Context, entity, tenantID, id string, scope dynrepo.RowScope) (*dynrepo.Record, error) {
	f.readAt = time.Now()
	time.Sleep(3 * time.Millisecond)
	return f.fakeTimerRecords.GetRecord(ctx, entity, tenantID, id, scope)
}

// capturingFirer records the event the handler fires.
type capturingFirer struct{ evts []workflow.Event }

func (c *capturingFirer) FireWorkflow(_ context.Context, _ workflow.Workflow, evt workflow.Event) error {
	c.evts = append(c.evts, evt)
	return nil
}

// TestWorkflowTimerJobHandler_OccurredAtPrecedesRecordRead (review of #26): the fired event's Data
// is the record as GetRecord read it, so its occurredAt — what an outbound_webhook body carries,
// and what a receiver keeps the largest of per recordId — must not be later than that read. An
// unstamped event took the marshal time instead, AFTER the read and the re-park, so an edit
// committed in between (Emit-stamped earlier) lost to this stale copy.
func TestWorkflowTimerJobHandler_OccurredAtPrecedesRecordRead(t *testing.T) {
	rules := &fakeTimerRules{rows: map[string]*workflow.WorkflowRecord{"wf-1": timerRuleRecord(t, true, nil)}}
	records := &snapshotClockRecords{fakeTimerRecords: fakeTimerRecords{rows: map[string]*dynrepo.Record{
		"r1": {ID: "r1", TenantID: "t1", CreatedBy: "owner-1", Data: map[string]any{"close_date": "2026-08-01"}},
	}}}
	firer := &capturingFirer{}
	h := NewWorkflowTimerJobHandler(rules, records, firer, nil, nil)

	if err := h(context.Background(), timerJob(t, "2026-08-01")); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(firer.evts) != 1 {
		t.Fatalf("fired %d events, want 1", len(firer.evts))
	}
	at := firer.evts[0].OccurredAt
	if at.IsZero() || at.After(records.readAt) {
		t.Fatalf("occurredAt = %s, want a stamp <= the record read at %s (zero = marshal time, after the read)", at, records.readAt)
	}
}
