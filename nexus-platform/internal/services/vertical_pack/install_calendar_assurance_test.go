package vertical_pack

// DAILY-QUEUE #82 (`pack-calendar-assurance`) — a pack that ships business_hours rules must SAY so
// when it lands in a tenant with no usable default working calendar.
//
// Measured 2026-09-15: tenant `northwind` had 0 live calendars, SeedDefaultBusinessCalendar deliberately
// does not re-seed a tenant that ever had one, and an admin can reach the same state on any tenant
// by un-defaulting or deleting the default calendar. Installing Ticketing there "succeeded" and
// every SLA deadline quietly became wall-clock — the fallback is the documented CALENDAR-1
// contract, so nothing failed; nothing told the installer either. The install result is the one
// screen that person reads, so the warning belongs there.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/calendar"
	"nexus/platform/internal/services"
)

// fakePackScheduleResolver answers "which default calendar does this tenant have".
type fakePackScheduleResolver struct {
	sched *calendar.Schedule
	err   error
	calls int
}

func (f *fakePackScheduleResolver) ResolveSchedule(_ context.Context, _ string, _ string) (*calendar.Schedule, error) {
	f.calls++
	return f.sched, f.err
}

// businessHoursPack: one entity with a datetime deadline + one set_deadline rule in business hours.
func businessHoursPack(unit string) *Pack {
	active := true
	return &Pack{
		Entities: []PackEntity{
			{Name: "wf_tickets", Label: "Tickets", Fields: []PackField{
				{Name: "subject", Label: "Subject", FieldType: "text"},
				{Name: "due_date", Label: "Hạn", FieldType: "datetime"},
			}},
		},
		Workflows: []PackWorkflow{{
			Name:       "Đặt hạn SLA",
			EntityName: "wf_tickets",
			Trigger:    "after_create",
			Actions: []PackWorkflowAction{
				{Type: "set_deadline", Field: "due_date", Offset: &PackWorkflowTimerOffset{Amount: 4, Unit: unit}},
			},
			IsActive: &active,
		}},
	}
}

func calendarWarnings(res *InstallResult) []string {
	var out []string
	for _, w := range res.Warnings {
		if strings.Contains(w, "lịch làm việc") {
			out = append(out, w)
		}
	}
	return out
}

func TestInstall_WarnsWhenBusinessHoursRulesHaveNoCalendar(t *testing.T) {
	officeWeek, err := services.DefaultCalendarSchedule()
	if err != nil {
		t.Fatalf("DefaultCalendarSchedule: %v", err)
	}
	install := func(t *testing.T, pack *Pack, res *fakePackScheduleResolver) *InstallResult {
		t.Helper()
		meta := &fakeMintMeta{}
		svc := NewService(Deps{
			EntityReader:     meta,
			RoleReader:       fakeMintRoles{},
			EntityWriter:     meta,
			WorkflowWriter:   &fakeWorkflowWriter{},
			ScheduleResolver: res,
		})
		out, err := svc.Install(context.Background(), "tenant-1", "user-1", pack)
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		return out
	}

	t.Run("no default calendar: the install result says the deadlines will be wall-clock", func(t *testing.T) {
		out := install(t, businessHoursPack("business_hours"), &fakePackScheduleResolver{})
		got := calendarWarnings(out)
		if len(got) != 1 {
			t.Fatalf("warnings = %q; want exactly one naming the missing working calendar", out.Warnings)
		}
		// The installer has to be able to act on it: which rules, what happens, where to fix it.
		for _, want := range []string{"Đặt hạn SLA", "giờ treo tường", "Lịch làm việc"} {
			if !strings.Contains(got[0], want) {
				t.Errorf("warning %q does not mention %q", got[0], want)
			}
		}
	})

	t.Run("default calendar present: no warning", func(t *testing.T) {
		res := &fakePackScheduleResolver{sched: officeWeek}
		if got := calendarWarnings(install(t, businessHoursPack("business_hours"), res)); len(got) != 0 {
			t.Fatalf("warnings = %q; a tenant with a working calendar must not be told it has none", got)
		}
		if res.calls != 1 {
			t.Fatalf("resolver consulted %d times, want 1", res.calls)
		}
	})

	t.Run("pack without business_hours never asks", func(t *testing.T) {
		res := &fakePackScheduleResolver{}
		if got := calendarWarnings(install(t, businessHoursPack("hours"), res)); len(got) != 0 {
			t.Fatalf("warnings = %q; an `hours` rule does not depend on a calendar", got)
		}
		if res.calls != 0 {
			t.Fatalf("resolver consulted %d times for a pack with no business_hours rule, want 0", res.calls)
		}
	})

	// The timer branch of businessHoursRuleNames: a date_offset timer in business_hours walks the
	// default calendar only when it pins none of its own. The Ticketing pack's timers use `hours`, so
	// nothing else in this package reaches that branch — this subtest is what keeps the specs/037
	// §2.4 claim ("Pass 8a names unpinned business_hours timers too") true.
	t.Run("business_hours timers: the unpinned one is named, the pinned one is not", func(t *testing.T) {
		const unpinned, pinned, wallClock = "Nhắc hạn theo lịch mặc định", "Nhắc hạn theo lịch riêng", "Nhắc hạn theo giờ treo tường"
		pack := businessHoursPack("hours") // its set_deadline rule must not be what trips the warning
		active := true
		timerRule := func(name, unit, calendarID string) PackWorkflow {
			return PackWorkflow{
				Name:       name,
				EntityName: "wf_tickets",
				Trigger:    "date_offset",
				Condition: &PackWorkflowCondition{Timer: &PackWorkflowTimer{
					DateField:  "due_date",
					Offset:     PackWorkflowTimerOffset{Amount: 2, Unit: unit},
					Direction:  "before",
					CalendarID: calendarID,
				}},
				Actions: []PackWorkflowAction{
					{Type: "in_app_notification", UserID: "$owner", Title: "Sắp tới hạn", Message: "Phiếu sắp tới hạn."},
				},
				IsActive: &active,
			}
		}
		pack.Workflows = append(pack.Workflows,
			timerRule(unpinned, "business_hours", ""),
			timerRule(pinned, "business_hours", "cal-own"),
			timerRule(wallClock, "hours", ""))

		out := install(t, pack, &fakePackScheduleResolver{})
		got := calendarWarnings(out)
		if len(got) != 1 {
			t.Fatalf("warnings = %q; want exactly one naming the unpinned business_hours timer", out.Warnings)
		}
		if !strings.Contains(got[0], unpinned) {
			t.Errorf("warning %q does not name %q — an unpinned business_hours timer parks on the default calendar", got[0], unpinned)
		}
		if strings.Contains(got[0], pinned) {
			t.Errorf("warning %q names %q — a timer that pins its own calendar never reads the default", got[0], pinned)
		}
		for _, notDefault := range []string{wallClock, "Đặt hạn SLA"} {
			if strings.Contains(got[0], notDefault) {
				t.Errorf("warning %q names %q — an `hours` rule does not depend on a calendar", got[0], notDefault)
			}
		}
	})

	// set_elapsed (DAILY-QUEUE #41) ALWAYS walks the default calendar — it has no unit to opt out
	// with — so a pack carrying one must trip the same warning, in words that fit a measured number
	// as well as a deadline.
	t.Run("set_elapsed rule: named, because it always measures working time", func(t *testing.T) {
		const elapsedRule = "Ghi số phút xử lý"
		pack := businessHoursPack("hours") // the set_deadline rule is wall-clock: not what trips it
		pack.Entities[0].Fields = append(pack.Entities[0].Fields, PackField{Name: "resolution_minutes", Label: "Số phút xử lý", FieldType: "number"})
		active := true
		pack.Workflows = append(pack.Workflows, PackWorkflow{
			Name: elapsedRule, EntityName: "wf_tickets", Trigger: "after_update",
			Actions:  []PackWorkflowAction{{Type: "set_elapsed", Field: "resolution_minutes"}},
			IsActive: &active,
		})
		got := calendarWarnings(install(t, pack, &fakePackScheduleResolver{}))
		if len(got) != 1 || !strings.Contains(got[0], elapsedRule) || strings.Contains(got[0], "Đặt hạn SLA") {
			t.Fatalf("warnings = %q; want exactly one naming %q and not the wall-clock rule", got, elapsedRule)
		}
		if strings.Contains(got[0], "hạn") {
			t.Errorf("warning %q talks about deadlines; a set_elapsed rule stamps a number of minutes", got[0])
		}
	})

	t.Run("calendar store down: install still completes, and says it could not check", func(t *testing.T) {
		out := install(t, businessHoursPack("business_hours"), &fakePackScheduleResolver{err: errors.New("dial tcp 10.0.0.5:3306: connection refused")})
		got := calendarWarnings(out)
		if len(got) != 1 {
			t.Fatalf("warnings = %q; want one saying the calendar could not be checked", out.Warnings)
		}
		// The raw driver error is for the server log, not for an API response body.
		if strings.Contains(got[0], "10.0.0.5") {
			t.Errorf("warning %q leaks the store error to the client", got[0])
		}
	})
}

// DAILY-QUEUE #104: the same sentence belongs in the DRY RUN. The install result is read after the
// pack has already landed; the plan is what the installer reads before clicking "Cài". Parity is
// stated directly: the plan's warning must be the install's warning, word for word.
func TestDryRun_WarnsWhenBusinessHoursRulesHaveNoCalendar(t *testing.T) {
	officeWeek, err := services.DefaultCalendarSchedule()
	if err != nil {
		t.Fatalf("DefaultCalendarSchedule: %v", err)
	}
	newSvc := func(res *fakePackScheduleResolver) *Service {
		meta := &fakeMintMeta{}
		return NewService(Deps{
			EntityReader:     meta,
			RoleReader:       fakeMintRoles{},
			EntityWriter:     meta,
			WorkflowWriter:   &fakeWorkflowWriter{},
			ScheduleResolver: res,
		})
	}
	ctx := context.Background()

	t.Run("no default calendar: the plan carries the install's warning, and nothing is written", func(t *testing.T) {
		wr := &fakeWorkflowWriter{}
		meta := &fakeMintMeta{}
		svc := NewService(Deps{EntityReader: meta, RoleReader: fakeMintRoles{}, EntityWriter: meta,
			WorkflowWriter: wr, ScheduleResolver: &fakePackScheduleResolver{}})
		plan, err := svc.DryRunInstall(ctx, "tenant-1", businessHoursPack("business_hours"))
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "Đặt hạn SLA") || !strings.Contains(plan.Warnings[0], "giờ treo tường") {
			t.Fatalf("plan.Warnings = %q; want exactly the working-calendar warning naming the SLA rule", plan.Warnings)
		}
		if len(wr.created) != 0 || len(meta.defs["tenant-1"]) != 0 {
			t.Fatalf("dry run wrote: workflows %v, entities %v", wr.created, meta.defs["tenant-1"])
		}
		out, err := newSvc(&fakePackScheduleResolver{}).Install(ctx, "tenant-1", "user-1", businessHoursPack("business_hours"))
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		if got := calendarWarnings(out); len(got) != 1 || got[0] != plan.Warnings[0] {
			t.Fatalf("install warned %q, plan promised %q — the dry run must say what the install will say", got, plan.Warnings)
		}
	})

	t.Run("default calendar present: no warning in the plan", func(t *testing.T) {
		plan, err := newSvc(&fakePackScheduleResolver{sched: officeWeek}).DryRunInstall(ctx, "tenant-1", businessHoursPack("business_hours"))
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if plan.Warnings == nil || len(plan.Warnings) != 0 {
			t.Fatalf("plan.Warnings = %#v; want an empty (non-nil) list", plan.Warnings)
		}
	})
}
