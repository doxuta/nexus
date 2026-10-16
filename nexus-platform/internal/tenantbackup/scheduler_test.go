package tenantbackup

import (
	"fmt"
	"testing"
	"time"
)

func ict(y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, ictZone)
}

func TestDueSlots(t *testing.T) {
	v := func(n int64) *int64 { return &n }
	tests := []struct {
		name string
		now  time.Time
		ver  int64
		last *int64
		want []string
	}{
		{"before the hour, nothing yet", ict(2026, 10, 14, 1, 59), 90, v(90), nil},
		{"at the hour, daily slot", ict(2026, 10, 14, 2, 0), 90, v(90), []string{"d:2026-10-14"}},
		{"daily slot uses the LOCAL day across UTC midnight", ict(2026, 10, 15, 6, 0).UTC(), 90, v(90), []string{"d:2026-10-15"}},
		{"01:00 local is still the previous UTC day but before the hour", ict(2026, 10, 15, 1, 0).UTC(), 90, v(90), nil},
		{"older latest ready adds a catch-up slot after the daily one", ict(2026, 10, 14, 9, 0), 91, v(90), []string{"d:2026-10-14", "v:91"}},
		{"catch-up fires before the hour too", ict(2026, 10, 14, 1, 0), 91, v(90), []string{"v:91"}},
		{"no ready backup yet means no catch-up", ict(2026, 10, 14, 1, 0), 91, nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dueSlots(tc.now, 2, tc.ver, tc.last)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("dueSlots = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNextDue(t *testing.T) {
	now := ict(2026, 10, 14, 9, 0)
	if got := nextDue(ict(2026, 10, 14, 1, 0), 2, false); !got.Equal(ict(2026, 10, 14, 2, 0)) {
		t.Errorf("before the hour: %v", got)
	}
	if got := nextDue(now, 2, true); !got.Equal(ict(2026, 10, 15, 2, 0)) {
		t.Errorf("today taken: %v", got)
	}
	if got := nextDue(now, 2, false); !got.Equal(now) {
		t.Errorf("due now: %v", got)
	}
}

func bk(id, kind, status string, age time.Duration, now time.Time) Backup {
	at := now.Add(-age)
	b := Backup{ID: id, Kind: kind, Status: status, CreatedAt: at}
	if status != StatusRunning {
		b.FinishedAt = &at
	}
	return b
}

func idSet(rows []Backup) map[string]bool {
	m := map[string]bool{}
	for _, r := range rows {
		m[r.ID] = true
	}
	return m
}

func TestExpired(t *testing.T) {
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	h := time.Hour
	rows := []Backup{
		bk("s1", KindScheduled, StatusReady, 1*h, now),
		bk("s2", KindScheduled, StatusReady, 2*h, now),
		bk("s3", KindScheduled, StatusReady, 3*h, now),
		bk("s-fail-new", KindScheduled, StatusFailed, 4*h, now), // failed does not count toward N
		bk("m-old", KindManual, StatusReady, 900*h, now),        // manual is never auto-removed
		bk("p1", KindPreRestore, StatusReady, 1*h, now),
		bk("p2", KindPreRestore, StatusReady, 2*h, now),
		bk("p3", KindPreRestore, StatusReady, 3*h, now),
		bk("f-old", KindManual, StatusFailed, 8*24*h, now),
		bk("run", KindScheduled, StatusRunning, 100*h, now),
	}
	pol := retentionPolicy{KeepScheduled: 2, KeepPreRestore: 2, FailedDays: 7}
	got := idSet(expired(rows, now, pol, map[string]bool{"p3": true}))
	want := map[string]bool{"s3": true, "f-old": true} // p3 is expired by count but protected
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("expired = %v, want %v", got, want)
	}
	// Without protection p3 goes too; running / ready-manual / young-failed never do.
	got = idSet(expired(rows, now, pol, nil))
	if !got["p3"] || got["run"] || got["m-old"] || got["s-fail-new"] {
		t.Errorf("unprotected expired = %v", got)
	}
}
