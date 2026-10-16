package bpm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// specs/043 S6a — routing: a step names its holder by rule (team pool, queue, record field,
// submitter) and a fallback; the engine resolves it once, when the instance enters the step.
// The record / directory lookups behind a Router are proven in services (BPMRouter) and on MySQL;
// here the Router is a fake so the ENGINE's handling of found / not found / error is pinned.

type fakeRouter struct {
	holders map[string]Approver // "field:<name>" | "submitter" → who
	// teams answers a team strategy (S6b): the team id → the members it hands out, in order.
	teams     map[string][]Approver
	err       error
	inputs    []RouteInput
	teamCalls int
}

func (f *fakeRouter) ResolveTeam(_ context.Context, in RouteInput, a Assignment) ([]Approver, error) {
	f.inputs = append(f.inputs, in)
	f.teamCalls++
	if f.err != nil {
		return nil, f.err
	}
	return f.teams[a.ID], nil
}

func (f *fakeRouter) Resolve(_ context.Context, in RouteInput, a Assignment) (Approver, bool, error) {
	f.inputs = append(f.inputs, in)
	if f.err != nil {
		return Approver{}, false, f.err
	}
	key := string(a.Type)
	if a.Type == AssignField {
		key += ":" + a.Field
	}
	h, ok := f.holders[key]
	return h, ok, nil
}

func fieldNode(id, field string) Node {
	return Node{Step: Step{ID: id, Name: "Duyệt " + id, Assignment: &Assignment{Type: AssignField, Field: field}, SLASeconds: 3600}, Kind: NodeApproval}
}

func routedProcess(opts *Options, nodes ...Node) Process {
	return Process{
		ID: "p1", TenantID: "t1", EntityName: "crm_quotes", Trigger: TriggerAfterCreate,
		Graph: &Graph{Nodes: nodes}, Options: opts, IsActive: true,
	}
}

func armRouted(t *testing.T, store *fakeStore, rt Router, p Process, actor string) {
	t.Helper()
	e := NewAsyncEngine(EngineConfig{Buffer: 4, Workers: 1}, fakeLoader{procs: []Process{p}}, store, store, nil).WithRouter(rt)
	e.arm(Event{TenantID: "t1", EntityName: "crm_quotes", RecordID: "q1", Trigger: TriggerAfterCreate, ActorUserID: actor, Data: map[string]any{}})
}

func TestCheckAssignment(t *testing.T) {
	step := func(a *Assignment) Step { return Step{ID: "s1", Assignment: a} }
	cases := []struct {
		name      string
		step      Step
		wantField string
	}{
		{"no assignment is not judged here", Step{ID: "s1"}, ""},
		{"user", step(&Assignment{Type: AssignUser, ID: "u1"}), ""},
		{"team pool", step(&Assignment{Type: AssignTeam, ID: "tm1", Strategy: StrategyPool}), ""},
		{"queue", step(&Assignment{Type: AssignQueue, ID: "q1"}), ""},
		{"field", step(&Assignment{Type: AssignField, Field: "assigned_to"}), ""},
		{"owner pseudo-field", step(&Assignment{Type: AssignField, Field: OwnerField}), ""},
		{"submitter", step(&Assignment{Type: AssignSubmitter}), ""},
		{"unknown type", step(&Assignment{Type: "boss", ID: "x"}), "assignment.type"},
		{"user without id", step(&Assignment{Type: AssignUser}), "assignment.id"},
		{"id with a space around it", step(&Assignment{Type: AssignRole, ID: " r1"}), "assignment.id"},
		{"field without a field", step(&Assignment{Type: AssignField}), "assignment.field"},
		{"field name that is not an identifier", step(&Assignment{Type: AssignField, Field: "a b;drop"}), "assignment.field"},
		{"field with an id", step(&Assignment{Type: AssignField, Field: "x", ID: "u1"}), "assignment.id"},
		{"user with a field", step(&Assignment{Type: AssignUser, ID: "u1", Field: "x"}), "assignment.field"},
		{"submitter with an id", step(&Assignment{Type: AssignSubmitter, ID: "u1"}), "assignment.id"},
		{"team round_robin (opened in S6b)", step(&Assignment{Type: AssignTeam, ID: "tm1", Strategy: StrategyRoundRobin}), ""},
		{"team load_based (opened in S6b)", step(&Assignment{Type: AssignTeam, ID: "tm1", Strategy: StrategyLoadBased}), ""},
		{"team each_member (opened in S6b)", step(&Assignment{Type: AssignTeam, ID: "tm1", Strategy: StrategyEachMember}), ""},
		{"each_member with a quorum", Step{ID: "s1", Assignment: &Assignment{Type: AssignTeam, ID: "tm1", Strategy: StrategyEachMember}, Completion: &Completion{Mode: CompletionQuorum, Percent: 60}}, ""},
		{"each_member with a bad quorum", Step{ID: "s1", Assignment: &Assignment{Type: AssignTeam, ID: "tm1", Strategy: StrategyEachMember}, Completion: &Completion{Mode: CompletionQuorum, Percent: 0}}, "completion.percent"},
		{"round_robin on a role", step(&Assignment{Type: AssignRole, ID: "r1", Strategy: StrategyRoundRobin}), "assignment.strategy"},
		{"each_member on a queue", step(&Assignment{Type: AssignQueue, ID: "q1", Strategy: StrategyEachMember}), "assignment.strategy"},
		{"load_based on a field", step(&Assignment{Type: AssignField, Field: "x", Strategy: StrategyLoadBased}), "assignment.strategy"},
		{"completion beside a pool team", Step{ID: "s1", Assignment: &Assignment{Type: AssignTeam, ID: "tm1"}, Completion: &Completion{Mode: CompletionAll}}, "assignment"},
		{"completion beside round_robin (one person has no policy)", Step{ID: "s1", Assignment: &Assignment{Type: AssignTeam, ID: "tm1", Strategy: StrategyRoundRobin}, Completion: &Completion{Mode: CompletionAll}}, "assignment"},
		{"made-up strategy", step(&Assignment{Type: AssignTeam, ID: "tm1", Strategy: "lottery"}), "assignment.strategy"},
		{"beside approverType", Step{ID: "s1", ApproverType: ApproverUser, ApproverID: "u1", Assignment: &Assignment{Type: AssignSubmitter}}, "assignment"},
		{"beside approvers", Step{ID: "s1", Approvers: []Approver{{Type: ApproverUser, ID: "a"}, {Type: ApproverUser, ID: "b"}}, Assignment: &Assignment{Type: AssignSubmitter}}, "assignment"},
		{"beside completion", Step{ID: "s1", Completion: &Completion{Mode: CompletionAll}, Assignment: &Assignment{Type: AssignSubmitter}}, "assignment"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, reason := CheckAssignment(c.step)
			if f != c.wantField {
				t.Fatalf("CheckAssignment blames %q (%s); want %q", f, reason, c.wantField)
			}
			if (f == "") != (reason == "") {
				t.Fatalf("field %q and reason %q must come together", f, reason)
			}
			// the API and the engine share the body: CheckApprovers must say the same thing.
			if f2, _ := CheckApprovers(c.step); c.step.Assignment != nil && f2 != f {
				t.Fatalf("CheckApprovers blames %q; want %q (one body for API and engine)", f2, f)
			}
		})
	}
}

func TestCheckOptions(t *testing.T) {
	cases := []struct {
		name string
		o    *Options
		want string
	}{
		{"none", nil, ""},
		{"empty", &Options{}, ""},
		{"user fallback", &Options{FallbackAssignee: &Approver{Type: ApproverUser, ID: "u1"}}, ""},
		{"team fallback", &Options{FallbackAssignee: &Approver{Type: ApproverTeam, ID: "t1"}}, ""},
		{"bad type", &Options{FallbackAssignee: &Approver{Type: "boss", ID: "u1"}}, "fallbackAssignee.type"},
		{"no id", &Options{FallbackAssignee: &Approver{Type: ApproverUser}}, "fallbackAssignee.id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if f, _ := CheckOptions(c.o); f != c.want {
				t.Fatalf("CheckOptions blames %q; want %q", f, c.want)
			}
		})
	}
}

// A graph carrying an assignment this binary cannot run is refused whole (fail-closed), one it can
// run is walked — and the assignment replaces approverType/approverId, never sits beside them.
func TestApprovalChain_AssignmentNodes(t *testing.T) {
	ok := &Graph{Nodes: []Node{
		{Step: Step{ID: "a", Assignment: &Assignment{Type: AssignTeam, ID: "tm1"}}, Kind: NodeApproval},
		{Step: Step{ID: "b", Assignment: &Assignment{Type: AssignSubmitter}}, Kind: NodeTask},
	}}
	if got := ok.approvalChain(); len(got) != 2 || got[0].Assignment == nil || got[1].NodeKind != NodeTask {
		t.Fatalf("chain = %+v; want both assignment nodes walked", got)
	}
	both := &Graph{Nodes: []Node{{Step: Step{ID: "a", ApproverType: ApproverUser, ApproverID: "u", Assignment: &Assignment{Type: AssignSubmitter}}, Kind: NodeApproval}}}
	if got := both.approvalChain(); got != nil {
		t.Fatalf("a node with approverType AND assignment ran: %+v", got)
	}
	rr := &Graph{Nodes: []Node{{Step: Step{ID: "a", Assignment: &Assignment{Type: AssignTeam, ID: "tm1", Strategy: StrategyRoundRobin}}, Kind: NodeApproval}}}
	if got := rr.approvalChain(); len(got) != 1 { // S6b opened it
		t.Fatalf("a round_robin node did not run on an S6b binary: %+v", got)
	}
	lottery := &Graph{Nodes: []Node{{Step: Step{ID: "a", Assignment: &Assignment{Type: AssignTeam, ID: "tm1", Strategy: "lottery"}}, Kind: NodeApproval}}}
	if got := lottery.approvalChain(); got != nil {
		t.Fatalf("a node with a strategy this binary does not know ran: %+v", got)
	}
}

func TestStepSeats_StaticAssignment(t *testing.T) {
	due := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		a    Assignment
		want ApproverType
	}{
		{Assignment{Type: AssignTeam, ID: "tm1"}, ApproverTeam},
		{Assignment{Type: AssignQueue, ID: "q1"}, ApproverQueue},
		{Assignment{Type: AssignRole, ID: "r1"}, ApproverRole},
		{Assignment{Type: AssignUser, ID: "u1"}, ApproverUser},
	} {
		seats := stepSeats(Step{ID: "n", Assignment: &c.a}, &due, SLABasisBusiness)
		if len(seats) != 1 || seats[0].AssigneeType != c.want || seats[0].AssigneeID != c.a.ID || seats[0].NodeID != "n" {
			t.Fatalf("%s: seats = %+v; want one %s seat on n", c.a.Type, seats, c.want)
		}
	}
	// a rule that needs a lookup opens nothing by itself
	if seats := stepSeats(Step{ID: "n", Assignment: &Assignment{Type: AssignField, Field: "x"}}, nil, ""); len(seats) != 0 {
		t.Fatalf("a field assignment opened seats %+v without a lookup", seats)
	}
	// a team seat never reaches the ENUM projection (D-5)
	if at, id := projection(stepSeats(Step{ID: "n", Assignment: &Assignment{Type: AssignTeam, ID: "tm1"}}, nil, "")); at != "" || id != "" {
		t.Fatalf("projection of a team seat = %s/%s; want NULL (the ENUM is user|role)", at, id)
	}
}

// T109 — the person in the field holds the step: the seat is that user's, on this node, and the
// router was asked about THIS record and its submitter.
func TestRouting_FieldOwner(t *testing.T) {
	store := newFakeStore()
	rt := &fakeRouter{holders: map[string]Approver{"field:owner_id": {Type: ApproverUser, ID: "u-owner"}}}
	armRouted(t, store, rt, routedProcess(nil, fieldNode("duyet", OwnerField)), "u-sender")

	if len(store.created) != 1 {
		t.Fatalf("armed %d instances; want 1", len(store.created))
	}
	in := store.created[0]
	if len(in.Seats) != 1 || in.Seats[0].AssigneeType != ApproverUser || in.Seats[0].AssigneeID != "u-owner" || in.Seats[0].NodeID != "duyet" {
		t.Fatalf("seats = %+v; want one user seat of u-owner on node duyet", in.Seats)
	}
	if in.CurrentApproverType != ApproverUser || in.CurrentApproverID != "u-owner" || in.LastError != "" {
		t.Fatalf("projection %s/%s lastError %q; want user/u-owner, not stuck", in.CurrentApproverType, in.CurrentApproverID, in.LastError)
	}
	if len(rt.inputs) != 1 || rt.inputs[0].RecordID != "q1" || rt.inputs[0].EntityName != "crm_quotes" || rt.inputs[0].TenantID != "t1" || rt.inputs[0].Submitter != "u-sender" {
		t.Fatalf("router asked %+v; want tenant t1, crm_quotes/q1, submitter u-sender", rt.inputs)
	}
	if len(store.notes) != 1 || store.notes[0].UserID != "u-owner" {
		t.Fatalf("notes = %+v; want one notice to u-owner", store.notes)
	}
}

// T109 — nobody in the field: the process's fallback assignee holds the step.
func TestRouting_FieldEmptyUsesFallback(t *testing.T) {
	store := newFakeStore()
	rt := &fakeRouter{} // finds nobody
	opts := &Options{FallbackAssignee: &Approver{Type: ApproverRole, ID: "r-ops"}}
	armRouted(t, store, rt, routedProcess(opts, fieldNode("duyet", "assigned_to")), "u-sender")

	in := store.created[0]
	if len(in.Seats) != 1 || in.Seats[0].AssigneeType != ApproverRole || in.Seats[0].AssigneeID != "r-ops" {
		t.Fatalf("seats = %+v; want the fallback role seat", in.Seats)
	}
	if in.LastError != "" {
		t.Fatalf("lastError %q; a fallback is a holder, not a stuck step", in.LastError)
	}
	if in.Definition.Options == nil || in.Definition.Options.FallbackAssignee == nil {
		t.Fatalf("pin %+v; the fallback must be pinned with the steps", in.Definition)
	}
}

// T110 — no holder and no fallback: the approval is NOT lost. It is armed pending, with no seat and
// a reason, and nobody is pinged about a step nobody holds.
func TestRouting_NoAssigneeNoFallbackIsStuckNotLost(t *testing.T) {
	store := newFakeStore()
	armRouted(t, store, &fakeRouter{}, routedProcess(nil, fieldNode("duyet", "assigned_to")), "")

	if len(store.created) != 1 {
		t.Fatalf("armed %d instances; want 1 — a missing holder must not drop the approval", len(store.created))
	}
	in := store.created[0]
	if len(in.Seats) != 0 || in.CurrentApproverID != "" {
		t.Fatalf("seats %+v approver %q; want nobody", in.Seats, in.CurrentApproverID)
	}
	if !strings.Contains(in.LastError, "duyet") {
		t.Fatalf("lastError %q; want a reason naming the step", in.LastError)
	}
	if len(store.notes) != 0 {
		t.Fatalf("notes %+v; nobody holds the step, nobody is told", store.notes)
	}
	// and an admin can still act on it (authority is not lost with the seat)
	d := NewDecider(store, store, store, nil, nil)
	inst, err := d.Decide(context.Background(), "t1", "inst-q1", DecisionApprove, "xử lý tay", Actor{UserID: "u-admin", IsAdmin: true})
	if err != nil || inst.Status != StatusApproved {
		t.Fatalf("admin decision on a stuck step: %v %+v; want approved", err, inst)
	}
}

// With no Router wired at all a dynamic step behaves as "nobody found" — never as a nil panic.
func TestRouting_NoRouterWiredFallsBackThenStuck(t *testing.T) {
	store := newFakeStore()
	armRouted(t, store, nil, routedProcess(&Options{FallbackAssignee: &Approver{Type: ApproverUser, ID: "u-fb"}}, fieldNode("duyet", "x")), "u")
	if in := store.created[0]; len(in.Seats) != 1 || in.Seats[0].AssigneeID != "u-fb" {
		t.Fatalf("seats %+v; want the fallback", in.Seats)
	}
	store2 := newFakeStore()
	armRouted(t, store2, nil, routedProcess(nil, fieldNode("duyet", "x")), "u")
	if in := store2.created[0]; len(in.Seats) != 0 || in.LastError == "" {
		t.Fatalf("seats %+v lastError %q; want stuck", in.Seats, in.LastError)
	}
}

// The async arming path has nobody to show an error to: a router that cannot read its store arms
// the approval stuck (visible), not dropped (invisible).
func TestRouting_RouterErrorOnArmIsStuckNotDropped(t *testing.T) {
	store := newFakeStore()
	armRouted(t, store, &fakeRouter{err: errors.New("db down")}, routedProcess(nil, fieldNode("duyet", "x")), "u")
	if len(store.created) != 1 || store.created[0].LastError == "" || len(store.created[0].Seats) != 0 {
		t.Fatalf("created %+v; want one stuck instance", store.created)
	}
}

// A person pressing Submit is told when the holder cannot be looked up, and nothing is created.
func TestRouting_SubmitRouterErrorIsReturned(t *testing.T) {
	store := newFakeStore()
	p := routedProcess(nil, fieldNode("duyet", "x"))
	p.Trigger = TriggerManual
	r := NewRequester(fakeLoader{procs: []Process{p}}, store, store, store, nil).WithRouter(&fakeRouter{err: errors.New("db down")})
	if _, err := r.Submit(context.Background(), "t1", "crm_quotes", "q1", map[string]any{}, Actor{UserID: "u-sender"}); err == nil {
		t.Fatal("Submit swallowed a routing store failure")
	}
	if len(store.created) != 0 {
		t.Fatalf("created %d instances after a failed lookup; want none", len(store.created))
	}
}

// Submit routes by the person pressing the button, and a stuck result is created, not refused.
func TestRouting_SubmitSubmitterAndStuck(t *testing.T) {
	store := newFakeStore()
	node := Node{Step: Step{ID: "duyet", Assignment: &Assignment{Type: AssignSubmitter}}, Kind: NodeApproval}
	p := routedProcess(nil, node)
	p.Trigger = TriggerManual
	rt := &fakeRouter{holders: map[string]Approver{"submitter": {Type: ApproverUser, ID: "u-sender"}}}
	r := NewRequester(fakeLoader{procs: []Process{p}}, store, store, store, nil).WithRouter(rt)
	inst, err := r.Submit(context.Background(), "t1", "crm_quotes", "q1", map[string]any{}, Actor{UserID: "u-sender"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if rt.inputs[0].Submitter != "u-sender" || len(store.created[0].Seats) != 1 || store.created[0].Seats[0].AssigneeID != "u-sender" {
		t.Fatalf("router input %+v seats %+v; want the sender", rt.inputs, store.created[0].Seats)
	}
	_ = inst

	store2 := newFakeStore()
	r2 := NewRequester(fakeLoader{procs: []Process{p}}, store2, store2, store2, nil).WithRouter(&fakeRouter{})
	inst2, err := r2.Submit(context.Background(), "t1", "crm_quotes", "q1", map[string]any{}, Actor{UserID: "u-sender"})
	if err != nil {
		t.Fatalf("Submit with nobody found: %v — the approval must exist, stuck", err)
	}
	if inst2.LastError == "" {
		t.Fatalf("instance %+v; want a lastError", inst2)
	}
	// the "submitted" history write that follows the create must not wipe the reason
	if got := store2.instances[inst2.ID].LastError; got == "" {
		t.Fatal("the submit history append cleared last_error")
	}
}

// Deciding step 1 opens step 2, whose holder a rule finds NOW (the record may have changed since
// the approval started): the field is looked up at entry of THAT step, with the pinned fallback.
func TestRouting_DecideOpensNextStepByRule(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	nodes := []Node{
		{Step: Step{ID: "one", ApproverType: ApproverUser, ApproverID: "u-1"}, Kind: NodeApproval},
		fieldNode("two", "assigned_to"),
		{Step: Step{ID: "three", ApproverType: ApproverUser, ApproverID: "u-3"}, Kind: NodeApproval},
	}
	rt := &fakeRouter{holders: map[string]Approver{"field:assigned_to": {Type: ApproverUser, ID: "u-field"}}}
	armRouted(t, store, rt, routedProcess(nil, nodes...), "u-sender")
	d := NewDecider(store, store, store, nil, nil).WithRouter(rt)

	inst, err := d.Decide(ctx, "t1", "inst-q1", DecisionApprove, "", Actor{UserID: "u-1"})
	if err != nil {
		t.Fatalf("approve one: %v", err)
	}
	if inst.CurrentStep != 1 || inst.CurrentApproverID != "u-field" || inst.LastError != "" {
		t.Fatalf("after one: step %d approver %q lastError %q; want step 1 held by u-field", inst.CurrentStep, inst.CurrentApproverID, inst.LastError)
	}
	if got := store.lastNext.Seats.Open; len(got) != 1 || got[0].AssigneeID != "u-field" || got[0].NodeID != "two" {
		t.Fatalf("seats opened %+v; want u-field on node two", got)
	}
	// the submitter handed to the router is whoever created the instance
	if last := rt.inputs[len(rt.inputs)-1]; last.Submitter != "u-sender" || last.RecordID != "q1" {
		t.Fatalf("router input %+v; want submitter u-sender on q1", last)
	}
}

// The decision stands even when the next step has no holder: the approver is not punished for the
// configuration; the next step is stuck and says so, and the next real decision un-sticks it.
func TestRouting_DecideIntoStuckStepKeepsTheDecision(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	nodes := []Node{
		{Step: Step{ID: "one", ApproverType: ApproverUser, ApproverID: "u-1"}, Kind: NodeApproval},
		fieldNode("two", "assigned_to"),
	}
	armRouted(t, store, &fakeRouter{}, routedProcess(nil, nodes...), "u-sender")
	d := NewDecider(store, store, store, nil, nil).WithRouter(&fakeRouter{})
	inst, err := d.Decide(ctx, "t1", "inst-q1", DecisionApprove, "", Actor{UserID: "u-1"})
	if err != nil {
		t.Fatalf("approve one: %v", err)
	}
	if inst.Status != StatusPending || inst.CurrentStep != 1 || inst.LastError == "" || len(store.lastNext.Seats.Open) != 0 {
		t.Fatalf("after one: %s@%d lastError %q seats %+v; want pending@1, stuck, no seat", inst.Status, inst.CurrentStep, inst.LastError, store.lastNext.Seats.Open)
	}
	if store.instances["inst-q1"].LastError == "" {
		t.Fatal("the stuck reason was not written with the transition")
	}
	inst, err = d.Decide(ctx, "t1", "inst-q1", DecisionApprove, "", Actor{UserID: "u-admin", IsAdmin: true})
	if err != nil || inst.Status != StatusApproved || inst.LastError != "" {
		t.Fatalf("admin on the stuck step: %v %+v; want approved and the reason cleared", err, inst)
	}
}

func TestRouting_DecideRouterErrorFailsTheDecision(t *testing.T) {
	store := newFakeStore()
	nodes := []Node{
		{Step: Step{ID: "one", ApproverType: ApproverUser, ApproverID: "u-1"}, Kind: NodeApproval},
		fieldNode("two", "assigned_to"),
	}
	armRouted(t, store, &fakeRouter{}, routedProcess(nil, nodes...), "u-sender")
	d := NewDecider(store, store, store, nil, nil).WithRouter(&fakeRouter{err: errors.New("db down")})
	if _, err := d.Decide(context.Background(), "t1", "inst-q1", DecisionApprove, "", Actor{UserID: "u-1"}); err == nil {
		t.Fatal("a decision into a step whose holder could not be looked up succeeded")
	}
	if store.instances["inst-q1"].CurrentStep != 0 {
		t.Fatal("the instance moved although the lookup failed")
	}
}

// T111 — the hole at engine.go:287: a role / team seat's holders now get the "Cần phê duyệt" bell,
// capped at 50, each once.
func TestNotify_RoleAndTeamHoldersReceiveNotice(t *testing.T) {
	store := newFakeStore()
	store.roleUsers = map[string][]string{"r-mgr": {"duyet1", "duyet2", "duyet3"}}
	store.teamUsers = map[string][]string{"tm-ban": {"duyet1", "duyet2"}}
	nodes := []Node{{Step: Step{ID: "n", ApproverType: ApproverRole, ApproverID: "r-mgr"}, Kind: NodeApproval}}
	armRouted(t, store, nil, routedProcess(nil, nodes...), "u-sender")
	if got := noteUsers(store); strings.Join(got, ",") != "duyet1,duyet2,duyet3" {
		t.Fatalf("role holders notified: %v; want duyet1..3", got)
	}
	for _, n := range store.notes {
		if n.Title != "Cần phê duyệt" || n.Type != "approval" || n.RecordID != "q1" {
			t.Fatalf("note %+v; want an approval notice about q1", n)
		}
	}

	store = newFakeStore()
	store.teamUsers = map[string][]string{"tm-ban": {"duyet1", "duyet2"}}
	nodes = []Node{{Step: Step{ID: "n", Assignment: &Assignment{Type: AssignTeam, ID: "tm-ban"}}, Kind: NodeApproval}}
	armRouted(t, store, nil, routedProcess(nil, nodes...), "u-sender")
	if got := noteUsers(store); strings.Join(got, ",") != "duyet1,duyet2" {
		t.Fatalf("team members notified: %v; want duyet1, duyet2", got)
	}

	// a task says "Việc mới"
	store = newFakeStore()
	store.teamUsers = map[string][]string{"tm-ban": {"duyet1"}}
	nodes = []Node{{Step: Step{ID: "n", Assignment: &Assignment{Type: AssignTeam, ID: "tm-ban"}}, Kind: NodeTask}}
	armRouted(t, store, nil, routedProcess(nil, nodes...), "u-sender")
	if len(store.notes) != 1 || store.notes[0].Title != "Việc mới" {
		t.Fatalf("notes %+v; want one 'Việc mới' for a task", store.notes)
	}
}

func TestNotify_CappedAtFifty(t *testing.T) {
	store := newFakeStore()
	var many []string
	for i := 0; i < 120; i++ {
		many = append(many, fmt.Sprintf("u%03d", i))
	}
	store.roleUsers = map[string][]string{"r-big": many}
	nodes := []Node{{Step: Step{ID: "n", ApproverType: ApproverRole, ApproverID: "r-big"}, Kind: NodeApproval}}
	armRouted(t, store, nil, routedProcess(nil, nodes...), "u-sender")
	if len(store.notes) != 50 {
		t.Fatalf("%d notices for a 120-person role; want the cap, 50", len(store.notes))
	}
}

// The next step's holders are told when a decision opens it — not only the first step's.
func TestNotify_DecideTellsNextStepRoleHolders(t *testing.T) {
	store := newFakeStore()
	store.roleUsers = map[string][]string{"r-dir": {"dir1", "dir2"}}
	nodes := []Node{
		{Step: Step{ID: "one", ApproverType: ApproverUser, ApproverID: "u-1"}, Kind: NodeApproval},
		{Step: Step{ID: "two", ApproverType: ApproverRole, ApproverID: "r-dir"}, Kind: NodeApproval},
	}
	armRouted(t, store, nil, routedProcess(nil, nodes...), "u-sender")
	store.notes = nil
	d := NewDecider(store, store, store, nil, nil)
	if _, err := d.Decide(context.Background(), "t1", "inst-q1", DecisionApprove, "", Actor{UserID: "u-1"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got := noteUsers(store); strings.Join(got, ",") != "dir1,dir2" {
		t.Fatalf("notified %v; want dir1, dir2 (holders of the role that now awaits)", got)
	}
}

func noteUsers(s *fakeStore) []string {
	var out []string
	for _, n := range s.notes {
		out = append(out, n.UserID)
	}
	return out
}

// A team seat is a pool its members hold: any member is a candidate and awaits it, a stranger
// does not — membership travels in Actor.TeamIDs (HolderIDs), not in the role list.
func TestTeamSeat_HeldByMembersOnly(t *testing.T) {
	seat := Seat{NodeID: "s1", AssigneeType: ApproverTeam, AssigneeID: "tm-ban"} // s1 = the node an unpinned instance at step 0 stands on
	inst := &Instance{ID: "i", Status: StatusPending}
	member := Actor{UserID: "duyet1", RoleIDs: []string{"r-mgr"}, TeamIDs: []string{"tm-ban"}}
	stranger := Actor{UserID: "test", RoleIDs: []string{"r-rep"}, TeamIDs: []string{"tm-other"}}
	roleOnly := Actor{UserID: "x", RoleIDs: []string{"tm-ban"}} // a ROLE id equal to the team id is still a role id: see below

	if !AwaitsActor(inst, []Seat{seat}, member.UserID, member.HolderIDs(), nil) {
		t.Fatal("a team member does not await the team pool")
	}
	if AwaitsActor(inst, []Seat{seat}, stranger.UserID, stranger.HolderIDs(), nil) {
		t.Fatal("a non-member awaits the team pool")
	}
	// claimed by duyet1 → duyet2 (member) is refused with ErrSeatClaimed, the stranger learns nothing
	claimed := seat
	claimed.ClaimedBy = "duyet1"
	if err := authorize(inst, []Seat{claimed}, Actor{UserID: "duyet2", TeamIDs: []string{"tm-ban"}}); !errors.Is(err, ErrSeatClaimed) {
		t.Fatalf("second member on a claimed team seat: %v; want ErrSeatClaimed", err)
	}
	if err := authorize(inst, []Seat{claimed}, stranger); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("stranger on a claimed team seat: %v; want ErrNotAuthorized", err)
	}
	if err := authorize(inst, []Seat{seat}, member); err != nil {
		t.Fatalf("member on the open team seat: %v", err)
	}
	if !seat.Claimable() {
		t.Fatal("a team seat must be claimable (it is a pool)")
	}
	_ = roleOnly
}

// planRepin: a target step that finds its holder by rule is not entered by a repin — refused, never
// guessed; a static one (team) rebuilds the seat like any approver.
func TestRepin_DynamicTargetRefusedStaticTeamRebuilt(t *testing.T) {
	v1 := &Definition{Steps: repinSteps("s1"), Version: 1, FirstDueBasis: SLABasisBusiness}
	now := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	inst := repinInstance("s1", v1)
	open := []Seat{{NodeID: "s1", AssigneeType: ApproverUser, AssigneeID: "u-s1"}}

	dyn := &Definition{Graph: &Graph{Nodes: []Node{{Step: Step{ID: "s1", Assignment: &Assignment{Type: AssignSubmitter}}, Kind: NodeApproval}}}, Version: 2}
	if _, reason := planRepin(inst, open, dyn, "admin", now); reason != RepinNeedsRouting {
		t.Fatalf("reason = %q; want %q", reason, RepinNeedsRouting)
	}
	static := &Definition{Graph: &Graph{Nodes: []Node{{Step: Step{ID: "s1", Assignment: &Assignment{Type: AssignTeam, ID: "tm-ban"}}, Kind: NodeApproval}}}, Version: 2}
	plan, reason := planRepin(inst, open, static, "admin", now)
	if reason != "" {
		t.Fatalf("reason = %q; want a plan", reason)
	}
	if s := plan.Next.Seats.Open; len(s) != 1 || s[0].AssigneeType != ApproverTeam || s[0].AssigneeID != "tm-ban" {
		t.Fatalf("seats = %+v; want the team pool", s)
	}
	if plan.Next.CurrentApproverType != "" {
		t.Fatalf("projection %q; a team never reaches the ENUM", plan.Next.CurrentApproverType)
	}
}

func TestPinDefinition_CarriesOptionsAndAssignment(t *testing.T) {
	opts := &Options{FallbackAssignee: &Approver{Type: ApproverUser, ID: "u-fb"}}
	p := routedProcess(opts, Node{Step: Step{ID: "n", Assignment: &Assignment{Type: AssignTeam, ID: "tm"}}, Kind: NodeApproval})
	def := pinDefinition(p)
	if def.Options == nil || def.Options.FallbackAssignee == nil || def.Options.FallbackAssignee.ID != "u-fb" {
		t.Fatalf("pin options = %+v; want the fallback", def.Options)
	}
	if n := def.Graph.Nodes[0]; n.Assignment == nil || n.Assignment.ID != "tm" {
		t.Fatalf("pinned node %+v; want its assignment kept", n)
	}
}

// A step nobody could be found for is seatless ON PURPOSE (last_error says why): Doctor's seat-drift
// count must not call it drift — it would hold the projection fallback in place forever.
func TestSeatDrift_StuckInstanceIsNotDrift(t *testing.T) {
	stuck := &Instance{ID: "i", Status: StatusPending, LastError: "không tìm được người nhận"}
	if d := seatDriftOf(stuck, nil); d != "" {
		t.Fatalf("drift of a stuck instance = %q; want none", d)
	}
	if d := seatDriftOf(&Instance{ID: "i", Status: StatusPending}, nil); d != SeatDriftMissing {
		t.Fatalf("drift of a seatless instance with no reason = %q; want missing", d)
	}
}
