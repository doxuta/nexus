package bpm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/workflow"
)

// specs/043 S12a — the wait node: "chờ phản hồi đầu tiên". Nobody holds it; the walk rests on it until
// the record answers, with a deadline picked by the record's priority and a breach told to the people
// sla.notify names.

func notOpen() *workflow.Condition {
	return cond("", workflow.Rule{Field: "status", Op: "neq", Value: "Open"})
}

func priorityIs(v string) *workflow.Condition {
	return cond("", workflow.Rule{Field: "priority", Op: "eq", Value: v})
}

// slaMatrix: Khẩn 1 working hour, Cao 30 wall minutes, anything else 4 wall hours.
func slaMatrix() *WaitSLA {
	return &WaitSLA{
		Amount: 4, Unit: workflow.TimerUnitHours,
		Matrix: []SLARow{
			{When: priorityIs("Khẩn"), Amount: 1, Unit: workflow.TimerUnitBusinessHours},
			{When: priorityIs("Cao"), Amount: 30, Unit: workflow.TimerUnitMinutes},
		},
		WriteDueTo: "first_response_due",
		Notify:     &Assignment{Type: AssignField, Field: "assigned_to"},
	}
}

func waitGraph(sla *WaitSLA) *Graph {
	return &Graph{Nodes: []Node{
		{Step: Step{ID: "phan-hoi", Name: "Phản hồi đầu tiên"}, Kind: NodeWait, Until: notOpen(), SLA: sla},
		{Step: Step{ID: "xong", Name: "Xong"}, Kind: NodeEnd, Result: ResultApproved},
	}}
}

// officeCalc answers DueAt like the real calculator would for the office the T181 case names: the
// working day is 08:00–17:00, so one working hour from 16:30 on a Friday is 08:30 on Monday.
type officeCalc struct {
	asked []time.Duration
	err   error
}

func (f *officeCalc) DueAt(_ context.Context, _, _ string, from time.Time, d time.Duration) (time.Time, SLABasis, error) {
	f.asked = append(f.asked, d)
	if f.err != nil {
		return time.Time{}, "", f.err
	}
	if d == time.Hour && from.Weekday() == time.Friday && from.Hour() == 16 && from.Minute() == 30 {
		return time.Date(from.Year(), from.Month(), from.Day()+3, 8, 30, 0, 0, from.Location()), SLABasisBusiness, nil
	}
	return from.Add(d), SLABasisBusiness, nil
}

type waitRig struct {
	*walkRig
	calc *officeCalc
	now  time.Time
}

// newWaitRig is a walk rig whose runner knows the working calendar and a fixed clock.
func newWaitRig(g *Graph, data map[string]any, now time.Time) *waitRig {
	w := newWalkRig(g, nil, data)
	calc := &officeCalc{}
	w.runner.deadlines = calc
	w.runner.now = func() time.Time { return now }
	return &waitRig{walkRig: w, calc: calc, now: now}
}

// Friday 2026-10-09 16:30 UTC. (2026-10-09 is a Friday.)
var friday1630 = time.Date(2026, 10, 9, 16, 30, 0, 0, time.UTC)

// TestWaitNode_PriorityMatrix_BusinessHours (T181): the record's priority picks the matrix row; an
// urgent ticket opened at 16:30 on a Friday is due 08:30 on Monday, a wall-clock row is plain
// arithmetic, and no row ⇒ the base.
func TestWaitNode_PriorityMatrix_BusinessHours(t *testing.T) {
	tests := []struct {
		name     string
		priority string
		wantDue  time.Time
		wantCalc bool
		basis    SLABasis
	}{
		{"Khẩn: one WORKING hour from Friday 16:30 is Monday 08:30", "Khẩn", time.Date(2026, 10, 12, 8, 30, 0, 0, time.UTC), true, SLABasisBusiness},
		{"Cao: thirty wall minutes", "Cao", friday1630.Add(30 * time.Minute), false, SLABasisWall},
		{"no row holds: the base four hours", "Thấp", friday1630.Add(4 * time.Hour), false, SLABasisWall},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := map[string]any{"status": "Open", "priority": tc.priority}
			w := newWaitRig(waitGraph(slaMatrix()), data, friday1630)
			inst := w.arm(data)
			if inst == nil || inst.Status != StatusPending || nodeOf(inst) != "phan-hoi" {
				t.Fatalf("instance %+v; want it resting on the wait node", inst)
			}
			if inst.SLADueAt == nil || !inst.SLADueAt.Equal(tc.wantDue) {
				t.Fatalf("sla_due_at = %v; want %v", inst.SLADueAt, tc.wantDue)
			}
			if got := len(w.calc.asked) > 0; got != tc.wantCalc {
				t.Fatalf("calendar asked = %v; want %v (only the business_hours row walks the calendar)", got, tc.wantCalc)
			}
			last := inst.History[len(inst.History)-1]
			if last.Action != HistoryWaiting || last.SLABasis != tc.basis || last.Node != "phan-hoi" {
				t.Fatalf("last history %+v; want waiting on phan-hoi under basis %s", last, tc.basis)
			}
			if len(w.store.seats["inst-q1"]) != 0 || inst.CurrentApproverID != "" {
				t.Fatalf("a wait node opened seats %+v / approver %q; nobody holds it", w.store.seats["inst-q1"], inst.CurrentApproverID)
			}
		})
	}
}

// TestWaitNode_RestsOnceAndWakesOnTheAnswer (T181): the walk rests (a second Continue writes nothing),
// and once the record answers the same Continue goes on, closing the wait with a public line.
func TestWaitNode_RestsOnceAndWakesOnTheAnswer(t *testing.T) {
	data := map[string]any{"status": "Open", "priority": "Cao"}
	w := newWaitRig(waitGraph(slaMatrix()), data, friday1630)
	inst := w.arm(data)
	seq := inst.Seq
	for i := 0; i < 3; i++ { // three record updates that do not answer yet
		if err := w.runner.Continue(context.Background(), "t1", inst.ID); err != nil {
			t.Fatalf("Continue: %v", err)
		}
	}
	if got := w.store.instances[inst.ID]; got.Seq != seq || got.Escalated {
		t.Fatalf("seq %d→%d: a resting wait must not be rewritten (that would re-stamp its deadline)", seq, got.Seq)
	}

	w.rec.data["status"] = "Đang xử lý" // an agent answered
	if err := w.runner.Continue(context.Background(), "t1", inst.ID); err != nil {
		t.Fatalf("Continue after the answer: %v", err)
	}
	got := w.store.instances[inst.ID]
	if got.Status != StatusApproved || got.SLADueAt != nil {
		t.Fatalf("after the answer: %s due %v; want approved with no deadline", got.Status, got.SLADueAt)
	}
	met := got.History[len(got.History)-2] // …, wait_met, ended
	if met.Action != HistoryWaitMet || met.Node != "phan-hoi" || InternalHistoryAction(met.Action) {
		t.Fatalf("history %+v; want a PUBLIC wait_met line on phan-hoi", met)
	}
	if !InternalHistoryAction(HistoryWaiting) {
		t.Fatal("the resting mark is bookkeeping, not for the customer face")
	}
}

// TestWaitNode_UntilMetAdvancesAndRecordsDuration (T181): a record that has already answered when the
// walk lands never rests — no deadline, no mark, straight on.
func TestWaitNode_UntilMetAdvancesAndRecordsDuration(t *testing.T) {
	data := map[string]any{"status": "Đã trả lời", "priority": "Khẩn"}
	w := newWaitRig(waitGraph(slaMatrix()), data, friday1630)
	inst := w.arm(data)
	if inst.Status != StatusApproved {
		t.Fatalf("status %s; want approved straight away", inst.Status)
	}
	for _, h := range inst.History {
		if h.Action == HistoryWaiting {
			t.Fatalf("history has a waiting mark %+v for a wait that never waited", h)
		}
	}
	if len(w.rec.writes) != 0 {
		t.Fatalf("wrote %v onto the record; nothing to promise when it already answered", w.rec.writes)
	}
}

// TestWaitNode_WriteDueToDatetimeOnly (T182, engine half): the deadline lands in the named field as
// RFC3339 UTC — after the rest is committed.
func TestWaitNode_WriteDueToField(t *testing.T) {
	data := map[string]any{"status": "Open", "priority": "Cao"}
	w := newWaitRig(waitGraph(slaMatrix()), data, friday1630)
	w.arm(data)
	if len(w.rec.writes) != 1 {
		t.Fatalf("record writes %v; want exactly one", w.rec.writes)
	}
	want := friday1630.Add(30 * time.Minute).Format(time.RFC3339)
	if got := w.rec.writes[0]["first_response_due"]; got != want {
		t.Fatalf("first_response_due = %v; want %s", got, want)
	}
	// A refused write is recorded, not lost, and does not un-rest the instance.
	w2 := newWaitRig(waitGraph(slaMatrix()), data, friday1630)
	w2.rec.writeErr = context.DeadlineExceeded
	inst := w2.arm(data)
	if inst.Status != StatusPending || inst.SLADueAt == nil {
		t.Fatalf("a refused due-field write un-rested the instance: %s due %v", inst.Status, inst.SLADueAt)
	}
	var failed bool
	for _, h := range inst.History {
		failed = failed || h.Action == HistoryEffectFailed
	}
	if !failed {
		t.Fatal("the refused write left no effect_failed line")
	}
	if !resting(inst) {
		t.Fatal("an effect_failed line must not hide the resting mark (the next signal would re-stamp the deadline)")
	}
}

// TestWaitNode_CalendarUnreadable_FallsBackToWall: the working calendar cannot be read ⇒ the deadline
// is a wall-clock number under its own basis, never a lost instance.
func TestWaitNode_CalendarUnreadable_FallsBackToWall(t *testing.T) {
	data := map[string]any{"status": "Open", "priority": "Khẩn"}
	w := newWaitRig(waitGraph(slaMatrix()), data, friday1630)
	w.calc.err = context.DeadlineExceeded
	inst := w.arm(data)
	if inst.Status != StatusPending || inst.SLADueAt == nil || !inst.SLADueAt.Equal(friday1630.Add(time.Hour)) {
		t.Fatalf("instance %s due %v; want pending, due one wall hour on", inst.Status, inst.SLADueAt)
	}
	if b := inst.History[len(inst.History)-1].SLABasis; b != SLABasisWallCalendarError {
		t.Fatalf("basis %q; want wall_calendar_error", b)
	}
}

// TestResting_IsPerVisit: an old waiting mark of an earlier visit to the same node is not "resting".
func TestResting_IsPerVisit(t *testing.T) {
	inst := &Instance{CurrentStep: 0, History: []HistoryEntry{
		{Step: 0, Action: HistoryWaiting}, {Step: 0, Action: HistoryRejected}, {Step: 0, Action: HistoryResubmitted},
	}}
	if resting(inst) {
		t.Fatal("a mark from before a reject/resubmit loop counted as the present visit")
	}
	inst.History = []HistoryEntry{{Step: 0, Action: HistoryWaiting}, {Step: 0, Action: HistoryEscalated}, {Step: 0, Action: HistoryBreachNoRecipient}, {Step: 0, Action: HistoryRepinned}}
	if !resting(inst) {
		t.Fatal("a breach line hid the resting mark of the same visit")
	}
}

// TestStuck_RestingWaitIsNotStuck: waiting for the record is a wait node's job; the admin "cần xử lý"
// flag is for a step that cannot move (last_error) or a machine node the walk has not passed.
func TestStuck_RestingWaitIsNotStuck(t *testing.T) {
	data := map[string]any{"status": "Open", "priority": "Cao"}
	w := newWaitRig(waitGraph(slaMatrix()), data, friday1630)
	inst := w.arm(data)
	if inst.Stuck() {
		t.Fatal("a wait node resting for its record was flagged stuck")
	}
	landed := *inst
	landed.History = landed.History[:len(landed.History)-1] // landed but never rested: the walk has not passed it
	if !landed.Stuck() {
		t.Fatal("a wait node the walk never rested on should read as not yet passed")
	}
	stalled := *inst
	stalled.LastError = "boom"
	if !stalled.Stuck() {
		t.Fatal("last_error did not mark the instance stuck")
	}
}

// TestSignal_OnlyOpenInstancesOfRecord (T185): an after_update of record q1 wakes the wait node of q1
// and nothing else — not another record's, not a finished instance of q1.
func TestSignal_OnlyOpenInstancesOfRecord(t *testing.T) {
	data := map[string]any{"status": "Open", "priority": "Cao"}
	w := newWaitRig(waitGraph(slaMatrix()), data, friday1630)
	q1 := w.arm(data)
	// another record waiting on the same kind of node, and a finished instance of q1
	other := *q1
	other.ID, other.RecordID = "inst-q2", "q2"
	w.store.instances[other.ID] = &other
	done := *q1
	done.ID, done.Status = "inst-old", StatusApproved
	w.store.instances[done.ID] = &done
	seq1, seq2, seqDone := q1.Seq, other.Seq, done.Seq

	w.rec.data["status"] = "Đang xử lý"
	w.eng.WithRunner(w.runner).arm(Event{TenantID: "t1", EntityName: "crm_quotes", RecordID: "q1",
		Trigger: TriggerAfterUpdate, Data: w.rec.data, Before: map[string]any{"status": "Open"}})

	if got := w.store.instances["inst-q1"]; got.Status != StatusApproved || got.Seq == seq1 {
		t.Fatalf("q1 instance %s seq %d→%d; want it woken and approved", got.Status, seq1, got.Seq)
	}
	if got := w.store.instances["inst-q2"]; got.Seq != seq2 || got.Status != StatusPending {
		t.Fatalf("another record's instance moved: %s seq %d→%d", got.Status, seq2, got.Seq)
	}
	if got := w.store.instances["inst-old"]; got.Seq != seqDone {
		t.Fatalf("a finished instance was touched: seq %d→%d", seqDone, got.Seq)
	}
	// an after_create event never signals
	w.store.instances["inst-q2"].RecordID = "q9"
	w.eng.arm(Event{TenantID: "t1", EntityName: "crm_quotes", RecordID: "q9", Trigger: TriggerAfterCreate, Data: w.rec.data})
	if got := w.store.instances["inst-q2"]; got.Seq != seq2 {
		t.Fatalf("a create event woke a wait node: seq %d→%d", seq2, got.Seq)
	}
}

// ---- breach ----------------------------------------------------------------------------------

// restingBreached plants a wait instance whose deadline passed, standing on node phan-hoi.
func restingBreached(t *testing.T, notify *Assignment, opts *Options) (*fakeStore, *Instance) {
	t.Helper()
	store := newFakeStore()
	due := time.Now().UTC().Add(-time.Minute)
	g := waitGraph(&WaitSLA{Amount: 1, Unit: workflow.TimerUnitMinutes, Notify: notify})
	def := &Definition{Steps: []Step{}, Graph: g.normalized(), Options: opts}
	id, err := store.CreateInstance(context.Background(), CreateInstanceInput{
		TenantID: "t1", ProcessID: "p1", EntityName: "tk_tickets", RecordID: "r1", SLADueAt: &due,
		Definition: def, CreatedBy: "u-sub",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, store.instances[id]
}

func breachOnce(store *fakeStore, rt Router) {
	s := NewSLAScheduler(SLASchedulerConfig{}, store, store, nil, nil).WithRouter(rt)
	s.runBreaches(context.Background(), time.Now().UTC())
	s.runBreaches(context.Background(), time.Now().UTC()) // a second sweep must add nothing
}

// TestWaitNode_BreachNotifiesAssignedToOnce (T267): the user in the notify field is told, once, with
// the "Quá hạn" notice — and nobody is told who merely holds no seat (there is none).
func TestWaitNode_BreachNotifiesAssignedToOnce(t *testing.T) {
	store, inst := restingBreached(t, &Assignment{Type: AssignField, Field: "assigned_to"}, nil)
	breachOnce(store, &fakeRouter{holders: map[string]Approver{"field:assigned_to": uA("tk_agent")}})
	if len(store.notes) != 1 || store.notes[0].UserID != "tk_agent" || store.notes[0].Title != "Quá hạn" || store.notes[0].Type != "due" {
		t.Fatalf("notices %+v; want one \"Quá hạn\" to tk_agent", store.notes)
	}
	if store.notes[0].RecordID != "r1" || store.notes[0].EntityName != "tk_tickets" {
		t.Fatalf("notice does not link back to the record: %+v", store.notes[0])
	}
	if !store.instances[inst.ID].Escalated {
		t.Fatal("the breach was not claimed")
	}
}

// TestWaitNode_BreachFallsBackToOwnerThenFallback (T267): nobody in the notify field ⇒ the record's
// owner; no owner either ⇒ the process's fallback assignee.
func TestWaitNode_BreachFallsBackToOwnerThenFallback(t *testing.T) {
	notify := &Assignment{Type: AssignField, Field: "assigned_to"}
	t.Run("owner", func(t *testing.T) {
		store, _ := restingBreached(t, notify, &Options{FallbackAssignee: &Approver{Type: ApproverUser, ID: "u-fb"}})
		breachOnce(store, &fakeRouter{holders: map[string]Approver{"field:" + OwnerField: uA("u-owner")}})
		if len(store.notes) != 1 || store.notes[0].UserID != "u-owner" {
			t.Fatalf("notices %+v; want one to the owner", store.notes)
		}
	})
	t.Run("fallback assignee", func(t *testing.T) {
		store, _ := restingBreached(t, notify, &Options{FallbackAssignee: &Approver{Type: ApproverUser, ID: "u-fb"}})
		breachOnce(store, &fakeRouter{})
		if len(store.notes) != 1 || store.notes[0].UserID != "u-fb" {
			t.Fatalf("notices %+v; want one to the fallback assignee", store.notes)
		}
	})
	t.Run("a named role is told as a whole", func(t *testing.T) {
		store, _ := restingBreached(t, &Assignment{Type: AssignRole, ID: "r-lead"}, nil)
		store.roleUsers = map[string][]string{"r-lead": {"lead1", "lead2"}}
		breachOnce(store, nil)
		if len(store.notes) != 2 {
			t.Fatalf("notices %+v; want one per member of the role", store.notes)
		}
	})
}

// TestWaitNode_NoRecipient_RecordsHistory (T267): nobody to tell ⇒ no notice, and the timeline says so.
func TestWaitNode_NoRecipient_RecordsHistory(t *testing.T) {
	store, inst := restingBreached(t, &Assignment{Type: AssignField, Field: "assigned_to"}, nil)
	breachOnce(store, &fakeRouter{})
	if len(store.notes) != 0 {
		t.Fatalf("notices %+v; want none", store.notes)
	}
	var found bool
	for _, h := range store.instances[inst.ID].History {
		found = found || h.Action == HistoryBreachNoRecipient
	}
	if !found {
		t.Fatalf("history %+v has no breach_no_recipient line", store.instances[inst.ID].History)
	}
	if !InternalHistoryAction(HistoryBreachNoRecipient) {
		t.Fatal("breach_no_recipient is for the Nexus team, not the customer face")
	}
}

// ---- config ----------------------------------------------------------------------------------

func TestCheckWait(t *testing.T) {
	ok := func() Node {
		return Node{Step: Step{ID: "w"}, Kind: NodeWait, Until: notOpen(), SLA: slaMatrix()}
	}
	tests := []struct {
		name      string
		mutate    func(*Node)
		wantField string
	}{
		{"valid", func(*Node) {}, ""},
		{"no sla at all is a plain wait", func(n *Node) { n.SLA = nil }, ""},
		{"until missing", func(n *Node) { n.Until = nil }, "until"},
		{"until without a rule", func(n *Node) { n.Until = cond("") }, "until"},
		{"until reads the before image", func(n *Node) {
			n.Until = cond("", workflow.Rule{Field: "status", Op: workflow.OpChanged})
		}, "until"},
		{"sla without notify", func(n *Node) { n.SLA.Notify = nil }, "sla.notify"},
		{"notify names a queue", func(n *Node) { n.SLA.Notify = &Assignment{Type: AssignQueue, ID: "q"} }, "sla.notify.type"},
		{"notify with a strategy", func(n *Node) {
			n.SLA.Notify = &Assignment{Type: AssignTeam, ID: "t", Strategy: StrategyRoundRobin}
		}, "sla.notify.strategy"},
		{"notify field blank", func(n *Node) { n.SLA.Notify = &Assignment{Type: AssignField} }, "sla.notify.field"},
		{"sla names no deadline", func(n *Node) { n.SLA = &WaitSLA{Notify: &Assignment{Type: AssignSubmitter}} }, "sla.amount"},
		{"bad unit", func(n *Node) { n.SLA.Unit = "weeks" }, "sla.unit"},
		{"row amount zero", func(n *Node) { n.SLA.Matrix[0].Amount = 0 }, "sla.matrix[0].amount"},
		{"row amount too large", func(n *Node) { n.SLA.Matrix[0].Amount = MaxWaitAmount + 1 }, "sla.matrix[0].amount"},
		{"row unit unknown", func(n *Node) { n.SLA.Matrix[0].Unit = "business_days" }, "sla.matrix[0].unit"},
		{"row without condition", func(n *Node) { n.SLA.Matrix[1].When = nil }, "sla.matrix[1].when"},
		{"too many rows", func(n *Node) {
			n.SLA.Matrix = nil
			for i := 0; i <= MaxWaitMatrixRows; i++ {
				n.SLA.Matrix = append(n.SLA.Matrix, SLARow{When: priorityIs("x"), Amount: 1, Unit: workflow.TimerUnitHours})
			}
		}, "sla.matrix"},
		{"writeDueTo is not a field name", func(n *Node) { n.SLA.WriteDueTo = "a b" }, "sla.writeDueTo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := ok()
			n.SLA = slaMatrixCopy()
			tc.mutate(&n)
			f, why := CheckWait(n)
			if f != tc.wantField {
				t.Fatalf("CheckWait field = %q (%s); want %q", f, why, tc.wantField)
			}
			if tc.wantField != "" && why == "" {
				t.Fatal("a refusal must say why")
			}
		})
	}
}

func slaMatrixCopy() *WaitSLA {
	s := slaMatrix()
	s.Matrix = append([]SLARow(nil), s.Matrix...)
	return s
}

// TestWaitNode_GraphFailsClosedOnBadConfig: a wait node this binary cannot run makes the whole graph
// un-runnable (arm skips, never walks it as something else), and an sla key it does not know does too.
func TestWaitNode_GraphFailsClosedOnBadConfig(t *testing.T) {
	good := waitGraph(slaMatrix())
	if len(good.approvalChain()) != 2 {
		t.Fatal("the valid wait graph must run")
	}
	// a wait node's keys on another kind
	bad := waitGraph(nil)
	bad.Nodes[1].Until = notOpen()
	if bad.approvalChain() != nil {
		t.Fatal("an end node carrying `until` ran")
	}
	// no notify: not runnable even if the API were bypassed
	noNotify := waitGraph(slaMatrix())
	noNotify.Nodes[0].SLA.Notify = nil
	if noNotify.approvalChain() != nil {
		t.Fatal("a wait deadline with nobody to tell ran")
	}
	// an unknown key inside sla (bogusKey)
	var g Graph
	raw := `{"nodes":[{"id":"w","kind":"wait","until":{"rules":[{"field":"status","op":"neq","value":"Open"}]},
		"sla":{"amount":1,"unit":"hours","notify":{"type":"submitter"},"bogusKey":"x"}},{"id":"e","kind":"end","result":"approved"}]}`
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		t.Fatal(err)
	}
	if g.approvalChain() != nil {
		t.Fatal("a graph with an sla key of a later slice ran")
	}
	if f, why := CheckWait(g.Nodes[0]); f != "sla.bogusKey" || !strings.Contains(why, "bogusKey") {
		t.Fatalf("CheckWait = %q (%s); want it to name sla.bogusKey", f, why)
	}
	// and the round trip keeps what the node says
	raw2, err := json.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	var back Graph
	if err := json.Unmarshal(raw2, &back); err != nil || back.approvalChain() == nil {
		t.Fatalf("a wait graph did not survive its own JSON: %v", err)
	}
}

// TestWaitSLA_PickAmount_FirstMatchWins: when two rows hold on the record the FIRST one decides, and
// any matching row beats the base amount.
func TestWaitSLA_PickAmount_FirstMatchWins(t *testing.T) {
	sla := &WaitSLA{
		Amount: 4, Unit: workflow.TimerUnitHours,
		Matrix: []SLARow{
			{When: priorityIs("Khẩn"), Amount: 1, Unit: workflow.TimerUnitHours},
			{When: notOpen(), Amount: 2, Unit: workflow.TimerUnitMinutes},
		},
	}
	tests := []struct {
		name     string
		data     map[string]any
		wantAmt  int
		wantUnit string
	}{
		{"both rows hold: row 1 wins", map[string]any{"priority": "Khẩn", "status": "Closed"}, 1, workflow.TimerUnitHours},
		{"only row 2 holds: it beats the base", map[string]any{"priority": "Thấp", "status": "Closed"}, 2, workflow.TimerUnitMinutes},
		{"none holds: the base", map[string]any{"priority": "Thấp", "status": "Open"}, 4, workflow.TimerUnitHours},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amt, unit, ok := sla.pickAmount(tc.data)
			if !ok || amt != tc.wantAmt || unit != tc.wantUnit {
				t.Fatalf("pickAmount = %d %s ok=%v; want %d %s", amt, unit, ok, tc.wantAmt, tc.wantUnit)
			}
		})
	}
}
