//go:build integration

// specs/043 S3c — the durable approval-breach job (bpm_sla_due) over a freshly-migrated MySQL:
//   - every state write that leaves a step with a deadline parks exactly one job for (instance, seq)
//     (T290 invariant — arm, decide, delegate);
//   - the job alone (the sweep never run) escalates the step, pings every holder of a role seat once
//     (capped), and a second delivery is a no-op;
//   - a job of another tenant never touches the instance.
//
// Run with:
//
//	TEST_DATABASE_URL=... go test -tags=integration -race ./internal/services/ -run 'TestSLADue'
package services

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/jobs"
)

// withDueJobs attaches the job queue to the stack's repository the way main.go does.
func (s *bpmStack) withDueJobs() *jobs.SQLStore {
	store := jobs.NewSQLStore(s.sdb, 5)
	s.repo.WithSLADueJobs(store)
	return store
}

type dueJobRow struct {
	ID      string          `db:"id"`
	Tenant  string          `db:"tenant_id"`
	Payload json.RawMessage `db:"payload"`
	RunAt   time.Time       `db:"run_at"`
}

// dueJobsOf returns the bpm_sla_due jobs parked for one instance, with the seq each carries.
func dueJobsOf(t *testing.T, st *bpmStack, tenant, instanceID string) (rows []dueJobRow, seqs []int64) {
	t.Helper()
	if err := st.sdb.Select(&rows,
		`SELECT id, tenant_id, payload, run_at FROM sys_jobs WHERE tenant_id = ? AND job_type = ? AND payload->>'$.instanceId' = ? ORDER BY created_at, id`,
		tenant, bpm.SLADueJobType, instanceID); err != nil {
		t.Fatalf("read due jobs: %v", err)
	}
	for _, r := range rows {
		var p struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			t.Fatalf("payload %s: %v", r.Payload, err)
		}
		seqs = append(seqs, p.Seq)
	}
	return rows, seqs
}

// TestSLADue_InvariantJobPerSeqWhenDueSet (T290): after EVERY state write that leaves sla_due_at
// set on a pending instance, a job exists for the seq the instance now stands at — and one only.
// Table-driven: later slices add their transitions as rows.
func TestSLADue_InvariantJobPerSeqWhenDueSet(t *testing.T) {
	st := bpmWire(t)
	st.withDueJobs()
	ctx := context.Background()
	tenant, id := armTwoStep(t, st, "sladueinv")

	steps := []struct {
		name string
		do   func() error
	}{
		{"arm (CreateInstance)", func() error { return nil }},
		{"delegate (same step, same due)", func() error {
			_, err := st.requester.Delegate(ctx, tenant, id, seatAp3, "nghỉ phép", bpm.Actor{UserID: seatAp1})
			return err
		}},
		{"decide step 1 -> step 2", func() error {
			_, err := st.approval.Decide(ctx, tenant, id, bpm.DecisionApprove, "ok", bpm.Actor{UserID: seatAp3})
			return err
		}},
		{"repin to a new version (S11, T292)", func() error {
			var pid string
			if err := st.sdb.Get(&pid, `SELECT id FROM sys_approval_processes WHERE tenant_id = ? AND entity_name = ? AND deleted_at IS NULL`, tenant, "sladueinv"); err != nil {
				return err
			}
			if _, err := st.approval.UpdateProcess(ctx, tenant, pid, verBody("sladueinv", "sladueinv-2",
				repinStep("s1", "Trưởng phòng", seatAp1), repinStep("s2", "Kế toán trưởng", seatAp2))); err != nil { // v2 renames s2
				return err
			}
			out, err := st.approval.RepinInstances(ctx, tenant, pid, bpm.RepinInput{InstanceIDs: []string{id}, By: seatAp1})
			if err != nil {
				return err
			}
			if len(out.Results) != 1 || !out.Results[0].OK {
				return fmt.Errorf("repin = %+v", out.Results)
			}
			return nil
		}},
	}
	for _, s := range steps {
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		inst, err := st.repo.GetInstance(ctx, tenant, id)
		if err != nil {
			t.Fatalf("%s: GetInstance: %v", s.name, err)
		}
		if inst.SLADueAt == nil {
			t.Fatalf("%s: fixture lost its deadline", s.name)
		}
		_, seqs := dueJobsOf(t, st, tenant, id)
		n := 0
		for _, q := range seqs {
			if q == inst.Seq {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%s: %d jobs for seq %d (all seqs %v); want exactly one", s.name, n, inst.Seq, seqs)
		}
	}
	// The terminal decision leaves no deadline and no new job.
	if _, err := st.approval.Decide(ctx, tenant, id, bpm.DecisionApprove, "ok", bpm.Actor{UserID: seatAp2}); err != nil {
		t.Fatalf("final decide: %v", err)
	}
	_, before := dueJobsOf(t, st, tenant, id)
	if len(before) != 4 {
		t.Fatalf("jobs after terminal decision = %v; want the four of the earlier writes only", before)
	}
}

// TestSLADueJob_FiresWithSchedulerDisabled_Integration: the sweep is never run; the claimed job
// alone escalates the step, pings the approver once, and a re-delivery changes nothing.
func TestSLADueJob_FiresWithSchedulerDisabled_Integration(t *testing.T) {
	st := bpmWire(t)
	st.withDueJobs()
	ctx := context.Background()
	rid := armBreached(t, st, o3aTenantA, "sladuefire")
	st.drain(t)
	inst := st.onlyPendingInstance(t, o3aTenantA, "sladuefire", rid)
	rows, _ := dueJobsOf(t, st, o3aTenantA, inst.ID)
	if len(rows) != 1 {
		t.Fatalf("jobs = %d; want 1 parked at arm", len(rows))
	}
	if !rows[0].RunAt.UTC().Truncate(time.Second).Equal(inst.SLADueAt.UTC().Truncate(time.Second)) {
		t.Fatalf("job run_at %v != sla_due_at %v", rows[0].RunAt, inst.SLADueAt)
	}

	time.Sleep(1200 * time.Millisecond) // the step's SLA is 1s
	h := bpm.NewSLAScheduler(bpm.SLASchedulerConfig{}, st.repo, st.repo, nil, nil).NewSLADueJobHandler(st.repo)
	job := jobs.Job{ID: rows[0].ID, TenantID: rows[0].Tenant, Type: bpm.SLADueJobType, Payload: rows[0].Payload}
	before := st.notifCount(t, o3aTenantA, seatAp1)
	if err := h(ctx, job); err != nil {
		t.Fatalf("handler: %v", err)
	}
	after, _ := st.repo.GetInstance(ctx, o3aTenantA, inst.ID)
	if !after.Escalated || escalatedEntries(after) != 1 {
		t.Fatalf("escalated=%v entries=%d; want escalated once", after.Escalated, escalatedEntries(after))
	}
	if got := st.notifCount(t, o3aTenantA, seatAp1); got != before+1 {
		t.Fatalf("notices %d -> %d; want exactly one breach notice", before, got)
	}
	if err := h(ctx, job); err != nil {
		t.Fatalf("re-delivery: %v", err)
	}
	again, _ := st.repo.GetInstance(ctx, o3aTenantA, inst.ID)
	if escalatedEntries(again) != 1 || st.notifCount(t, o3aTenantA, seatAp1) != before+1 {
		t.Fatalf("re-delivery double-fired")
	}
	// A job of another tenant carrying this instance id finds nothing.
	other := jobs.Job{ID: "x", TenantID: o3aTenantB, Type: bpm.SLADueJobType, Payload: rows[0].Payload}
	if err := h(ctx, other); err != nil {
		t.Fatalf("cross-tenant job: %v", err)
	}
}

// TestBreach_RoleSeatNotifiesEveryHolderOnce_Integration: a role seat pings each active user
// carrying the role (not a disabled one, not another tenant's), once, capped at 50.
func TestBreach_RoleSeatNotifiesEveryHolderOnce_Integration(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	tenant := o3aTenantA
	role := uuid.NewString()
	if _, err := st.sdb.Exec(`INSERT INTO acl_roles (id, tenant_id, name, label) VALUES (?, ?, 'sla_mgr', 'Quản lý')`, role, tenant); err != nil {
		t.Fatalf("role: %v", err)
	}
	mkUser := func(n int, tenantID, status string) string {
		id := uuid.NewString()
		if _, err := st.sdb.Exec(`INSERT INTO users (id, tenant_id, email, password_hash, status, role_id) VALUES (?, ?, ?, 'x', ?, ?)`,
			id, tenantID, fmt.Sprintf("sla%d-%s@x.vn", n, id[:6]), status, role); err != nil {
			t.Fatalf("user: %v", err)
		}
		return id
	}
	var holders []string
	for i := 0; i < 3; i++ {
		holders = append(holders, mkUser(i, tenant, "active"))
	}
	disabled := mkUser(90, tenant, "disabled")

	st.createEntity(t, ctx, tenant, "sladuerole", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	if _, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: "sladuerole-p", EntityName: "sladuerole", Trigger: "after_create",
		Steps:    []ApprovalStepInput{{Name: "Quản lý", ApproverType: "role", ApproverID: role, SLASeconds: 1}},
		IsActive: boolPtr(true), ActorID: seatSubmitter,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	rid, err := st.recordSvc.Create(ctx, bpmAdmin(seatSubmitter), tenant, seatSubmitter, "sladuerole", map[string]any{"status": "x"})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}
	st.drain(t)
	st.onlyPendingInstance(t, tenant, "sladuerole", rid)

	s := bpm.NewSLAScheduler(bpm.SLASchedulerConfig{}, st.repo, st.repo, nil, nil)
	now := time.Now().UTC().Add(time.Hour)
	s.RunBreachesForTest(ctx, now)
	s.RunBreachesForTest(ctx, now)
	for _, u := range holders {
		var n int
		if err := st.sdb.Get(&n, `SELECT COUNT(*) FROM sys_notifications WHERE tenant_id = ? AND user_id = ? AND title LIKE 'Quá hạn%'`, tenant, u); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("holder %s got %d breach notices; want 1", u, n)
		}
	}
	if n := st.notifCount(t, tenant, disabled); n != 0 {
		t.Fatalf("a disabled user was pinged (%d)", n)
	}

	// The cap: 60 more holders of the role ⇒ resolving the seat yields 50, never more.
	for i := 0; i < 60; i++ {
		mkUser(100+i, tenant, "active")
	}
	got, err := st.repo.ResolveSeatUsers(ctx, tenant, []bpm.Seat{{AssigneeType: bpm.ApproverRole, AssigneeID: role}}, 50)
	if err != nil || len(got) != 50 {
		t.Fatalf("ResolveSeatUsers = %d ids, err %v; want 50", len(got), err)
	}
	if other, err := st.repo.ResolveSeatUsers(ctx, o3aTenantB, []bpm.Seat{{AssigneeType: bpm.ApproverRole, AssigneeID: role}}, 50); err != nil || len(other) != 0 {
		t.Fatalf("another tenant resolved the role's users: %v %v", other, err)
	}
}
