package bpm

import (
	"encoding/json"
	"testing"
	"time"
)

// specs/043 S11 — repin, the pure half: where an instance lands on another version of its process
// and what is written for it. The repository half (CAS, seats, jobs) is proven on MySQL.

func repinSteps(ids ...string) []Step {
	out := make([]Step, len(ids))
	for i, id := range ids {
		out[i] = Step{ID: id, Name: "bước " + id, ApproverType: ApproverUser, ApproverID: "u-" + id, SLASeconds: 3600, SLAClock: SLAClockBusiness}
	}
	return out
}

func repinInstance(node string, def *Definition) *Instance {
	due := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	step := 0
	for i, s := range def.Chain() {
		if s.ID == node {
			step = i
		}
	}
	return &Instance{
		ID: "i1", TenantID: "t1", ProcessID: "p1", Status: StatusPending, CurrentStep: step, Seq: 4,
		CurrentApproverType: ApproverUser, CurrentApproverID: "u-" + node, SLADueAt: &due, Definition: def,
		ProcessVersion: def.Version,
	}
}

// TestRepin_MapsByNodeID_RebuildsSeats_KeepsDelegateAndDue (T172): the instance stays on the SAME
// step (by stable id) even when its index moved; the seat is rebuilt from the new definition, the
// deadline and the escalation stay, a delegated seat keeps its delegate.
func TestRepin_MapsByNodeID_RebuildsSeats_KeepsDelegateAndDue(t *testing.T) {
	v1 := &Definition{Steps: repinSteps("s1", "s2"), Version: 1, FirstDueBasis: SLABasisBusiness}
	newS2 := repinSteps("s9", "s1", "s2")
	newS2[2].ApproverID = "u-new-s2" // the approver of s2 changed in v2
	v2 := &Definition{Steps: newS2, Version: 2}
	now := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)

	t.Run("index moves, seat rebuilt, due kept", func(t *testing.T) {
		inst := repinInstance("s2", v1)
		plan, reason := planRepin(inst, []Seat{{NodeID: "s2", AssigneeType: ApproverUser, AssigneeID: "u-s2"}}, v2, "admin", now)
		if reason != "" {
			t.Fatalf("reason = %q; want a plan", reason)
		}
		n := plan.Next
		if n.CurrentStep != 2 || plan.ToNode != "s2" || plan.FromNode != "s2" {
			t.Fatalf("landed on step %d node %q (from %q); want index 2 node s2", n.CurrentStep, plan.ToNode, plan.FromNode)
		}
		if n.Status != StatusPending || n.SLADueAt == nil || !n.SLADueAt.Equal(*inst.SLADueAt) || n.Escalated {
			t.Fatalf("transition = %+v; want pending, due kept, not escalated", n)
		}
		if len(n.Seats.Open) != 1 || n.Seats.Open[0].AssigneeID != "u-new-s2" || n.Seats.Open[0].NodeID != "s2" {
			t.Fatalf("seats = %+v; want one seat held by the NEW approver", n.Seats.Open)
		}
		if n.CurrentApproverID != "u-new-s2" || !n.Seats.SameVisit || n.Seats.Close != SeatRepinned || n.Seats.Node != "s2" {
			t.Fatalf("projection/seat write = %+v / %+v", n, n.Seats)
		}
		if n.Pin == nil || n.Pin.Version != 2 || n.Pin.FirstDueBasis != SLABasisBusiness {
			t.Fatalf("pin = %+v; want version 2 carrying the first-due basis", n.Pin)
		}
		last := plan.History[len(plan.History)-1]
		if last.Action != HistoryRepinned || last.FromVersion != 1 || last.Version != 2 || last.Node != "s2" || last.Actor != "admin" {
			t.Fatalf("history entry = %+v", last)
		}
	})

	t.Run("delegated seat keeps its delegate", func(t *testing.T) {
		inst := repinInstance("s2", v1)
		inst.CurrentApproverID = "u-delegate"
		seat := Seat{NodeID: "s2", AssigneeType: ApproverUser, AssigneeID: "u-delegate", DelegatedFrom: "u-s2"}
		plan, reason := planRepin(inst, []Seat{seat}, v2, "admin", now)
		if reason != "" {
			t.Fatalf("reason = %q", reason)
		}
		got := plan.Next.Seats.Open
		if len(got) != 1 || got[0].AssigneeID != "u-delegate" || got[0].DelegatedFrom != "u-s2" || plan.Next.CurrentApproverID != "u-delegate" {
			t.Fatalf("seats = %+v projection %q; want the delegate kept", got, plan.Next.CurrentApproverID)
		}
	})

	t.Run("escalated step stays escalated", func(t *testing.T) {
		inst := repinInstance("s2", v1)
		inst.Escalated = true
		plan, _ := planRepin(inst, []Seat{{NodeID: "s2", AssigneeType: ApproverUser, AssigneeID: "u-s2"}}, v2, "admin", now)
		if !plan.Next.Escalated || len(plan.Next.Seats.Open) != 1 || plan.Next.Seats.Open[0].BreachNotifiedAt == nil {
			t.Fatalf("transition = %+v; want escalated, seat marked breached (no second alert)", plan.Next)
		}
	})
}

// TestRepin_NodeMissingRefusedPerInstance (T173 + T172's re-issued-id case): a step the target no
// longer has is refused, never mapped onto a neighbour; an id the target gave to a DIFFERENT step
// is a different node by construction (S10 never re-issues one).
func TestRepin_NodeMissingRefusedPerInstance(t *testing.T) {
	v1 := &Definition{Steps: repinSteps("s1", "s2"), Version: 1}
	v3 := &Definition{Steps: repinSteps("s1", "s3"), Version: 3} // s2 deleted, a new step got s3
	inst := repinInstance("s2", v1)
	_, reason := planRepin(inst, nil, v3, "admin", time.Now())
	if reason != RepinNodeMissing {
		t.Fatalf("reason = %q; want %q", reason, RepinNodeMissing)
	}
}

// TestRepin_NodeNotHumanRefused (T173): the node exists in the target but is not a person's step
// (a later slice's node kind); and a graph this binary cannot run is refused whole.
func TestRepin_NodeNotHumanRefused(t *testing.T) {
	v1 := &Definition{Steps: repinSteps("s1", "s2"), Version: 1}
	var g Graph
	if err := json.Unmarshal([]byte(`{"nodes":[{"id":"s1","kind":"approval","approverType":"user","approverId":"u"},{"id":"s2","kind":"wait"}]}`), &g); err != nil {
		t.Fatal(err)
	}
	v2 := &Definition{Steps: []Step{}, Graph: g.normalized(), Version: 2}
	_, reason := planRepin(repinInstance("s2", v1), nil, v2, "admin", time.Now())
	if reason != RepinNodeNotHuman {
		t.Fatalf("reason = %q; want %q", reason, RepinNodeNotHuman)
	}
	_, reason = planRepin(repinInstance("s1", v1), nil, v2, "admin", time.Now())
	if reason != RepinTargetUnrunnable { // the whole graph is unrunnable here, so even s1 cannot land
		t.Fatalf("unrunnable graph, s1: reason = %q; want %q", reason, RepinTargetUnrunnable)
	}
}

// TestRepin_LegacyNullSnapshot (T175): an instance armed before pins existed has no definition;
// it stands on s{current_step+1} (seatNodeID, Migration A's answer) and is pinned by the repin.
func TestRepin_LegacyNullSnapshot(t *testing.T) {
	inst := &Instance{ID: "i1", Status: StatusPending, CurrentStep: 1, Seq: 2, CurrentApproverType: ApproverUser, CurrentApproverID: "u-s2"}
	v2 := &Definition{Steps: repinSteps("s1", "s2"), Version: 2}
	plan, reason := planRepin(inst, nil, v2, "admin", time.Now())
	if reason != "" || plan.Next.CurrentStep != 1 || plan.FromNode != "s2" || plan.Next.Pin == nil || plan.Next.Pin.Version != 2 {
		t.Fatalf("plan = %+v reason %q; want s2 pinned to v2 at index 1", plan, reason)
	}
	if plan.Next.SLADueAt != nil {
		t.Fatalf("an instance without a deadline got %v", plan.Next.SLADueAt)
	}
}
