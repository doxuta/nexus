package bpm

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// specs/043 S4 `bpm-parallel-approval` (T078–T082): the pure completion rule, the approver
// validator, and the Decider's parallel path against the in-memory store. The concurrency and
// SQL contracts live in internal/services/bpm_parallel_integration_test.go (real MySQL, -race).

func TestEvaluateCompletion_AllAnyTable(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		mode                      CompletionMode
		total, approved, rejected int
		want                      Verdict
	}{
		{"all: nobody yet", CompletionAll, 3, 0, 0, VerdictPending},
		{"all: 2 of 3", CompletionAll, 3, 2, 0, VerdictPending},
		{"all: everyone", CompletionAll, 3, 3, 0, VerdictApproved},
		{"all: one rejection ends it at once", CompletionAll, 3, 1, 1, VerdictRejected},
		{"all: rejection beats a full count", CompletionAll, 3, 3, 1, VerdictRejected},
		{"all: no seat in play is never approved", CompletionAll, 0, 0, 0, VerdictPending},
		{"any: nobody yet", CompletionAny, 3, 0, 0, VerdictPending},
		{"any: first approval decides", CompletionAny, 3, 1, 0, VerdictApproved},
		{"any: first rejection decides", CompletionAny, 3, 0, 1, VerdictRejected},
		{"any: both at once (cannot happen) is the safe answer", CompletionAny, 3, 1, 1, VerdictRejected},
		{"empty mode reads as all", "", 2, 1, 0, VerdictPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := evaluateCompletion(tc.mode, tc.total, tc.approved, tc.rejected); got != tc.want {
				t.Fatalf("evaluateCompletion(%q, %d, %d, %d) = %s; want %s", tc.mode, tc.total, tc.approved, tc.rejected, got, tc.want)
			}
		})
	}
}

func TestCheckApprovers_Table(t *testing.T) {
	u := func(id string) Approver { return Approver{Type: ApproverUser, ID: id} }
	for _, tc := range []struct {
		name      string
		step      Step
		wantField string
	}{
		{"plain user step", Step{ApproverType: ApproverUser, ApproverID: "u1"}, ""},
		{"plain step without approver", Step{ApproverType: ApproverUser}, "approverId"},
		{"plain step with a bad type", Step{ApproverType: "team", ApproverID: "x"}, "approverType"},
		{"completion on a one-person step", Step{ApproverType: ApproverUser, ApproverID: "u1", Completion: &Completion{Mode: CompletionAll}}, "completion"},
		{"two approvers, mode omitted", Step{Approvers: []Approver{u("u1"), u("u2")}}, ""},
		{"two approvers, any", Step{Approvers: []Approver{u("u1"), u("u2")}, Completion: &Completion{Mode: CompletionAny}}, ""},
		{"one approver is not parallel", Step{Approvers: []Approver{u("u1")}}, "approvers"},
		{"approvers beside approverType", Step{ApproverType: ApproverUser, ApproverID: "u1", Approvers: []Approver{u("u2"), u("u3")}}, "approverType"},
		{"duplicate approver", Step{Approvers: []Approver{u("u1"), u("u1")}}, "approvers"},
		{"bad approver type", Step{Approvers: []Approver{u("u1"), {Type: "team", ID: "t"}}}, "approvers"},
		{"blank approver id", Step{Approvers: []Approver{u("u1"), u(" ")}}, "approvers"},
		{"unknown mode", Step{Approvers: []Approver{u("u1"), u("u2")}, Completion: &Completion{Mode: "majority"}}, "completion.mode"}, // "quorum" was this row until S6b opened it
		{"empty mode object", Step{Approvers: []Approver{u("u1"), u("u2")}, Completion: &Completion{}}, "completion.mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, reason := CheckApprovers(tc.step); got != tc.wantField {
				t.Fatalf("CheckApprovers = (%q, %q); want field %q", got, reason, tc.wantField)
			}
		})
	}
	over := Step{}
	for i := 0; i <= MaxApprovers; i++ {
		over.Approvers = append(over.Approvers, u(string(rune('a'+i%26))+string(rune('a'+i/26))))
	}
	if f, _ := CheckApprovers(over); f != "approvers" {
		t.Fatalf("%d approvers: field %q; want approvers (cap %d)", len(over.Approvers), f, MaxApprovers)
	}
}

// A stored parallel node runs: the chain keeps its approvers and policy, it opens one seat per
// approver, and the instance projects NO single holder (D-5 — several seats).
func TestGraphProcess_ParallelNodeRuns(t *testing.T) {
	raw := `{"nodes":[{"id":"hoi-dong","kind":"approval","approvers":[{"type":"user","id":"u1"},{"type":"role","id":"r2"},{"type":"user","id":"u3"}],"completion":{"mode":"any"}}]}`
	var g Graph
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		t.Fatalf("decode: %v", err)
	}
	chain := g.normalized().approvalChain()
	if len(chain) != 1 || !chain[0].Parallel() || chain[0].CompletionMode() != CompletionAny {
		t.Fatalf("chain = %+v; want one parallel step with mode any", chain)
	}
	seats := stepSeats(chain[0], nil, "")
	if len(seats) != 3 || seats[1].AssigneeType != ApproverRole || seats[1].NodeID != "hoi-dong" {
		t.Fatalf("seats = %+v; want 3 on node hoi-dong, the second a role seat", seats)
	}
	if at, id := projection(seats); at != "" || id != "" {
		t.Fatalf("projection of 3 seats = %s/%s; want none", at, id)
	}
	// Half-configured stays refused: both sources of approver, or completion without approvers.
	for _, bad := range []string{
		`{"nodes":[{"id":"a","kind":"approval","approverType":"user","approverId":"u","approvers":[{"type":"user","id":"x"},{"type":"user","id":"y"}]}]}`,
		`{"nodes":[{"id":"a","kind":"approval","approverType":"user","approverId":"u","completion":{"mode":"all"}}]}`,
		`{"nodes":[{"id":"a","kind":"approval","approvers":[{"type":"user","id":"x"}]}]}`,
	} {
		var bg Graph
		if err := json.Unmarshal([]byte(bad), &bg); err != nil {
			t.Fatal(err)
		}
		if c := bg.approvalChain(); c != nil {
			t.Fatalf("chain of %s = %+v; want nil (fail closed)", bad, c)
		}
	}
}

func TestGraphFromSteps_LiftsLinearSteps(t *testing.T) {
	g := GraphFromSteps([]Step{{ID: "a", ApproverType: ApproverUser, ApproverID: "u"}, {ID: "b", Approvers: []Approver{{Type: ApproverUser, ID: "x"}, {Type: ApproverUser, ID: "y"}}}})
	if len(g.Nodes) != 2 || g.Nodes[0].Kind != NodeApproval || g.Nodes[1].ID != "b" || len(g.approvalChain()) != 2 {
		t.Fatalf("graph = %+v; want 2 approval nodes that run", g)
	}
}

// ---- Decider on a parallel step (in-memory store) -----------------------------------------------

func uA(id string) Approver { return Approver{Type: ApproverUser, ID: id} }

// armParallel plants an instance standing on a parallel first node followed by a one-person node.
func armParallel(store *fakeStore, mode CompletionMode, approvers ...Approver) string {
	g := &Graph{Nodes: []Node{
		{Step: Step{ID: "hoi-dong", Name: "Hội đồng", Approvers: approvers, Completion: &Completion{Mode: mode}, SLASeconds: 3600}, Kind: NodeApproval},
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

func newParallelDecider(store *fakeStore) (*Decider, *spyWriter) {
	w := &spyWriter{}
	return NewDecider(store, store, store, &fakeRecorder{}, nil).WithRecordWriter(w), w
}

func TestParallel_AllRequiresEveryone(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	id := armParallel(store, CompletionAll, uA("u1"), uA("u2"), uA("u3"))
	d, w := newParallelDecider(store)

	for i, voter := range []string{"u1", "u2"} {
		inst, err := d.Decide(ctx, "t1", id, DecisionApprove, "ok", Actor{UserID: voter})
		if err != nil {
			t.Fatalf("vote %d by %s: %v", i+1, voter, err)
		}
		if inst.Status != StatusPending || inst.CurrentStep != 0 {
			t.Fatalf("after %d of 3 votes: %s@%d; want still pending on the council step", i+1, inst.Status, inst.CurrentStep)
		}
		// The voter's work leaves their inbox; the others still have it.
		if AwaitsActor(store.instances[id], store.seats[id], voter, nil, nil) {
			t.Fatalf("%s still awaited after voting", voter)
		}
		if !AwaitsActor(store.instances[id], store.seats[id], "u3", nil, nil) {
			t.Fatalf("u3 lost the work after %d votes", i+1)
		}
	}
	if len(w.calls) != 0 {
		t.Fatalf("outcome written before the step finished: %+v", w.calls)
	}
	inst, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "u3"})
	if err != nil {
		t.Fatalf("last vote: %v", err)
	}
	if inst.Status != StatusPending || inst.CurrentStep != 1 || inst.CurrentApproverID != "u-dir" {
		t.Fatalf("after 3 of 3: %s@%d approver %q; want pending on the next step u-dir", inst.Status, inst.CurrentStep, inst.CurrentApproverID)
	}
	if store.voteCnt != 3 {
		t.Fatalf("votes taken = %d; want 3", store.voteCnt)
	}
}

func TestParallel_AllOneRejectEnds(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	id := armParallel(store, CompletionAll, uA("u1"), uA("u2"), uA("u3"))
	d, w := newParallelDecider(store)
	if _, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "u1"}); err != nil {
		t.Fatal(err)
	}
	inst, err := d.Decide(ctx, "t1", id, DecisionReject, "thiếu hồ sơ", Actor{UserID: "u2"})
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if inst.Status != StatusRejected {
		t.Fatalf("status after one rejection = %s; want rejected", inst.Status)
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

func TestParallel_AnyCancelsSiblings(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	id := armParallel(store, CompletionAny, uA("u1"), uA("u2"), uA("u3"))
	d, _ := newParallelDecider(store)
	inst, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "u2"})
	if err != nil {
		t.Fatalf("first vote: %v", err)
	}
	if inst.CurrentStep != 1 || inst.CurrentApproverID != "u-dir" {
		t.Fatalf("after the first vote: step %d approver %q; want the next step", inst.CurrentStep, inst.CurrentApproverID)
	}
	var cancelled int
	for _, v := range store.visits[id] {
		if v.NodeID == "hoi-dong" && v.Status == "cancelled" && v.Outcome == SeatSuperseded {
			cancelled++
		}
	}
	if cancelled != 2 {
		t.Fatalf("siblings superseded = %d; want 2 (visit %+v)", cancelled, store.visits[id])
	}
	// A late click from a sibling finds the work gone from them.
	if _, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "u1"}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a sibling voting after the step ended: err = %v; want ErrNotAuthorized", err)
	}
}

func TestParallel_SameActorTwoSeats(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	// u1 is named AND holds role r1, which is also seated on the step.
	id := armParallel(store, CompletionAll, uA("u1"), Approver{Type: ApproverRole, ID: "r1"}, uA("u3"))
	d, _ := newParallelDecider(store)
	actor := Actor{UserID: "u1", RoleIDs: []string{"r1"}}
	if _, err := d.Decide(ctx, "t1", id, DecisionApprove, "", actor); err != nil {
		t.Fatalf("first vote: %v", err)
	}
	if _, err := d.Decide(ctx, "t1", id, DecisionApprove, "", actor); !errors.Is(err, ErrAlreadyDecidedByActor) {
		t.Fatalf("second vote of the same person on their other seat: err = %v; want ErrAlreadyDecidedByActor", err)
	}
	if store.voteCnt != 1 {
		t.Fatalf("votes taken = %d; want 1", store.voteCnt)
	}
	// Someone else holding the role still has the role seat.
	if _, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "u9", RoleIDs: []string{"r1"}}); err != nil {
		t.Fatalf("a different role holder votes the role seat: %v", err)
	}
}

func TestParallel_AdminWithoutSeatMustOverride(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	id := armParallel(store, CompletionAll, uA("u1"), uA("u2"))
	d, _ := newParallelDecider(store)
	admin := Actor{UserID: "u-admin", IsAdmin: true}
	if _, err := d.Decide(ctx, "t1", id, DecisionApprove, "", admin); !errors.Is(err, ErrAdminOverrideRequired) {
		t.Fatalf("admin approving a council step: err = %v; want ErrAdminOverrideRequired", err)
	}
	if store.voteCnt != 0 || store.instances[id].CurrentStep != 0 {
		t.Fatalf("the refused admin decision changed state (votes %d)", store.voteCnt)
	}
	// An admin who IS one of the approvers votes their own seat like anyone.
	id2 := armParallelAs(store, "i2", CompletionAll, uA("u-admin"), uA("u2"))
	if _, err := d.Decide(ctx, "t1", id2, DecisionApprove, "", admin); err != nil {
		t.Fatalf("admin voting their own seat: %v", err)
	}
	if store.voteCnt != 1 {
		t.Fatalf("votes taken = %d; want 1", store.voteCnt)
	}
}

// armParallelAs is armParallel under another instance id (a second record).
func armParallelAs(store *fakeStore, id string, mode CompletionMode, approvers ...Approver) string {
	armParallel(store, mode, approvers...)
	inst := store.instances["i1"]
	cp := *inst
	cp.ID, cp.RecordID = id, "q-"+id
	store.instances[id] = &cp
	store.pending[pkey("t1", "crm_quotes", cp.RecordID)] = true
	store.plantParallel(id, stepSeats(cp.Definition.Chain()[0], nil, ""))
	return id
}

// An admin who holds no seat decides a council step with the explicit override: the step ends
// at once, the outcome is written, whatever the votes were. Only an admin may.
func TestParallel_AdminOverrideSettlesTheStep(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	id := armParallel(store, CompletionAll, uA("u1"), uA("u2"), uA("u3"))
	d, w := newParallelDecider(store)
	if _, err := d.Override(ctx, "t1", id, DecisionReject, "không đủ điều kiện", Actor{UserID: "u1"}, nil); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("override by a non-admin: err = %v; want ErrNotAuthorized", err)
	}
	inst, err := d.Override(ctx, "t1", id, DecisionReject, "không đủ điều kiện", Actor{UserID: "u-admin", IsAdmin: true}, nil)
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if inst.Status != StatusRejected {
		t.Fatalf("status after the override = %s; want rejected", inst.Status)
	}
	last := inst.History[len(inst.History)-1]
	if last.Via != "override" || last.Comment != "không đủ điều kiện" || last.Action != HistoryRejected {
		t.Fatalf("history entry = %+v; want rejected via override with the reason", last)
	}
	if len(w.calls) != 1 || w.calls[0] != "crm_quotes.status=no" {
		t.Fatalf("outcome write-back = %+v; want the rejected value once", w.calls)
	}
}

// A single-seat step keeps today's behaviour for an admin: they decide it directly (D-13).
func TestAdmin_SingleSeatUnchanged(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	steps := []Step{{ID: "s1", ApproverType: ApproverUser, ApproverID: "u-mgr"}, {ID: "s2", ApproverType: ApproverUser, ApproverID: "u-dir"}}
	procWithSteps(t, store, "p1", "t1", steps)
	pinnedInstance(store, "i1", "t1", "p1", &Definition{Steps: steps})
	d := NewDecider(store, store, store, &fakeRecorder{}, nil)
	inst, err := d.Decide(ctx, "t1", "i1", DecisionApprove, "", Actor{UserID: "u-admin", IsAdmin: true})
	if err != nil {
		t.Fatalf("admin on a single-seat step: %v", err)
	}
	if inst.CurrentStep != 1 || store.voteCnt != 0 {
		t.Fatalf("step %d, votes %d; want advance to 1 by the plain decision path", inst.CurrentStep, store.voteCnt)
	}
}

// ---- review fixes: delegate target guard, parallel-ness from the step, stale override ------------

func newParallelRequester(store *fakeStore) *Requester {
	return NewRequester(&triggerLoader{}, store, store, store, nil)
}

// A council seat handed to someone who already holds a seat of the same visit, or who already
// voted in it, would be a dead seat: the target can cast one vote per visit (FR-022), so in `all`
// mode the step could never finish. The delegation is refused and nothing is written.
func TestParallel_DelegateRefusesTargetAlreadyInTheCouncil(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	id := armParallel(store, CompletionAll, uA("u1"), uA("u2"), uA("u3"))
	d, _ := newParallelDecider(store)
	r := newParallelRequester(store)

	// u2 holds an open seat.
	before := store.advanceCnt
	if _, err := r.Delegate(ctx, "t1", id, "u2", "", Actor{UserID: "u1"}); !errors.Is(err, ErrDelegateTargetSeated) {
		t.Fatalf("delegate to a seat holder: err = %v; want ErrDelegateTargetSeated", err)
	}
	// u2 votes; their seat is closed but they have voted in this visit.
	if _, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "u2"}); err != nil {
		t.Fatalf("u2 votes: %v", err)
	}
	if _, err := r.Delegate(ctx, "t1", id, "u2", "", Actor{UserID: "u1"}); !errors.Is(err, ErrDelegateTargetSeated) {
		t.Fatalf("delegate to someone who already voted: err = %v; want ErrDelegateTargetSeated", err)
	}
	if store.advanceCnt != before {
		t.Fatalf("a refused delegation wrote a transition (advanceCnt %d -> %d)", before, store.advanceCnt)
	}
	// A colleague outside the council is fine.
	if _, err := r.Delegate(ctx, "t1", id, "u9", "", Actor{UserID: "u1"}); err != nil {
		t.Fatalf("delegate to an outsider: %v", err)
	}
}

// "Many seats" is a property of the STEP, not of how many seats happen to be open: once all but
// one seat has voted, an admin without a seat still cannot move the last seat to anyone (they
// are told to override, exactly as for approve).
func TestParallel_DelegateLastOpenSeatStillNeedsASeat(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	id := armParallel(store, CompletionAll, uA("u1"), uA("u2"))
	d, _ := newParallelDecider(store)
	r := newParallelRequester(store)
	if _, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "u1"}); err != nil {
		t.Fatalf("u1 votes: %v", err)
	}
	before := store.advanceCnt
	if _, err := r.Delegate(ctx, "t1", id, "u9", "", Actor{UserID: "u-admin", IsAdmin: true}); !errors.Is(err, ErrAdminOverrideRequired) {
		t.Fatalf("admin without a seat delegating the last council seat: err = %v; want ErrAdminOverrideRequired", err)
	}
	if store.advanceCnt != before {
		t.Fatal("the refused delegation wrote a transition")
	}
	// The seat holder can, and the projection stays empty (still a council step).
	got, err := r.Delegate(ctx, "t1", id, "u9", "", Actor{UserID: "u2"})
	if err != nil {
		t.Fatalf("u2 delegates their own seat: %v", err)
	}
	if got.CurrentApproverType != "" || got.CurrentApproverID != "" {
		t.Fatalf("projection after delegating on a council step = %s/%s; want none", got.CurrentApproverType, got.CurrentApproverID)
	}
}

// The override carries the step the admin's screen showed, like every ordinary decision: a stale
// tab must not settle the NEXT step with a verb that bypasses every seat.
func TestParallel_OverrideStaleStepIsRefused(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	id := armParallel(store, CompletionAny, uA("u1"), uA("u2"))
	d, _ := newParallelDecider(store)
	admin := Actor{UserID: "u-admin", IsAdmin: true}
	// u1's vote ends the council step: the instance now stands on step 1 (the director).
	if _, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "u1"}); err != nil {
		t.Fatalf("u1 votes: %v", err)
	}
	if got := store.instances[id].CurrentStep; got != 1 {
		t.Fatalf("setup: current step = %d; want 1", got)
	}
	stale, fresh := 0, 1
	if _, err := d.Override(ctx, "t1", id, DecisionReject, "tab cũ", admin, &stale); !errors.Is(err, ErrConcurrent) {
		t.Fatalf("override from a screen that saw step 0: err = %v; want ErrConcurrent", err)
	}
	if store.instances[id].Status != StatusPending {
		t.Fatal("the stale override changed the instance")
	}
	if _, err := d.Override(ctx, "t1", id, DecisionReject, "đúng bước", admin, &fresh); err != nil {
		t.Fatalf("override at the step the screen saw: %v", err)
	}
}
