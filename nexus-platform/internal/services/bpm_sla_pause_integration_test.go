//go:build integration

// specs/043 S13 — the SLA clock stops while the record meets options.pauseWhen, on MySQL through the
// production composition: record update → engine event → pause/resume → columns, seats, history.
// The engine is single-worker and FIFO, so create → update → update is armed, paused, resumed in order.
package services

import (
	"context"
	"testing"
	"time"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/workflow"
)

// pausableRecord stands up a one-step process (1 wall hour) that pauses while status = Pending and
// creates a record under it. The caller sends the updates, then drains.
func pausableRecord(t *testing.T, st *bpmStack, entity string) (tenant, recordID string) {
	t.Helper()
	ctx := context.Background()
	tenant = o3aTenantA
	st.createEntity(t, ctx, tenant, entity, []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	if _, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: entity + "-p", EntityName: entity, Trigger: "after_create",
		Steps: []ApprovalStepInput{{Name: "Trưởng phòng", ApproverType: "user", ApproverID: seatAp1, SLASeconds: 3600, SLAClock: "wall"}},
		Options: &ApprovalOptionsInput{PauseWhen: &workflow.Condition{
			Rules: []workflow.Rule{{Field: "status", Op: "eq", Value: "Pending"}},
		}},
		IsActive: boolPtr(true), ActorID: seatSubmitter,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	rid, err := st.recordSvc.Create(ctx, bpmAdmin(seatSubmitter), tenant, seatSubmitter, entity, map[string]any{"status": "Open"})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}
	return tenant, rid
}

func setStatus(t *testing.T, st *bpmStack, tenant, entity, rid, status string) {
	t.Helper()
	if err := st.recordSvc.Update(context.Background(), bpmAdmin(seatSubmitter), tenant, entity, rid, map[string]any{"status": status}, nil); err != nil {
		t.Fatalf("update record to %s: %v", status, err)
	}
}

func openSeatDue(t *testing.T, st *bpmStack, tenant, id string) (n int, due *time.Time) {
	t.Helper()
	var rows []struct {
		DueAt *time.Time `db:"due_at"`
	}
	if err := st.sdb.Select(&rows, `SELECT due_at FROM sys_bpm_tasks WHERE tenant_id = ? AND instance_id = ? AND status = 'open'`, tenant, id); err != nil {
		t.Fatalf("read seats: %v", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return len(rows), rows[0].DueAt
}

// TestSLAPause_RecordUpdateFreezesTheClock_Integration (T192/T201, engine half): the record turns
// Pending ⇒ the instance loses its due date, keeps the remaining seconds and its open seat (due
// cleared with it), and says "paused".
func TestSLAPause_RecordUpdateFreezesTheClock_Integration(t *testing.T) {
	st := bpmWire(t)
	st.withDueJobs()
	tenant, rid := pausableRecord(t, st, "slapause")
	setStatus(t, st, tenant, "slapause", rid, "Pending")
	st.drain(t)

	inst := st.onlyPendingInstance(t, tenant, "slapause", rid)
	if inst.SLADueAt != nil || inst.SLAPausedAt == nil || inst.SLARemainingSeconds == nil {
		t.Fatalf("instance due %v pausedAt %v remaining %v; want no due, a pause mark and the remaining seconds", inst.SLADueAt, inst.SLAPausedAt, inst.SLARemainingSeconds)
	}
	if r := *inst.SLARemainingSeconds; r < 3500 || r > 3600 {
		t.Fatalf("remaining = %ds; want about the whole hour left (3500..3600)", r)
	}
	if n, due := openSeatDue(t, st, tenant, inst.ID); n != 1 || due != nil {
		t.Fatalf("open seats = %d, due %v; want one seat still open with its due cleared", n, due)
	}
	if last := inst.History[len(inst.History)-1]; last.Action != bpm.HistoryPaused {
		t.Fatalf("last history %+v; want paused", last)
	}
}

// TestSLAPause_ResumeRestoresDueAndParksJob_Integration (T192/T201): back to Open ⇒ a due date again
// (the remaining time from NOW), the seat's due restored, a fresh bpm_sla_due job for the new seq,
// and the pause marks gone.
func TestSLAPause_ResumeRestoresDueAndParksJob_Integration(t *testing.T) {
	st := bpmWire(t)
	st.withDueJobs()
	tenant, rid := pausableRecord(t, st, "slaresume")
	setStatus(t, st, tenant, "slaresume", rid, "Pending")
	setStatus(t, st, tenant, "slaresume", rid, "Open")
	st.drain(t)

	inst := st.onlyPendingInstance(t, tenant, "slaresume", rid)
	if inst.SLAPausedAt != nil || inst.SLARemainingSeconds != nil || inst.SLADueAt == nil {
		t.Fatalf("instance due %v pausedAt %v remaining %v; want a running clock", inst.SLADueAt, inst.SLAPausedAt, inst.SLARemainingSeconds)
	}
	if d := time.Until(*inst.SLADueAt); d < 50*time.Minute || d > 61*time.Minute {
		t.Fatalf("due in %v; want about the hour that was left", d)
	}
	if n, due := openSeatDue(t, st, tenant, inst.ID); n != 1 || due == nil || !due.Equal(*inst.SLADueAt) {
		t.Fatalf("open seats = %d, due %v; want the seat due = instance due %v", n, due, inst.SLADueAt)
	}
	_, seqs := dueJobsOf(t, st, tenant, inst.ID)
	live := 0
	for _, q := range seqs {
		if q == inst.Seq {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("%d bpm_sla_due jobs for seq %d (all %v); want exactly one", live, inst.Seq, seqs)
	}
}
