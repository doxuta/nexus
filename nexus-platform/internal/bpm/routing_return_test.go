package bpm

import (
	"context"
	"testing"
)

// specs/043 S6a/S6b — a return to an earlier node (onReject) and a resubmission ENTER that node
// again, so a node held by RULE (a team's turn, a record field, the submitter) is routed again:
// the instance must never come out holderless AND silent.

// reworkToRuleNode stands an instance on s2 (duyet2, onReject → s1) of a two-node graph whose s1 is
// the given rule node, with a router wired.
func reworkToRuleNode(t *testing.T, s1 Node, rt Router) (*fakeStore, *Decider, string) {
	t.Helper()
	s1.Step.ID, s1.Kind = "s1", NodeApproval
	g := &Graph{Nodes: []Node{
		s1,
		{Step: Step{ID: "s2", Name: "Kế toán", ApproverType: ApproverUser, ApproverID: "duyet2", SLASeconds: 7200, SLAClock: "wall"}, Kind: NodeApproval, OnReject: &OnReject{To: "s1"}},
	}}
	def := &Definition{Steps: []Step{}, Graph: g.normalized(), Outcome: &Outcome{StatusField: "status", RejectedValue: "Từ chối"}}
	if len(def.Chain()) != 2 {
		t.Fatalf("the fixture graph is not runnable: chain = %d", len(def.Chain()))
	}
	store := newFakeStore()
	id, err := store.CreateInstance(context.Background(), CreateInstanceInput{
		TenantID: "t1", ProcessID: "p1", EntityName: "crm_quotes", RecordID: "r1",
		CurrentApproverType: ApproverUser, CurrentApproverID: "duyet2", Definition: def, CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	store.instances[id].CurrentStep = 1
	store.seats[id] = []Seat{{NodeID: "s2", AssigneeType: ApproverUser, AssigneeID: "duyet2"}}
	return store, NewDecider(store, store, store, nil, nil).WithRouter(rt), id
}

func TestReject_ReturnToATeamStrategyNodeRoutesIt(t *testing.T) {
	rt := &fakeRouter{teams: map[string][]Approver{"tm1": {uA("acc1")}}}
	store, d, id := reworkToRuleNode(t, teamNode("s1", StrategyRoundRobin, nil), rt)
	got, err := d.Decide(context.Background(), "t1", id, DecisionReject, "sai đơn giá", Actor{UserID: "duyet2"})
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	open := store.lastNext.Seats.Open
	if got.CurrentStep != 0 || len(open) != 1 || open[0].AssigneeID != "acc1" || open[0].NodeID != "s1" {
		t.Fatalf("step %d seats %+v; want the round-robin pick seated on s1 (a return is an ENTRY into the node)", got.CurrentStep, open)
	}
	if rt.teamCalls != 1 || got.CurrentApproverID != "acc1" {
		t.Fatalf("rotation asked %d times, holder %q; want once and acc1", rt.teamCalls, got.CurrentApproverID)
	}
}

func TestReject_ReturnToAFieldNodeRoutesItOrIsStuckNotHolderless(t *testing.T) {
	rt := &fakeRouter{holders: map[string]Approver{"field:" + OwnerField: uA("u-owner")}}
	store, d, id := reworkToRuleNode(t, fieldNode("s1", OwnerField), rt)
	if _, err := d.Decide(context.Background(), "t1", id, DecisionReject, "x", Actor{UserID: "duyet2"}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if open := store.lastNext.Seats.Open; len(open) != 1 || open[0].AssigneeID != "u-owner" {
		t.Fatalf("seats %+v; want the record's owner on s1", open)
	}
	// nobody in the field, no fallback: the instance says why instead of waiting on no one in silence
	store2, d2, id2 := reworkToRuleNode(t, fieldNode("s1", "assigned_to"), &fakeRouter{})
	if _, err := d2.Decide(context.Background(), "t1", id2, DecisionReject, "x", Actor{UserID: "duyet2"}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if len(store2.lastNext.Seats.Open) != 0 || store2.lastNext.LastError == "" {
		t.Fatalf("seats %+v lastError %q; want stuck WITH a reason", store2.lastNext.Seats.Open, store2.lastNext.LastError)
	}
}
