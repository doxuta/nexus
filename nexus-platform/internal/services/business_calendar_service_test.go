package services

// Unit tests — BusinessCalendarService (CALENDAR-1). Fake store, no DB. Pins:
//   * config-time validation codes (tz / hours / holidays / empty / name), which share ONE
//     parse body (calendar.FromJSON) with the run-time consumers;
//   * repo sentinel mapping (dup name → CALENDAR_ALREADY_EXISTS, unknown → CALENDAR_NOT_FOUND);
//   * the stored hours/holidays JSON round-trips through calendar.FromJSON (a saved calendar
//     can never fail to parse at run time).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/calendar"
	"nexus/platform/internal/calmeta"
)

// fakeCalendarStore is an in-memory calendarStore.
type fakeCalendarStore struct {
	rows    map[string]*calmeta.BusinessCalendar
	nextID  int
	created *calmeta.CreateInput       // last Create payload (for shape asserts)
	usages  map[string][]calmeta.Usage // "tenant|calendarID" → rules pinning it
}

func newFakeCalendarStore() *fakeCalendarStore {
	return &fakeCalendarStore{rows: map[string]*calmeta.BusinessCalendar{}}
}

func (f *fakeCalendarStore) Create(_ context.Context, in calmeta.CreateInput) (string, error) {
	for _, r := range f.rows {
		if r.TenantID == in.TenantID && r.Name == in.Name {
			return "", calmeta.ErrCalendarDuplicateName
		}
	}
	f.nextID++
	id := strings.Repeat("0", 35) + string(rune('0'+f.nextID))
	cp := in
	f.created = &cp
	if in.IsDefault {
		for _, r := range f.rows {
			if r.TenantID == in.TenantID {
				r.IsDefault = false
			}
		}
	}
	f.rows[id] = &calmeta.BusinessCalendar{
		ID: id, TenantID: in.TenantID, Name: in.Name, Timezone: in.Timezone,
		Hours: in.Hours, Holidays: in.Holidays, IsDefault: in.IsDefault,
	}
	return id, nil
}

func (f *fakeCalendarStore) GetByID(_ context.Context, tenantID, id string) (*calmeta.BusinessCalendar, error) {
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return nil, calmeta.ErrCalendarNotFound
	}
	cp := *r
	return &cp, nil
}

func (f *fakeCalendarStore) List(_ context.Context, tenantID string) ([]calmeta.BusinessCalendar, error) {
	out := make([]calmeta.BusinessCalendar, 0)
	for _, r := range f.rows {
		if r.TenantID == tenantID {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *fakeCalendarStore) Update(_ context.Context, tenantID, id string, in calmeta.UpdateInput) error {
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return calmeta.ErrCalendarNotFound
	}
	for oid, o := range f.rows {
		if oid != id && o.TenantID == tenantID && o.Name == in.Name {
			return calmeta.ErrCalendarDuplicateName
		}
	}
	if in.IsDefault {
		for oid, o := range f.rows {
			if oid != id && o.TenantID == tenantID {
				o.IsDefault = false
			}
		}
	}
	r.Name, r.Timezone, r.Hours, r.Holidays, r.IsDefault = in.Name, in.Timezone, in.Hours, in.Holidays, in.IsDefault
	return nil
}

func (f *fakeCalendarStore) SoftDelete(_ context.Context, tenantID, id string) error {
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return calmeta.ErrCalendarNotFound
	}
	delete(f.rows, id)
	return nil
}

func (f *fakeCalendarStore) Usages(_ context.Context, tenantID, id string) ([]calmeta.Usage, error) {
	return f.usages[tenantID+"|"+id], nil
}

// DAILY-QUEUE #326: a calendar a live rule pins cannot be deleted — 409 CALENDAR_IN_USE naming the
// rules; a free calendar deletes as before; another tenant's pin on the same id does not block.
func TestBusinessCalendarService_DeleteRefusesWhilePinned(t *testing.T) {
	store := newFakeCalendarStore()
	svc := NewBusinessCalendarService(store, nil)
	ctx := context.Background()

	cal, err := svc.CreateCalendar(ctx, "tenant-1", validCalInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	store.usages = map[string][]calmeta.Usage{
		"tenant-1|" + cal.ID: {{WorkflowID: "w1", WorkflowName: "Nhắc hạn"}, {WorkflowID: "w2", WorkflowName: "SLA ticket"}},
		"tenant-2|" + cal.ID: {{WorkflowID: "w9", WorkflowName: "Luật tổ chức khác"}},
	}

	err = svc.DeleteCalendar(ctx, "tenant-1", cal.ID)
	if calCode(t, err) != "CALENDAR_IN_USE" {
		t.Fatalf("delete pinned code = %s; want CALENDAR_IN_USE", calCode(t, err))
	}
	var ve *ValidationError
	if !errors.As(err, &ve) || !strings.Contains(ve.Msg, "Nhắc hạn") || !strings.Contains(ve.Msg, "SLA ticket") {
		t.Fatalf("message must name both rules, got %v", err)
	}
	if strings.Contains(ve.Msg, "tổ chức khác") {
		t.Fatalf("message leaked another tenant's rule: %q", ve.Msg)
	}
	if _, ok := store.rows[cal.ID]; !ok {
		t.Fatal("calendar was deleted despite being pinned")
	}

	delete(store.usages, "tenant-1|"+cal.ID)
	if err := svc.DeleteCalendar(ctx, "tenant-1", cal.ID); err != nil {
		t.Fatalf("delete free calendar: %v", err)
	}
}

func validCalInput() CalendarInput {
	return CalendarInput{
		Name:     "Giờ hành chính",
		Timezone: "Asia/Ho_Chi_Minh",
		Hours: map[string][]calendar.Interval{
			"mon": {{Start: "08:00", End: "12:00"}, {Start: "13:00", End: "17:00"}},
			"fri": {{Start: "08:00", End: "12:00"}},
		},
		Holidays: []string{"2026-09-02"},
		ActorID:  "actor-1",
	}
}

func calCode(t *testing.T, err error) string {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v; want *ValidationError", err)
	}
	return ve.Code
}

func TestBusinessCalendarService_CreateValidation(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*CalendarInput)
		wantCode string
	}{
		{"missing name", func(in *CalendarInput) { in.Name = "  " }, "CALENDAR_NAME_REQUIRED"},
		{"name too long", func(in *CalendarInput) { in.Name = strings.Repeat("a", 81) }, "CALENDAR_FIELD_TOO_LONG"},
		{"bad timezone", func(in *CalendarInput) { in.Timezone = "Mars/Olympus" }, "CALENDAR_TIMEZONE_INVALID"},
		{"empty timezone", func(in *CalendarInput) { in.Timezone = "" }, "CALENDAR_TIMEZONE_INVALID"},
		{"bad weekday key", func(in *CalendarInput) {
			in.Hours = map[string][]calendar.Interval{"monday": {{Start: "08:00", End: "17:00"}}}
		}, "CALENDAR_HOURS_INVALID"},
		{"overlapping intervals", func(in *CalendarInput) {
			in.Hours = map[string][]calendar.Interval{"mon": {
				{Start: "08:00", End: "12:00"}, {Start: "11:00", End: "15:00"},
			}}
		}, "CALENDAR_HOURS_INVALID"},
		{"start not before end", func(in *CalendarInput) {
			in.Hours = map[string][]calendar.Interval{"mon": {{Start: "12:00", End: "12:00"}}}
		}, "CALENDAR_HOURS_INVALID"},
		{"no working hours at all", func(in *CalendarInput) {
			in.Hours = map[string][]calendar.Interval{"mon": {}}
		}, "CALENDAR_HOURS_EMPTY"},
		{"nil hours map", func(in *CalendarInput) { in.Hours = nil }, "CALENDAR_HOURS_EMPTY"},
		{"bad holiday", func(in *CalendarInput) { in.Holidays = []string{"02/09/2026"} }, "CALENDAR_HOLIDAYS_INVALID"},
		{"too many holidays", func(in *CalendarInput) {
			in.Holidays = make([]string, 367)
			for i := range in.Holidays {
				in.Holidays[i] = "2026-01-01"
			}
		}, "CALENDAR_HOLIDAYS_INVALID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewBusinessCalendarService(newFakeCalendarStore(), nil)
			in := validCalInput()
			tt.mutate(&in)
			_, err := svc.CreateCalendar(context.Background(), "tenant-1", in)
			if got := calCode(t, err); got != tt.wantCode {
				t.Fatalf("code = %s; want %s", got, tt.wantCode)
			}
		})
	}
}

func TestBusinessCalendarService_CreateStoresRuntimeParseableJSON(t *testing.T) {
	store := newFakeCalendarStore()
	svc := NewBusinessCalendarService(store, nil)

	cal, err := svc.CreateCalendar(context.Background(), "tenant-1", validCalInput())
	if err != nil {
		t.Fatalf("CreateCalendar: %v", err)
	}
	if cal.Name != "Giờ hành chính" || cal.TenantID != "tenant-1" {
		t.Fatalf("read-back = %+v", cal)
	}
	// The stored bytes must parse through the SAME body the SLA engine will use.
	sched, err := calendar.FromJSON(store.created.Timezone, store.created.Hours, store.created.Holidays)
	if err != nil {
		t.Fatalf("stored JSON does not parse at run time: %v", err)
	}
	if !sched.HasWorkingHours() {
		t.Fatal("stored schedule has no working hours")
	}
}

func TestBusinessCalendarService_DuplicateAndNotFound(t *testing.T) {
	store := newFakeCalendarStore()
	svc := NewBusinessCalendarService(store, nil)
	ctx := context.Background()

	first, err := svc.CreateCalendar(ctx, "tenant-1", validCalInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Duplicate name in the same tenant → 409 code.
	if _, err := svc.CreateCalendar(ctx, "tenant-1", validCalInput()); calCode(t, err) != "CALENDAR_ALREADY_EXISTS" {
		t.Fatalf("dup create code = %s; want CALENDAR_ALREADY_EXISTS", calCode(t, err))
	}
	// Same name in ANOTHER tenant is fine (unique per tenant).
	if _, err := svc.CreateCalendar(ctx, "tenant-2", validCalInput()); err != nil {
		t.Fatalf("cross-tenant same name: %v", err)
	}
	// Unknown id paths.
	if _, err := svc.GetCalendar(ctx, "tenant-1", "ghost"); calCode(t, err) != "CALENDAR_NOT_FOUND" {
		t.Fatal("get unknown: want CALENDAR_NOT_FOUND")
	}
	if _, err := svc.UpdateCalendar(ctx, "tenant-1", "ghost", validCalInput()); calCode(t, err) != "CALENDAR_NOT_FOUND" {
		t.Fatal("update unknown: want CALENDAR_NOT_FOUND")
	}
	if err := svc.DeleteCalendar(ctx, "tenant-1", "ghost"); calCode(t, err) != "CALENDAR_NOT_FOUND" {
		t.Fatal("delete unknown: want CALENDAR_NOT_FOUND")
	}
	// Cross-tenant id is not-found, never a leak.
	if _, err := svc.GetCalendar(ctx, "tenant-2", first.ID); calCode(t, err) != "CALENDAR_NOT_FOUND" {
		t.Fatal("cross-tenant get: want CALENDAR_NOT_FOUND")
	}
}

func TestBusinessCalendarService_DefaultFlagAndUpdate(t *testing.T) {
	store := newFakeCalendarStore()
	svc := NewBusinessCalendarService(store, nil)
	ctx := context.Background()

	in1 := validCalInput()
	in1.IsDefault = true
	cal1, err := svc.CreateCalendar(ctx, "tenant-1", in1)
	if err != nil {
		t.Fatalf("create 1: %v", err)
	}
	in2 := validCalInput()
	in2.Name = "Ca đêm"
	in2.IsDefault = true
	cal2, err := svc.CreateCalendar(ctx, "tenant-1", in2)
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}
	// ≤1 default per tenant: the second default create cleared the first.
	got1, _ := svc.GetCalendar(ctx, "tenant-1", cal1.ID)
	if got1.IsDefault {
		t.Fatal("calendar 1 still default after calendar 2 became default")
	}
	if !cal2.IsDefault {
		t.Fatal("calendar 2 not default")
	}

	// Full-replace update: new name + new tz land; validation still runs.
	upd := validCalInput()
	upd.Name = "Giờ mới"
	upd.Timezone = "UTC"
	updated, err := svc.UpdateCalendar(ctx, "tenant-1", cal1.ID, upd)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "Giờ mới" || updated.Timezone != "UTC" {
		t.Fatalf("updated = %+v", updated)
	}
	bad := validCalInput()
	bad.Timezone = "Nowhere/Nope"
	if _, err := svc.UpdateCalendar(ctx, "tenant-1", cal1.ID, bad); calCode(t, err) != "CALENDAR_TIMEZONE_INVALID" {
		t.Fatal("update with bad tz: want CALENDAR_TIMEZONE_INVALID")
	}
}
