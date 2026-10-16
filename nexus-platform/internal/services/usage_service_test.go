package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"nexus/platform/internal/usage"
)

type fakeUsageRead struct {
	fakeUsageStore
	stored    []usage.DayValue
	rangeFrom string
	rangeTo   string
	rangeErr  error
	last      *time.Time
	tenant    string
}

func (f *fakeUsageRead) Range(_ context.Context, tenant, from, to string) ([]usage.DayValue, error) {
	f.tenant, f.rangeFrom, f.rangeTo = tenant, from, to
	return f.stored, f.rangeErr
}
func (f *fakeUsageRead) LastRollupAt(context.Context, string, string) (*time.Time, error) {
	return f.last, nil
}

func newUsageSvc(f *fakeUsageRead) *UsageService {
	s := NewUsageService(f, true)
	s.now = func() time.Time { return time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC) } // 10:00 on 14 Oct in VN
	return s
}

func TestUsageService_DenseDaysTotalsAndLiveToday(t *testing.T) {
	f := &fakeUsageRead{stored: []usage.DayValue{
		{Day: "2026-10-12", Metric: usage.WorkflowRuns, Value: 5},
		{Day: "2026-10-12", Metric: usage.EmailsFailed, Value: 2},
		{Day: "2026-10-14", Metric: usage.WorkflowRuns, Value: 999}, // stale stored today must lose to live
	}}
	rep, err := newUsageSvc(f).Daily(context.Background(), "t1", "2026-10-11", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Days) != 4 || rep.From != "2026-10-11" || rep.To != "2026-10-14" {
		t.Fatalf("days = %d (%s..%s); want 4 dense days", len(rep.Days), rep.From, rep.To)
	}
	if rep.Days[0].WorkflowRuns != 0 || rep.Days[1].WorkflowRuns != 5 || rep.Days[1].EmailsFailed != 2 {
		t.Errorf("days = %+v", rep.Days)
	}
	if rep.Days[3].WorkflowRuns != 3 { // fakeUsageStore live counter returns 3
		t.Errorf("today = %d; want the live 3, not the stored one", rep.Days[3].WorkflowRuns)
	}
	if rep.Totals.WorkflowRuns != 8 || rep.Totals.EmailsFailed != 2 {
		t.Errorf("totals = %+v", rep.Totals)
	}
	if f.tenant != "t1" || f.rangeTo != "2026-10-13" {
		t.Errorf("Range(%q, %s..%s); want tenant t1 and stored days up to yesterday", f.tenant, f.rangeFrom, f.rangeTo)
	}
	if !rep.RollupEnabled || rep.Timezone == "" {
		t.Errorf("rollupEnabled/timezone = %v/%q", rep.RollupEnabled, rep.Timezone)
	}
}

func TestUsageService_DefaultsToThirtyDays(t *testing.T) {
	rep, err := newUsageSvc(&fakeUsageRead{}).Daily(context.Background(), "t1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Days) != 30 || rep.From != "2026-09-15" || rep.To != "2026-10-14" {
		t.Fatalf("default range = %d days %s..%s; want 30 days ending today", len(rep.Days), rep.From, rep.To)
	}
}

func TestUsageService_PastRangeSkipsLiveToday(t *testing.T) {
	f := &fakeUsageRead{}
	rep, err := newUsageSvc(f).Daily(context.Background(), "t1", "2026-10-01", "2026-10-03")
	if err != nil || len(rep.Days) != 3 {
		t.Fatalf("Daily = %v, %v", rep, err)
	}
	if f.rangeFrom != "2026-10-01" || f.rangeTo != "2026-10-03" {
		t.Errorf("Range = %s..%s", f.rangeFrom, f.rangeTo)
	}
}

func TestUsageService_365DaysAccepted(t *testing.T) {
	rep, err := newUsageSvc(&fakeUsageRead{}).Daily(context.Background(), "t1", "2025-10-15", "")
	if err != nil || len(rep.Days) != 365 {
		t.Fatalf("365-day range: %v days, err %v", len(rep.Days), err)
	}
}

func TestUsageService_RangeInvalid(t *testing.T) {
	cases := []struct{ name, from, to string }{
		{"from after to", "2026-10-10", "2026-10-01"},
		{"over 366 days", "2025-10-01", "2026-10-14"},
		{"bad format", "14/10/2026", ""},
		{"bad to", "", "tomorrow"},
		{"to after today", "2026-10-10", "2026-10-15"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newUsageSvc(&fakeUsageRead{}).Daily(context.Background(), "t1", tc.from, tc.to)
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != "USAGE_RANGE_INVALID" {
				t.Fatalf("err = %v; want USAGE_RANGE_INVALID", err)
			}
		})
	}
}

func TestUsageService_StoreErrorIsWrapped(t *testing.T) {
	boom := errors.New("db down")
	_, err := newUsageSvc(&fakeUsageRead{rangeErr: boom}).Daily(context.Background(), "t1", "", "")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v; want it to wrap the store error", err)
	}
}
