package bpm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"nexus/platform/internal/workflow"
)

// specs/043 S7: gateways, auto nodes and end nodes — the instance walks through them by itself and
// stops only on a person's step, a terminal state or a failure (stalled with last_error until an
// admin presses "Thử lại").

// fakeRecords is the record side of the walk: what a gateway reads, what an auto node and the
// outcome write-back write.
type fakeRecords struct {
	data       map[string]any
	readErr    error
	reads      int
	writes     []map[string]any // SetFields calls, in order
	writeErr   error
	outcomes   [][2]string // SetField (field, value) calls, in order
	outcomeErr error
}

func (f *fakeRecords) GetRecord(_ context.Context, _, _, _ string) (map[string]any, error) {
	f.reads++
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.data, nil
}

func (f *fakeRecords) SetFields(_ context.Context, _, _, _ string, fields map[string]any, _ string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes = append(f.writes, fields)
	return nil
}

func (f *fakeRecords) SetField(_ context.Context, _, _, _, field string, value any, _ string) error {
	if f.outcomeErr != nil {
		return f.outcomeErr
	}
	f.outcomes = append(f.outcomes, [2]string{field, value.(string)})
	return nil
}

func cond(match string, rules ...workflow.Rule) *workflow.Condition {
	return &workflow.Condition{Match: match, Rules: rules}
}

func amount(op string, v float64) workflow.Rule {
	return workflow.Rule{Field: "amount", Op: op, Value: v}
}

// quoteGraph is the spec's independent test: under 5 million approves itself, under 50 million goes
// to the manager, from 50 million to the manager and then the director.
func quoteGraph() *Graph {
	return &Graph{Start: "route", Nodes: []Node{
		{Step: Step{ID: "route", Name: "Phân luồng"}, Kind: NodeGateway, Default: "mgr", Branches: []Branch{
			{Name: "tu-duyet", When: cond("", amount("lt", 5e6)), To: "auto-ok"},
			{Name: "cap-1", When: cond("", amount("lt", 5e7)), To: "mgr"},
		}},
		{Step: Step{ID: "auto-ok", Name: "Tự duyệt"}, Kind: NodeAuto, Set: []SetField{{Field: "status", Value: "Chấp nhận"}}, Next: "done"},
		{Step: Step{ID: "mgr", Name: "Trưởng phòng", ApproverType: ApproverUser, ApproverID: "u-mgr"}, Kind: NodeApproval, Next: "big"},
		{Step: Step{ID: "big", Name: "Trên 50 triệu?"}, Kind: NodeGateway, Default: "done", Branches: []Branch{
			{Name: "tren-50tr", When: cond("", amount("gte", 5e7)), To: "dir"},
		}},
		{Step: Step{ID: "dir", Name: "Giám đốc", ApproverType: ApproverUser, ApproverID: "u-dir"}, Kind: NodeApproval, Next: "done"},
		{Step: Step{ID: "done", Name: "Xong"}, Kind: NodeEnd, Result: ResultApproved},
	}}
}

// walkRig wires an arming engine, a decider and a runner over one fake store and one fake record.
type walkRig struct {
	store  *fakeStore
	rec    *fakeRecords
	runner *Runner
	eng    *AsyncEngine
	dec    *Decider
}

func newWalkRig(g *Graph, out *Outcome, data map[string]any) *walkRig {
	store := newFakeStore()
	rec := &fakeRecords{data: data}
	runner := NewRunner(store, rec, rec, nil, nil).WithRecordWriter(rec)
	proc := Process{ID: "p1", TenantID: "t1", EntityName: "crm_quotes", Trigger: TriggerAfterCreate, Graph: g, Outcome: out, IsActive: true}
	eng := NewAsyncEngine(EngineConfig{Buffer: 4, Workers: 1}, fakeLoader{procs: []Process{proc}}, store, store, nil).WithRunner(runner)
	dec := NewDecider(store, store, store, nil, nil).WithRunner(runner).WithRecordWriter(rec)
	return &walkRig{store: store, rec: rec, runner: runner, eng: eng, dec: dec}
}

func (w *walkRig) arm(eventData map[string]any) *Instance {
	w.eng.arm(Event{TenantID: "t1", EntityName: "crm_quotes", RecordID: "q1", Trigger: TriggerAfterCreate, Data: eventData})
	return w.store.instances["inst-q1"]
}

func nodeOf(inst *Instance) string { return inst.Definition.Chain()[inst.CurrentStep].ID }

// TestGateway_FirstMatchingBranchThenDefault (T121): branches are tried in order and the FIRST that
// holds wins (3 triệu matches both of the first two); none holds ⇒ the default; the branch taken is
// recorded in the context for the timeline.
func TestGateway_FirstMatchingBranchThenDefault(t *testing.T) {
	tests := []struct {
		name       string
		data       map[string]any
		wantNode   string
		wantStatus InstanceStatus
		wantBranch string
	}{
		{"under 5m: first branch wins over the second that also holds", map[string]any{"amount": 3e6}, "done", StatusApproved, "tu-duyet"},
		{"under 50m: second branch", map[string]any{"amount": 3e7}, "mgr", StatusPending, "cap-1"},
		{"no branch holds: the default", map[string]any{"amount": 8e7}, "mgr", StatusPending, DefaultBranch},
		{"field absent: the default", map[string]any{}, "mgr", StatusPending, DefaultBranch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newWalkRig(quoteGraph(), nil, tc.data)
			inst := w.arm(tc.data)
			if inst == nil {
				t.Fatal("process did not arm")
			}
			if inst.Status != tc.wantStatus || nodeOf(inst) != tc.wantNode {
				t.Fatalf("stands on %s (%s); want %s (%s)", nodeOf(inst), inst.Status, tc.wantNode, tc.wantStatus)
			}
			if got := inst.Context.Gateway()["route"]; got != tc.wantBranch {
				t.Fatalf("context.gateway[route] = %q; want %q", got, tc.wantBranch)
			}
			if inst.LastError != "" {
				t.Fatalf("last_error %q; want none", inst.LastError)
			}
		})
	}

	// "duyet1 rồi duyet3": 80 triệu goes manager → director, 30 triệu stops after the manager.
	w := newWalkRig(quoteGraph(), nil, map[string]any{"amount": 8e7})
	inst := w.arm(map[string]any{"amount": 8e7})
	mgr := Actor{UserID: "u-mgr"}
	if _, err := w.dec.Decide(context.Background(), "t1", inst.ID, DecisionApprove, "", mgr); err != nil {
		t.Fatalf("manager approves: %v", err)
	}
	inst = w.store.instances[inst.ID]
	if nodeOf(inst) != "dir" || inst.Status != StatusPending || inst.CurrentApproverID != "u-dir" {
		t.Fatalf("after the manager: %s (%s) approver %q; want director's step", nodeOf(inst), inst.Status, inst.CurrentApproverID)
	}
	if got := inst.Context.Gateway(); got["route"] != DefaultBranch || got["big"] != "tren-50tr" {
		t.Fatalf("path taken %v; want route=default, big=tren-50tr", got)
	}
	out, err := w.dec.Decide(context.Background(), "t1", inst.ID, DecisionApprove, "", Actor{UserID: "u-dir"})
	if err != nil || out.Status != StatusApproved {
		t.Fatalf("director approves: %v status %v; want approved", err, out)
	}

	w = newWalkRig(quoteGraph(), nil, map[string]any{"amount": 3e7})
	inst = w.arm(map[string]any{"amount": 3e7})
	out, err = w.dec.Decide(context.Background(), "t1", inst.ID, DecisionApprove, "", mgr)
	if err != nil || out.Status != StatusApproved {
		t.Fatalf("30m after the manager: %v %v; want approved with no director step", err, out)
	}
}

// TestGateway_OrCondition (T121): a branch whose condition is OR holds when ANY rule does.
func TestGateway_OrCondition(t *testing.T) {
	g := &Graph{Nodes: []Node{
		{Step: Step{ID: "route"}, Kind: NodeGateway, Default: "end-no", Branches: []Branch{
			{Name: "vip-or-big", When: cond("or", amount("gt", 1e9), workflow.Rule{Field: "vip", Op: "eq", Value: true}), To: "end-yes"},
		}},
		{Step: Step{ID: "end-yes"}, Kind: NodeEnd, Result: ResultApproved},
		{Step: Step{ID: "end-no"}, Kind: NodeEnd, Result: ResultRejected},
	}}
	for name, tc := range map[string]struct {
		data map[string]any
		want InstanceStatus
	}{
		"first rule holds":  {map[string]any{"amount": 2e9, "vip": false}, StatusApproved},
		"second rule holds": {map[string]any{"amount": 1.0, "vip": true}, StatusApproved},
		"neither holds":     {map[string]any{"amount": 1.0, "vip": false}, StatusRejected},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWalkRig(g, nil, tc.data)
			if inst := w.arm(tc.data); inst.Status != tc.want {
				t.Fatalf("status %s; want %s", inst.Status, tc.want)
			}
		})
	}
}

// TestGateway_FreshRecordData (T121): the gateway judges the record as it is NOW. The event that
// armed the process said 3 triệu; by the time the walk reads the record it says 80 triệu, so the
// quote is NOT approved by itself.
func TestGateway_FreshRecordData(t *testing.T) {
	w := newWalkRig(quoteGraph(), nil, map[string]any{"amount": 8e7}) // the record now
	inst := w.arm(map[string]any{"amount": 3e6})                      // the stale event
	if inst.Status != StatusPending || nodeOf(inst) != "mgr" {
		t.Fatalf("stands on %s (%s); want the manager's step — the event's amount must not decide", nodeOf(inst), inst.Status)
	}
	if w.rec.reads == 0 {
		t.Fatal("the gateway never read the record")
	}
}

// TestAutoNode_WritesThroughRecordWriter_FailSetsLastError (T122): an auto node writes through the
// record writer; when the record refuses, the instance stays on that node, pending, with the reason
// readable and a history line — never half-written, never lost.
func TestAutoNode_WritesThroughRecordWriter_FailSetsLastError(t *testing.T) {
	data := map[string]any{"amount": 3e6}
	w := newWalkRig(quoteGraph(), nil, data)
	// Every layer wraps the error in its "pkg.method:" prefix; the admin reads the cause only.
	w.rec.writeErr = fmt.Errorf("services.ApprovalOutcomeWriter.SetFields: %w", errors.New(`giá trị "Chấp nhận" không có trong danh sách`))
	inst := w.arm(data)

	if inst.Status != StatusPending || nodeOf(inst) != "auto-ok" {
		t.Fatalf("stands on %s (%s); want pending on auto-ok", nodeOf(inst), inst.Status)
	}
	if !strings.Contains(inst.LastError, "không có trong danh sách") || strings.Contains(inst.LastError, "services.") {
		t.Fatalf("last_error %q; want the writer's reason without the code path", inst.LastError)
	}
	last := inst.History[len(inst.History)-1]
	if last.Action != HistoryEffectFailed || last.Node != "auto-ok" || last.Comment != inst.LastError {
		t.Fatalf("last history %+v; want effect_failed on auto-ok with the same reason", last)
	}
	// The walk stopped: a second Continue does not retry by itself (an admin's decision).
	before := w.store.advanceCnt
	if err := w.runner.Continue(context.Background(), "t1", inst.ID); err != nil || w.store.advanceCnt != before {
		t.Fatalf("Continue on a stalled instance moved it (err %v, writes %d→%d)", err, before, w.store.advanceCnt)
	}
	// And nobody can approve a gateway/auto node by hand, admin included.
	if _, err := w.dec.Decide(context.Background(), "t1", inst.ID, DecisionApprove, "", Actor{UserID: "admin", IsAdmin: true}); !errors.Is(err, ErrWrongStepKind) {
		t.Fatalf("admin approve on an auto node: %v; want ErrWrongStepKind", err)
	}
}

// TestRetry_AfterFixAdvances (T122): after the cause is fixed, Retry clears the error, writes the
// press to the history and the walk runs to the end.
func TestRetry_AfterFixAdvances(t *testing.T) {
	data := map[string]any{"amount": 3e6}
	w := newWalkRig(quoteGraph(), &Outcome{StatusField: "status", ApprovedValue: "Chấp nhận"}, data)
	w.rec.writeErr = errors.New("boom")
	inst := w.arm(data)
	if inst.LastError == "" {
		t.Fatal("setup: the instance should be stalled")
	}

	w.rec.writeErr = nil // the admin fixed the process / the data
	got, err := w.runner.Retry(context.Background(), "t1", inst.ID, Actor{UserID: "admin", IsAdmin: true}, "đã sửa danh sách")
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if got.Status != StatusApproved || got.LastError != "" {
		t.Fatalf("after retry: %s last_error %q; want approved and clear", got.Status, got.LastError)
	}
	var actions []string
	for _, h := range got.History {
		actions = append(actions, h.Action)
	}
	if !contains(actions, HistoryRetried) || !contains(actions, HistoryAuto) || !contains(actions, HistoryEnded) {
		t.Fatalf("history %v; want retried, auto and ended", actions)
	}
	if len(w.rec.writes) != 1 || w.rec.writes[0]["status"] != "Chấp nhận" {
		t.Fatalf("auto writes %v; want one write status=Chấp nhận", w.rec.writes)
	}
	if len(w.rec.outcomes) != 1 {
		t.Fatalf("outcome write-backs %v; want one (the end node settled approved)", w.rec.outcomes)
	}

	// Not stuck ⇒ nothing to retry; finished ⇒ not pending.
	if _, err := w.runner.Retry(context.Background(), "t1", inst.ID, Actor{UserID: "admin"}, ""); !errors.Is(err, ErrNotPending) {
		t.Fatalf("Retry on a finished instance: %v; want ErrNotPending", err)
	}
	w2 := newWalkRig(quoteGraph(), nil, map[string]any{"amount": 3e7})
	pendingOnPerson := w2.arm(map[string]any{"amount": 3e7})
	if _, err := w2.runner.Retry(context.Background(), "t1", pendingOnPerson.ID, Actor{UserID: "admin"}, ""); !errors.Is(err, ErrNotStuck) {
		t.Fatalf("Retry on an instance waiting for a person: %v; want ErrNotStuck", err)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// TestEndNode_AutoApproveWritesOutcome (T123): an end node settles the instance and writes the
// process outcome — approved or rejected — back onto the record, with no person involved.
func TestEndNode_AutoApproveWritesOutcome(t *testing.T) {
	g := &Graph{Nodes: []Node{
		{Step: Step{ID: "route"}, Kind: NodeGateway, Default: "end-no", Branches: []Branch{
			{Name: "small", When: cond("", amount("lt", 5e6)), To: "end-yes"},
		}},
		{Step: Step{ID: "end-yes"}, Kind: NodeEnd, Result: ResultApproved},
		{Step: Step{ID: "end-no"}, Kind: NodeEnd, Result: ResultRejected},
	}}
	out := &Outcome{StatusField: "status", ApprovedValue: "Chấp nhận", RejectedValue: "Từ chối"}
	for name, tc := range map[string]struct {
		amount    float64
		wantState InstanceStatus
		wantValue string
	}{
		"small approves itself": {3e6, StatusApproved, "Chấp nhận"},
		"large is rejected":     {9e6, StatusRejected, "Từ chối"},
	} {
		t.Run(name, func(t *testing.T) {
			data := map[string]any{"amount": tc.amount}
			w := newWalkRig(g, out, data)
			inst := w.arm(data)
			if inst.Status != tc.wantState {
				t.Fatalf("status %s; want %s", inst.Status, tc.wantState)
			}
			if len(w.rec.outcomes) != 1 || w.rec.outcomes[0] != [2]string{"status", tc.wantValue} {
				t.Fatalf("write-backs %v; want status=%s", w.rec.outcomes, tc.wantValue)
			}
			if h := inst.History[len(inst.History)-1]; h.Action != HistoryEnded {
				t.Fatalf("last history %+v; want an ended line", h)
			}
		})
	}
}

// TestEffectFailed_RecordedInHistory (T126, FR-010): the write-back after an end node fails — the
// settlement stands, and the failure is in the history, not only in the log.
func TestEffectFailed_RecordedInHistory(t *testing.T) {
	g := &Graph{Nodes: []Node{{Step: Step{ID: "end"}, Kind: NodeEnd, Result: ResultApproved}}}
	data := map[string]any{}
	w := newWalkRig(g, &Outcome{StatusField: "status", ApprovedValue: "Chấp nhận"}, data)
	w.rec.outcomeErr = errors.New("trường status bị khoá")
	inst := w.arm(data)
	if inst.Status != StatusApproved {
		t.Fatalf("status %s; the settlement must stand when the write-back fails", inst.Status)
	}
	last := inst.History[len(inst.History)-1]
	if last.Action != HistoryEffectFailed || !strings.Contains(last.Comment, "trường status bị khoá") {
		t.Fatalf("last history %+v; want effect_failed carrying the writer's reason", last)
	}
}

// TestContinueJob_StaleSeqNoop (T125): the durable wake-up does nothing when the instance has moved
// on — the synchronous walk won — and walks it when it has not.
func TestContinueJob_StaleSeqNoop(t *testing.T) {
	store := newFakeStore()
	g := &Graph{Nodes: []Node{{Step: Step{ID: "end"}, Kind: NodeEnd, Result: ResultApproved}}}
	pin := &Definition{Steps: []Step{}, Graph: g}
	store.instances["i1"] = &Instance{ID: "i1", TenantID: "t1", Status: StatusPending, Seq: 5, Definition: pin}
	store.instances["done"] = &Instance{ID: "done", TenantID: "t1", Status: StatusApproved, Seq: 5, Definition: pin}
	store.instances["stuck"] = &Instance{ID: "stuck", TenantID: "t1", Status: StatusPending, Seq: 5, LastError: "boom", Definition: pin}

	var walked []string
	h := NewContinueJobHandler(store, continuerFunc(func(_ context.Context, tenant, id string) error {
		walked = append(walked, tenant+"/"+id)
		return nil
	}))
	job := func(tenant, id string, seq int64) error {
		p, _ := json.Marshal(continuePayload{InstanceID: id, Seq: seq})
		return h(context.Background(), jobsJob(tenant, p))
	}
	for name, tc := range map[string]struct {
		tenant, id string
		seq        int64
		want       int
	}{
		"stale seq":           {"t1", "i1", 4, 0},
		"already settled":     {"t1", "done", 5, 0},
		"stalled on an error": {"t1", "stuck", 5, 0},
		"another tenant":      {"t2", "i1", 5, 0},
		"missing":             {"t1", "gone", 5, 0},
		"current":             {"t1", "i1", 5, 1},
	} {
		walked = nil
		if err := job(tc.tenant, tc.id, tc.seq); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(walked) != tc.want {
			t.Fatalf("%s: walked %v; want %d walk(s)", name, walked, tc.want)
		}
	}
}

// TestEnter_HopCap32SetsLastError (T125): a loop of machine nodes that slipped past the validator
// (a hand-edited pin) is stopped at the cap with a readable error, not spun forever.
func TestEnter_HopCap32SetsLastError(t *testing.T) {
	store := newFakeStore()
	rec := &fakeRecords{}
	g := &Graph{Nodes: []Node{
		{Step: Step{ID: "a"}, Kind: NodeAuto, Set: []SetField{{Field: "x", Value: "1"}}, Next: "b"},
		{Step: Step{ID: "b"}, Kind: NodeAuto, Set: []SetField{{Field: "x", Value: "2"}}, Next: "a"},
	}}
	store.instances["i1"] = &Instance{ID: "i1", TenantID: "t1", EntityName: "crm_quotes", RecordID: "q1",
		Status: StatusPending, Seq: 1, Definition: &Definition{Steps: []Step{}, Graph: g.normalized()}}
	if err := NewRunner(store, rec, rec, nil, nil).Continue(context.Background(), "t1", "i1"); err != nil {
		t.Fatalf("Continue: %v", err)
	}
	inst := store.instances["i1"]
	if inst.Status != StatusPending || !strings.Contains(inst.LastError, "32") {
		t.Fatalf("%s last_error %q; want pending with the 32-hop cap named", inst.Status, inst.LastError)
	}
	if len(rec.writes) != MaxAutoHops {
		t.Fatalf("auto node ran %d times; want exactly the cap (%d)", len(rec.writes), MaxAutoHops)
	}
}

// TestSubmit_StartsOnGatewayAndWalks: a manual submit on a process that begins with a gateway
// returns the instance AFTER the walk — a small quote comes back already approved.
func TestSubmit_StartsOnGatewayAndWalks(t *testing.T) {
	data := map[string]any{"amount": 3e6}
	store := newFakeStore()
	rec := &fakeRecords{data: data}
	runner := NewRunner(store, rec, rec, nil, nil).WithRecordWriter(rec)
	loader := fakeLoader{procs: []Process{{ID: "p1", TenantID: "t1", EntityName: "crm_quotes", Trigger: TriggerManual, Graph: quoteGraph(), IsActive: true}}}
	inst, err := NewRequester(loader, store, store, store, nil).WithRunner(runner).
		Submit(context.Background(), "t1", "crm_quotes", "q1", data, Actor{UserID: "u-sub"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if inst.Status != StatusApproved {
		t.Fatalf("submitted 3 triệu: %s; want approved by the rules", inst.Status)
	}
	if h := inst.History[0]; h.Action != HistorySubmitted || h.Node != "route" {
		t.Fatalf("first history %+v; want submitted on the start node", h)
	}
}
