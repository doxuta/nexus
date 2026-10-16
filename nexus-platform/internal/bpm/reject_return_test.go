package bpm

// specs/043 S8 (`bpm-reject-return`): a rejection that returns instead of ending — to an earlier
// node, or to the submitter who fixes the record and sends it again. The real transaction, the
// seats and the second-approval question are proven on MySQL in
// internal/services/bpm_reject_return_integration_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// recordingWriter is a RecordWriter that remembers what the engine wrote onto the record.
type recordingWriter struct{ writes []string }

func (w *recordingWriter) SetField(_ context.Context, _, _, _, field string, value any, _ string) error {
	w.writes = append(w.writes, field+"="+value.(string))
	return nil
}

// staticSubmitter resolves the submitter without a database: a creator id is "live" when listed,
// otherwise the record's owner answers.
type staticSubmitter struct {
	live  map[string]bool
	owner string
}

func (s staticSubmitter) ResolveSubmitter(_ context.Context, _, _, _, createdBy string) (string, error) {
	if s.live[createdBy] {
		return createdBy, nil
	}
	return s.owner, nil
}

// reworkFixture arms a two-node graph (s1 duyet1, s2 duyet2 carrying onReject) on a fake store and
// stands the instance on s2, created by "test". onReject is the raw JSON of s2's onReject.
func reworkFixture(t *testing.T, onReject string) (*fakeStore, *Decider, *recordingWriter, string) {
	t.Helper()
	raw := `{"nodes":[
		{"id":"s1","name":"Trưởng phòng","kind":"approval","approverType":"user","approverId":"duyet1","slaSeconds":3600,"slaClock":"wall"},
		{"id":"s2","name":"Kế toán","kind":"approval","approverType":"user","approverId":"duyet2","slaSeconds":7200,"slaClock":"wall","onReject":` + onReject + `}]}`
	var g Graph
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		t.Fatalf("graph: %v", err)
	}
	def := &Definition{
		Steps: []Step{}, Graph: g.normalized(),
		Outcome: &Outcome{StatusField: "status", ApprovedValue: "Chấp nhận", RejectedValue: "Từ chối", ReturnedValue: "Cần bổ sung"},
	}
	if len(def.Chain()) != 2 {
		t.Fatalf("the fixture graph is not runnable: chain = %d", len(def.Chain()))
	}
	store := newFakeStore()
	id, err := store.CreateInstance(context.Background(), CreateInstanceInput{
		TenantID: "t1", ProcessID: "p1", EntityName: "crm_quotes", RecordID: "r1",
		CurrentApproverType: ApproverUser, CurrentApproverID: "duyet1", Definition: def, CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	in := store.instances[id]
	in.CurrentStep, in.CurrentApproverID = 1, "duyet2"
	store.seats[id] = []Seat{{NodeID: "s2", AssigneeType: ApproverUser, AssigneeID: "duyet2"}}
	w := &recordingWriter{}
	d := NewDecider(store, store, store, nil, nil).WithRecordWriter(w)
	d.now = func() time.Time { return time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC) }
	return store, d, w, id
}

func lastRework(inst *Instance) HistoryEntry { return inst.History[len(inst.History)-1] }

// T134: reject → "về bước s1": the instance stands on s1 again as a NEW visit with its own deadline
// (s1's 3600s from now), s1's holder owns the seat, and the history says where it went. The write-
// back writes nothing (it is not terminal, and not a return to the submitter).
func TestReject_ReturnToEarlierNode_NewVisitNewSLA(t *testing.T) {
	store, d, w, id := reworkFixture(t, `{"to":"s1"}`)
	got, err := d.Decide(context.Background(), "t1", id, DecisionReject, "sai đơn giá", Actor{UserID: "duyet2"})
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if got.Status != StatusPending || got.CurrentStep != 0 {
		t.Fatalf("after the return: status=%s step=%d; want pending on step 0", got.Status, got.CurrentStep)
	}
	if got.CurrentApproverID != "duyet1" || got.SLADueAt == nil || !got.SLADueAt.Equal(d.now().Add(time.Hour)) {
		t.Fatalf("holder=%q due=%v; want duyet1 due now+1h (a new deadline)", got.CurrentApproverID, got.SLADueAt)
	}
	open := store.lastNext.Seats.Open
	if len(open) != 1 || open[0].NodeID != "s1" || open[0].AssigneeID != "duyet1" || open[0].Kind != SeatKindApproval {
		t.Fatalf("seats opened = %+v; want one approval seat of duyet1 on s1", open)
	}
	if e := lastRework(got); e.Action != HistoryRejected || e.ReturnedTo != "s1" || e.Comment != "sai đơn giá" || e.Node != "s2" {
		t.Fatalf("history = %+v; want rejected on s2 with returnedTo s1 and the reason", e)
	}
	if len(w.writes) != 0 {
		t.Fatalf("a return to a node wrote %v onto the record", w.writes)
	}
	if !store.pending[pkey("t1", "crm_quotes", "r1")] {
		t.Fatalf("the record stopped being pending — a second approval could be armed")
	}
}

// T134 (ABA): duyet2 reads the instance on s2; before the decision lands the approval loops —
// rejected to s1, approved by duyet1, back on s2 — leaving the SAME step and the SAME holder. The
// decision carries the seq it read, so it loses instead of settling a visit it never saw.
func TestReject_ReturnLoop_StaleApproveLoses(t *testing.T) {
	store, d, _, id := reworkFixture(t, `{"to":"s1"}`)
	fired := false
	store.onGet = func(f *fakeStore, _ *Instance) {
		if fired {
			return
		}
		fired = true
		ctx := context.Background()
		if _, err := d.Decide(ctx, "t1", id, DecisionReject, "lần một", Actor{UserID: "duyet2"}); err != nil {
			t.Errorf("loop reject: %v", err)
		}
		if _, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "duyet1"}); err != nil {
			t.Errorf("loop approve: %v", err)
		}
	}
	_, err := d.Decide(context.Background(), "t1", id, DecisionApprove, "", Actor{UserID: "duyet2"})
	if !errors.Is(err, ErrConcurrent) {
		t.Fatalf("stale approve after a loop: err = %v; want ErrConcurrent", err)
	}
	if in := store.instances[id]; in.CurrentStep != 1 || in.Status != StatusPending {
		t.Fatalf("the stale approve moved the instance: step=%d status=%s", in.CurrentStep, in.Status)
	}
}

// T135/T138: reject → "về người gửi": the instance stays PENDING on s2 (so a record edit cannot arm a
// second approval), the open seat is a rework seat of the submitter carrying the reason, the
// record gets outcome.returnedValue, and the submitter is told. Nobody has a deadline.
func TestReject_ReturnToSubmitter_ReworkSeatAndReturnedValue(t *testing.T) {
	store, d, w, id := reworkFixture(t, `{"to":"@submitter"}`)
	got, err := d.Decide(context.Background(), "t1", id, DecisionReject, "Sai đơn giá", Actor{UserID: "duyet2"})
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if got.Status != StatusPending || got.CurrentStep != 1 || got.SLADueAt != nil {
		t.Fatalf("status=%s step=%d due=%v; want pending on s2 with no deadline", got.Status, got.CurrentStep, got.SLADueAt)
	}
	open := store.lastNext.Seats.Open
	if len(open) != 1 || open[0].Kind != SeatKindRework || open[0].AssigneeID != "test" || open[0].NodeID != "s2" || open[0].Note != "Sai đơn giá" {
		t.Fatalf("seats = %+v; want one rework seat of test on s2 carrying the reason", open)
	}
	if got.CurrentApproverType != ApproverUser || got.CurrentApproverID != "test" {
		t.Fatalf("projection = %s/%s; want user/test", got.CurrentApproverType, got.CurrentApproverID)
	}
	if e := lastRework(got); e.Action != HistoryRejected || e.ReturnedTo != RejectToSubmitter || e.Target != "test" {
		t.Fatalf("history = %+v; want rejected, returnedTo @submitter, target test", e)
	}
	if len(w.writes) != 1 || w.writes[0] != "status=Cần bổ sung" {
		t.Fatalf("record writes = %v; want status=Cần bổ sung (returnedValue)", w.writes)
	}
	notified := false
	for _, n := range store.notes {
		notified = notified || (n.UserID == "test" && n.RecordID == "r1")
	}
	if !notified {
		t.Fatalf("the submitter was not told: %+v", store.notes)
	}
	// While it is with the submitter nobody can approve or reject it — only "Gửi lại" (complete).
	for _, k := range []DecisionKind{DecisionApprove, DecisionReject} {
		if _, err := d.Decide(context.Background(), "t1", id, k, "x", Actor{UserID: "test"}); !errors.Is(err, ErrWrongStepKind) {
			t.Fatalf("%s by the submitter during rework: err = %v; want ErrWrongStepKind", k, err)
		}
	}
	if _, err := d.Decide(context.Background(), "t1", id, DecisionApprove, "", Actor{UserID: "boss", IsAdmin: true}); !errors.Is(err, ErrWrongStepKind) {
		t.Fatalf("an admin approving during rework: err = %v; want ErrWrongStepKind", err)
	}
	if _, err := d.Override(context.Background(), "t1", id, DecisionApprove, "ép", Actor{UserID: "boss", IsAdmin: true}, nil); !errors.Is(err, ErrWrongStepKind) {
		t.Fatalf("an override during rework: err = %v; want ErrWrongStepKind", err)
	}
	if _, err := d.Decide(context.Background(), "t1", id, DecisionComplete, "", Actor{UserID: "duyet2"}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("the rejector completing the submitter's rework: err = %v; want ErrNotAuthorized", err)
	}
}

// T136: "Gửi lại" resumes at the rejector's step (default) or at the start, as a NEW visit with a
// new deadline, and clears the rework seat.
func TestResubmit_ResumesAtRejector(t *testing.T) {
	store, d, _, id := reworkFixture(t, `{"to":"@submitter"}`)
	ctx := context.Background()
	if _, err := d.Decide(ctx, "t1", id, DecisionReject, "Sai đơn giá", Actor{UserID: "duyet2"}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	store.seats[id] = []Seat{{NodeID: "s2", AssigneeType: ApproverUser, AssigneeID: "test", Kind: SeatKindRework}}
	got, err := d.Decide(ctx, "t1", id, DecisionComplete, "đã sửa", Actor{UserID: "test"})
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if got.Status != StatusPending || got.CurrentStep != 1 || got.CurrentApproverID != "duyet2" {
		t.Fatalf("after resubmit: status=%s step=%d holder=%s; want pending on s2, duyet2", got.Status, got.CurrentStep, got.CurrentApproverID)
	}
	if got.SLADueAt == nil || !got.SLADueAt.Equal(d.now().Add(2*time.Hour)) {
		t.Fatalf("due = %v; want now+2h (s2's SLA restarts)", got.SLADueAt)
	}
	if open := store.lastNext.Seats.Open; len(open) != 1 || open[0].Kind != SeatKindApproval || open[0].AssigneeID != "duyet2" {
		t.Fatalf("seats = %+v; want one approval seat of duyet2", open)
	}
	if store.lastNext.Seats.Close != SeatCompleted {
		t.Fatalf("the rework seat closed as %q; want completed", store.lastNext.Seats.Close)
	}
	if e := lastRework(got); e.Action != HistoryResubmitted || e.Actor != "test" || e.Comment != "đã sửa" || e.ReturnedTo != "s2" {
		t.Fatalf("history = %+v; want resubmitted by test resuming at s2", e)
	}
	// duyet2 can decide again, and an approve finishes the approval.
	if done, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "duyet2"}); err != nil || done.Status != StatusApproved {
		t.Fatalf("approve after resubmit: %+v err=%v; want approved", done, err)
	}
}

func TestResubmit_ResumesAtStart(t *testing.T) {
	store, d, _, id := reworkFixture(t, `{"to":"@submitter","resumeAt":"start"}`)
	ctx := context.Background()
	if _, err := d.Decide(ctx, "t1", id, DecisionReject, "thiếu", Actor{UserID: "duyet2"}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	store.seats[id] = []Seat{{NodeID: "s2", AssigneeType: ApproverUser, AssigneeID: "test", Kind: SeatKindRework}}
	got, err := d.Decide(ctx, "t1", id, DecisionComplete, "", Actor{UserID: "test"})
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if got.CurrentStep != 0 || got.CurrentApproverID != "duyet1" || got.SLADueAt == nil || !got.SLADueAt.Equal(d.now().Add(time.Hour)) {
		t.Fatalf("step=%d holder=%s due=%v; want s1 / duyet1 / now+1h", got.CurrentStep, got.CurrentApproverID, got.SLADueAt)
	}
	if e := lastRework(got); e.Action != HistoryResubmitted || e.ReturnedTo != "s1" || e.Node != "s2" {
		t.Fatalf("history = %+v; want resubmitted on s2 resuming at s1", e)
	}
}

// T137: an API-key submitter (not a live user) falls back to the record's owner; with nobody to
// return to the rejection is refused — it neither ends nor strands the approval.
func TestReject_ApiKeySubmitterFallsBackToOwner(t *testing.T) {
	store, d, _, id := reworkFixture(t, `{"to":"@submitter"}`)
	store.instances[id].CreatedBy = "apikey-7"
	d.WithSubmitterResolver(staticSubmitter{live: map[string]bool{"test": true}, owner: "owner-1"})
	got, err := d.Decide(context.Background(), "t1", id, DecisionReject, "thiếu", Actor{UserID: "duyet2"})
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if got.CurrentApproverID != "owner-1" || lastRework(got).Target != "owner-1" {
		t.Fatalf("holder=%s target=%s; want the record owner owner-1", got.CurrentApproverID, lastRework(got).Target)
	}

	store2, d2, _, id2 := reworkFixture(t, `{"to":"@submitter"}`)
	store2.instances[id2].CreatedBy = "apikey-7"
	d2.WithSubmitterResolver(staticSubmitter{})
	if _, err := d2.Decide(context.Background(), "t1", id2, DecisionReject, "thiếu", Actor{UserID: "duyet2"}); !errors.Is(err, ErrNoSubmitter) {
		t.Fatalf("nobody to return to: err = %v; want ErrNoSubmitter", err)
	}
	if in := store2.instances[id2]; in.Status != StatusPending || in.CurrentStep != 1 || store2.advanceCnt != 0 {
		t.Fatalf("a refused return wrote: %+v", in)
	}
	// An admin's override is an explicit command: it ends the approval rather than being blocked.
	if got, err := d2.Override(context.Background(), "t1", id2, DecisionReject, "đóng", Actor{UserID: "boss", IsAdmin: true}, nil); err != nil || got.Status != StatusRejected {
		t.Fatalf("override with no submitter: %+v err=%v; want rejected", got, err)
	}
}

// T137: the submitter withdraws a returned approval — the rework seat is withdrawn with it.
func TestRecall_DuringReworkCancels(t *testing.T) {
	store, d, _, id := reworkFixture(t, `{"to":"@submitter"}`)
	ctx := context.Background()
	if _, err := d.Decide(ctx, "t1", id, DecisionReject, "thiếu", Actor{UserID: "duyet2"}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	store.seats[id] = []Seat{{NodeID: "s2", AssigneeType: ApproverUser, AssigneeID: "test", Kind: SeatKindRework}}
	r := NewRequester(nil, store, store, store, nil)
	got, err := r.Recall(ctx, "t1", id, "thôi", Actor{UserID: "test"})
	if err != nil || got.Status != StatusCancelled {
		t.Fatalf("recall during rework: %+v err=%v; want cancelled", got, err)
	}
	if store.lastNext.Seats.Close != SeatRecalled {
		t.Fatalf("seats closed as %q; want recalled", store.lastNext.Seats.Close)
	}
	if _, err := r.Recall(ctx, "t1", id, "", Actor{UserID: "duyet2"}); !errors.Is(err, ErrNotPending) {
		t.Fatalf("second recall: err = %v; want ErrNotPending", err)
	}
}

// onReject is a closed shape the engine only runs when it can honour it (D-7): a target that is
// not an EARLIER node, a resume on a non-submitter target, an unknown resume point, or onReject
// on a task node make the whole graph unrunnable — never a silent final reject.
func TestGraph_OnRejectShapeIsFailClosed(t *testing.T) {
	node := func(or string) string {
		return `{"nodes":[{"id":"a","kind":"approval","approverType":"user","approverId":"u1"},` +
			`{"id":"b","kind":"approval","approverType":"user","approverId":"u2","onReject":` + or + `}]}`
	}
	for _, tc := range []struct {
		name, raw string
		ok        bool
	}{
		{"end", node(`{"to":"@end"}`), true},
		{"submitter", node(`{"to":"@submitter"}`), true},
		{"submitter resume start", node(`{"to":"@submitter","resumeAt":"start"}`), true},
		{"earlier node", node(`{"to":"a"}`), true},
		{"itself", node(`{"to":"b"}`), false},
		{"unknown node", node(`{"to":"zzz"}`), false},
		{"empty target", node(`{}`), false},
		{"bad resume", node(`{"to":"@submitter","resumeAt":"middle"}`), false},
		{"resume on a node target", node(`{"to":"a","resumeAt":"start"}`), false},
		{"onReject on a task",
			`{"nodes":[{"id":"a","kind":"task","approverType":"user","approverId":"u1","onReject":{"to":"@submitter"}}]}`, false},
		{"first node returning to a later one",
			`{"nodes":[{"id":"a","kind":"approval","approverType":"user","approverId":"u1","onReject":{"to":"b"}},` +
				`{"id":"b","kind":"approval","approverType":"user","approverId":"u2"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var g Graph
			if err := json.Unmarshal([]byte(tc.raw), &g); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := len(g.approvalChain()) > 0; got != tc.ok {
				t.Fatalf("runnable = %v; want %v", got, tc.ok)
			}
		})
	}
}

// T291: both new ways to open a step with a deadline (a return to a node, a resubmission) are
// writes the breach job rides on — the transition carries the due date the repository enqueues for.
func TestReturnToNode_EnqueuesSLADue(t *testing.T) {
	store, d, _, id := reworkFixture(t, `{"to":"s1"}`)
	if _, err := d.Decide(context.Background(), "t1", id, DecisionReject, "x", Actor{UserID: "duyet2"}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if store.lastNext.SLADueAt == nil || !store.lastNext.SLADueAt.Equal(d.now().Add(time.Hour)) {
		t.Fatalf("transition due = %v; want now+1h so commitTransition enqueues a bpm_sla_due job for it", store.lastNext.SLADueAt)
	}
}

func TestResubmit_EnqueuesSLADue(t *testing.T) {
	store, d, _, id := reworkFixture(t, `{"to":"@submitter"}`)
	ctx := context.Background()
	if _, err := d.Decide(ctx, "t1", id, DecisionReject, "x", Actor{UserID: "duyet2"}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if store.lastNext.SLADueAt != nil {
		t.Fatalf("the return to the submitter set a deadline %v; the clock restarts on resubmit", store.lastNext.SLADueAt)
	}
	store.seats[id] = []Seat{{NodeID: "s2", AssigneeType: ApproverUser, AssigneeID: "test", Kind: SeatKindRework}}
	if _, err := d.Decide(ctx, "t1", id, DecisionComplete, "", Actor{UserID: "test"}); err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if store.lastNext.SLADueAt == nil || !store.lastNext.SLADueAt.Equal(d.now().Add(2*time.Hour)) {
		t.Fatalf("transition due = %v; want now+2h", store.lastNext.SLADueAt)
	}
}

// A rework seat is the submitter's "fix and resend": it cannot be handed on as an approval
// (ErrWrongStepKind), and a repin keeps the seat WITH its return reason.
func TestRework_DelegateRefused_RepinKeepsReason(t *testing.T) {
	store, d, _, id := reworkFixture(t, `{"to":"@submitter"}`)
	ctx := context.Background()
	if _, err := d.Decide(ctx, "t1", id, DecisionReject, "Sai đơn giá", Actor{UserID: "duyet2"}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	seat := Seat{NodeID: "s2", AssigneeType: ApproverUser, AssigneeID: "test", Kind: SeatKindRework, Note: "Sai đơn giá"}
	store.seats[id] = []Seat{seat}
	req := NewRequester(nil, store, store, store, nil)
	if _, err := req.Delegate(ctx, "t1", id, "other", "", Actor{UserID: "test"}); !errors.Is(err, ErrWrongStepKind) {
		t.Fatalf("delegating a rework seat: err = %v; want ErrWrongStepKind", err)
	}
	inst := store.instances[id]
	plan, reason := planRepin(inst, []Seat{seat}, inst.Definition, "admin", time.Now())
	if reason != "" {
		t.Fatalf("repin reason = %q", reason)
	}
	got := plan.Next.Seats.Open
	if len(got) != 1 || got[0].Kind != SeatKindRework || got[0].Note != "Sai đơn giá" {
		t.Fatalf("repinned seats = %+v; want the rework seat with its reason", got)
	}
}
