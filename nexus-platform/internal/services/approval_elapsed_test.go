package services

// specs/043 S2 (#40) — "hồ sơ này duyệt mất bao lâu": GET /approvals/:id carries the elapsed time of
// the whole instance and of each step, measured on the tenant's working calendar from created_at
// and the persisted history marks (no column, no migration).

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/calmeta"
	"nexus/platform/internal/dynrepo"
)

// clockSvc is a detail service whose elapsed time is measured on the calendar store given, with the
// clock frozen at now. rec answers the record-visibility probe (nil = no record path).
func clockSvc(t *testing.T, inst *bpm.Instance, store timerCalendarReader, rec *fakeApprovalRecordReader, now time.Time) *ApprovalService {
	t.Helper()
	svc := NewApprovalService(&fakeApprovalProcRepo{}, &fakeApprovalInstRepo{inst: inst}, &fakeApprovalDecider{}).
		WithWorkingTime(mustCalc(t, store))
	if rec != nil {
		svc.WithRequestFlow(nil, rec)
	}
	svc.now = func() time.Time { return now }
	return svc
}

func ictTime(t *testing.T, y int, m time.Month, d, h, min int) time.Time {
	t.Helper()
	ict, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Fatal(err)
	}
	return time.Date(y, m, d, h, min, 0, 0, ict)
}

// friToMonInstance: sent 16:00 Friday 02/10/2026 (ICT), two pinned steps, history as given.
func friToMonInstance(t *testing.T, status bpm.InstanceStatus, current int, history ...bpm.HistoryEntry) *bpm.Instance {
	t.Helper()
	return &bpm.Instance{
		ID: "ai-40", TenantID: "t1", ProcessID: "p-40", EntityName: "crm_quotes", RecordID: "q-40",
		Status: status, CurrentStep: current, CurrentApproverType: bpm.ApproverUser, CurrentApproverID: "duyet2",
		CreatedBy: "submitter", CreatedAt: ictTime(t, 2026, 10, 2, 16, 0).UTC(),
		History: history,
		Definition: &bpm.Definition{Steps: []bpm.Step{
			{ID: "s1", Name: "Trưởng phòng", ApproverType: bpm.ApproverUser, ApproverID: "duyet1", SLASeconds: 4 * 3600},
			{ID: "s2", Name: "Giám đốc", ApproverType: bpm.ApproverUser, ApproverID: "duyet2", SLASeconds: 4 * 3600},
		}},
	}
}

func secs(p *int64) string {
	if p == nil {
		return "<nil>"
	}
	return time.Duration(*p * int64(time.Second)).String()
}

// TestApprovalDetail_BusinessDurations_FriToMon_3h is US1 acceptance #4: sent 16:00 Friday, approved
// 10:00 Monday on the Mon–Fri 08:00–17:00 calendar ⇒ "Mất 3 giờ làm việc" (1h Friday + 2h Monday),
// read off the persisted history marks — for the SUBMITTER too, who sees it through the record.
func TestApprovalDetail_BusinessDurations_FriToMon_3h(t *testing.T) {
	store := &fakeTimerCalendars{def: vnDefaultCalendar()}
	mon1000 := ictTime(t, 2026, 10, 5, 10, 0).UTC()

	t.Run("one step approved Monday 10:00 = 3 working hours, 66 wall hours", func(t *testing.T) {
		inst := friToMonInstance(t, bpm.StatusApproved, 0,
			bpm.HistoryEntry{Step: 0, Action: bpm.HistoryApproved, Actor: "duyet1", Node: "s1", StepName: "Trưởng phòng", At: mon1000})
		svc := clockSvc(t, inst, store, &fakeApprovalRecordReader{authzLevel: "owner"}, mon1000.Add(48*time.Hour))
		got, err := svc.GetApprovalDetail(context.Background(),
			RecordScope{UserID: "submitter", AccessLevel: string(dynrepo.AccessNone)}, "t1", "ai-40", bpm.Actor{UserID: "submitter"})
		if err != nil {
			t.Fatalf("submitter cannot read the detail: %v", err)
		}
		d := got.Durations
		if d == nil {
			t.Fatal("durations missing from the detail")
		}
		if d.Basis != bpm.SLABasisBusiness || secs(d.TotalBusinessSeconds) != "3h0m0s" || d.TotalWallSeconds != 66*3600 {
			t.Fatalf("total = %s business / %ds wall (basis %q); want 3h0m0s / %d (business)",
				secs(d.TotalBusinessSeconds), d.TotalWallSeconds, d.Basis, 66*3600)
		}
		if len(d.PerNode) != 1 {
			t.Fatalf("perNode = %+v; want one closed step", d.PerNode)
		}
		n := d.PerNode[0]
		if n.Step != 0 || n.Node != "s1" || n.StepName != "Trưởng phòng" || secs(n.BusinessSeconds) != "3h0m0s" ||
			n.EndedAt == nil || !n.EndedAt.Equal(mon1000) || !n.StartedAt.Equal(inst.CreatedAt) {
			t.Fatalf("perNode[0] = %+v (business %s); want s1 Trưởng phòng, created→Mon 10:00, 3h", n, secs(n.BusinessSeconds))
		}
	})

	t.Run("step 2 still open: measured up to now, no end", func(t *testing.T) {
		inst := friToMonInstance(t, bpm.StatusPending, 1,
			bpm.HistoryEntry{Step: 0, Action: bpm.HistorySubmitted, Actor: "submitter", Node: "s1", At: ictTime(t, 2026, 10, 2, 16, 0).UTC()},
			bpm.HistoryEntry{Step: 0, Action: bpm.HistoryApproved, Actor: "duyet1", Node: "s1", StepName: "Trưởng phòng", At: mon1000},
			// A delegation hands the SAME step on — it neither closes it nor opens a new one.
			bpm.HistoryEntry{Step: 1, Action: bpm.HistoryDelegated, Actor: "duyet2", Target: "duyet3", Node: "s2", At: mon1000.Add(time.Hour)})
		now := ictTime(t, 2026, 10, 5, 14, 30).UTC()
		got, err := clockSvc(t, inst, store, nil, now).GetApprovalDetail(context.Background(), RecordScope{}, "t1", "ai-40", bpm.Actor{UserID: "duyet2"})
		if err != nil {
			t.Fatalf("current approver cannot read the detail: %v", err)
		}
		d := got.Durations
		if d == nil || len(d.PerNode) != 2 {
			t.Fatalf("durations = %+v; want two steps (one closed, one open)", d)
		}
		open := d.PerNode[1]
		if open.Step != 1 || open.Node != "s2" || open.StepName != "Giám đốc" || open.EndedAt != nil ||
			!open.StartedAt.Equal(mon1000) || secs(open.BusinessSeconds) != "4h30m0s" {
			t.Fatalf("open step = %+v (business %s); want s2 Giám đốc from Mon 10:00, open, 4h30m", open, secs(open.BusinessSeconds))
		}
		if secs(d.TotalBusinessSeconds) != "7h30m0s" {
			t.Fatalf("total business = %s; want 7h30m0s (3h + 4h30m, still running)", secs(d.TotalBusinessSeconds))
		}
	})

	t.Run("rejected: the rejection closes the step and stops the clock", func(t *testing.T) {
		inst := friToMonInstance(t, bpm.StatusRejected, 0,
			bpm.HistoryEntry{Step: 0, Action: bpm.HistoryRejected, Actor: "duyet1", Comment: "Sai đơn giá", At: mon1000})
		got, err := clockSvc(t, inst, store, nil, mon1000.Add(72*time.Hour)).
			GetApprovalDetail(context.Background(), RecordScope{}, "t1", "ai-40", bpm.Actor{UserID: "admin", IsAdmin: true})
		if err != nil {
			t.Fatal(err)
		}
		d := got.Durations
		// A pre-S0 entry carries no node: the name comes from the pinned chain.
		if d == nil || len(d.PerNode) != 1 || d.PerNode[0].Node != "s1" || d.PerNode[0].StepName != "Trưởng phòng" ||
			secs(d.TotalBusinessSeconds) != "3h0m0s" {
			t.Fatalf("durations = %+v; want one closed step s1, total 3h", d)
		}
	})
}

// TestApprovalDetail_Durations_NoCalendarWall: a tenant with no live calendar gets wall-clock time
// that SAYS so — the business figure is absent, never a wall number dressed as working hours. A
// calendar store that cannot be read degrades the same way under its own label: the detail is where
// the approver decides, so a secondary readout must not take the page down.
func TestApprovalDetail_Durations_NoCalendarWall(t *testing.T) {
	mon1000 := ictTime(t, 2026, 10, 5, 10, 0).UTC()
	admin := bpm.Actor{UserID: "admin", IsAdmin: true}
	for _, tc := range []struct {
		name      string
		store     timerCalendarReader
		wantBasis bpm.SLABasis
	}{
		{"no calendar", &fakeTimerCalendars{}, bpm.SLABasisWallNoCalendar},
		{"calendar store unreadable", failingCalendars{err: errors.New("db down")}, bpm.SLABasisWallCalendarError},
		// A calendar with no working hours at all would read "0 working hours" for every stretch —
		// true of that calendar, and useless: same label as DueAt gives it (no reachable working time).
		{"calendar without working hours", &fakeTimerCalendars{def: &calmeta.BusinessCalendar{
			ID: "cal-empty", TenantID: "t1", Timezone: "Asia/Ho_Chi_Minh", IsDefault: true, Hours: json.RawMessage(`{}`),
		}}, bpm.SLABasisWallCalendarError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := friToMonInstance(t, bpm.StatusApproved, 0,
				bpm.HistoryEntry{Step: 0, Action: bpm.HistoryApproved, Actor: "duyet1", Node: "s1", At: mon1000})
			got, err := clockSvc(t, inst, tc.store, nil, mon1000).
				GetApprovalDetail(context.Background(), RecordScope{}, "t1", "ai-40", admin)
			if err != nil {
				t.Fatalf("detail err = %v; the elapsed readout must not fail the page", err)
			}
			d := got.Durations
			if d == nil || d.Basis != tc.wantBasis || d.TotalBusinessSeconds != nil || d.TotalWallSeconds != 66*3600 {
				t.Fatalf("durations = %+v; want basis %q, no business figure, 66h wall", d, tc.wantBasis)
			}
			if len(d.PerNode) != 1 || d.PerNode[0].BusinessSeconds != nil || d.PerNode[0].WallSeconds != 66*3600 {
				t.Fatalf("perNode = %+v; want 66h wall, no business figure", d.PerNode)
			}
		})
	}
}

// countingCalendars counts every calendar read.
type countingCalendars struct {
	inner timerCalendarReader
	reads int
}

func (c *countingCalendars) GetByID(ctx context.Context, tenantID, id string) (*calmeta.BusinessCalendar, error) {
	c.reads++
	return c.inner.GetByID(ctx, tenantID, id)
}

func (c *countingCalendars) GetDefault(ctx context.Context, tenantID string) (*calmeta.BusinessCalendar, error) {
	c.reads++
	return c.inner.GetDefault(ctx, tenantID)
}

// TestApprovalDetail_DurationsHiddenFromOutsider404: durations ride the detail, behind the same
// gate — a person who can neither decide the step nor see the record gets the unknown-id 404, and
// the calendar is not even read on their behalf.
func TestApprovalDetail_DurationsHiddenFromOutsider404(t *testing.T) {
	mon1000 := ictTime(t, 2026, 10, 5, 10, 0).UTC()
	inst := friToMonInstance(t, bpm.StatusApproved, 0,
		bpm.HistoryEntry{Step: 0, Action: bpm.HistoryApproved, Actor: "duyet1", Node: "s1", At: mon1000})
	store := &countingCalendars{inner: &fakeTimerCalendars{def: vnDefaultCalendar()}}
	svc := clockSvc(t, inst, store, &fakeApprovalRecordReader{err: ErrRecordNotFound}, mon1000)
	got, err := svc.GetApprovalDetail(context.Background(), RecordScope{UserID: "hotro1"}, "t1", "ai-40", bpm.Actor{UserID: "hotro1"})
	if codeOf(err) != "APPROVAL_INSTANCE_NOT_FOUND" || got != nil {
		t.Fatalf("outsider got (%+v, %v); want nil + APPROVAL_INSTANCE_NOT_FOUND", got, err)
	}
	if store.reads != 0 {
		t.Fatalf("calendar read %d time(s) for an outsider; want 0 — measure only what will be shown", store.reads)
	}
}
