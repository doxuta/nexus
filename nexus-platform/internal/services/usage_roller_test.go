package services

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"nexus/platform/internal/usage"
)

type fakeUsageStore struct {
	mu       sync.Mutex
	counters func(tenant string, from, to time.Time) (map[usage.Key]int64, error)
	writes   []usageWrite
	panicOn  string
}

type usageWrite struct {
	tenant string
	day    string
	mode   usage.Mode
	rows   []usage.Row
}

func (f *fakeUsageStore) CountersForDay(_ context.Context, tenant string, from, to time.Time) (map[usage.Key]int64, error) {
	if f.panicOn == tenant {
		panic("boom")
	}
	if f.counters != nil {
		return f.counters(tenant, from, to)
	}
	m := map[usage.Key]int64{}
	for _, k := range usage.Metrics {
		m[usage.Key{Metric: k}] = 0
	}
	m[usage.Key{Metric: usage.WorkflowRuns}] = 3
	return m, nil
}

func (f *fakeUsageStore) Upsert(_ context.Context, rows []usage.Row, mode usage.Mode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(rows) > 0 {
		f.writes = append(f.writes, usageWrite{tenant: rows[0].TenantID, day: rows[0].Day, mode: mode, rows: rows})
	}
	return nil
}

type fakeTenants []string

func (f fakeTenants) ListActiveTenants(context.Context, int) ([]string, error) { return f, nil }

func countModes(ws []usageWrite, tenant string) (over, fill int) {
	for _, w := range ws {
		if w.tenant != tenant {
			continue
		}
		if w.mode == usage.Overwrite {
			over++
		} else {
			fill++
		}
	}
	return
}

func TestUsageRoller_FirstTickThenOnlyRecent(t *testing.T) {
	t.Setenv("APP_TIMEZONE", "Asia/Ho_Chi_Minh")
	st := &fakeUsageStore{}
	r := NewUsageRoller(time.Hour, st, fakeTenants{"t1"}, nil)
	r.now = func() time.Time { return time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC) }

	r.Sweep(context.Background())
	over, fill := countModes(st.writes, "t1")
	if over != 2 || fill != 28 {
		t.Fatalf("first tick wrote %d overwrite / %d fillMissing days; want 2 / 28", over, fill)
	}
	if st.writes[0].day != "2026-10-14" || st.writes[1].day != "2026-10-13" {
		t.Errorf("overwrite days = %s, %s; want today then yesterday", st.writes[0].day, st.writes[1].day)
	}
	// every metric is written, zeros included
	if len(st.writes[0].rows) != len(usage.Metrics) {
		t.Errorf("rows per day = %d; want %d (zeros too)", len(st.writes[0].rows), len(usage.Metrics))
	}
	st.writes = nil
	r.Sweep(context.Background())
	over, fill = countModes(st.writes, "t1")
	if over != 2 || fill != 0 {
		t.Fatalf("second tick wrote %d / %d; want 2 / 0", over, fill)
	}
}

// 2026-10-13T17:00:00Z is 00:00 on 14 Oct in Vietnam: "today" flips exactly there.
func TestUsageRoller_DayFlipsAt1700UTC(t *testing.T) {
	t.Setenv("APP_TIMEZONE", "Asia/Ho_Chi_Minh")
	for _, tc := range []struct {
		name string
		now  time.Time
		day  string
	}{
		{"16:59:59Z is still 13 Oct", time.Date(2026, 10, 13, 16, 59, 59, 0, time.UTC), "2026-10-13"},
		{"17:00:00Z is 14 Oct", time.Date(2026, 10, 13, 17, 0, 0, 0, time.UTC), "2026-10-14"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			day, from, to := usageDayBounds(tc.now)
			if day != tc.day {
				t.Errorf("day = %s; want %s", day, tc.day)
			}
			if to.Sub(from) != 24*time.Hour || from.Hour() != 17 {
				t.Errorf("bounds = %s..%s; want 17:00Z..17:00Z", from, to)
			}
		})
	}
}

func TestUsageRoller_TenantErrorAndPanicDoNotStopOthers(t *testing.T) {
	st := &fakeUsageStore{counters: func(tenant string, _, _ time.Time) (map[usage.Key]int64, error) {
		if tenant == "bad" {
			return nil, errors.New("db down")
		}
		return map[usage.Key]int64{{Metric: usage.WorkflowRuns}: 1}, nil
	}}
	r := NewUsageRoller(time.Hour, st, fakeTenants{"bad", "ok"}, nil)
	r.Sweep(context.Background())
	if over, _ := countModes(st.writes, "ok"); over != 2 {
		t.Fatalf("healthy tenant got %d overwrite days; want 2", over)
	}
	if over, fill := countModes(st.writes, "bad"); over+fill != 0 {
		t.Fatalf("failing tenant wrote %d/%d", over, fill)
	}
	// a failed tenant is retried (its backfill is not marked done)
	if _, done := r.backfilled["bad"]; done {
		t.Error("failed tenant marked as backfilled")
	}
	// panic inside a sweep is recovered by sweepOnce
	st2 := &fakeUsageStore{panicOn: "p"}
	r2 := NewUsageRoller(time.Hour, st2, fakeTenants{"p"}, nil)
	r2.sweepOnce(context.Background())
}

func TestUsageRoller_StartStop(t *testing.T) {
	st := &fakeUsageStore{}
	r := NewUsageRoller(time.Hour, st, fakeTenants{"t1"}, nil)
	r.firstWait = 10 * time.Millisecond
	r.Start()
	r.Start() // idempotent
	deadline := time.Now().Add(2 * time.Second)
	for {
		st.mu.Lock()
		n := len(st.writes)
		st.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no tick within 2s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}
