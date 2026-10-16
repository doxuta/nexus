package bpm

// specs/043 S7 × S8: a rejection that returns, and the "Gửi lại" that starts over, may land on a
// MACHINE node — a gateway that routes on the record as it is by then. Both must walk on (not leave
// the hồ sơ standing on a node nobody holds).

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// gatewayFixture arms the graph route(gateway) → mgr(approval, onReject) → dir(approval) where route
// sends amount ≥ 5e7 to dir and everything else to mgr, and returns the stack with the hồ sơ
// standing on mgr (amount 1e6). startLate lists route LAST in the array (index 2) while it stays the
// start node, so "the first node" and "the start node" are different nodes.
func gatewayFixture(t *testing.T, onReject string, startLate bool) (*walkRig, *Instance) {
	t.Helper()
	route := `{"id":"route","name":"Phân luồng","kind":"gateway","default":"mgr","branches":[
			{"name":"lon","when":{"match":"and","rules":[{"field":"amount","op":"gte","value":50000000}]},"to":"dir"}]}`
	mgr := `{"id":"mgr","name":"Trưởng phòng","kind":"approval","approverType":"user","approverId":"u-mgr","next":"dir","onReject":` + onReject + `}`
	dir := `{"id":"dir","name":"Giám đốc","kind":"approval","approverType":"user","approverId":"u-dir"}`
	nodes := route + "," + mgr + "," + dir
	if startLate {
		nodes = mgr + "," + dir + "," + route
	}
	var g Graph
	if err := json.Unmarshal([]byte(`{"start":"route","nodes":[`+nodes+`]}`), &g); err != nil {
		t.Fatalf("graph: %v", err)
	}
	w := newWalkRig(g.normalized(), nil, map[string]any{"amount": 1e6})
	w.dec.now = func() time.Time { return time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC) }
	inst := w.arm(map[string]any{"amount": 1e6})
	if inst == nil || nodeOf(inst) != "mgr" || inst.Status != StatusPending {
		t.Fatalf("fixture: stands on %v; want pending on mgr", inst)
	}
	return w, inst
}

// A rejection "về bước route" (an earlier gateway): the walk reads the record NOW — which an editor
// has meanwhile raised above 50 triệu — and goes on to the director.
func TestReject_ReturnToGateway_RoutesOnTheRecordAsItIsNow(t *testing.T) {
	w, inst := gatewayFixture(t, `{"to":"route"}`, false)
	w.rec.data = map[string]any{"amount": 8e7} // raised while it waited

	got, err := w.dec.Decide(context.Background(), "t1", inst.ID, DecisionReject, "sai giá", Actor{UserID: "u-mgr"})
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if got.Status != StatusPending || nodeOf(got) != "dir" || got.CurrentApproverID != "u-dir" {
		t.Fatalf("after the return: %s on %s (%q); want pending on the director's step", got.Status, nodeOf(got), got.CurrentApproverID)
	}
	var actions []string
	for _, h := range got.History {
		actions = append(actions, h.Action)
	}
	if len(actions) < 3 || actions[len(actions)-2] != HistoryRejected || actions[len(actions)-1] != HistoryGateway {
		t.Fatalf("history %v; want …rejected, gateway", actions)
	}
	if e := got.History[len(got.History)-2]; e.ReturnedTo != "route" {
		t.Fatalf("rejected line = %+v; want returnedTo route", e)
	}
}

// "Gửi lại" with resumeAt "start" begins again at the graph's START node — the gateway — not at
// array index 0 of an unrelated node, and walks on from it.
func TestResubmit_StartOverAtTheGateway(t *testing.T) {
	w, inst := gatewayFixture(t, `{"to":"@submitter","resumeAt":"start"}`, true)
	w.store.instances[inst.ID].CreatedBy = "u-sub"

	if _, err := w.dec.Decide(context.Background(), "t1", inst.ID, DecisionReject, "thiếu chứng từ", Actor{UserID: "u-mgr"}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	// The rework seat belongs to the submitter (the fake store does not apply transitions' seats).
	w.store.seats[inst.ID] = []Seat{{NodeID: "mgr", AssigneeType: ApproverUser, AssigneeID: "u-sub", Kind: SeatKindRework}}
	w.rec.data = map[string]any{"amount": 8e7} // the submitter fixed the record: now a big one

	got, err := w.dec.Decide(context.Background(), "t1", inst.ID, DecisionComplete, "đã bổ sung", Actor{UserID: "u-sub"})
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if got.Status != StatusPending || nodeOf(got) != "dir" {
		t.Fatalf("after Gửi lại: %s on %s; want pending on dir (start over at the gateway, routed on the new amount)", got.Status, nodeOf(got))
	}
}
