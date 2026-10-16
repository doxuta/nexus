package bpm

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// specs/043 S6b — team strategies: a step handed to a TEAM is resolved, when the instance enters
// it, to ONE member (round_robin / load_based: the Router rotates or counts) or to ALL members
// (each_member: one seat each, judged by the step's completion policy). The lookups behind
// ResolveTeam (cursor, member list, open-seat count) are proven in services and on MySQL; here the
// Router is a fake so the ENGINE's handling of one / many / none / error is pinned.

func teamNode(id string, strategy string, completion *Completion) Node {
	return Node{Step: Step{ID: id, Name: "Duyệt " + id, Assignment: &Assignment{Type: AssignTeam, ID: "tm1", Strategy: strategy}, Completion: completion, SLASeconds: 3600}, Kind: NodeApproval}
}

func members(n int) []Approver {
	out := make([]Approver, n)
	for i := range out {
		out[i] = uA(fmt.Sprintf("m%02d", i+1))
	}
	return out
}

func TestAssignment_StrategiesAreDynamicAndEachMemberIsParallel(t *testing.T) {
	for _, tc := range []struct {
		strategy string
		dynamic  bool
		parallel bool
	}{
		{"", false, false}, {StrategyPool, false, false},
		{StrategyRoundRobin, true, false}, {StrategyLoadBased, true, false}, {StrategyEachMember, true, true},
	} {
		st := Step{ID: "n", Assignment: &Assignment{Type: AssignTeam, ID: "tm1", Strategy: tc.strategy}}
		if got := st.Assignment.Dynamic(); got != tc.dynamic {
			t.Errorf("strategy %q: Dynamic() = %v; want %v", tc.strategy, got, tc.dynamic)
		}
		if got := st.Parallel(); got != tc.parallel {
			t.Errorf("strategy %q: Parallel() = %v; want %v", tc.strategy, got, tc.parallel)
		}
		// a strategy that needs a lookup opens nothing by itself (the router decides who)
		if seats := stepSeats(st, nil, ""); tc.dynamic && len(seats) != 0 {
			t.Errorf("strategy %q opened %+v without a lookup", tc.strategy, seats)
		}
	}
	// the same words on a user / role / queue / field are not a team strategy
	if (&Assignment{Type: AssignRole, ID: "r1", Strategy: StrategyRoundRobin}).Dynamic() {
		t.Error("a role with round_robin is not a dynamic lookup (the validator refuses it anyway)")
	}
}

// T107 — round robin: the router's pick holds the step, as ONE user seat; the router was asked for
// THIS process and node (the cursor key) and the team.
func TestRouting_TeamRoundRobinSeatsThePick(t *testing.T) {
	store := newFakeStore()
	rt := &fakeRouter{teams: map[string][]Approver{"tm1": {uA("duyet2")}}}
	p := routedProcess(nil, teamNode("ban-duyet", StrategyRoundRobin, nil))
	armRouted(t, store, rt, p, "u-sender")

	in := store.created[0]
	if len(in.Seats) != 1 || in.Seats[0].AssigneeType != ApproverUser || in.Seats[0].AssigneeID != "duyet2" || in.Seats[0].NodeID != "ban-duyet" {
		t.Fatalf("seats = %+v; want one user seat of duyet2 on ban-duyet", in.Seats)
	}
	if in.CurrentApproverType != ApproverUser || in.CurrentApproverID != "duyet2" || in.LastError != "" {
		t.Fatalf("projection %s/%s lastError %q; want the pick, not stuck", in.CurrentApproverType, in.CurrentApproverID, in.LastError)
	}
	if len(rt.inputs) != 1 || rt.inputs[0].ProcessID != "p1" || rt.inputs[0].NodeID != "ban-duyet" || rt.inputs[0].TenantID != "t1" {
		t.Fatalf("router asked %+v; want tenant t1, process p1, node ban-duyet (the cursor key)", rt.inputs)
	}
	if len(store.notes) != 1 || store.notes[0].UserID != "duyet2" {
		t.Fatalf("notes = %+v; want one notice, to the pick only", store.notes)
	}
}

// T112 — each_member: one user seat per member, projection NULL (several seats), every member told.
func TestRouting_EachMemberOpensASeatPerMember(t *testing.T) {
	store := newFakeStore()
	rt := &fakeRouter{teams: map[string][]Approver{"tm1": {uA("duyet1"), uA("duyet2"), uA("duyet3")}}}
	armRouted(t, store, rt, routedProcess(nil, teamNode("ban-duyet", StrategyEachMember, &Completion{Mode: CompletionQuorum, Percent: 60})), "u-sender")

	in := store.created[0]
	if len(in.Seats) != 3 {
		t.Fatalf("seats = %+v; want one per member", in.Seats)
	}
	for i, want := range []string{"duyet1", "duyet2", "duyet3"} {
		if in.Seats[i].AssigneeType != ApproverUser || in.Seats[i].AssigneeID != want || in.Seats[i].NodeID != "ban-duyet" {
			t.Fatalf("seat %d = %+v; want a user seat of %s", i, in.Seats[i], want)
		}
	}
	if in.CurrentApproverType != "" || in.CurrentApproverID != "" {
		t.Fatalf("projection %s/%s; want NULL on a several-seat step (D-5)", in.CurrentApproverType, in.CurrentApproverID)
	}
	if len(store.notes) != 3 {
		t.Fatalf("notes = %d; want a notice to each of the 3 members", len(store.notes))
	}
}

// T112 — the snapshot of a team is capped at MaxApprovers (50): a team of 60 opens 50 seats, the
// first 50 in the order the router handed them out.
func TestParallel_EachMemberSnapshotCap50(t *testing.T) {
	store := newFakeStore()
	rt := &fakeRouter{teams: map[string][]Approver{"tm1": members(MaxApprovers + 10)}}
	armRouted(t, store, rt, routedProcess(nil, teamNode("ban-duyet", StrategyEachMember, nil)), "u-sender")

	in := store.created[0]
	if len(in.Seats) != MaxApprovers {
		t.Fatalf("seats = %d; want the snapshot capped at %d", len(in.Seats), MaxApprovers)
	}
	if in.Seats[0].AssigneeID != "m01" || in.Seats[MaxApprovers-1].AssigneeID != fmt.Sprintf("m%02d", MaxApprovers) {
		t.Fatalf("first/last seat = %s/%s; want the first %d members in order", in.Seats[0].AssigneeID, in.Seats[MaxApprovers-1].AssigneeID, MaxApprovers)
	}
}

// An empty team is "nobody found" like an empty field: the fallback holds the step, else it is stuck.
func TestRouting_EmptyTeamUsesFallbackThenStuck(t *testing.T) {
	for _, strategy := range []string{StrategyRoundRobin, StrategyLoadBased, StrategyEachMember} {
		store := newFakeStore()
		opts := &Options{FallbackAssignee: &Approver{Type: ApproverRole, ID: "r-ops"}}
		armRouted(t, store, &fakeRouter{}, routedProcess(opts, teamNode("ban-duyet", strategy, nil)), "u")
		if in := store.created[0]; len(in.Seats) != 1 || in.Seats[0].AssigneeType != ApproverRole || in.Seats[0].AssigneeID != "r-ops" || in.LastError != "" {
			t.Fatalf("%s: seats %+v lastError %q; want the fallback role seat", strategy, in.Seats, in.LastError)
		}
		store2 := newFakeStore()
		armRouted(t, store2, &fakeRouter{}, routedProcess(nil, teamNode("ban-duyet", strategy, nil)), "u")
		if in := store2.created[0]; len(in.Seats) != 0 || in.LastError == "" {
			t.Fatalf("%s: seats %+v lastError %q; want stuck with a reason, not lost", strategy, in.Seats, in.LastError)
		}
	}
}

// A router that cannot read its store arms the approval stuck, never dropped.
func TestRouting_TeamRouterErrorOnArmIsStuck(t *testing.T) {
	store := newFakeStore()
	armRouted(t, store, &fakeRouter{err: errors.New("db down")}, routedProcess(nil, teamNode("ban-duyet", StrategyRoundRobin, nil)), "u")
	if len(store.created) != 1 || store.created[0].LastError == "" || len(store.created[0].Seats) != 0 {
		t.Fatalf("created %+v; want one stuck instance", store.created)
	}
}

// Without a router wired at all a team strategy behaves as "nobody found" — never a nil panic.
func TestRouting_TeamStrategyNoRouterWired(t *testing.T) {
	store := newFakeStore()
	armRouted(t, store, nil, routedProcess(nil, teamNode("ban-duyet", StrategyEachMember, nil)), "u")
	if in := store.created[0]; len(in.Seats) != 0 || in.LastError == "" {
		t.Fatalf("seats %+v lastError %q; want stuck", in.Seats, in.LastError)
	}
}

// End to end on the fake store: an each_member quorum-60 step armed from a team of 3 — duyet1 and
// duyet2 approve, the step is over, duyet3's seat closes (US3 scenario 3 through the team form).
func TestEachMemberQuorumEndToEnd(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	rt := &fakeRouter{teams: map[string][]Approver{"tm1": {uA("duyet1"), uA("duyet2"), uA("duyet3")}}}
	p := routedProcess(nil, teamNode("ban-duyet", StrategyEachMember, &Completion{Mode: CompletionQuorum, Percent: 60}),
		Node{Step: Step{ID: "giam-doc", Name: "Giám đốc", ApproverType: ApproverUser, ApproverID: "u-dir"}, Kind: NodeApproval})
	armRouted(t, store, rt, p, "u-sender")
	id := "inst-q1"
	store.plantParallel(id, store.created[0].Seats)
	d := NewDecider(store, store, store, &fakeRecorder{}, nil).WithRouter(rt)

	if inst, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "duyet1"}); err != nil || inst.CurrentStep != 0 {
		t.Fatalf("first approval: %v step %d; want still on the team step", err, inst.CurrentStep)
	}
	inst, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "duyet2"})
	if err != nil {
		t.Fatalf("second approval: %v", err)
	}
	if inst.CurrentStep != 1 || inst.CurrentApproverID != "u-dir" {
		t.Fatalf("after 2 of 3: step %d approver %q; want the director's step", inst.CurrentStep, inst.CurrentApproverID)
	}
	if _, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "duyet3"}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("duyet3 after the quorum: %v; want ErrNotAuthorized", err)
	}
}

// ---- the NEXT step is a stateful team strategy: the rotation advances once per ENTRY ------------

// armCouncilThenRoundRobin plants a 3-seat `all` council followed by a round_robin team step.
func armCouncilThenRoundRobin(store *fakeStore) string {
	g := &Graph{Nodes: []Node{
		{Step: Step{ID: "hoi-dong", Name: "Hội đồng", Approvers: []Approver{uA("u1"), uA("u2"), uA("u3")}, Completion: &Completion{Mode: CompletionAll}, SLASeconds: 3600}, Kind: NodeApproval},
		teamNode("ke-toan", StrategyRoundRobin, nil),
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

// A vote that does not finish the council must not turn the round-robin cursor of the step AFTER
// it: only the vote that opens that step does. (Before this, every approving vote routed the next
// step "just in case" — with 3 voters the cursor moved 3 times for one entry.)
func TestParallel_PartialVotesDoNotAdvanceTheNextStepsRotation(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	id := armCouncilThenRoundRobin(store)
	rt := &fakeRouter{teams: map[string][]Approver{"tm1": {uA("acc1")}}}
	d := NewDecider(store, store, store, &fakeRecorder{}, nil).WithRouter(rt)

	for _, voter := range []string{"u1", "u2"} {
		if _, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: voter}); err != nil {
			t.Fatalf("%s: %v", voter, err)
		}
	}
	if rt.teamCalls != 0 {
		t.Fatalf("the rotation was asked %d times by votes that did not open the next step; want 0", rt.teamCalls)
	}
	inst, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "u3"})
	if err != nil {
		t.Fatalf("last vote: %v", err)
	}
	if rt.teamCalls != 1 {
		t.Fatalf("the rotation was asked %d times by the vote that opened the step; want exactly 1", rt.teamCalls)
	}
	if inst.CurrentStep != 1 || inst.CurrentApproverID != "acc1" {
		t.Fatalf("step %d approver %q; want the round-robin pick on the next step", inst.CurrentStep, inst.CurrentApproverID)
	}
}

// The prediction "this vote will not finish the step" can be wrong when two votes land together:
// the transaction then finds the step finished with no seats prepared for the next one. The vote
// is retried with the next step routed — never applied with the next step holderless.
func TestParallel_WrongPredictionRetriesWithTheNextStepRouted(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	id := armCouncilThenRoundRobin(store)
	rt := &fakeRouter{teams: map[string][]Approver{"tm1": {uA("acc1")}}}
	d := NewDecider(store, store, store, &fakeRecorder{}, nil).WithRouter(rt)

	if _, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "u1"}); err != nil {
		t.Fatalf("u1: %v", err)
	}
	// u3 reads the visit with u2 still open (so its vote looks like it leaves the step open); u2's
	// vote lands before u3's transaction — u3's vote is the one that finishes the step.
	store.beforeVote = func(f *fakeStore) {
		f.beforeVote = nil
		after, _, err := castVote(f.visits[id], "hoi-dong-seat-1", "u2", SeatApproved, Completion{Mode: CompletionAll}, "", time.Now())
		if err != nil {
			t.Errorf("sibling vote: %v", err)
		}
		f.visits[id] = after
	}
	inst, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "u3"})
	if err != nil {
		t.Fatalf("u3: %v", err)
	}
	if inst.CurrentStep != 1 || inst.CurrentApproverID != "acc1" {
		t.Fatalf("step %d approver %q; want the round-robin pick on the next step (never a holderless step)", inst.CurrentStep, inst.CurrentApproverID)
	}
	if rt.teamCalls != 1 {
		t.Fatalf("the rotation was asked %d times; want exactly 1 (the retry), not once per attempt", rt.teamCalls)
	}
}
