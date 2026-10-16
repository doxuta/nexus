package services

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestDefaultCalendarHoursJSON_ParsesAndComputes is validator parity BY CONSTRUCTION for the seed.
// SeedDefaultBusinessCalendar writes its JSON with a raw INSERT, bypassing validateCalendarInput —
// so nothing else would catch a typo'd weekday key or a malformed interval. The row would land,
// calendar.FromJSON would fail at read time, the resolver treats an unparseable calendar as NO
// calendar, and every business_hours deadline in that tenant would quietly become wall-clock: the
// exact failure the seed exists to prevent, with no error anywhere.
//
// It also pins the Q9 contract the queue states in user terms: a ticket raised 15:00 Friday with a
// +4 business-hour SLA is due 10:00 Monday, not 19:00 Friday.
func TestDefaultCalendarHoursJSON_ParsesAndComputes(t *testing.T) {
	sched, err := DefaultCalendarSchedule()
	if err != nil {
		t.Fatalf("the seeded calendar does not parse: %v", err)
	}
	if !sched.HasWorkingHours() {
		t.Fatal("the seeded calendar has no working hours — every deadline would fail with ErrNoWorkingTime")
	}
	loc := sched.Location()
	if loc.String() != DefaultCalendarTimezone {
		t.Fatalf("timezone = %s, want %s", loc, DefaultCalendarTimezone)
	}

	friday := time.Date(2026, 7, 10, 15, 0, 0, 0, loc) // Friday 15:00 ICT, inside working hours
	got, err := sched.AddBusinessDuration(friday, 4*time.Hour)
	if err != nil {
		t.Fatalf("AddBusinessDuration: %v", err)
	}
	want := time.Date(2026, 7, 13, 10, 0, 0, 0, loc) // Monday 10:00 ICT
	if !got.Equal(want) {
		t.Fatalf("15:00 Friday + 4 business hours = %s, want %s (the weekend must not count)",
			got.In(loc).Format(time.RFC3339), want.Format(time.RFC3339))
	}

	// Saturday is a non-working day: the walk must roll forward to Monday's opening, not add
	// wall-clock hours on a day nobody is in.
	saturday := time.Date(2026, 7, 11, 9, 0, 0, 0, loc)
	got, err = sched.AddBusinessDuration(saturday, 4*time.Hour)
	if err != nil {
		t.Fatalf("AddBusinessDuration (weekend): %v", err)
	}
	if want := time.Date(2026, 7, 13, 12, 0, 0, 0, loc); !got.Equal(want) {
		t.Fatalf("Saturday 09:00 + 4 business hours = %s, want %s", got.In(loc).Format(time.RFC3339), want.Format(time.RFC3339))
	}

	// A working day with no weekend to cross must still behave like plain hours.
	tuesday := time.Date(2026, 7, 14, 9, 0, 0, 0, loc)
	got, err = sched.AddBusinessDuration(tuesday, 4*time.Hour)
	if err != nil {
		t.Fatalf("AddBusinessDuration (midweek): %v", err)
	}
	if want := time.Date(2026, 7, 14, 13, 0, 0, 0, loc); !got.Equal(want) {
		t.Fatalf("Tuesday 09:00 + 4 business hours = %s, want %s (no lunch break in v1)", got.In(loc).Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestShippedBinariesEmbedTimezoneDatabase is a STRUCTURAL gate, and it is structural on purpose:
// nothing this test could do at runtime would fail on a developer machine.
//
// The seed hardcodes an IANA timezone (DefaultCalendarTimezone) and every business_hours deadline
// resolves it through time.LoadLocation. macOS and the CI runner both ship /usr/share/zoneinfo, so
// the parse tests above pass there no matter what. The RUNTIME IMAGE does not: Dockerfile stage 2
// is alpine with ca-certificates only, and the binaries are CGO_ENABLED=0. Verified by running that
// exact image shape — `unknown time zone Asia/Ho_Chi_Minh` without the import, fine with it.
//
// The consequence of losing it is the worst shape available: calendar.FromJSON fails →
// WorkflowScheduleResolver reports "no calendar" → the deadline silently becomes wall-clock and the
// log says "no business calendar for this tenant" while the admin is looking at the calendar in the
// UI. Wrong AND misdiagnosed, in production only, with every gate green.
//
// So the gate asserts the import exists in the source of both shipped binaries. Blunt, but it fails
// in the one place a reviewer would look.
func TestShippedBinariesEmbedTimezoneDatabase(t *testing.T) {
	for _, main := range []string{"../../cmd/server/main.go", "../../cmd/bootstrap-admin/main.go"} {
		src, err := os.ReadFile(main)
		if err != nil {
			t.Fatalf("read %s: %v", main, err)
		}
		if !strings.Contains(string(src), `_ "time/tzdata"`) {
			t.Errorf(`%s does not import _ "time/tzdata".
The runtime image (alpine, no tzdata) cannot load %q without it, and the failure is silent:
business_hours deadlines degrade to wall-clock while the log blames a missing calendar.`,
				main, DefaultCalendarTimezone)
		}
	}
}

// TestResolveSchedule_UnwiredStoreErrors pins the difference the whole slice rests on. A typed-nil
// *WorkflowScheduleResolver passed to WithScheduleResolver makes actionDeps.sched NON-nil, so
// runSetDeadline's "no resolver wired" guard never fires; if ResolveSchedule then answered
// (nil, nil) the deadline would take the silent wall-clock arm — the exact ambiguity this slice
// removed, reintroduced one layer down where nothing looks at it.
func TestResolveSchedule_UnwiredStoreErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    *WorkflowScheduleResolver
	}{
		{"typed-nil resolver", nil},
		{"resolver with no store", NewWorkflowScheduleResolver(nil, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sched, err := tc.r.ResolveSchedule(context.Background(), "t1", "")
			if err == nil {
				t.Fatal("want an error for an unwired calendar store; (nil, nil) would be read as \"this tenant has no calendar\" and stamp wall-clock silently")
			}
			if sched != nil {
				t.Fatalf("got a schedule %v from an unwired store", sched)
			}
		})
	}
}
