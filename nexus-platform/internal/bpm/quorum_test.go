package bpm

import (
	"context"
	"errors"
	"testing"
)

// specs/043 S6b (T112, T115) — quorum %: a council step ends when ceil(percent x N / 100) people
// approved, or as soon as that many can no longer be reached.

func TestEvaluateCompletion_QuorumTable(t *testing.T) {
	q := func(p int) Completion { return Completion{Mode: CompletionQuorum, Percent: p} }
	for _, tc := range []struct {
		name                      string
		c                         Completion
		total, approved, rejected int
		want                      Verdict
	}{
		{"60% of 3 needs 2: one approval is not enough", q(60), 3, 1, 0, VerdictPending},
		{"60% of 3 needs 2: two approvals carry it", q(60), 3, 2, 0, VerdictApproved},
		{"60% of 3: one rejection leaves it reachable", q(60), 3, 0, 1, VerdictPending},
		{"60% of 3: two rejections make 2 approvals unreachable", q(60), 3, 0, 2, VerdictRejected},
		{"60% of 3: a rejection does not stop two later approvals", q(60), 3, 2, 1, VerdictApproved},
		{"50% of 4 needs exactly 2", q(50), 4, 2, 0, VerdictApproved},
		{"50% of 4: two rejections leave 2 reachable", q(50), 4, 0, 2, VerdictPending},
		{"50% of 4: three rejections do not", q(50), 4, 0, 3, VerdictRejected},
		{"51% of 4 rounds UP to 3", q(51), 4, 2, 0, VerdictPending},
		{"1% of 50 still needs one person", q(1), 50, 0, 0, VerdictPending},
		{"1% of 50: one approval carries it", q(1), 50, 1, 0, VerdictApproved},
		{"100% is `all`: one rejection ends it", q(100), 3, 2, 1, VerdictRejected},
		{"100% is `all`: everyone", q(100), 3, 3, 0, VerdictApproved},
		{"quorum of one seat", q(60), 1, 1, 0, VerdictApproved},
		{"no seat in play is never approved", q(60), 0, 0, 0, VerdictPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := evaluateVotes(tc.c, tc.total, tc.approved, tc.rejected); got != tc.want {
				t.Fatalf("evaluateVotes(%+v, total %d, approved %d, rejected %d) = %s; want %s", tc.c, tc.total, tc.approved, tc.rejected, got, tc.want)
			}
		})
	}
	if got := evaluateVotes(Completion{Mode: CompletionAny}, 3, 1, 0); got != VerdictApproved {
		t.Fatalf("`any` through evaluateVotes = %s; want approved (the old rule is unchanged)", got)
	}
}

func TestQuorumNeed(t *testing.T) {
	for _, tc := range []struct{ percent, total, want int }{
		{60, 3, 2}, {50, 4, 2}, {51, 4, 3}, {100, 3, 3}, {1, 50, 1}, {34, 3, 2}, {33, 3, 1}, {60, 1, 1}, {60, 0, 0},
	} {
		if got := QuorumNeed(tc.percent, tc.total); got != tc.want {
			t.Errorf("QuorumNeed(%d%%, %d) = %d; want %d", tc.percent, tc.total, got, tc.want)
		}
	}
}

func TestCheckApprovers_Quorum(t *testing.T) {
	two := []Approver{uA("u1"), uA("u2")}
	for _, tc := range []struct {
		name      string
		c         *Completion
		wantField string
	}{
		{"quorum 60", &Completion{Mode: CompletionQuorum, Percent: 60}, ""},
		{"quorum 1", &Completion{Mode: CompletionQuorum, Percent: 1}, ""},
		{"quorum 100", &Completion{Mode: CompletionQuorum, Percent: 100}, ""},
		{"quorum without percent", &Completion{Mode: CompletionQuorum}, "completion.percent"},
		{"quorum 101", &Completion{Mode: CompletionQuorum, Percent: 101}, "completion.percent"},
		{"quorum negative", &Completion{Mode: CompletionQuorum, Percent: -5}, "completion.percent"},
		{"percent beside all is a mistake, not ignored", &Completion{Mode: CompletionAll, Percent: 60}, "completion.percent"},
		{"percent beside any is a mistake, not ignored", &Completion{Mode: CompletionAny, Percent: 60}, "completion.percent"},
		{"percent beside no mode", &Completion{Percent: 60}, "completion.mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if f, reason := CheckApprovers(Step{Approvers: two, Completion: tc.c}); f != tc.wantField {
				t.Fatalf("CheckApprovers = (%q, %q); want field %q", f, reason, tc.wantField)
			}
		})
	}
}

// armQuorum plants a council step with the given policy, followed by a one-person step.
func armQuorum(store *fakeStore, percent int, approvers ...Approver) string {
	g := &Graph{Nodes: []Node{
		{Step: Step{ID: "ban-duyet", Name: "Ban duyệt", Approvers: approvers, Completion: &Completion{Mode: CompletionQuorum, Percent: percent}, SLASeconds: 3600}, Kind: NodeApproval},
		{Step: Step{ID: "giam-doc", Name: "Giám đốc", ApproverType: ApproverUser, ApproverID: "u-dir"}, Kind: NodeApproval},
	}}
	def := pinDefinition(Process{Graph: g, Outcome: &Outcome{StatusField: "status", ApprovedValue: "ok", RejectedValue: "no"}})
	store.instances["i1"] = &Instance{
		ID: "i1", TenantID: "t1", ProcessID: "p1", EntityName: "crm_quotes", RecordID: "q1",
		Status: StatusPending, Seq: 1, Definition: def, CreatedBy: "u-sub",
	}
	store.pending[pkey("t1", "crm_quotes", "q1")] = true
	store.plantParallel("i1", stepSeats(def.Chain()[0], nil, ""))
	return "i1"
}

// Spec US3 scenario 3: 60% of 3 — duyet1 and duyet2 approve, the step is done and duyet3's seat
// closes (superseded).
func TestParallel_QuorumTwoOfThreeCarries(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	id := armQuorum(store, 60, uA("u1"), uA("u2"), uA("u3"))
	d, _ := newParallelDecider(store)

	inst, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "u1"})
	if err != nil || inst.CurrentStep != 0 {
		t.Fatalf("first approval: %v step %d; want still on the council step", err, inst.CurrentStep)
	}
	inst, err = d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "u2"})
	if err != nil {
		t.Fatalf("second approval: %v", err)
	}
	if inst.CurrentStep != 1 || inst.CurrentApproverID != "u-dir" {
		t.Fatalf("after 2 of 3: step %d approver %q; want the next step", inst.CurrentStep, inst.CurrentApproverID)
	}
	var superseded int
	for _, v := range store.visits[id] {
		if v.NodeID == "ban-duyet" && v.AssigneeID == "u3" && v.Status == "cancelled" && v.Outcome == SeatSuperseded {
			superseded++
		}
	}
	if superseded != 1 {
		t.Fatalf("u3's seat superseded %d times; want once (visit %+v)", superseded, store.visits[id])
	}
	if _, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "u3"}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("u3 voting after the quorum: err = %v; want ErrNotAuthorized", err)
	}
}

// T112 — two rejections of three make 60% unreachable: the approval is rejected at once, the third
// person is not asked.
func TestParallel_QuorumUnreachableRejectsEarly(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	id := armQuorum(store, 60, uA("u1"), uA("u2"), uA("u3"))
	d, w := newParallelDecider(store)

	inst, err := d.Decide(ctx, "t1", id, DecisionReject, "giá cao", Actor{UserID: "u1"})
	if err != nil || inst.Status != StatusPending {
		t.Fatalf("first rejection: %v %s; want still pending (2 approvals still reachable)", err, inst.Status)
	}
	inst, err = d.Decide(ctx, "t1", id, DecisionReject, "giá cao", Actor{UserID: "u2"})
	if err != nil {
		t.Fatalf("second rejection: %v", err)
	}
	if inst.Status != StatusRejected {
		t.Fatalf("status after 2 rejections of 3 = %s; want rejected (quorum unreachable)", inst.Status)
	}
	if len(w.calls) != 1 || w.calls[0] != "crm_quotes.status=no" {
		t.Fatalf("outcome write-back = %+v; want the rejected value once", w.calls)
	}
	for _, v := range store.visits[id] {
		if v.Status == "open" {
			t.Fatalf("seat %+v left open on a rejected instance", v)
		}
	}
}

// A rejection that does not make the quorum unreachable is just one vote.
func TestParallel_QuorumOneRejectionDoesNotBlock(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	id := armQuorum(store, 60, uA("u1"), uA("u2"), uA("u3"))
	d, _ := newParallelDecider(store)
	for _, v := range []struct {
		who  string
		kind DecisionKind
	}{{"u1", DecisionReject}, {"u2", DecisionApprove}} {
		if inst, err := d.Decide(ctx, "t1", id, v.kind, "x", Actor{UserID: v.who}); err != nil || inst.CurrentStep != 0 {
			t.Fatalf("%s: %v step %d; want still on the council step", v.who, err, inst.CurrentStep)
		}
	}
	inst, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "u3"})
	if err != nil || inst.CurrentStep != 1 {
		t.Fatalf("third vote: %v step %d; want 2 approvals to carry it past one rejection", err, inst.CurrentStep)
	}
}
