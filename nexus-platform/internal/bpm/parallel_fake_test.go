package bpm

import (
	"context"
	"strconv"
	"time"
)

// The S4 half of fakeStore (kept beside the parallel tests, the struct fields live in
// engine_test.go): the seats of a visit in any state, and the vote transaction.

// ListVisitSeats answers with the visit a test planted (or VoteTaskTx / a completion moved on).
func (f *fakeStore) ListVisitSeats(_ context.Context, tenant, id, nodeID string) ([]VisitSeat, error) {
	inst, ok := f.instances[id]
	if !ok || inst.TenantID != tenant {
		return nil, nil
	}
	var out []VisitSeat
	for _, v := range f.visits[id] {
		if nodeID == "" || v.NodeID == "" || v.NodeID == nodeID { // the real read is per node
			out = append(out, v)
		}
	}
	return out, nil
}

// openVisit turns the seats a step opens into the open seats of a fresh visit.
func openVisit(seats []Seat, entrySeq int64) []VisitSeat {
	out := make([]VisitSeat, len(seats))
	for i, st := range seats {
		out[i] = VisitSeat{
			ID: st.NodeID + "-seat-" + strconv.Itoa(i), NodeID: st.NodeID, AssigneeType: st.AssigneeType,
			AssigneeID: st.AssigneeID, Status: "open", EntrySeq: entrySeq,
		}
	}
	return out
}

// plantParallel makes the instance stand on a parallel step: its seats (open) in both views.
func (f *fakeStore) plantParallel(id string, seats []Seat) {
	f.seats[id] = seats
	f.visits[id] = openVisit(seats, f.instances[id].Seq)
}

// VoteTaskTx mirrors the real transaction: the step the caller saw must still be the current one,
// the vote is judged by the same pure castVote, an unfinished step only moves the voter's seat,
// a finished one applies the prepared transition and replaces the visit.
func (f *fakeStore) VoteTaskTx(_ context.Context, tenant, id string, g VoteGuard, next InstanceTransition) (*VoteResult, error) {
	if f.beforeVote != nil {
		f.beforeVote(f)
	}
	inst, ok := f.instances[id]
	if !ok || inst.TenantID != tenant {
		return nil, ErrInstanceNotFound
	}
	if inst.Status != StatusPending {
		return nil, ErrNotPending
	}
	if inst.CurrentStep != g.ExpectStep {
		return nil, ErrConcurrent
	}
	after, verdict, err := castVote(f.visits[id], g.SeatID, g.Actor.UserID, g.Kind, g.policy(), g.Comment, time.Now())
	if err != nil {
		return nil, err
	}
	if g.Unrouted && verdict != VerdictPending {
		return nil, errRouteNeeded // as the real transaction: nothing written
	}
	open, approved, rejected := Tally(after)
	res := &VoteResult{Verdict: verdict, Open: open, Approved: approved, Rejected: rejected}
	f.voteCnt++
	inst.Seq++
	if verdict == VerdictPending {
		f.visits[id] = after
		var still []Seat
		for _, v := range after {
			if v.Status == "open" {
				still = append(still, Seat{NodeID: v.NodeID, AssigneeType: v.AssigneeType, AssigneeID: v.AssigneeID})
			}
		}
		f.seats[id] = still
		return res, nil
	}
	res.Completed = true
	for i := range after {
		if after[i].Status == "open" {
			after[i].Status, after[i].Outcome = "cancelled", SeatSuperseded
		}
	}
	inst.CurrentStep, inst.Status = next.CurrentStep, next.Status
	inst.CurrentApproverType, inst.CurrentApproverID = next.CurrentApproverType, next.CurrentApproverID
	inst.SLADueAt, inst.Escalated = next.SLADueAt, next.Escalated
	f.seats[id] = next.Seats.Open
	f.visits[id] = append(after, openVisit(next.Seats.Open, inst.Seq)...)
	if next.Status != StatusPending {
		f.pending[pkey(inst.TenantID, inst.EntityName, inst.RecordID)] = false
	}
	return res, nil
}
