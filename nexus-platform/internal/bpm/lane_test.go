package bpm

import (
	"errors"
	"testing"

	"nexus/platform/internal/workflow"
)

func trackerProcess(id string) Process {
	return Process{
		ID: id, TenantID: "t1", EntityName: "tk_tickets", Trigger: TriggerAfterCreate, IsActive: true,
		Options: &Options{Lane: LaneIndependent},
		Graph: &Graph{Start: "wait", Nodes: []Node{
			{Step: Step{ID: "wait", Name: "Phản hồi đầu tiên"}, Kind: NodeWait, Next: "end",
				Until: &workflow.Condition{Rules: []workflow.Rule{{Field: "status", Op: "neq", Value: "Open"}}},
				SLA:   &WaitSLA{Amount: 4, Unit: "hours", Notify: &Assignment{Type: AssignSubmitter}}},
			{Step: Step{ID: "end"}, Kind: NodeEnd, Result: ResultApproved},
		}},
	}
}

// TestIndependentLane_CoexistsWithApproval (T183): a record already under approval still gets its SLA
// tracker; the tracker is stamped 'independent' and does not use up the approval slot.
func TestIndependentLane_CoexistsWithApproval(t *testing.T) {
	store := newFakeStore()
	store.pending[pkey("t1", "tk_tickets", "r1")] = true // an approval is already open on the record
	approval := Process{ID: "p-ap", TenantID: "t1", EntityName: "tk_tickets", Trigger: TriggerAfterCreate, IsActive: true,
		Steps: []Step{{ApproverType: ApproverUser, ApproverID: "u-mgr"}}}
	// the tracker is listed AFTER the approval: it must still arm
	e := NewAsyncEngine(EngineConfig{Buffer: 4, Workers: 1}, fakeLoader{procs: []Process{approval, trackerProcess("p-sla")}}, store, store, nil)
	e.arm(Event{TenantID: "t1", EntityName: "tk_tickets", RecordID: "r1", Trigger: TriggerAfterCreate, Data: map[string]any{"status": "Open"}})
	if len(store.created) != 1 || store.created[0].ProcessID != "p-sla" || store.created[0].Lane != LaneIndependent {
		t.Fatalf("created = %+v; want exactly the tracker, on the independent lane (the approval is already open)", store.created)
	}

	// no approval open: both arm — the approval on the default lane, the tracker beside it
	store2 := newFakeStore()
	e2 := NewAsyncEngine(EngineConfig{Buffer: 4, Workers: 1}, fakeLoader{procs: []Process{trackerProcess("p-sla"), approval}}, store2, store2, nil)
	e2.arm(Event{TenantID: "t1", EntityName: "tk_tickets", RecordID: "r1", Trigger: TriggerAfterCreate, Data: map[string]any{"status": "Open"}})
	lanes := map[string]string{}
	for _, c := range store2.created {
		lanes[c.ProcessID] = c.Lane
	}
	if len(lanes) != 2 || lanes["p-sla"] != LaneIndependent || lanes["p-ap"] != LaneExclusive {
		t.Fatalf("lanes = %v; want the approval exclusive and the tracker independent", lanes)
	}
}

// TestIndependentLane_HiddenFromApprovalsTabAndRecall (T183): a tracker cannot be withdrawn — not
// even by its "creator" or an admin — and a manual send never picks a tracker process.
func TestIndependentLane_HiddenFromApprovalsTabAndRecall(t *testing.T) {
	store := newFakeStore()
	e := NewAsyncEngine(EngineConfig{Buffer: 4, Workers: 1}, fakeLoader{procs: []Process{trackerProcess("p-sla")}}, store, store, nil)
	e.arm(Event{TenantID: "t1", EntityName: "tk_tickets", RecordID: "r1", Trigger: TriggerAfterCreate, ActorUserID: "u1", Data: map[string]any{"status": "Open"}})
	if len(store.created) != 1 {
		t.Fatalf("created %d; want the tracker armed", len(store.created))
	}
	manual := trackerProcess("p-sla")
	manual.Trigger = TriggerManual // would be refused at save; the engine must not trust that
	r := NewRequester(fakeLoader{procs: []Process{manual}}, store, store, store, nil)
	for _, actor := range []Actor{{UserID: "u1"}, {UserID: "adm", IsAdmin: true}} {
		if _, err := r.Recall(t.Context(), "t1", "inst-r1", "", actor); !errors.Is(err, ErrTrackingLane) {
			t.Fatalf("Recall(%+v) err = %v; want ErrTrackingLane", actor, err)
		}
	}
	if _, err := r.Submit(t.Context(), "t1", "tk_tickets", "r2", map[string]any{}, Actor{UserID: "u1"}); !errors.Is(err, ErrNoApprovalProcess) {
		t.Fatalf("Submit err = %v; a tracker is not a process a person can send", err)
	}
}

// TestValidate_IndependentLaneRejectsHumanNodes (T184).
func TestValidate_IndependentLaneRejectsHumanNodes(t *testing.T) {
	ind := &Options{Lane: LaneIndependent}
	tests := []struct {
		name    string
		o       *Options
		trigger Trigger
		kinds   []NodeKind
		field   string
	}{
		{"machine nodes", ind, TriggerAfterCreate, []NodeKind{NodeWait, NodeGateway, NodeAuto, NodeEnd}, ""},
		{"approval node", ind, TriggerAfterCreate, []NodeKind{NodeWait, NodeApproval}, "graph.nodes[1].kind"},
		{"task node", ind, TriggerAfterUpdate, []NodeKind{NodeTask}, "graph.nodes[0].kind"},
		{"manual trigger", ind, TriggerManual, []NodeKind{NodeWait}, "trigger"},
		{"linear steps", ind, TriggerAfterCreate, nil, "graph"},
		{"exclusive is never judged", &Options{}, TriggerManual, nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if f, _ := CheckLane(tc.o, tc.trigger, tc.kinds); f != tc.field {
				t.Fatalf("CheckLane field = %q; want %q", f, tc.field)
			}
		})
	}
	if f, _ := CheckOptions(&Options{Lane: "sideways"}); f != "lane" {
		t.Fatalf("CheckOptions blames %q for an unknown lane; want \"lane\"", f)
	}
}
