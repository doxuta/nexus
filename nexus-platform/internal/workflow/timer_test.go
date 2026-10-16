package workflow

import (
	"context"
	"sync"
	"testing"
	"time"

	"nexus/platform/internal/calendar"
)

// mustSchedule builds a mon–fri 08:00–17:00 Asia/Ho_Chi_Minh schedule (no holidays).
func mustSchedule(t *testing.T) *calendar.Schedule {
	t.Helper()
	hours := map[string][]calendar.Interval{
		"mon": {{Start: "08:00", End: "17:00"}},
		"tue": {{Start: "08:00", End: "17:00"}},
		"wed": {{Start: "08:00", End: "17:00"}},
		"thu": {{Start: "08:00", End: "17:00"}},
		"fri": {{Start: "08:00", End: "17:00"}},
	}
	s, err := calendar.New(calendar.Config{Timezone: "Asia/Ho_Chi_Minh", Hours: hours})
	if err != nil {
		t.Fatalf("calendar.New: %v", err)
	}
	return s
}

func TestParseTimerDate(t *testing.T) {
	ts := time.Date(2026, 7, 10, 8, 30, 0, 123e6, time.UTC)
	tests := []struct {
		name          string
		in            any
		wantOK        bool
		wantCanonical string
	}{
		{"time.Time (created_at)", ts, true, "2026-07-10T08:30:00.123Z"},
		{"zero time", time.Time{}, false, ""},
		{"datetime RFC3339", "2026-07-10T08:30:00Z", true, "2026-07-10T08:30:00Z"},
		// The persisted datetime shape (normalizeForStore → canonicalDatetimeUTC). Canonical is the
		// raw string so schedule↔fire re-reads compare byte-identically.
		{"datetime MySQL space-form", "2026-07-10 08:30:00", true, "2026-07-10 08:30:00"},
		{"date only", "2026-07-10", true, "2026-07-10"},
		{"empty string", "  ", false, ""},
		{"garbage string", "not-a-date", false, ""},
		{"nil", nil, false, ""},
		{"wrong type", 42.0, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			anchor, canonical, ok := ParseTimerDate(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if canonical != tt.wantCanonical {
				t.Fatalf("canonical = %q, want %q", canonical, tt.wantCanonical)
			}
			if ok && anchor.IsZero() {
				t.Fatalf("ok but zero anchor")
			}
		})
	}
}

// TestParseTimerDate_SpaceFormInstant pins the anchor instant for the persisted MySQL space-form:
// the engine stores datetime in UTC ("2006-01-02 15:04:05"), so ParseTimerDate must anchor it at
// that exact UTC instant (fed to ComputeFireAt at schedule time). The RFC3339 encoding of the same
// instant must yield an equal anchor — same moment, different input shape.
func TestParseTimerDate_SpaceFormInstant(t *testing.T) {
	anchor, canonical, ok := ParseTimerDate("2026-07-10 08:30:00")
	if !ok {
		t.Fatal("space-form datetime rejected")
	}
	want := time.Date(2026, 7, 10, 8, 30, 0, 0, time.UTC)
	if !anchor.Equal(want) {
		t.Fatalf("anchor = %v, want %v (UTC)", anchor, want)
	}
	if canonical != "2026-07-10 08:30:00" {
		t.Fatalf("canonical = %q, want raw space-form", canonical)
	}
	rfcAnchor, _, ok := ParseTimerDate("2026-07-10T08:30:00Z")
	if !ok || !rfcAnchor.Equal(anchor) {
		t.Fatalf("RFC3339 anchor %v != space-form anchor %v (same instant)", rfcAnchor, anchor)
	}
}

func TestComputeFireAt(t *testing.T) {
	// Anchor: Friday 2026-07-10 08:00 UTC = 15:00 ICT (inside the schedule's working window).
	anchor := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	sched := mustSchedule(t)

	tests := []struct {
		name    string
		cfg     TimerConfig
		sched   *calendar.Schedule
		want    time.Time
		wantErr bool
	}{
		{
			name: "30 minutes after (wall-clock)",
			cfg:  TimerConfig{Offset: TimerOffset{Amount: 30, Unit: TimerUnitMinutes}, Direction: TimerDirectionAfter},
			want: anchor.Add(30 * time.Minute),
		},
		{
			name: "2 hours before (wall-clock)",
			cfg:  TimerConfig{Offset: TimerOffset{Amount: 2, Unit: TimerUnitHours}, Direction: TimerDirectionBefore},
			want: anchor.Add(-2 * time.Hour),
		},
		{
			name: "3 days before (wall-clock 72h, calendar-blind)",
			cfg:  TimerConfig{Offset: TimerOffset{Amount: 3, Unit: TimerUnitDays}, Direction: TimerDirectionBefore},
			want: anchor.Add(-72 * time.Hour),
		},
		{
			name: "4 business hours after crosses the weekend",
			// Friday 15:00 ICT + 4 business hours: 2h left Friday (→17:00), remaining 2h resume
			// Monday 08:00 → fires Monday 10:00 ICT = 2026-07-13 03:00 UTC.
			cfg:   TimerConfig{Offset: TimerOffset{Amount: 4, Unit: TimerUnitBusinessHours}, Direction: TimerDirectionAfter},
			sched: sched,
			want:  time.Date(2026, 7, 13, 3, 0, 0, 0, time.UTC),
		},
		{
			name: "business hours with nil schedule falls back to wall-clock",
			cfg:  TimerConfig{Offset: TimerOffset{Amount: 4, Unit: TimerUnitBusinessHours}, Direction: TimerDirectionAfter},
			want: anchor.Add(4 * time.Hour),
		},
		{
			name:    "business hours before is rejected (v1)",
			cfg:     TimerConfig{Offset: TimerOffset{Amount: 1, Unit: TimerUnitBusinessHours}, Direction: TimerDirectionBefore},
			sched:   sched,
			wantErr: true,
		},
		{
			name:    "zero amount rejected",
			cfg:     TimerConfig{Offset: TimerOffset{Amount: 0, Unit: TimerUnitHours}, Direction: TimerDirectionAfter},
			wantErr: true,
		},
		{
			name:    "unknown unit rejected",
			cfg:     TimerConfig{Offset: TimerOffset{Amount: 1, Unit: "weeks"}, Direction: TimerDirectionAfter},
			wantErr: true,
		},
		{
			name:    "unknown direction rejected",
			cfg:     TimerConfig{Offset: TimerOffset{Amount: 1, Unit: TimerUnitHours}, Direction: "sideways"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ComputeFireAt(tt.cfg, anchor, tt.sched)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got fireAt=%v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ComputeFireAt: %v", err)
			}
			if !got.Equal(tt.want) {
				t.Fatalf("fireAt = %v, want %v", got, tt.want)
			}
			if got.Nanosecond() != 0 {
				t.Fatalf("fireAt not truncated to second: %v", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------
// Dispatcher scheduling seam: process() parks date_offset wake-ups via the TimerScheduler on
// after_create/after_update only, fail-open.
// ---------------------------------------------------------------------------------------------

// timerLoaderFake serves fixed rule lists per trigger.
type timerLoaderFake struct {
	byTrigger map[Trigger][]Workflow
}

func (f *timerLoaderFake) ListActive(_ context.Context, _, _ string, trigger Trigger) ([]Workflow, error) {
	return f.byTrigger[trigger], nil
}

// timerSchedFake records ScheduleTimer calls.
type timerSchedFake struct {
	mu    sync.Mutex
	calls []string // workflowID
}

func (f *timerSchedFake) ScheduleTimer(_ context.Context, wf Workflow, _ Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, wf.ID)
	return nil
}

func (f *timerSchedFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func TestDispatcherSchedulesDateOffsetTimers(t *testing.T) {
	timerRule := Workflow{
		ID: "wf-timer", TenantID: "t1", EntityName: "deals", Trigger: TriggerDateOffset,
		Condition: &Condition{Timer: &TimerConfig{
			DateField: "close_date",
			Offset:    TimerOffset{Amount: 1, Unit: TimerUnitDays},
			Direction: TimerDirectionBefore,
		}},
		Actions: []Action{{Type: ActionInAppNotification, UserID: "u1", Message: "m"}}, IsActive: true,
	}
	loader := &timerLoaderFake{byTrigger: map[Trigger][]Workflow{TriggerDateOffset: {timerRule}}}

	tests := []struct {
		name      string
		trigger   Trigger
		wantCalls int
	}{
		{"after_create schedules", TriggerAfterCreate, 1},
		{"after_update schedules", TriggerAfterUpdate, 1},
		{"after_delete does NOT schedule", TriggerAfterDelete, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sched := &timerSchedFake{}
			d := NewAsyncDispatcher(DispatcherConfig{Buffer: 8, Workers: 1}, loader, nil, nil, nil).
				WithTimerScheduler(sched)
			d.Emit(context.Background(), Event{
				TenantID: "t1", EntityName: "deals", RecordID: "r1", Trigger: tt.trigger,
				Data: map[string]any{"close_date": "2026-08-01"},
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := d.Shutdown(ctx); err != nil {
				t.Fatalf("drain: %v", err)
			}
			if got := sched.count(); got != tt.wantCalls {
				t.Fatalf("ScheduleTimer calls = %d, want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestDispatcherNoTimerSchedulerIsNoop(t *testing.T) {
	loader := &timerLoaderFake{byTrigger: map[Trigger][]Workflow{}}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 8, Workers: 1}, loader, nil, nil, nil)
	// No TimerScheduler wired — an after_create event must not panic or error.
	d.Emit(context.Background(), Event{TenantID: "t1", EntityName: "deals", Trigger: TriggerAfterCreate})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

func TestEvaluateIgnoresTimer(t *testing.T) {
	// A timer-only condition (no rules) matches everything — Timer is scheduling config, not a
	// predicate.
	cond := &Condition{Timer: &TimerConfig{DateField: "d", Offset: TimerOffset{Amount: 1, Unit: TimerUnitDays}, Direction: TimerDirectionAfter}}
	if !Evaluate(cond, map[string]any{"anything": 1}) {
		t.Fatal("timer-only condition must match all")
	}
}

// ---------------------------------------------------------------------------------------------
// Ordering pin (DAILY-QUEUE #6 — timer-anchor-current-record): process() runs the event's rules
// BEFORE it asks the TimerScheduler to park. A field_update in the same dispatch may write the
// very date field a date_offset rule anchors on (a machine-set due_date), and the scheduler reads
// the record back to pick that value up — which only works if the action loop already ran.
// Reorder those two and every machine-set deadline anchors on stale data again.
// ---------------------------------------------------------------------------------------------

// orderLog is a goroutine-safe list of the steps the fakes below record.
type orderLog struct {
	mu    sync.Mutex
	steps []string
}

func (l *orderLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = append(l.steps, s)
}

func (l *orderLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.steps...)
}

// orderMutator records field_update executions on the shared log (RecordMutator).
type orderMutator struct{ log *orderLog }

func (m *orderMutator) UpdateField(_ context.Context, _, _, _, _ string, field string, _ any) error {
	m.log.add("field_update:" + field)
	return nil
}

func (m *orderMutator) SetOwner(context.Context, string, string, string, string, string) error {
	return nil
}

func (m *orderMutator) RelatedEntity(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (m *orderMutator) ReadForUpdate(context.Context, string, string, string) (map[string]any, int, error) {
	return map[string]any{}, 0, nil
}

func (m *orderMutator) UpdateFieldsIfVersion(context.Context, string, string, string, string, map[string]any, int) error {
	return nil
}

// orderSched records ScheduleTimer calls on the shared log (TimerScheduler).
type orderSched struct{ log *orderLog }

func (s *orderSched) ScheduleTimer(_ context.Context, wf Workflow, _ Event) error {
	s.log.add("schedule_timer:" + wf.ID)
	return nil
}

func TestDispatcherSchedulesTimersAfterActions(t *testing.T) {
	setDue := Workflow{
		ID: "wf-set-due", TenantID: "t1", EntityName: "tickets", Trigger: TriggerAfterUpdate,
		Condition: &Condition{Match: "and", Rules: []Rule{{Field: "status", Op: "eq", Value: "Pending"}}},
		Actions:   []Action{{Type: ActionFieldUpdate, Field: "due_date", Value: "2026-12-01T10:00:00Z"}},
		IsActive:  true,
	}
	remind := Workflow{
		ID: "wf-remind", TenantID: "t1", EntityName: "tickets", Trigger: TriggerDateOffset,
		Condition: &Condition{Timer: &TimerConfig{
			DateField: "due_date",
			Offset:    TimerOffset{Amount: 2, Unit: TimerUnitHours},
			Direction: TimerDirectionBefore,
		}},
		Actions:  []Action{{Type: ActionInAppNotification, UserID: "$owner", Message: "m"}},
		IsActive: true,
	}
	loader := &timerLoaderFake{byTrigger: map[Trigger][]Workflow{
		TriggerAfterUpdate: {setDue},
		TriggerDateOffset:  {remind},
	}}

	log := &orderLog{}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 8, Workers: 1}, loader, &orderMutator{log: log}, nil, nil).
		WithTimerScheduler(&orderSched{log: log})
	d.Emit(context.Background(), Event{
		TenantID: "t1", EntityName: "tickets", RecordID: "r1", Trigger: TriggerAfterUpdate,
		Data: map[string]any{"status": "Pending"}, // no due_date yet — the action writes it
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	got := log.get()
	want := []string{"field_update:due_date", "schedule_timer:wf-remind"}
	if len(got) != len(want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("step %d = %q, want %q (actions must run before timers park)", i, got[i], want[i])
		}
	}
}
