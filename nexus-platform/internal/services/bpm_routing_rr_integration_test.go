//go:build integration

// specs/043 S6b — `bpm-routing-rr-quorum` (tasks T107, T112, T256, T258, T261 at the engine seam):
// a step handed to a TEAM is dealt out by round_robin (each member in turn), load_based (the member
// with the fewest open seats) or each_member (one seat per member, judged by quorum). Proven
// against real MySQL through the production composition (engine + decider + requester + BPMRouter
// with the team strategies + ApprovalService).
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run 'TestRouting_TeamRoundRobin|TestRouting_RR|TestRouting_LoadBased|TestRouting_EachMember|TestParallel_EachMember'
package services

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/bpm"
)

func strategyNode(id, team, strategy string, completion *ApprovalCompletionInput) ApprovalNodeInput {
	return ApprovalNodeInput{Kind: "approval", ApprovalStepInput: ApprovalStepInput{
		ID: id, Name: "Ban duyệt", Assignment: &ApprovalAssignmentInput{Type: "team", ID: team, Strategy: strategy}, Completion: completion,
	}}
}

// holderOf is the single user who holds the open seat of a record's approval.
func (s *rtStack) holderOf(t *testing.T, entity, rid string) string {
	t.Helper()
	seats := s.seatsOf(t, s.onlyPendingInstance(t, o3aTenantA, entity, rid).ID)
	if len(seats) != 1 || seats[0].AssigneeType != bpm.ApproverUser {
		t.Fatalf("%s/%s seats = %+v; want ONE user seat", entity, rid[:8], seats)
	}
	return seats[0].AssigneeID
}

// submit sends a fresh record of entity to approval as the sender ("Gửi duyệt": armed synchronously,
// so successive calls enter the step one after the other) and returns the instance.
func (s *rtStack) submit(t *testing.T, entity string) *bpm.Instance {
	t.Helper()
	rid := s.rtRecord(t, entity, nil)
	inst, err := s.requester.Submit(context.Background(), o3aTenantA, entity, rid, map[string]any{"status": "draft"}, bpm.Actor{UserID: s.sender})
	if err != nil {
		t.Fatalf("submit %s: %v", entity, err)
	}
	return inst
}

// holderOfInstance is the single user who holds the open seat of an instance.
func (s *rtStack) holderOfInstance(t *testing.T, id string) string {
	t.Helper()
	seats := s.seatsOf(t, id)
	if len(seats) != 1 || seats[0].AssigneeType != bpm.ApproverUser {
		t.Fatalf("instance %s seats = %+v; want ONE user seat", id[:8], seats)
	}
	return seats[0].AssigneeID
}

// T107 / T261 — "Ban duyệt" (duyet1, duyet2; a disabled member and a foreign-tenant junk member are
// in the team too): test@ sends 4 quotes → each active member gets exactly 2, in turn, and the
// others do not see them.
func TestRouting_TeamRoundRobinAlternates(t *testing.T) {
	s := rtWire(t)
	ctx := context.Background()
	s.rtEntity(t, "rtrr")
	s.rtProcessOn(t, "rtrr", "manual", nil, strategyNode("ban", s.team, "round_robin", nil))
	got := map[string]int{}
	var order []string
	for i := 0; i < 4; i++ {
		h := s.holderOfInstance(t, s.submit(t, "rtrr").ID)
		got[h]++
		order = append(order, h)
	}
	if got[s.duyet1] != 2 || got[s.duyet2] != 2 || len(got) != 2 {
		t.Fatalf("holders %v; want duyet1 and duyet2 two each and nobody else (disabled / foreign members never)", got)
	}
	if order[0] == order[1] || order[1] == order[2] || order[2] == order[3] {
		t.Fatalf("order %v; want the two members to alternate", order)
	}
	// a pick sees it in the approvals inbox; a non-member does not
	for _, u := range []string{s.duyet1, s.duyet2} {
		rows, err := s.approval.ListPending(ctx, o3aTenantA, bpm.Actor{UserID: u})
		if err != nil || len(rows) != 2 {
			t.Fatalf("inbox of %s = %d (%v); want its 2 turns", u[:8], len(rows), err)
		}
		if n := s.notifCount(t, o3aTenantA, u); n != 2 {
			t.Fatalf("bell of %s = %d; want 2", u[:8], n)
		}
	}
	if n := s.notifCount(t, o3aTenantA, s.duyet3); n != 0 {
		t.Fatalf("a non-member got %d notices", n)
	}
}

// T107 — the rotation under contention: 20 callers ask the same node's ring at once, two members →
// ten each (the cursor row is locked FOR UPDATE; a plain read would hand the same member out twice).
func TestRouting_RRConcurrentDistinct(t *testing.T) {
	s := rtWire(t)
	ctx := context.Background()
	router := NewBPMRouter(nil, s.repo, nil).WithTeams(NewAssignmentService(s.sdb, nil))
	in := bpm.RouteInput{TenantID: o3aTenantA, ProcessID: uuid.NewString(), NodeID: "ban"}
	a := bpm.Assignment{Type: bpm.AssignTeam, ID: s.team, Strategy: bpm.StrategyRoundRobin}
	const callers = 20
	var wg sync.WaitGroup
	picks := make([]string, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h, err := router.ResolveTeam(ctx, in, a)
			if err == nil && len(h) == 1 {
				picks[i] = h[0].ID
			} else if err == nil {
				err = fmt.Errorf("got %d holders", len(h))
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	got := map[string]int{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		got[picks[i]]++
	}
	if got[s.duyet1] != callers/2 || got[s.duyet2] != callers/2 || len(got) != 2 {
		t.Fatalf("picks %v; want %d each", got, callers/2)
	}
	// another node of the same process turns its OWN ring
	in2 := in
	in2.NodeID = "khac"
	h1, err1 := router.ResolveTeam(ctx, in2, a)
	h0, err0 := router.ResolveTeam(ctx, bpm.RouteInput{TenantID: o3aTenantA, ProcessID: uuid.NewString(), NodeID: "ban"}, a)
	if err1 != nil || err0 != nil || len(h1) != 1 || len(h0) != 1 || h1[0].ID != h0[0].ID {
		t.Fatalf("a fresh ring starts from the same member: %v %v (%v %v)", h1, h0, err1, err0)
	}
}

// T256 — load_based counts OPEN seats: duyet1 holds 3, duyet2 holds 1 → the new item goes to duyet2.
// Pool seats a member CLAIMED count as theirs too.
func TestRouting_LoadBasedCountsOpenSeats(t *testing.T) {
	s := rtWire(t)
	ctx := context.Background()
	// load: three personal seats for duyet1, one for duyet2
	s.rtEntity(t, "rtload0")
	s.rtProcessOn(t, "rtload0", "manual", nil, graphNode("p1", "P1", "user", s.duyet1, 0))
	for i := 0; i < 3; i++ {
		s.submit(t, "rtload0")
	}
	s.rtEntity(t, "rtload1")
	s.rtProcessOn(t, "rtload1", "manual", nil, graphNode("p2", "P2", "user", s.duyet2, 0))
	s.submit(t, "rtload1")

	s.rtEntity(t, "rtload")
	s.rtProcessOn(t, "rtload", "manual", nil, strategyNode("ban", s.team, "load_based", nil))
	if h := s.holderOfInstance(t, s.submit(t, "rtload").ID); h != s.duyet2 {
		t.Fatalf("holder = %s; want duyet2 (1 open seat) over duyet1 (3)", h)
	}

	// duyet2 now holds 2; claim two pool seats → 4 vs duyet1's 3: the next goes to duyet1
	s.rtEntity(t, "rtpool")
	s.rtProcessOn(t, "rtpool", "manual", nil, teamNode("pool", s.team))
	for i := 0; i < 2; i++ {
		s.submit(t, "rtpool")
	}
	member := WorkItemActor{UserID: s.duyet2}
	for _, id := range listIDs(t, s.wiStack, o3aTenantA, member, "pool") {
		if _, err := s.svc.Claim(ctx, o3aTenantA, id, member); err != nil {
			t.Fatalf("claim: %v", err)
		}
	}
	if h := s.holderOfInstance(t, s.submit(t, "rtload").ID); h != s.duyet1 {
		t.Fatalf("holder = %s; want duyet1 (3 open) over duyet2 (2 own + 2 claimed = 4)", h)
	}
}

// T112 / T261 — each_member + quorum 60% on a team of three: every member gets a seat and the bell;
// duyet1 and duyet2 approve → the step is over and duyet3's seat closes (superseded).
func TestRouting_EachMemberQuorumEndToEnd(t *testing.T) {
	s := rtWire(t)
	ctx := context.Background()
	if _, err := s.sdb.Exec(`INSERT INTO team_memberships (id, user_id, team_id) VALUES (?, ?, ?)`, uuid.NewString(), s.duyet3, s.team); err != nil {
		t.Fatalf("join duyet3: %v", err)
	}
	s.rtEntity(t, "rtmem")
	s.rtProcess(t, "rtmem", nil,
		strategyNode("ban", s.team, "each_member", &ApprovalCompletionInput{Mode: "quorum", Percent: 60}),
		graphNode("gd", "Giám đốc", "user", s.owner, 0))
	rid := s.rtRecord(t, "rtmem", nil)
	s.drain(t)
	inst := s.onlyPendingInstance(t, o3aTenantA, "rtmem", rid)
	seats := s.seatsOf(t, inst.ID)
	holders := []string{}
	for _, st := range seats {
		if st.AssigneeType != bpm.ApproverUser || st.NodeID != "ban" {
			t.Fatalf("seat %+v; want a user seat on node ban", st)
		}
		holders = append(holders, st.AssigneeID)
	}
	wantIDs(t, "seat holders", holders, s.duyet1, s.duyet2, s.duyet3) // the disabled and the foreign member are not seated
	if inst.CurrentApproverType != "" || inst.CurrentApproverID != "" {
		t.Fatalf("projection %q/%q; want NULL on a several-seat step", inst.CurrentApproverType, inst.CurrentApproverID)
	}
	for _, u := range holders {
		if n := s.notifCount(t, o3aTenantA, u); n != 1 {
			t.Fatalf("bell of %s = %d; want 1", u[:8], n)
		}
	}
	d, err := s.approval.GetApprovalDetail(ctx, RecordScope{UserID: s.duyet1}, o3aTenantA, inst.ID, bpm.Actor{UserID: s.duyet1})
	if err != nil || d.Completion == nil || d.Completion.Required != 2 || d.Completion.Total != 3 || d.Completion.Mode != "quorum" {
		t.Fatalf("detail completion = %+v (%v); want quorum, 3 seats, 2 required", d.Completion, err)
	}

	for _, u := range []string{s.duyet1, s.duyet2} {
		if _, err := s.approval.DecideAt(ctx, o3aTenantA, inst.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: u}, nil); err != nil {
			t.Fatalf("approve by %s: %v", u[:8], err)
		}
	}
	after := s.onlyPendingInstance(t, o3aTenantA, "rtmem", rid)
	if after.CurrentStep != 1 || after.CurrentApproverID != s.owner {
		t.Fatalf("after the quorum: step %d approver %q; want the director's step", after.CurrentStep, after.CurrentApproverID)
	}
	_, err = s.approval.DecideAt(ctx, o3aTenantA, inst.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: s.duyet3}, nil)
	wantItemCode(t, err, "APPROVAL_NOT_AUTHORIZED") // duyet3's seat is closed
	if rows, _ := s.approval.ListPending(ctx, o3aTenantA, bpm.Actor{UserID: s.duyet3}); len(rows) != 0 {
		t.Fatalf("duyet3 still has the finished step in the inbox")
	}
}

// T112 — the snapshot of a big team is capped at 50 seats.
func TestParallel_EachMemberSnapshotCap50_MySQL(t *testing.T) {
	s := rtWire(t)
	for i := 0; i < 55; i++ {
		u := uuid.NewString()
		if _, err := s.sdb.Exec(`INSERT INTO users (id, tenant_id, email, password_hash, status) VALUES (?, ?, ?, 'x', 'active')`, u, o3aTenantA, "cap-"+u[:8]+"@x.vn"); err != nil {
			t.Fatalf("user: %v", err)
		}
		if _, err := s.sdb.Exec(`INSERT INTO team_memberships (id, user_id, team_id) VALUES (?, ?, ?)`, uuid.NewString(), u, s.team); err != nil {
			t.Fatalf("membership: %v", err)
		}
	}
	s.rtEntity(t, "rtcap")
	s.rtProcess(t, "rtcap", nil, strategyNode("ban", s.team, "each_member", nil))
	rid := s.rtRecord(t, "rtcap", nil)
	s.drain(t)
	if n := len(s.seatsOf(t, s.onlyPendingInstance(t, o3aTenantA, "rtcap", rid).ID)); n != bpm.MaxApprovers {
		t.Fatalf("seats = %d; want the snapshot capped at %d", n, bpm.MaxApprovers)
	}
}

// Tenant isolation: tenant B asking for tenant A's team (a junk membership row of a B user sits in
// A's team) is dealt nobody — the team is not B's.
func TestRouting_TeamStrategiesAreTenantPinned(t *testing.T) {
	s := rtWire(t)
	ctx := context.Background()
	router := NewBPMRouter(nil, s.repo, nil).WithTeams(NewAssignmentService(s.sdb, nil))
	for _, strat := range []string{bpm.StrategyEachMember, bpm.StrategyRoundRobin, bpm.StrategyLoadBased} {
		got, err := router.ResolveTeam(ctx, bpm.RouteInput{TenantID: o3aTenantB, ProcessID: uuid.NewString(), NodeID: "n"},
			bpm.Assignment{Type: bpm.AssignTeam, ID: s.team, Strategy: strat})
		if err != nil || len(got) != 0 {
			t.Fatalf("%s: tenant B asking for A's team got %v (%v); want nobody", strat, got, err)
		}
	}
}
