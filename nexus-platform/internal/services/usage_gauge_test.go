package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/usage"
)

type fakeGaugeStore struct {
	stored   *usage.Gauge
	at       *time.Time
	live     *usage.Gauge
	liveErr  error
	liveCall int
	gotNames []string
	gotSeen  time.Time
	gotTen   string
}

func (f *fakeGaugeStore) Gauges(_ context.Context, tenant string, seen time.Time, names []string, _ *zap.Logger) (*usage.Gauge, error) {
	f.liveCall++
	f.gotTen, f.gotSeen, f.gotNames = tenant, seen, names
	return f.live, f.liveErr
}
func (f *fakeGaugeStore) LatestGauge(context.Context, string, string) (*usage.Gauge, *time.Time, error) {
	return f.stored, f.at, nil
}

type fakeEntities []metadata.EntityDefinition

func (f fakeEntities) ListEntities(string) ([]metadata.EntityDefinition, error) { return f, nil }

var gaugeNow = time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC) // 10:00 on 14 Oct in VN

func newGaugeSvc(g *fakeGaugeStore, ents fakeEntities) *UsageService {
	svc := NewUsageService(&fakeUsageRead{}, true).WithGauges(&UsageGaugeSource{Store: g, Entities: ents})
	svc.now = func() time.Time { return gaugeNow }
	return svc
}

func TestUsageService_CurrentFromStoredSnapshot(t *testing.T) {
	at := gaugeNow.Add(-20 * time.Minute)
	g := &fakeGaugeStore{at: &at, stored: &usage.Gauge{
		UsersActive: 4, UsersSeen30d: 3, FileBytes: 2048, FileCount: 2,
		Records: []usage.RecordCount{{Entity: "crm_contacts", Count: 7}, {Entity: "gone", Count: 99}, {Entity: "plain", Count: 1}},
	}}
	rep, err := newGaugeSvc(g, fakeEntities{
		{Name: "crm_contacts", Label: "Liên hệ"}, {Name: "plain"},
	}).Daily(context.Background(), "t1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	c := rep.Current
	if c == nil || g.liveCall != 0 {
		t.Fatalf("current = %+v, live calls = %d; want the stored snapshot, no recount", c, g.liveCall)
	}
	if c.UsersActive != 4 || c.UsersSeen30d != 3 || c.FileBytes != 2048 || !c.ComputedAt.Equal(at) {
		t.Errorf("current = %+v", c)
	}
	if len(c.Records) != 2 || c.Records[0].Label != "Liên hệ" || c.Records[1].Label != "plain" || c.RecordsTotal != 8 {
		t.Errorf("records = %+v total %d; want business label, name fallback, deleted entity dropped (total 8)", c.Records, c.RecordsTotal)
	}
}

func TestUsageService_CurrentLiveWhenNoSnapshot(t *testing.T) {
	g := &fakeGaugeStore{live: &usage.Gauge{UsersActive: 1, Records: []usage.RecordCount{{Entity: "crm_contacts", Count: 2}}}}
	rep, err := newGaugeSvc(g, fakeEntities{{Name: "crm_contacts", Label: "Liên hệ"}}).Daily(context.Background(), "t1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if g.liveCall != 1 || g.gotTen != "t1" || len(g.gotNames) != 1 || g.gotNames[0] != "crm_contacts" {
		t.Errorf("live call = %d tenant %q names %v; want one live count over the tenant's metadata names", g.liveCall, g.gotTen, g.gotNames)
	}
	// seen window = today and the 29 days before it, in UTC edges of the VN calendar
	if want := time.Date(2026, 9, 14, 17, 0, 0, 0, time.UTC); !g.gotSeen.Equal(want) {
		t.Errorf("seenFrom = %v; want %v", g.gotSeen, want)
	}
	if rep.Current == nil || !rep.Current.ComputedAt.Equal(gaugeNow) || rep.Current.RecordsTotal != 2 {
		t.Errorf("current = %+v; want computedAt=now", rep.Current)
	}
}

func TestUsageService_CurrentErrorAndUsersSeenColumn(t *testing.T) {
	g := &fakeGaugeStore{liveErr: errors.New("boom")}
	if _, err := newGaugeSvc(g, nil).Daily(context.Background(), "t1", "", ""); err == nil {
		t.Fatal("a failing live count must surface")
	}
	f := &fakeUsageRead{stored: []usage.DayValue{{Day: "2026-10-12", Metric: usage.UsersSeen, Value: 3}, {Day: "2026-10-13", Metric: usage.UsersSeen, Value: 2}}}
	rep, err := newUsageSvc(f).Daily(context.Background(), "t1", "2026-10-12", "2026-10-13")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Days[0].UsersSeen != 3 || rep.Days[1].UsersSeen != 2 || rep.Totals.UsersSeen != 0 || rep.Current != nil {
		t.Errorf("days %+v totals %+v current %v; want per-day usersSeen, never summed, no current without gauges", rep.Days, rep.Totals, rep.Current)
	}
}

func TestUsageRoller_WritesTodaysGaugeOverwrite(t *testing.T) {
	store := &fakeUsageStore{}
	g := &fakeGaugeStore{live: &usage.Gauge{UsersActive: 5, FileBytes: 10, Records: []usage.RecordCount{{Entity: "e", Count: 3}}}}
	r := NewUsageRoller(time.Hour, store, fakeTenants{"t1"}, nil).WithGauges(&UsageGaugeSource{Store: g, Entities: fakeEntities{{Name: "e"}}})
	r.now = func() time.Time { return gaugeNow }
	r.Sweep(context.Background())
	seen := map[string]int64{}
	for _, w := range store.writes {
		if w.mode != usage.Overwrite {
			continue
		}
		for _, row := range w.rows {
			if row.Day == "2026-10-14" && (row.Metric == usage.UsersActive || row.Metric == usage.FileBytes || row.Metric == usage.Records) {
				seen[row.Metric+"/"+row.Dim] = row.Value
			}
		}
	}
	if seen[usage.UsersActive+"/"] != 5 || seen[usage.FileBytes+"/"] != 10 || seen[usage.Records+"/e"] != 3 {
		t.Errorf("overwritten gauge rows for today = %v", seen)
	}
}
