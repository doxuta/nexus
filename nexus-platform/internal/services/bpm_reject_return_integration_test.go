//go:build integration

// specs/043 S8 — `bpm-reject-return` (tasks T134–T138, T291): a rejection that returns the approval
// to an earlier node or to its submitter, proven against real MySQL through the production
// composition (Decider + ApprovalService + WorkItemService + the async arming engine).
//
// What it promises:
//   - "về người gửi" keeps the instance PENDING with an open `rework` seat for the submitter, the
//     record gets outcome.returnedValue, and editing the record — with an after_update process
//     live — does NOT arm a second approval (FR-029);
//   - "Gửi lại" (complete on the rework seat) resumes at the rejector with a new deadline and a
//     bpm_sla_due job for the new seq;
//   - "về bước X" opens X as a new visit with its own deadline, and the old click of the rejector
//     is a conflict, not an answer about who they are;
//   - a creator who is not a live user falls back to the record's owner.
//
// Run with:
//
//	TEST_DATABASE_URL=... bash scripts/run-integration.sh -run 'TestReject_|TestResubmit_|TestSLADue_Return' ./internal/services/
package services

import (
	"context"
	"fmt"
	"testing"
	"time"

	"nexus/platform/internal/bpm"
)

type rrStack struct {
	*bpmStack
	decider  *bpm.Decider
	approval *ApprovalService
	work     *WorkItemService
}

// rrWire is the production composition for the reject-return slice: the decider carries the
// outcome writer AND the submitter resolver exactly as main.go wires them.
func rrWire(t *testing.T) *rrStack {
	t.Helper()
	st := bpmWire(t)
	st.withDueJobs()
	decider := bpm.NewDecider(st.repo, st.repo, st.repo, nil, nil).
		WithRecordWriter(NewApprovalOutcomeWriter(st.recordSvc)).
		WithSubmitterResolver(NewApprovalSubmitterResolver(st.repo, st.recordSvc))
	approval := NewApprovalService(st.repo, st.repo, decider).WithEntityChecker(st.approvalEntities()).WithRequestFlow(st.requester, st.recordSvc)
	return &rrStack{
		bpmStack: st, decider: decider, approval: approval,
		work: NewWorkItemService(st.repo, decider, st.recordSvc, wiACL{}, nil),
	}
}

// rrUsers makes the three people of the fixture real, active users of the tenant.
func (s *rrStack) rrUsers(t *testing.T, tenant string) {
	t.Helper()
	for i, id := range []string{seatSubmitter, seatAp1, seatAp2} {
		if _, err := s.sdb.Exec(`INSERT IGNORE INTO users (id, tenant_id, email, password_hash, status) VALUES (?, ?, ?, 'x', 'active')`,
			id, tenant, fmt.Sprintf("rr%d-%s@x.vn", i, id[:8])); err != nil {
			t.Fatalf("user %s: %v", id, err)
		}
	}
}

// rrWaitInstance polls until the async engine has armed the record's instance.
func (s *rrStack) rrWaitInstance(t *testing.T, tenant, entity, rid string) *bpm.Instance {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.instanceCount(t, tenant, entity, rid) > 0 {
			return s.onlyPendingInstance(t, tenant, entity, rid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no instance armed for %s/%s", entity, rid)
	return nil
}

func (s *rrStack) rrStatus(t *testing.T, tenant, entity, rid string) string {
	t.Helper()
	rec, err := s.recordSvc.Get(context.Background(), bpmAdmin(seatSubmitter), tenant, entity, rid, nil, nil)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	v, _ := rec.Data["status"].(string)
	return v
}

func rrOpenSeat(t *testing.T, s *rrStack, tenant, instID string) seatRow {
	t.Helper()
	open := openSeats(seatsOf(t, s.sdb, tenant, instID))
	if len(open) != 1 {
		t.Fatalf("open seats = %+v; want exactly one", open)
	}
	return open[0]
}

// T135 + T138 + T136: reject → back to the submitter, with an after_update process live.
func TestReject_ReturnToSubmitter_EditDoesNotRearm(t *testing.T) {
	st := rrWire(t)
	ctx := context.Background()
	tenant, entity := o3aTenantA, "rrsubmitter"
	st.rrUsers(t, tenant)
	st.createEntity(t, ctx, tenant, entity, []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	if _, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: entity + "-p", EntityName: entity, Trigger: "after_update",
		Steps: []ApprovalStepInput{
			{ID: "s1", Name: "Trưởng phòng", ApproverType: "user", ApproverID: seatAp1, SLASeconds: 3600, SLAClock: "wall"},
			{ID: "s2", Name: "Kế toán", ApproverType: "user", ApproverID: seatAp2, SLASeconds: 7200, SLAClock: "wall",
				OnReject: &ApprovalOnRejectInput{To: "@submitter"}},
		},
		Outcome:  &ApprovalOutcomeInput{StatusField: "status", ApprovedValue: "Chấp nhận", ReturnedValue: "Cần bổ sung"},
		IsActive: boolPtr(true), ActorID: seatSubmitter,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	rid, err := st.recordSvc.Create(ctx, bpmAdmin(seatSubmitter), tenant, seatSubmitter, entity, map[string]any{"status": "Nháp"})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}
	// The submitter's edit starts the approval (after_update).
	if err := st.recordSvc.Update(ctx, bpmAdmin(seatSubmitter), tenant, entity, rid, map[string]any{"status": "Chờ duyệt"}, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	inst := st.rrWaitInstance(t, tenant, entity, rid)
	if _, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: seatAp1}); err != nil {
		t.Fatalf("approve s1: %v", err)
	}

	// An empty reason is refused before anything moves (US2 AS1).
	_, err = st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionReject, "  ", bpm.Actor{UserID: seatAp2})
	wantItemCode(t, err, "APPROVAL_REJECT_REASON_REQUIRED")

	// duyet2 rejects with a reason: the approval goes back to the submitter.
	got, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionReject, "Sai đơn giá", bpm.Actor{UserID: seatAp2})
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if got.Status != bpm.StatusPending || got.CurrentStep != 1 || got.SLADueAt != nil {
		t.Fatalf("after the return: status=%s step=%d due=%v; want pending on s2 with no deadline", got.Status, got.CurrentStep, got.SLADueAt)
	}
	seat := rrOpenSeat(t, st, tenant, inst.ID)
	if seat.Kind != bpm.SeatKindRework || seat.AssigneeID != seatSubmitter || seat.NodeID != "s2" || str(seat.Comment) != "Sai đơn giá" {
		t.Fatalf("open seat = %+v; want a rework seat of the submitter on s2 carrying the reason", seat)
	}
	// The engine's own seat reader (what repin / planRepin rewrite from) keeps the reason.
	if open, err := st.repo.ListOpenSeats(ctx, tenant, inst.ID); err != nil || len(open) != 1 || open[0].Note != "Sai đơn giá" {
		t.Fatalf("ListOpenSeats = %+v err=%v; want the rework seat carrying Note Sai đơn giá", open, err)
	}
	if v := st.rrStatus(t, tenant, entity, rid); v != "Cần bổ sung" {
		t.Fatalf("record status = %q; want the returnedValue Cần bổ sung (T138)", v)
	}
	if _, seqs := dueJobsOf(t, st.bpmStack, tenant, inst.ID); len(seqs) != 2 {
		t.Fatalf("due jobs %v; want the two parked before the return (arm, s1->s2) and none for the deadline-less rework seat", seqs)
	}

	// The submitter's pending list flags the returned row, so the inbox offers it no Duyệt / Từ chối.
	if pend, err := st.approval.ListPending(ctx, tenant, bpm.Actor{UserID: seatSubmitter}); err != nil || len(pend) != 1 || !pend[0].InRework {
		t.Fatalf("pending = %+v err=%v; want the returned row flagged InRework", pend, err)
	}
	// The submitter's inbox shows the rework item with the reason (view=mine).
	page, err := st.work.List(ctx, tenant, WorkItemActor{UserID: seatSubmitter}, "mine", "", 50, "")
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("mine = %+v err=%v; want exactly the rework item", page, err)
	}
	item := page.Items[0]
	if item.Kind != bpm.SeatKindRework || item.ReturnReason != "Sai đơn giá" || item.InstanceID != inst.ID {
		t.Fatalf("item = %+v; want kind rework, returnReason Sai đơn giá", item)
	}
	// The detail the submitter opens says the approval is waiting on them, and why.
	detail, err := st.approval.GetApprovalDetail(ctx, RecordScope{UserID: seatSubmitter}, tenant, inst.ID, bpm.Actor{UserID: seatSubmitter})
	if err != nil {
		t.Fatalf("detail as the submitter: %v", err)
	}
	if !detail.AwaitingRework || detail.ReturnReason != "Sai đơn giá" {
		t.Fatalf("detail awaitingRework=%v returnReason=%q; want true / Sai đơn giá", detail.AwaitingRework, detail.ReturnReason)
	}
	if last := detail.History[len(detail.History)-1]; last.Action != bpm.HistoryRejected || last.ReturnedTo != bpm.RejectToSubmitter || last.Target != seatSubmitter {
		t.Fatalf("detail history tail = %+v; want a rejected entry returned to the submitter", last)
	}
	// While it is with the submitter nobody approves it — not even the rejector.
	if _, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp2}); err == nil {
		t.Fatalf("the rejector approved a record that is with its submitter")
	}

	// T135: the submitter edits the record — the after_update process is live and the record
	// changes — and NO second approval is armed.
	if err := st.recordSvc.Update(ctx, bpmAdmin(seatSubmitter), tenant, entity, rid, map[string]any{"status": "Đã sửa"}, nil); err != nil {
		t.Fatalf("fix the record: %v", err)
	}
	// "Gửi lại" resumes at the rejector with a new deadline (T136).
	back, err := st.work.Complete(ctx, tenant, item.ID, "đã sửa đơn giá", WorkItemActor{UserID: seatSubmitter})
	if err != nil {
		t.Fatalf("Gửi lại: %v", err)
	}
	if back.Status != bpm.StatusPending || back.CurrentStep != 1 || back.CurrentApproverID != seatAp2 || back.SLADueAt == nil {
		t.Fatalf("after Gửi lại: status=%s step=%d holder=%s due=%v; want pending on s2 for duyet2 with a new deadline",
			back.Status, back.CurrentStep, back.CurrentApproverID, back.SLADueAt)
	}
	seat = rrOpenSeat(t, st, tenant, inst.ID)
	if seat.Kind != bpm.SeatKindApproval || seat.AssigneeID != seatAp2 || seat.NodeID != "s2" {
		t.Fatalf("open seat after Gửi lại = %+v; want an approval seat of duyet2 on s2", seat)
	}
	if last := back.History[len(back.History)-1]; last.Action != bpm.HistoryResubmitted || last.Actor != seatSubmitter || last.Comment != "đã sửa đơn giá" {
		t.Fatalf("last history entry = %+v; want resubmitted by the submitter with the note", last)
	}
	inst2, err := st.repo.GetInstance(ctx, tenant, inst.ID)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if _, seqs := dueJobsOf(t, st.bpmStack, tenant, inst.ID); !containsSeq(seqs, inst2.Seq) {
		t.Fatalf("due jobs %v; want one for the resubmit's seq %d (T291)", seqs, inst2.Seq)
	}

	// Every event the edits emitted has now been processed: still ONE instance for the record.
	st.drain(t)
	if n := st.instanceCount(t, tenant, entity, rid); n != 1 {
		t.Fatalf("instances for the record = %d; want 1 — editing a returned record must not arm a second approval", n)
	}
	// duyet2 approves the resubmitted record: approved, outcome written.
	done, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: seatAp2})
	if err != nil || done.Status != bpm.StatusApproved {
		t.Fatalf("approve after Gửi lại: %+v err=%v", done, err)
	}
	if v := st.rrStatus(t, tenant, entity, rid); v != "Chấp nhận" {
		t.Fatalf("record status = %q; want Chấp nhận", v)
	}
}

func containsSeq(seqs []int64, want int64) bool {
	for _, s := range seqs {
		if s == want {
			return true
		}
	}
	return false
}

// T134: reject → back to step 1 as a new visit with a new deadline; a due job for the new seq; the
// old click of the rejector is a conflict. T291 rides here (the invariant: one job per seq).
func TestReject_ReturnToEarlierNode_NewVisitNewSLA_Integration(t *testing.T) {
	st := rrWire(t)
	ctx := context.Background()
	tenant, entity := o3aTenantA, "rrnode"
	st.rrUsers(t, tenant)
	st.createEntity(t, ctx, tenant, entity, []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	if _, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: entity + "-p", EntityName: entity, Trigger: "after_create",
		Steps: []ApprovalStepInput{
			{ID: "s1", Name: "Trưởng phòng", ApproverType: "user", ApproverID: seatAp1, SLASeconds: 3600, SLAClock: "wall"},
			{ID: "s2", Name: "Kế toán", ApproverType: "user", ApproverID: seatAp2, SLASeconds: 7200, SLAClock: "wall",
				OnReject: &ApprovalOnRejectInput{To: "s1"}},
		},
		IsActive: boolPtr(true), ActorID: seatSubmitter,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	rid, err := st.recordSvc.Create(ctx, bpmAdmin(seatSubmitter), tenant, seatSubmitter, entity, map[string]any{"status": "Nháp"})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}
	st.drain(t)
	inst := st.onlyPendingInstance(t, tenant, entity, rid)
	if _, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: seatAp1}); err != nil {
		t.Fatalf("approve s1: %v", err)
	}
	firstVisit := seatsOf(t, st.sdb, tenant, inst.ID)

	saw := 1 // duyet2's screen shows s2
	got, err := st.approval.DecideAt(ctx, tenant, inst.ID, bpm.DecisionReject, "thiếu chứng từ", bpm.Actor{UserID: seatAp2}, &saw)
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if got.Status != bpm.StatusPending || got.CurrentStep != 0 || got.CurrentApproverID != seatAp1 || got.SLADueAt == nil {
		t.Fatalf("after the return: status=%s step=%d holder=%s due=%v; want pending on s1 for duyet1 with a deadline",
			got.Status, got.CurrentStep, got.CurrentApproverID, got.SLADueAt)
	}
	seat := rrOpenSeat(t, st, tenant, inst.ID)
	if seat.NodeID != "s1" || seat.AssigneeID != seatAp1 || seat.Kind != bpm.SeatKindApproval {
		t.Fatalf("open seat = %+v; want duyet1's approval seat on s1", seat)
	}
	// A NEW visit of s1: its entry_seq is later than the first visit's.
	for _, old := range firstVisit {
		if old.NodeID == "s1" && seat.EntrySeq <= old.EntrySeq {
			t.Fatalf("returned s1 seat entry_seq %d <= first visit's %d; want a new visit", seat.EntrySeq, old.EntrySeq)
		}
	}
	nowInst, err := st.repo.GetInstance(ctx, tenant, inst.ID)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if _, seqs := dueJobsOf(t, st.bpmStack, tenant, inst.ID); !containsSeq(seqs, nowInst.Seq) {
		t.Fatalf("due jobs %v; want a job for the returned step's seq %d (T291)", seqs, nowInst.Seq)
	}
	// The old "Duyệt" click of duyet2 (screen showed s2): a conflict, and nothing moves.
	_, err = st.approval.DecideAt(ctx, tenant, inst.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp2}, &saw)
	wantItemCode(t, err, "APPROVAL_CONFLICT")
	// A user who never held a seat of the instance hears the plain refusal.
	_, err = st.approval.DecideAt(ctx, tenant, inst.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp3}, &saw)
	wantItemCode(t, err, "APPROVAL_NOT_AUTHORIZED")
	if now, _ := st.repo.GetInstance(ctx, tenant, inst.ID); now.CurrentStep != 0 || now.Status != bpm.StatusPending {
		t.Fatalf("a refused click moved the instance: %+v", now)
	}
	// duyet1 approves again; the loop reaches s2 once more for duyet2.
	if _, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp1}); err != nil {
		t.Fatalf("approve s1 again: %v", err)
	}
	if again := rrOpenSeat(t, st, tenant, inst.ID); again.NodeID != "s2" || again.AssigneeID != seatAp2 {
		t.Fatalf("open seat after the loop = %+v; want duyet2 on s2", again)
	}
}

// T137: a creator that is not a live user (an API key's principal) falls back to the record's
// owner; with neither, the rejection is refused APPROVAL_NO_SUBMITTER and the approval stays put.
func TestReject_ApiKeySubmitterFallsBackToOwner_Integration(t *testing.T) {
	st := rrWire(t)
	ctx := context.Background()
	tenant, entity := o3aTenantA, "rrapikey"
	st.rrUsers(t, tenant)
	st.createEntity(t, ctx, tenant, entity, []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	if _, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: entity + "-p", EntityName: entity, Trigger: "after_create",
		Steps:    []ApprovalStepInput{{ID: "s1", ApproverType: "user", ApproverID: seatAp2, OnReject: &ApprovalOnRejectInput{To: "@submitter"}}},
		IsActive: boolPtr(true), ActorID: seatSubmitter,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	// The record is created by a principal that is NOT a user row (what an API key looks like).
	const apiKey = "aaaaaaaa-0000-0000-0000-00000000ffff"
	rid, err := st.recordSvc.Create(ctx, bpmAdmin(apiKey), tenant, apiKey, entity, map[string]any{"status": "Nháp"})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}
	noOwner, err := st.recordSvc.Create(ctx, bpmAdmin(apiKey), tenant, apiKey, entity, map[string]any{"status": "Nháp"})
	if err != nil {
		t.Fatalf("create record 2: %v", err)
	}
	st.drain(t)
	inst := st.onlyPendingInstance(t, tenant, entity, rid)
	stuck := st.onlyPendingInstance(t, tenant, entity, noOwner)
	// Hand the first record to a live owner, as "Giao việc & đổi chủ" does.
	if _, err := st.sdb.Exec("UPDATE `obj_"+entity+"` SET owner_id = ? WHERE tenant_id = ? AND id = ?", seatAp1, tenant, rid); err != nil {
		t.Fatalf("set owner: %v", err)
	}
	if _, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionReject, "thiếu", bpm.Actor{UserID: seatAp2}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if seat := rrOpenSeat(t, st, tenant, inst.ID); seat.Kind != bpm.SeatKindRework || seat.AssigneeID != seatAp1 {
		t.Fatalf("rework seat = %+v; want the record owner (duyet1) — the API key is not a person", seat)
	}

	// No live owner either: refused, and the approval is untouched.
	_, err = st.approval.Decide(ctx, tenant, stuck.ID, bpm.DecisionReject, "thiếu", bpm.Actor{UserID: seatAp2})
	wantItemCode(t, err, "APPROVAL_NO_SUBMITTER")
	if now, _ := st.repo.GetInstance(ctx, tenant, stuck.ID); now.Status != bpm.StatusPending || len(now.History) != 0 {
		t.Fatalf("a refused return changed the instance: %+v", now)
	}
	if seat := rrOpenSeat(t, st, tenant, stuck.ID); seat.Kind != bpm.SeatKindApproval || seat.AssigneeID != seatAp2 {
		t.Fatalf("seat after the refused return = %+v; want duyet2's approval seat untouched", seat)
	}
}
