package bpm

// specs/043 S5 (`bpm-work-items-claim`): pool seats that can be claimed, task steps that are
// completed, and the queue assignee. The real transaction and queries are proven on MySQL in
// internal/services/work_item_integration_test.go; these pin the rules the engine itself owns.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// TestSeat_HeldByPoolAndClaim (T092, T094): a role seat belongs to every holder of the role until
// one of them claims it; then to that one alone. A queue seat follows the queue's own visibility
// (its role, or everyone when it names none, and only while the queue lives). A user seat is its
// user's, claim or no claim.
func TestSeat_HeldByPoolAndClaim(t *testing.T) {
	roleSeat := Seat{AssigneeType: ApproverRole, AssigneeID: "r-acc"}
	claimed := roleSeat
	claimed.ClaimedBy = "duyet1"
	queueSeat := Seat{AssigneeType: ApproverQueue, AssigneeID: "q1", QueueLive: true, QueueRoleID: "r-acc"}
	openQueue := Seat{AssigneeType: ApproverQueue, AssigneeID: "q2", QueueLive: true}
	deadQueue := Seat{AssigneeType: ApproverQueue, AssigneeID: "q3", QueueLive: false, QueueRoleID: ""}
	claimedQueue := queueSeat
	claimedQueue.ClaimedBy = "duyet1"

	acc := []string{"r-acc"}
	sales := []string{"r-sales"}
	cases := []struct {
		name string
		seat Seat
		user string
		role []string
		want bool
	}{
		{"unclaimed role seat: a holder of the role", roleSeat, "duyet2", acc, true},
		{"unclaimed role seat: someone without the role", roleSeat, "test", sales, false},
		{"claimed role seat: the claimer", claimed, "duyet1", acc, true},
		{"claimed role seat: another holder of the role", claimed, "duyet2", acc, false},
		{"claimed role seat: the claimer who lost the role", claimed, "duyet1", sales, false},
		{"queue seat: holder of the queue's role", queueSeat, "duyet2", acc, true},
		{"queue seat: someone without the queue's role", queueSeat, "test", sales, false},
		{"role-less queue: everyone", openQueue, "test", sales, true},
		{"role-less queue: even a user holding no role", openQueue, "test", nil, true},
		{"queue that no longer lives: nobody", deadQueue, "test", sales, false},
		{"claimed queue seat: the claimer", claimedQueue, "duyet1", acc, true},
		{"claimed queue seat: another holder", claimedQueue, "duyet2", acc, false},
		{"user seat: its user", Seat{AssigneeType: ApproverUser, AssigneeID: "u1"}, "u1", nil, true},
		{"user seat: someone else", Seat{AssigneeType: ApproverUser, AssigneeID: "u1"}, "u2", nil, false},
		{"seat naming nobody", Seat{AssigneeType: ApproverRole}, "u1", acc, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.seat.heldBy(tc.user, tc.role); got != tc.want {
				t.Fatalf("heldBy = %v, want %v", got, tc.want)
			}
		})
	}
}

// poolFixture arms a one-step instance whose seat belongs to role r-acc, with the seat planted
// the way the repository reads it. claimedBy "" = unclaimed.
func poolFixture(t *testing.T, claimedBy string) (*fakeStore, *Decider, string) {
	t.Helper()
	store := newFakeStore()
	def := &Definition{Steps: NormalizeSteps([]Step{
		{Name: "Kế toán duyệt", ApproverType: ApproverRole, ApproverID: "r-acc"},
		{Name: "Giám đốc duyệt", ApproverType: ApproverUser, ApproverID: "boss"},
	})}
	id, err := store.CreateInstance(context.Background(), CreateInstanceInput{
		TenantID: "t1", ProcessID: "p1", EntityName: "quotes", RecordID: "r1",
		CurrentApproverType: ApproverRole, CurrentApproverID: "r-acc", Definition: def,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	store.seats[id] = []Seat{{NodeID: "s1", AssigneeType: ApproverRole, AssigneeID: "r-acc", ClaimedBy: claimedBy}}
	return store, NewDecider(store, store, nil, nil, nil), id
}

// TestApprovalDecide_PoolSeatClaimedByOther409 (T092): a candidate who did not claim the seat is
// told it is taken (ErrSeatClaimed → 409); a stranger is told nothing new (ErrNotAuthorized → 404
// shape); the instance does not move either way.
func TestApprovalDecide_PoolSeatClaimedByOther409(t *testing.T) {
	store, d, id := poolFixture(t, "duyet1")
	ctx := context.Background()

	_, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "duyet2", RoleIDs: []string{"r-acc"}})
	if !errors.Is(err, ErrSeatClaimed) {
		t.Fatalf("candidate, seat claimed by another: err = %v; want ErrSeatClaimed", err)
	}
	_, err = d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "test", RoleIDs: []string{"r-sales"}})
	if !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("stranger: err = %v; want ErrNotAuthorized (no hint the item exists)", err)
	}
	if store.advanceCnt != 0 || store.instances[id].CurrentStep != 0 {
		t.Fatalf("a refused decision moved the instance")
	}
}

// TestApprovalDecide_FindsActorsClaimedPoolSeat (T092): the claimer decides through the ordinary
// approve verb — the engine finds their claimed pool seat, no second route needed — and an admin
// may still decide a claimed seat.
func TestApprovalDecide_FindsActorsClaimedPoolSeat(t *testing.T) {
	ctx := context.Background()
	t.Run("claimer", func(t *testing.T) {
		store, d, id := poolFixture(t, "duyet1")
		got, err := d.Decide(ctx, "t1", id, DecisionApprove, "ok", Actor{UserID: "duyet1", RoleIDs: []string{"r-acc"}})
		if err != nil || got.CurrentStep != 1 {
			t.Fatalf("claimer: step=%v err=%v; want step 1", got, err)
		}
		if store.lastNext.Seats.Close != SeatApproved || store.lastNext.Seats.By != "duyet1" {
			t.Fatalf("seat side = %+v; want the claimer's seat closed as approved by duyet1", store.lastNext.Seats)
		}
	})
	t.Run("admin on a claimed seat", func(t *testing.T) {
		_, d, id := poolFixture(t, "duyet1")
		if _, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "boss", IsAdmin: true}); err != nil {
			t.Fatalf("admin: %v", err)
		}
	})
	t.Run("unclaimed: any candidate decides", func(t *testing.T) {
		_, d, id := poolFixture(t, "")
		if _, err := d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "duyet2", RoleIDs: []string{"r-acc"}}); err != nil {
			t.Fatalf("candidate on an unclaimed seat: %v", err)
		}
	})
}

// taskFixture arms a three-step instance — task (user u1), approval (u2), task (role r-acc) — as a
// graph, the way a pinned graph process reaches the engine.
func taskFixture(t *testing.T) (*fakeStore, *Decider, string) {
	t.Helper()
	g := &Graph{Nodes: []Node{
		{Step: Step{ID: "check", Name: "Kiểm tra chứng từ", ApproverType: ApproverUser, ApproverID: "u1"}, Kind: NodeTask, RequiredFields: []string{"doc_date"}},
		{Step: Step{ID: "boss", Name: "Giám đốc duyệt", ApproverType: ApproverUser, ApproverID: "u2"}, Kind: NodeApproval},
		{Step: Step{ID: "file", Name: "Lưu hồ sơ", ApproverType: ApproverRole, ApproverID: "r-acc"}, Kind: NodeTask},
	}}
	def := &Definition{Steps: []Step{}, Graph: g.normalized()}
	if len(def.Chain()) != 3 {
		t.Fatalf("graph with task nodes must be runnable, chain = %v", def.Chain())
	}
	store := newFakeStore()
	id, err := store.CreateInstance(context.Background(), CreateInstanceInput{
		TenantID: "t1", ProcessID: "p1", EntityName: "quotes", RecordID: "r1",
		CurrentApproverType: ApproverUser, CurrentApproverID: "u1", Definition: def,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	return store, NewDecider(store, store, store, nil, nil), id
}

// TestTaskStep_CompleteOnly (T093/T100): a task step is completed, never approved or rejected, and
// an approval step is never "completed"; completing advances like an approve, writes a "completed"
// history entry and closes the seat as completed; the next step's notice says "Việc mới" for a
// task and "Cần phê duyệt" for an approval.
func TestTaskStep_CompleteOnly(t *testing.T) {
	ctx := context.Background()
	u1 := Actor{UserID: "u1"}
	store, d, id := taskFixture(t)

	for _, k := range []DecisionKind{DecisionApprove, DecisionReject} {
		if _, err := d.Decide(ctx, "t1", id, k, "x", u1); !errors.Is(err, ErrWrongStepKind) {
			t.Fatalf("%s on a task: err = %v; want ErrWrongStepKind", k, err)
		}
	}
	if store.advanceCnt != 0 {
		t.Fatalf("a refused verb wrote")
	}

	got, err := d.Decide(ctx, "t1", id, DecisionComplete, "đủ chứng từ", u1)
	if err != nil || got.CurrentStep != 1 {
		t.Fatalf("complete: step=%v err=%v; want step 1", got, err)
	}
	if e := lastEntry(t, got); e.Action != HistoryCompleted || e.Node != "check" || e.StepName != "Kiểm tra chứng từ" {
		t.Fatalf("history entry = %+v; want completed on node check", e)
	}
	if store.lastNext.Seats.Close != SeatCompleted {
		t.Fatalf("seat close = %q; want completed", store.lastNext.Seats.Close)
	}
	if len(store.lastNext.Seats.Open) != 1 || store.lastNext.Seats.Open[0].Kind != SeatKindApproval {
		t.Fatalf("next seat = %+v; want one approval seat", store.lastNext.Seats.Open)
	}
	if n := store.notes[len(store.notes)-1]; n.UserID != "u2" || n.Title != "Cần phê duyệt" {
		t.Fatalf("notice for the approval step = %+v", n)
	}

	// Step 2 is an approval: complete is refused there; approve moves on to the task step.
	if _, err := d.Decide(ctx, "t1", id, DecisionComplete, "", Actor{UserID: "u2"}); !errors.Is(err, ErrWrongStepKind) {
		t.Fatalf("complete on an approval: err = %v; want ErrWrongStepKind", err)
	}
	got, err = d.Decide(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "u2"})
	if err != nil || got.CurrentStep != 2 {
		t.Fatalf("approve: step=%v err=%v; want step 2", got, err)
	}
	if len(store.lastNext.Seats.Open) != 1 || store.lastNext.Seats.Open[0].Kind != SeatKindTask ||
		store.lastNext.Seats.Open[0].AssigneeType != ApproverRole {
		t.Fatalf("next seat = %+v; want one task seat on the role", store.lastNext.Seats.Open)
	}

	// The last step is a task: completing it finishes the instance as approved.
	store.seats[id] = []Seat{{NodeID: "file", AssigneeType: ApproverRole, AssigneeID: "r-acc", Kind: SeatKindTask}}
	got, err = d.Decide(ctx, "t1", id, DecisionComplete, "", Actor{UserID: "duyet1", RoleIDs: []string{"r-acc"}})
	if err != nil || got.Status != StatusApproved {
		t.Fatalf("last task: status=%v err=%v; want approved", got, err)
	}
}

// TestTaskStep_NoticeSaysWork: a user-assigned task entered by the arming engine pings its holder
// with "Việc mới", not an invitation to approve.
func TestTaskStep_NoticeSaysWork(t *testing.T) {
	store := newFakeStore()
	g := &Graph{Nodes: []Node{{Step: Step{ID: "check", ApproverType: ApproverUser, ApproverID: "u1"}, Kind: NodeTask}}}
	loader := fakeLoader{procs: []Process{{ID: "p1", TenantID: "t1", EntityName: "quotes", Trigger: TriggerAfterCreate, Graph: g, IsActive: true}}}
	e := NewAsyncEngine(EngineConfig{Buffer: 1, Workers: 1}, loader, store, store, nil)
	e.arm(Event{TenantID: "t1", EntityName: "quotes", RecordID: "r1", Trigger: TriggerAfterCreate})
	if len(store.created) != 1 || len(store.created[0].Seats) != 1 || store.created[0].Seats[0].Kind != SeatKindTask {
		t.Fatalf("armed seats = %+v; want one task seat", store.created)
	}
	if len(store.notes) != 1 || store.notes[0].Title != "Việc mới" {
		t.Fatalf("notice = %+v; want Việc mới", store.notes)
	}
}

// TestGraph_TaskAndQueueNodes (T100/T101): a graph with task nodes and queue assignees is a graph
// this binary runs; a task keeps requiredFields through the pin; requiredFields on an approval
// node, a kind of a later slice, or an assignee type nobody runs are still refused (fail-closed).
func TestGraph_TaskAndQueueNodes(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		runs    bool
		fields  []string
		queueAt int // index of a node expected to be a queue assignee, -1 none
	}{
		{"task on a user with required fields",
			`{"nodes":[{"id":"a","kind":"task","approverType":"user","approverId":"u1","requiredFields":["doc_date","owner"]}]}`,
			true, []string{"doc_date", "owner"}, -1},
		{"approval on a queue",
			`{"nodes":[{"id":"a","kind":"approval","approverType":"queue","approverId":"q1"}]}`, true, nil, 0},
		{"task on a queue",
			`{"nodes":[{"id":"a","kind":"task","approverType":"queue","approverId":"q1"}]}`, true, nil, 0},
		{"requiredFields on an approval node",
			`{"nodes":[{"id":"a","kind":"approval","approverType":"user","approverId":"u1","requiredFields":["x"]}]}`, false, nil, -1},
		{"gateway is a later slice",
			`{"nodes":[{"id":"a","kind":"gateway","approverType":"user","approverId":"u1"}]}`, false, nil, -1},
		{"team assignee is a later slice",
			`{"nodes":[{"id":"a","kind":"task","approverType":"team","approverId":"t1"}]}`, false, nil, -1},
		{"unknown key still refuses the whole graph",
			`{"nodes":[{"id":"a","kind":"task","approverType":"user","approverId":"u1","lane":"all"}]}`, false, nil, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var g Graph
			if err := json.Unmarshal([]byte(tc.raw), &g); err != nil {
				t.Fatalf("decode: %v", err)
			}
			chain := (&Definition{Steps: []Step{}, Graph: g.normalized()}).Chain()
			if (len(chain) > 0) != tc.runs {
				t.Fatalf("runs = %v, want %v (chain %v)", len(chain) > 0, tc.runs, chain)
			}
			if !tc.runs {
				return
			}
			if len(tc.fields) > 0 {
				def := &Definition{Steps: []Step{}, Graph: g.normalized()}
				if got := def.RequiredFieldsAt(0); len(got) != len(tc.fields) || got[0] != tc.fields[0] || !chain[0].IsTask() {
					t.Fatalf("chain[0] = %+v, required %v; want a task carrying %v", chain[0], got, tc.fields)
				}
			}
			if tc.queueAt >= 0 && chain[tc.queueAt].ApproverType != ApproverQueue {
				t.Fatalf("chain[%d].ApproverType = %q; want queue", tc.queueAt, chain[tc.queueAt].ApproverType)
			}
			// The pin round-trips: marshal + decode keeps the node runnable and the task a task.
			b, err := json.Marshal(&Definition{Steps: []Step{}, Graph: g.normalized()})
			if err != nil {
				t.Fatalf("marshal pin: %v", err)
			}
			var back Definition
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatalf("decode pin: %v", err)
			}
			back.Graph = back.Graph.normalized()
			again := back.Chain()
			if len(again) != len(chain) || again[0].IsTask() != chain[0].IsTask() || len(back.RequiredFieldsAt(0)) != len(tc.fields) {
				t.Fatalf("pin round trip changed the chain: %+v → %+v", chain, again)
			}
		})
	}
}

// TestProjection_NeverQueue (D-5): the instance projection is an ENUM(user, role); a queue seat
// leaves it NULL instead of writing 'queue' (error 1265 on the fail-open arming path).
func TestProjection_NeverQueue(t *testing.T) {
	if at, id := projection([]Seat{{AssigneeType: ApproverQueue, AssigneeID: "q1"}}); at != "" || id != "" {
		t.Fatalf("projection of a queue seat = %q/%q; want empty", at, id)
	}
}

// TestQueueSeat_NeedsEntityView: a queue seat is held only by someone who may view the record's
// entity. The one rule serves the inbox (AwaitsActor) and the decision (authorize), so a queue
// seat is not decided by a user the work-item routes would hide it from. User and role seats are
// not subject to it; nil view judges nothing.
func TestQueueSeat_NeedsEntityView(t *testing.T) {
	inst := &Instance{Status: StatusPending, EntityName: "crm_quotes",
		Definition: &Definition{Steps: []Step{{ID: "s1"}}}}
	queue := Seat{NodeID: "s1", AssigneeType: ApproverQueue, AssigneeID: "q1", QueueLive: true, QueueRoleID: "r-acc"}
	role := Seat{NodeID: "s1", AssigneeType: ApproverRole, AssigneeID: "r-acc"}
	deny := EntityView(func(string) bool { return false })
	allow := EntityView(func(e string) bool { return e == "crm_quotes" })
	cases := []struct {
		name string
		seat Seat
		view EntityView
		want bool
	}{
		{"queue seat, viewer", queue, allow, true},
		{"queue seat, not a viewer", queue, deny, false},
		{"queue seat, viewer of another entity only", queue, EntityView(func(e string) bool { return e == "other" }), false},
		{"queue seat, not judged", queue, nil, true},
		{"role seat is not subject to the view rule", role, deny, true},
	}
	for _, c := range cases {
		a := Actor{UserID: "u1", RoleIDs: []string{"r-acc"}, QueueView: c.view}
		if got := AwaitsActor(inst, []Seat{c.seat}, a.UserID, a.RoleIDs, c.view); got != c.want {
			t.Errorf("%s: AwaitsActor = %v; want %v", c.name, got, c.want)
		}
		err := authorize(inst, []Seat{c.seat}, a)
		if (err == nil) != c.want {
			t.Errorf("%s: authorize = %v; want allowed=%v", c.name, err, c.want)
		}
		if !c.want && !errors.Is(err, ErrNotAuthorized) {
			t.Errorf("%s: authorize = %v; want ErrNotAuthorized", c.name, err)
		}
	}
	// An admin is never judged.
	if err := authorize(inst, []Seat{queue}, Actor{UserID: "boss", IsAdmin: true, QueueView: deny}); err != nil {
		t.Errorf("admin: authorize = %v; want nil", err)
	}
	// A claimed queue seat is "taken" only for those who could have had it.
	claimed := queue
	claimed.ClaimedBy = "u-other"
	if err := authorize(inst, []Seat{claimed}, Actor{UserID: "u1", RoleIDs: []string{"r-acc"}, QueueView: allow}); !errors.Is(err, ErrSeatClaimed) {
		t.Errorf("claimed, viewer: authorize = %v; want ErrSeatClaimed", err)
	}
	if err := authorize(inst, []Seat{claimed}, Actor{UserID: "u1", RoleIDs: []string{"r-acc"}, QueueView: deny}); !errors.Is(err, ErrNotAuthorized) {
		t.Errorf("claimed, not a viewer: authorize = %v; want ErrNotAuthorized (not a 409 that proves the item exists)", err)
	}
}
