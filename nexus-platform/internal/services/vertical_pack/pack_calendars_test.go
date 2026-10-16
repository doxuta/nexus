package vertical_pack

// DAILY-QUEUE #720 (1/4 of #295): working calendars travel with the pack by NAME. Điện lạnh lost
// its Mon-Sat calendar and every rule pinned to it.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"nexus/platform/internal/calendar"
	"nexus/platform/internal/calmeta"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

type fakeCalendarStore struct {
	rows    []calmeta.BusinessCalendar
	created []services.CalendarInput
}

func (f *fakeCalendarStore) ListCalendars(context.Context, string) ([]calmeta.BusinessCalendar, error) {
	return append([]calmeta.BusinessCalendar(nil), f.rows...), nil
}

func (f *fakeCalendarStore) CreateCalendar(_ context.Context, tenantID string, in services.CalendarInput) (*calmeta.BusinessCalendar, error) {
	f.created = append(f.created, in)
	h, _ := json.Marshal(in.Hours)
	row := calmeta.BusinessCalendar{ID: fmt.Sprintf("new-%d", len(f.created)), TenantID: tenantID, Name: in.Name, Timezone: in.Timezone, Hours: h, IsDefault: in.IsDefault}
	f.rows = append(f.rows, row)
	return &row, nil
}

const monSat = `{"mon":[{"start":"08:00","end":"17:00"}],"sat":[{"start":"08:00","end":"12:00"}]}`

func TestPackCalendars_ExportInstallRoundTrip(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMintMeta{}
	if _, _, err := meta.CreateEntity(ctx, "src", services.CreateEntityInput{Name: "cal_jobs", Label: "Jobs"}); err != nil {
		t.Fatal(err)
	}
	srcCals := &fakeCalendarStore{rows: []calmeta.BusinessCalendar{
		{ID: "cal-1", Name: "Thứ 2 - Thứ 7", Timezone: "Asia/Ho_Chi_Minh", Hours: json.RawMessage(monSat), Holidays: json.RawMessage(`["2026-09-02"]`)},
		{ID: "cal-unused", Name: "Không dùng", Timezone: "Asia/Ho_Chi_Minh", Hours: json.RawMessage(monSat)},
	}}
	srcWf := &fakeWorkflowWriter{existing: []workflow.WorkflowRecord{
		{ID: "wf-1", TenantID: "src", Name: "Hạn theo lịch", EntityName: "cal_jobs", Trigger: "after_create", IsActive: true,
			Actions: json.RawMessage(`[{"type":"set_deadline","field":"due_date","offset":{"amount":4,"unit":"business_hours"},"calendarId":"cal-1"}]`)},
	}}
	pack, warns, err := NewService(Deps{EntityReader: meta, LayoutReader: fakeApLayouts{}, RoleReader: fakeMintRoles{}, WorkflowWriter: srcWf, CalendarStore: srcCals}).
		Export(ctx, "src", []string{"cal_jobs"}, false, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(pack.Workflows) != 1 {
		t.Fatalf("rule dropped: %v", warns)
	}
	if len(pack.Calendars) != 1 || pack.Calendars[0].Name != "Thứ 2 - Thứ 7" || len(pack.Calendars[0].Holidays) != 1 {
		t.Fatalf("calendars = %+v, want only the pinned one with its holiday", pack.Calendars)
	}
	if got := pack.Workflows[0].Actions[0]; got.CalendarName != "Thứ 2 - Thứ 7" || got.CalendarID != "" {
		t.Fatalf("exported pin = %+v; want name, no source id", got)
	}

	var wire Pack
	if err := json.Unmarshal(mustJSON(t, pack), &wire); err != nil {
		t.Fatal(err)
	}
	// resolve against a target that already owns a calendar with the same name and one default
	dstCals := &fakeCalendarStore{}
	svc := NewService(Deps{CalendarStore: dstCals})
	res := &InstallResult{}
	ids, err := svc.installCalendars(ctx, "dst", &wire, res)
	if err != nil {
		t.Fatalf("installCalendars: %v", err)
	}
	if len(dstCals.created) != 1 || ids["Thứ 2 - Thứ 7"] == "" || len(res.CalendarsCreated) != 1 {
		t.Fatalf("created=%+v ids=%v", dstCals.created, ids)
	}
	if h := dstCals.created[0].Hours["sat"]; len(h) != 1 || h[0] != (calendar.Interval{Start: "08:00", End: "12:00"}) {
		t.Fatalf("hours lost in transit: %+v", dstCals.created[0].Hours)
	}
	pw, err := resolveCalendarNames(wire.Workflows[0], ids)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if pw.Actions[0].CalendarID != ids["Thứ 2 - Thứ 7"] {
		t.Fatalf("pin = %q, want the target id %q", pw.Actions[0].CalendarID, ids["Thứ 2 - Thứ 7"])
	}
	if wire.Workflows[0].Actions[0].CalendarID != "" {
		t.Fatal("resolveCalendarNames mutated its input")
	}
	// second install: same name -> kept, nothing created
	res2 := &InstallResult{}
	if _, err := svc.installCalendars(ctx, "dst", &wire, res2); err != nil || len(res2.CalendarsCreated) != 0 || len(dstCals.created) != 1 {
		t.Fatalf("re-install must not create again: err=%v created=%v", err, dstCals.created)
	}
	// unresolved name = hard error
	if _, err := resolveCalendarNames(wire.Workflows[0], map[string]string{}); err == nil {
		t.Fatal("unresolved calendar name must fail")
	}
}

func TestPackCalendars_DefaultOnlyWhenTargetHasNone(t *testing.T) {
	pack := &Pack{Calendars: []PackCalendar{{Name: "Mới", Timezone: "Asia/Ho_Chi_Minh", Hours: map[string][]calendar.Interval{"mon": {{Start: "08:00", End: "17:00"}}}, IsDefault: true}}}
	for _, tc := range []struct {
		name        string
		existing    []calmeta.BusinessCalendar
		wantDefault bool
	}{
		{"target has none", nil, true},
		{"target keeps its own", []calmeta.BusinessCalendar{{ID: "x", Name: "Giờ hành chính", IsDefault: true}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeCalendarStore{rows: tc.existing}
			if _, err := NewService(Deps{CalendarStore: st}).installCalendars(context.Background(), "dst", pack, &InstallResult{}); err != nil {
				t.Fatal(err)
			}
			if got := st.created[0].IsDefault; got != tc.wantDefault {
				t.Fatalf("IsDefault = %v, want %v", got, tc.wantDefault)
			}
		})
	}
}
