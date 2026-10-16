//go:build integration

// specs/043 S1 (#39) over real MySQL: the basis of a deadline is stored where the plan says (the
// pin for the first step, the decision entry for later ones), survives the JSON columns, and is
// read back by the approval detail — with the calendar resolved from a real sys_business_calendars
// row through the SAME resolver the server wires. The exact 16:00-Friday case is a unit test (fixed
// clock); here the deadline is checked against the calendar arithmetic for the real submit time.
//
//	TEST_DATABASE_URL=... go test -tags=integration ./internal/services/ -run BPM_BusinessClock
package services

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/calendar"
	"nexus/platform/internal/calmeta"
)

func TestBPM_BusinessClock_PersistsBasis(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const submitter = "11111111-0000-0000-0000-000000000391"
	const approver1 = "22222222-0000-0000-0000-000000000392"
	const approver2 = "33333333-0000-0000-0000-000000000393"
	hours := []byte(`{"mon":[{"start":"08:00","end":"17:00"}],"tue":[{"start":"08:00","end":"17:00"}],
		"wed":[{"start":"08:00","end":"17:00"}],"thu":[{"start":"08:00","end":"17:00"}],"fri":[{"start":"08:00","end":"17:00"}]}`)
	calendars := calmeta.NewSQLRepository(st.sdb)
	if _, err := calendars.Create(ctx, calmeta.CreateInput{TenantID: o3aTenantA, Name: "Giờ hành chính",
		Timezone: "Asia/Ho_Chi_Minh", Hours: hours, IsDefault: true}); err != nil {
		t.Fatalf("seed calendar: %v", err)
	}
	sched, err := calendar.FromJSON("Asia/Ho_Chi_Minh", hours, nil)
	if err != nil {
		t.Fatal(err)
	}
	calc, err := NewBPMDeadlineCalculator(NewWorkflowScheduleResolver(calendars, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	requester := bpm.NewRequester(st.repo, st.repo, st.repo, st.repo, nil).WithDeadlines(calc)
	decider := bpm.NewDecider(st.repo, st.repo, st.repo, nil, nil).WithDeadlines(calc)

	// Tenant A has the calendar; tenant B has none (#82).
	for _, tenant := range []string{o3aTenantA, o3aTenantB} {
		st.createEntity(t, ctx, tenant, "quotes39", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
		if _, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
			Name: "duyet-39", EntityName: "quotes39", Trigger: "manual",
			Steps: []ApprovalStepInput{
				{Name: "Trưởng phòng", ApproverType: "user", ApproverID: approver1, SLASeconds: 4 * 3600},
				{Name: "Giám đốc", ApproverType: "user", ApproverID: approver2, SLASeconds: 4 * 3600, SLAClock: "wall"},
			},
			IsActive: boolPtr(true), ActorID: submitter,
		}); err != nil {
			t.Fatalf("CreateProcess %s: %v", tenant, err)
		}
	}
	submit := func(tenant string) (*bpm.Instance, time.Time, time.Time) {
		t.Helper()
		id, err := st.recordSvc.Create(ctx, bpmAdmin(submitter), tenant, submitter, "quotes39", map[string]any{"status": "draft"})
		if err != nil {
			t.Fatalf("create quote: %v", err)
		}
		before := time.Now().UTC().Truncate(time.Second)
		sub, err := requester.Submit(ctx, tenant, "quotes39", id, map[string]any{"status": "draft"}, bpm.Actor{UserID: submitter})
		after := time.Now().UTC().Add(time.Second)
		if err != nil {
			t.Fatalf("Submit %s: %v", tenant, err)
		}
		inst, err := st.repo.GetInstance(ctx, tenant, sub.ID) // the ROW, not the returned struct
		if err != nil {
			t.Fatalf("GetInstance: %v", err)
		}
		return inst, before, after
	}

	// With a calendar: first step on working hours; the pin says so explicitly.
	inst, before, after := submit(o3aTenantA)
	lo, _ := sched.AddBusinessDuration(before, 4*time.Hour)
	hi, _ := sched.AddBusinessDuration(after, 4*time.Hour)
	if inst.SLADueAt == nil || inst.SLADueAt.Before(lo) || inst.SLADueAt.After(hi) {
		t.Fatalf("first-step due = %v, want the calendar's answer in [%s, %s]", inst.SLADueAt, lo, hi)
	}
	if d := inst.Definition; d == nil || d.Steps[0].SLAClock != "business" || d.Steps[1].SLAClock != "wall" || d.FirstDueBasis != bpm.SLABasisBusiness {
		t.Fatalf("persisted pin = %+v, want clocks [business wall] + firstDueBasis business", d)
	}
	if b := inst.CurrentSLABasis(); b != bpm.SLABasisBusiness {
		t.Fatalf("basis from the row = %q, want business", b)
	}

	// Step 2 is wall clock by configuration: the decision entry records that basis, and the detail
	// endpoint reads it back from the stored history.
	start := time.Now().UTC().Truncate(time.Second)
	if _, err := decider.Decide(ctx, o3aTenantA, inst.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: approver1}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	row, err := st.repo.GetInstance(ctx, o3aTenantA, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.SLADueAt == nil || row.SLADueAt.Before(start.Add(4*time.Hour)) || row.SLADueAt.After(time.Now().UTC().Add(4*time.Hour+time.Second)) {
		t.Fatalf("second-step due = %v, want now + 4h wall clock", row.SLADueAt)
	}
	last := row.History[len(row.History)-1]
	if last.Action != bpm.HistoryApproved || last.SLABasis != bpm.SLABasisWall {
		b, _ := json.Marshal(row.History)
		t.Fatalf("stored history = %s, want the approval entry to carry slaBasis wall", b)
	}
	detail, err := st.approval.GetApprovalDetail(ctx, RecordScope{}, o3aTenantA, inst.ID, bpm.Actor{UserID: approver2})
	if err != nil {
		t.Fatalf("GetApprovalDetail: %v", err)
	}
	if detail.SLA == nil || detail.SLA.Basis != bpm.SLABasisWall || !detail.SLA.DueAt.Equal(*row.SLADueAt) {
		t.Fatalf("detail sla = %+v, want {dueAt: %v, basis: wall}", detail.SLA, row.SLADueAt)
	}

	// No calendar: wall clock, and the basis says why.
	instB, beforeB, afterB := submit(o3aTenantB)
	if instB.SLADueAt == nil || instB.SLADueAt.Before(beforeB.Add(4*time.Hour)) || instB.SLADueAt.After(afterB.Add(4*time.Hour)) {
		t.Fatalf("no-calendar due = %v, want now + 4h", instB.SLADueAt)
	}
	if b := instB.CurrentSLABasis(); b != bpm.SLABasisWallNoCalendar {
		t.Fatalf("no-calendar basis = %q, want wall_no_calendar", b)
	}
}
