package services

// specs/043 S1 (#39) — the calendar-backed approval deadline (bpm.DeadlineCalculator) over the SAME
// pinned-then-default lookup the workflow timer and set_deadline resolve with.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/calmeta"
	"nexus/platform/internal/workflow"
)

// vnDefaultCalendar is the Q9 default: Mon–Fri 08:00–17:00 Asia/Ho_Chi_Minh.
func vnDefaultCalendar() *calmeta.BusinessCalendar {
	return &calmeta.BusinessCalendar{
		ID: "cal-vn", TenantID: "t1", Timezone: "Asia/Ho_Chi_Minh", IsDefault: true,
		Hours: json.RawMessage(`{"mon":[{"start":"08:00","end":"17:00"}],"tue":[{"start":"08:00","end":"17:00"}],
			"wed":[{"start":"08:00","end":"17:00"}],"thu":[{"start":"08:00","end":"17:00"}],
			"fri":[{"start":"08:00","end":"17:00"}]}`),
	}
}

// failingCalendars is a calendar store that cannot be read.
type failingCalendars struct{ err error }

func (f failingCalendars) GetByID(context.Context, string, string) (*calmeta.BusinessCalendar, error) {
	return nil, f.err
}

func (f failingCalendars) GetDefault(context.Context, string) (*calmeta.BusinessCalendar, error) {
	return nil, f.err
}

func mustCalc(t *testing.T, store timerCalendarReader) *BPMDeadlineCalculator {
	t.Helper()
	c, err := NewBPMDeadlineCalculator(NewWorkflowScheduleResolver(store, nil), nil)
	if err != nil {
		t.Fatalf("NewBPMDeadlineCalculator: %v", err)
	}
	return c
}

func TestBPMDeadline_DueAt(t *testing.T) {
	ict, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Fatal(err)
	}
	fri1600 := time.Date(2026, 10, 2, 16, 0, 0, 0, ict)
	oneMinuteAWeek := &calmeta.BusinessCalendar{
		ID: "cal-tiny", TenantID: "t1", Timezone: "UTC", IsDefault: true,
		Hours: json.RawMessage(`{"mon":[{"start":"08:00","end":"08:01"}]}`),
	}
	cases := []struct {
		name      string
		store     timerCalendarReader
		d         time.Duration
		wantDue   time.Time
		wantBasis bpm.SLABasis
	}{
		{"default calendar: 16:00 Fri + 4h = 11:00 Mon", &fakeTimerCalendars{def: vnDefaultCalendar()}, 4 * time.Hour,
			time.Date(2026, 10, 5, 11, 0, 0, 0, ict), bpm.SLABasisBusiness},
		{"no calendar: wall clock, and says so", &fakeTimerCalendars{}, 4 * time.Hour,
			time.Date(2026, 10, 2, 20, 0, 0, 0, ict), bpm.SLABasisWallNoCalendar},
		// A calendar that exists but cannot hold the duration inside the walk cap (#80): a temporary
		// wall-clock number under the calendar-error label — never "no calendar", never a hang.
		{"no reachable working time: wall clock, calendar-error basis", &fakeTimerCalendars{def: oneMinuteAWeek}, 1000 * time.Minute,
			fri1600.Add(1000 * time.Minute), bpm.SLABasisWallCalendarError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			due, basis, err := mustCalc(t, tc.store).DueAt(context.Background(), "t1", "", fri1600, tc.d)
			if err != nil {
				t.Fatalf("DueAt err = %v", err)
			}
			if !due.Equal(tc.wantDue) || basis != tc.wantBasis {
				t.Fatalf("DueAt = %s %q; want %s %q", due.In(ict).Format("Mon 02/01 15:04"), basis,
					tc.wantDue.In(ict).Format("Mon 02/01 15:04"), tc.wantBasis)
			}
		})
	}
}

// T020 (services half) — a store that cannot be read is an ERROR, not "no calendar": the engine
// fails a person's action with it and labels the async fallback wall_calendar_error.
func TestBPMDeadline_StoreErrorIsAnError(t *testing.T) {
	down := errors.New("connection refused")
	_, basis, err := mustCalc(t, failingCalendars{err: down}).DueAt(context.Background(), "t1", "", time.Now(), time.Hour)
	if !errors.Is(err, down) {
		t.Fatalf("err = %v (basis %q); want the store error, never a wall-clock answer", err, basis)
	}
}

// T021 (services half) — the calculator refuses to be built over nothing, so main cannot wire a
// calculator that answers "no calendar" for every tenant because nobody gave it the store.
func TestNewBPMDeadlineCalculator_UnwiredResolverRefused(t *testing.T) {
	var typedNil *WorkflowScheduleResolver
	for name, r := range map[string]workflow.ScheduleResolver{
		"nil":             nil,
		"typed nil":       typedNil,
		"resolver, no db": NewWorkflowScheduleResolver(nil, nil),
	} {
		if _, err := NewBPMDeadlineCalculator(r, nil); err == nil {
			t.Errorf("%s: want a construction error", name)
		}
	}
}
