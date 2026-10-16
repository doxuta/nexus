package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/calendar"
)

// DAILY-QUEUE #79 (`set-deadline-calendar-pin`): a set_deadline action may pin its own working
// calendar (Action.CalendarID) instead of the tenant default — the same pin a date_offset timer has
// carried since TIMER-1 (TimerConfig.CalendarID), resolved through the same ScheduleResolver.
//
// byIDResolver answers per calendar id, so the test can tell WHICH calendar the action walked from
// the instant it wrote — not just from which id it asked for.
type byIDResolver struct {
	byID map[string]*calendar.Schedule // "" = the tenant default
	ids  []string
}

func (r *byIDResolver) ResolveSchedule(_ context.Context, _ string, calendarID string) (*calendar.Schedule, error) {
	r.ids = append(r.ids, calendarID)
	return r.byID[calendarID], nil
}

func roundTheClock(t *testing.T) *calendar.Schedule {
	t.Helper()
	day := []calendar.Interval{{Start: "00:00", End: "24:00"}}
	s, err := calendar.New(calendar.Config{Timezone: "UTC", Hours: map[string][]calendar.Interval{
		"mon": day, "tue": day, "wed": day, "thu": day, "fri": day, "sat": day, "sun": day,
	}})
	if err != nil {
		t.Fatalf("calendar.New: %v", err)
	}
	return s
}

func TestSetDeadline_PinnedCalendar(t *testing.T) {
	office := mustSchedule(t) // Mon-Fri 08:00-17:00 ICT: 12 business hours ALWAYS cross a night
	pinnedWF := func(calendarID string) Workflow {
		wf := deadlineWF(12, TimerUnitBusinessHours)
		wf.Actions[0].CalendarID = calendarID
		return wf
	}

	t.Run("the pinned calendar is walked, not the tenant default", func(t *testing.T) {
		res := &byIDResolver{byID: map[string]*calendar.Schedule{"": office, "cal-247": roundTheClock(t)}}
		mut := &fakeMutator{}
		before := time.Now().UTC()
		results, err := runActions(context.Background(), pinnedWF("cal-247"), deadlineEvent(TriggerAfterCreate), actionDeps{mut: mut, sched: res})
		if err != nil {
			t.Fatalf("runActions: %v", err)
		}
		if len(res.ids) != 1 || res.ids[0] != "cal-247" {
			t.Fatalf("resolver asked for calendars %q, want exactly [cal-247]", res.ids)
		}
		if len(mut.calls) != 1 {
			t.Fatalf("got %d writes, want 1", len(mut.calls))
		}
		// On a 24/7 calendar 12 business hours are 12 wall hours; on the office week they are
		// never less than a night longer. Landing on now+12h is only possible through the pin.
		got := parseWritten(t, mut.calls[0].value)
		if d := got.Sub(before.Add(12 * time.Hour)); d < -5*time.Second || d > 5*time.Second {
			t.Fatalf("deadline %s is %s away from now+12h — the default calendar was walked, not the pin", got.Format(time.RFC3339), d)
		}
		if results[0].Warning != "" {
			t.Fatalf("a calendar-backed deadline carries warning %q, want none", results[0].Warning)
		}
	})

	t.Run("no pin still walks the tenant default", func(t *testing.T) {
		res := &byIDResolver{byID: map[string]*calendar.Schedule{"": office}}
		if _, err := runActions(context.Background(), pinnedWF(""), deadlineEvent(TriggerAfterCreate), actionDeps{mut: &fakeMutator{}, sched: res}); err != nil {
			t.Fatalf("runActions: %v", err)
		}
		if len(res.ids) != 1 || res.ids[0] != "" {
			t.Fatalf("resolver asked for calendars %q, want exactly the default (\"\")", res.ids)
		}
	})

	// A pin saved against a live calendar can outlive it (the calendar is deleted later). The
	// resolver then answers "no calendar" — the documented wall-clock fallback the pinned TIMER
	// takes too — but the run history must not tell the admin to set a DEFAULT calendar: the
	// default is not what this rule reads, and following that advice would change nothing.
	t.Run("a pin that no longer resolves falls back loudly, naming the pin", func(t *testing.T) {
		res := &byIDResolver{byID: map[string]*calendar.Schedule{"": office}}
		mut := &fakeMutator{}
		results, err := runActions(context.Background(), pinnedWF("cal-gone"), deadlineEvent(TriggerAfterCreate), actionDeps{mut: mut, sched: res})
		if err != nil {
			t.Fatalf("runActions: %v", err)
		}
		if len(mut.calls) != 1 {
			t.Fatalf("got %d writes, want the wall-clock deadline written anyway", len(mut.calls))
		}
		w := results[0].Warning
		if w == "" || w == wallClockFallbackWarning {
			t.Fatalf("warning = %q; want the pinned-calendar sentence, not the default-calendar one", w)
		}
		if !strings.Contains(w, "ghim") || !strings.Contains(w, "giờ treo tường") {
			t.Fatalf("warning %q must say the pinned calendar is unusable and the deadline is wall-clock", w)
		}
		// The way out must name the control that exists (Studio's "Lịch làm việc" dropdown on the
		// "Đặt hạn xử lý" action, whose first entry clears the pin) — not a generic "bỏ ghim".
		for _, want := range []string{"Lịch làm việc", "Đặt hạn xử lý", "Lịch mặc định của tổ chức"} {
			if !strings.Contains(w, want) {
				t.Fatalf("warning %q must name the Studio control that fixes it (%q)", w, want)
			}
		}
	})
}
