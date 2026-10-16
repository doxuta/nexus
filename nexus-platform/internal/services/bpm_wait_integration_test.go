//go:build integration

// specs/043 S12a — the wait node over a freshly-migrated MySQL, through the production composition
// (record service → arming engine → runner → real record writes):
//   - a ticket that is still Open rests on the wait node: pending, NO open seat, the deadline picked by
//     its priority is on the instance AND written into the record's due field, exactly one bpm_sla_due
//     job is parked for the seq it stands at;
//   - an update of the record that answers it wakes ONLY that record's instance (after_update signal);
//   - a missed deadline is told once to sla.notify, and a re-delivered job changes nothing;
//   - the pending-by-record read is tenant-pinned.
//
// Run with:
//
//	TEST_DATABASE_URL=... go test -tags=integration -race ./internal/services/ -run 'TestBPMWait'
package services

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/jobs"
)

const waitNotified = "44444444-0000-0000-0000-0000000000a4"

func TestBPMWait_RestsSignalsAndBreaches_Integration(t *testing.T) {
	st := bpmWire(t)
	st.withDueJobs()
	ctx := context.Background()
	const tenant, entity = o3aTenantA, "wt_tickets"
	st.createEntity(t, ctx, tenant, entity, []FieldInput{
		{Name: "title", Label: "Tiêu đề", FieldType: "text"},
		{Name: "status", Label: "Trạng thái", FieldType: "picklist", Options: map[string]any{"values": []any{"Open", "Pending"}}},
		{Name: "priority", Label: "Ưu tiên", FieldType: "picklist", Options: map[string]any{"values": []any{"Low", "High"}}},
		{Name: "first_response_due", Label: "Hạn phản hồi", FieldType: "datetime"},
	})
	waitNode := ApprovalNodeInput{
		ApprovalStepInput: ApprovalStepInput{ID: "phan-hoi", Name: "Phản hồi đầu tiên"}, Kind: "wait",
		Until: &ApprovalConditionInput{Rules: []ApprovalCriteriaRuleInput{crit("status", "neq", "Open")}},
		SLA: &ApprovalWaitSLAInput{
			Amount: 4, Unit: "hours",
			Matrix:     []ApprovalSLARowInput{{When: ApprovalConditionInput{Rules: []ApprovalCriteriaRuleInput{crit("priority", "eq", "High")}}, Amount: 1, Unit: "minutes"}},
			WriteDueTo: "first_response_due",
			Notify:     &ApprovalAssignmentInput{Type: "user", ID: waitNotified},
		},
	}
	st.routedProcess(t, ctx, tenant, entity, "after_create", &ApprovalGraphInput{Nodes: []ApprovalNodeInput{waitNode, endNode("xong", "approved")}}, nil)

	mk := func(title, priority string) string {
		id, err := st.recordSvc.Create(ctx, bpmAdmin(rtSubmitter), tenant, rtSubmitter, entity,
			map[string]any{"title": title, "status": "Open", "priority": priority})
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		return id
	}
	slow, answered := mk("chờ", "High"), mk("đã trả lời", "Low")
	// Both rest first — if the answer were already on the record when the arming event is handled, the
	// walk would find it by itself and the signal would prove nothing.
	for _, rid := range []string{slow, answered} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			var resting int
			if err := st.sdb.Get(&resting, `SELECT COUNT(*) FROM sys_approval_instances WHERE tenant_id = ? AND record_id = ? AND status = 'pending' AND history LIKE '%"waiting"%'`, tenant, rid); err != nil {
				t.Fatalf("poll resting instance: %v", err)
			}
			if resting == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("record %s never rested on the wait node", rid)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	// the answer arrives for ONE of them, by an update event of its own
	if err := st.recordSvc.Update(ctx, bpmAdmin(rtSubmitter), tenant, entity, answered, map[string]any{"status": "Pending"}, nil); err != nil {
		t.Fatalf("answer the second ticket: %v", err)
	}
	st.drain(t)

	// the one still Open rests: no seat, deadline from the priority row, due field written, one job
	r := st.lastInstanceOf(t, tenant, entity, slow)
	if r.Status != bpm.StatusPending || r.NodeID() != "phan-hoi" || r.SLADueAt == nil {
		t.Fatalf("open ticket: %s on %s due %v; want pending on phan-hoi with a deadline", r.Status, r.NodeID(), r.SLADueAt)
	}
	if d := time.Until(*r.SLADueAt); d < 40*time.Second || d > 80*time.Second {
		t.Fatalf("deadline in %v; want ≈1 minute (the High row, not the 4-hour base)", d)
	}
	var open int
	if err := st.sdb.Get(&open, `SELECT COUNT(*) FROM sys_bpm_tasks WHERE tenant_id = ? AND instance_id = ? AND status = 'open'`, tenant, r.ID); err != nil || open != 0 {
		t.Fatalf("open seats of a wait node = %d (%v); want 0", open, err)
	}
	if got := st.routedField(t, tenant, entity, slow, "first_response_due"); got == nil || got == "" {
		t.Fatalf("first_response_due = %v; want the deadline written onto the record", got)
	}
	_, seqs := dueJobsOf(t, st, tenant, r.ID)
	n := 0
	for _, q := range seqs {
		if q == r.Seq {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d bpm_sla_due jobs for seq %d (all %v); want exactly one", n, r.Seq, seqs)
	}

	// the answered one was woken by the update — and only it
	a := st.lastInstanceOf(t, tenant, entity, answered)
	if a.Status != bpm.StatusApproved {
		t.Fatalf("answered ticket: %s; want approved by the after_update signal", a.Status)
	}
	var sawMet bool
	for _, act := range historyActions(a) {
		sawMet = sawMet || act == bpm.HistoryWaitMet
	}
	if !sawMet {
		t.Fatalf("answered history %v; want a wait_met line", historyActions(a))
	}

	// the pending-by-record read is tenant-pinned
	if got, err := st.repo.ListPendingByRecord(ctx, tenant, entity, slow); err != nil || len(got) != 1 {
		t.Fatalf("pending of the open ticket = %d (%v); want 1", len(got), err)
	}
	if got, err := st.repo.ListPendingByRecord(ctx, o3aTenantB, entity, slow); err != nil || len(got) != 0 {
		t.Fatalf("another tenant sees %d pending instances of this record (%v)", len(got), err)
	}
	if got, err := st.repo.ListPendingByRecord(ctx, tenant, entity, answered); err != nil || len(got) != 0 {
		t.Fatalf("finished instance listed as pending: %d (%v)", len(got), err)
	}

	// the deadline passes: the job tells waitNotified once; a re-delivery changes nothing
	if _, err := st.sdb.Exec(`UPDATE sys_approval_instances SET sla_due_at = ? WHERE tenant_id = ? AND id = ?`,
		time.Now().UTC().Add(-time.Minute), tenant, r.ID); err != nil {
		t.Fatalf("age the deadline: %v", err)
	}
	rows, _ := dueJobsOf(t, st, tenant, r.ID)
	var job jobs.Job
	for _, row := range rows {
		var p struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal(row.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Seq == r.Seq {
			job = jobs.Job{ID: row.ID, TenantID: row.Tenant, Type: bpm.SLADueJobType, Payload: row.Payload}
		}
	}
	h := bpm.NewSLAScheduler(bpm.SLASchedulerConfig{}, st.repo, st.repo, nil, nil).NewSLADueJobHandler(st.repo)
	before := st.notifCount(t, tenant, waitNotified)
	if err := h(ctx, job); err != nil {
		t.Fatalf("breach job: %v", err)
	}
	if err := h(ctx, job); err != nil {
		t.Fatalf("breach job re-delivery: %v", err)
	}
	if got := st.notifCount(t, tenant, waitNotified); got != before+1 {
		t.Fatalf("notices %d -> %d; want exactly one \"Quá hạn\"", before, got)
	}
	after := st.lastInstanceOf(t, tenant, entity, slow)
	if !after.Escalated || escalatedEntries(after) != 1 || after.Status != bpm.StatusPending {
		t.Fatalf("after breach: escalated=%v entries=%d status=%s; want escalated once and still waiting", after.Escalated, escalatedEntries(after), after.Status)
	}
	if after.Stuck() {
		t.Fatal("a wait node that is merely overdue was flagged stuck")
	}
}
