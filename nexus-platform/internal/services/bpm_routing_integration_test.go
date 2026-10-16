//go:build integration

// specs/043 S6a — `bpm-routing` (tasks T108–T116, T120 at the engine seam): a step finds its holder
// by rule — a team pool, the person in a record field, the submitter — with a fallback assignee and
// a stuck-not-lost ending, and everyone who holds a role / team seat gets the "Cần phê duyệt" bell.
// Proven against real MySQL through the production composition (engine + decider + requester +
// BPMRouter + ApprovalService + WorkItemService).
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run 'TestRouting_|TestNotify_|TestTeamPool|TestProcessOptions'
package services

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/dynrepo"
)

// rtStack is the work-item stack with the S6a wiring main.go adds: the router on all three paths,
// the holder/team directory on the services.
type rtStack struct {
	*wiStack
	sender, owner, duyet1, duyet2, duyet3, ghost, userB string
	roleMgr                                             string
	team, teamB                                         string
}

func rtWire(t *testing.T) *rtStack {
	t.Helper()
	w := wiWire(t)
	router := NewBPMRouter(dynrepo.New(w.sdb), w.repo, nil).WithTeams(NewAssignmentService(w.sdb, nil)) // S6b: team strategies
	w.engine.WithRouter(router)
	w.decider.WithRouter(router)
	w.requester.WithRouter(router)
	w.approval.WithHolderChecker(w.repo.HolderExists).WithTeamDirectory(w.repo)
	w.svc.WithTeamDirectory(w.repo)

	s := &rtStack{wiStack: w}
	s.roleMgr = uuid.NewString()
	if _, err := w.sdb.Exec(`INSERT INTO acl_roles (id, tenant_id, name, label) VALUES (?, ?, 'rt_mgr', 'Quản lý')`, s.roleMgr, o3aTenantA); err != nil {
		t.Fatalf("role: %v", err)
	}
	mkUser := func(tenant, status, role string) string {
		id := uuid.NewString()
		var r any
		if role != "" {
			r = role
		}
		if _, err := w.sdb.Exec(`INSERT INTO users (id, tenant_id, email, password_hash, status, role_id) VALUES (?, ?, ?, 'x', ?, ?)`,
			id, tenant, "rt-"+id[:8]+"@x.vn", status, r); err != nil {
			t.Fatalf("user: %v", err)
		}
		return id
	}
	s.sender = mkUser(o3aTenantA, "active", "")
	s.owner = mkUser(o3aTenantA, "active", "")
	s.duyet1 = mkUser(o3aTenantA, "active", s.roleMgr)
	s.duyet2 = mkUser(o3aTenantA, "active", s.roleMgr)
	s.duyet3 = mkUser(o3aTenantA, "active", s.roleMgr)
	s.ghost = mkUser(o3aTenantA, "disabled", "")
	s.userB = mkUser(o3aTenantB, "active", "")

	mkTeam := func(tenant, name string) string {
		id := uuid.NewString()
		if _, err := w.sdb.Exec(`INSERT INTO teams (id, tenant_id, name) VALUES (?, ?, ?)`, id, tenant, name); err != nil {
			t.Fatalf("team: %v", err)
		}
		return id
	}
	join := func(team, user string) {
		if _, err := w.sdb.Exec(`INSERT INTO team_memberships (id, user_id, team_id) VALUES (?, ?, ?)`, uuid.NewString(), user, team); err != nil {
			t.Fatalf("membership: %v", err)
		}
	}
	s.team = mkTeam(o3aTenantA, "Ban duyệt")
	s.teamB = mkTeam(o3aTenantB, "Ban B")
	join(s.team, s.duyet1)
	join(s.team, s.duyet2)
	join(s.team, s.ghost) // a disabled member never holds the pool
	join(s.team, s.userB) // junk: a tenant-B user joined to a tenant-A team
	join(s.teamB, s.userB)
	return s
}

func sorted(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

func wantIDs(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if strings.Join(sorted(got), ",") != strings.Join(sorted(want), ",") {
		t.Fatalf("%s = %v; want %v", what, sorted(got), sorted(want))
	}
}

// T108 — membership is pinned to the TEAM's tenant: team_memberships has no tenant_id, so every
// lookup joins teams.tenant_id. A user of another tenant, a disabled user, and a team of another
// tenant are never a holder.
func TestRouting_TeamMembershipTenantPinned(t *testing.T) {
	s := rtWire(t)
	ctx := context.Background()
	seat := []bpm.Seat{{NodeID: "n", AssigneeType: bpm.ApproverTeam, AssigneeID: s.team}}

	got, err := s.repo.ResolveSeatUsers(ctx, o3aTenantA, seat, 50)
	if err != nil {
		t.Fatalf("ResolveSeatUsers: %v", err)
	}
	wantIDs(t, "holders of A's team in tenant A", got, s.duyet1, s.duyet2)

	got, err = s.repo.ResolveSeatUsers(ctx, o3aTenantB, seat, 50)
	if err != nil || len(got) != 0 {
		t.Fatalf("A's team asked as tenant B = %v (%v); want nobody — the team is not B's", got, err)
	}
	if got, err = s.repo.ResolveSeatUsers(ctx, o3aTenantA, seat, 1); err != nil || len(got) != 1 {
		t.Fatalf("limit 1 = %v (%v); want exactly one", got, err)
	}

	teams, err := s.repo.UserTeamIDs(ctx, o3aTenantA, s.duyet1)
	if err != nil {
		t.Fatalf("UserTeamIDs: %v", err)
	}
	wantIDs(t, "duyet1's teams in A", teams, s.team)
	if teams, err = s.repo.UserTeamIDs(ctx, o3aTenantB, s.duyet1); err != nil || len(teams) != 0 {
		t.Fatalf("duyet1's teams asked as B = %v (%v); want none", teams, err)
	}
	// userB is a member of A's team (junk row) AND of B's own team: as tenant B only B's team counts,
	// as tenant A the junk membership grants nothing because userB is not a user of A (the team
	// pool is held by whoever UserTeamIDs(A, user) names — and A never asks about a B user).
	if teams, err = s.repo.UserTeamIDs(ctx, o3aTenantB, s.userB); err != nil {
		t.Fatalf("UserTeamIDs: %v", err)
	}
	wantIDs(t, "userB's teams in B", teams, s.teamB)

	for _, c := range []struct {
		tenant string
		kind   bpm.ApproverType
		id     string
		want   bool
	}{
		{o3aTenantA, bpm.ApproverTeam, s.team, true},
		{o3aTenantB, bpm.ApproverTeam, s.team, false},
		{o3aTenantA, bpm.ApproverRole, s.roleMgr, true},
		{o3aTenantB, bpm.ApproverRole, s.roleMgr, false},
		{o3aTenantA, bpm.ApproverUser, s.duyet1, true},
		{o3aTenantA, bpm.ApproverUser, s.ghost, false}, // disabled
		{o3aTenantB, bpm.ApproverUser, s.duyet1, false},
		{o3aTenantA, bpm.ApproverQueue, s.team, false}, // not this method's business
	} {
		got, err := s.repo.HolderExists(ctx, c.tenant, c.kind, c.id)
		if err != nil || got != c.want {
			t.Fatalf("HolderExists(%s,%s) in %s = %v (%v); want %v", c.kind, c.id[:8], c.tenant[:8], got, err, c.want)
		}
	}
	if ok, err := s.repo.UserActive(ctx, o3aTenantA, s.owner); err != nil || !ok {
		t.Fatalf("UserActive(owner) = %v (%v)", ok, err)
	}
	if ok, err := s.repo.UserActive(ctx, o3aTenantA, s.ghost); err != nil || ok {
		t.Fatalf("UserActive(disabled) = %v (%v); want false", ok, err)
	}
}

func (s *rtStack) rtEntity(t *testing.T, entity string) {
	t.Helper()
	s.createEntity(t, context.Background(), o3aTenantA, entity, []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
		{Name: "assigned_to", Label: "Người phụ trách", FieldType: "user"},
	})
}

func (s *rtStack) rtProcess(t *testing.T, entity string, opts *ApprovalOptionsInput, nodes ...ApprovalNodeInput) *bpm.ProcessRecord {
	t.Helper()
	return s.rtProcessOn(t, entity, "after_create", opts, nodes...)
}

// rtProcessOn is rtProcess for another trigger ("manual": armed synchronously by Submit, so a test
// can assert the order in which successive entries are routed).
func (s *rtStack) rtProcessOn(t *testing.T, entity, trigger string, opts *ApprovalOptionsInput, nodes ...ApprovalNodeInput) *bpm.ProcessRecord {
	t.Helper()
	rec, err := s.approval.CreateProcess(context.Background(), o3aTenantA, CreateApprovalProcessInput{
		Name: entity + "-p", EntityName: entity, Trigger: trigger, Graph: graphNodes(nodes...), Options: opts,
		IsActive: boolPtr(true), ActorID: s.sender,
	})
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	return rec
}

func (s *rtStack) rtRecord(t *testing.T, entity string, data map[string]any) string {
	t.Helper()
	if data == nil {
		data = map[string]any{}
	}
	data["status"] = "draft"
	id, err := s.recordSvc.Create(context.Background(), bpmAdmin(s.sender), o3aTenantA, s.sender, entity, data)
	if err != nil {
		t.Fatalf("create record: %v", err)
	}
	return id
}

func teamNode(id, team string) ApprovalNodeInput {
	return ApprovalNodeInput{Kind: "approval", ApprovalStepInput: ApprovalStepInput{ID: id, Name: "Ban duyệt", Assignment: &ApprovalAssignmentInput{Type: "team", ID: team}}}
}

func ruleNode(id, ty, field string) ApprovalNodeInput {
	return ApprovalNodeInput{Kind: "approval", ApprovalStepInput: ApprovalStepInput{ID: id, Name: "Duyệt " + id, Assignment: &ApprovalAssignmentInput{Type: ty, Field: field}}}
}

func (s *rtStack) seatsOf(t *testing.T, instanceID string) []bpm.Seat {
	t.Helper()
	open, err := s.repo.ListOpenSeats(context.Background(), o3aTenantA, instanceID)
	if err != nil {
		t.Fatalf("seats: %v", err)
	}
	return open
}

func (s *rtStack) lastError(t *testing.T, instanceID string) string {
	t.Helper()
	var le *string
	if err := s.sdb.Get(&le, `SELECT last_error FROM sys_approval_instances WHERE tenant_id = ? AND id = ?`, o3aTenantA, instanceID); err != nil {
		t.Fatalf("last_error: %v", err)
	}
	if le == nil {
		return ""
	}
	return *le
}

// T120 (engine seam) — the team "Ban duyệt" as a POOL: its members — and only they — see the item in
// their pool and get the bell; one claims it, the other is told "taken" (and cannot decide it), a
// non-member learns nothing; the claimer decides and the record moves on.
func TestRouting_TeamPoolEndToEnd(t *testing.T) {
	s := rtWire(t)
	ctx := context.Background()
	s.rtEntity(t, "rtteam")
	s.rtProcess(t, "rtteam", nil, teamNode("ban", s.team))
	rid := s.rtRecord(t, "rtteam", nil)
	s.drain(t)

	inst := s.onlyPendingInstance(t, o3aTenantA, "rtteam", rid)
	seats := s.seatsOf(t, inst.ID)
	if len(seats) != 1 || seats[0].AssigneeType != bpm.ApproverTeam || seats[0].AssigneeID != s.team || seats[0].NodeID != "ban" {
		t.Fatalf("seats = %+v; want ONE team pool seat on node ban", seats)
	}
	if inst.CurrentApproverType != "" || inst.CurrentApproverID != "" {
		t.Fatalf("projection %q/%q; a team never reaches the ENUM (D-5)", inst.CurrentApproverType, inst.CurrentApproverID)
	}

	member := func(u string) WorkItemActor { return WorkItemActor{UserID: u} } // no role: membership alone
	for _, u := range []string{s.duyet1, s.duyet2} {
		if ids := listIDs(t, s.wiStack, o3aTenantA, member(u), "pool"); len(ids) != 1 {
			t.Fatalf("pool of member %s = %v; want the team's item", u[:8], ids)
		}
		if n := s.notifCount(t, o3aTenantA, u); n != 1 {
			t.Fatalf("bell of member %s = %d; want 1 (FR-019)", u[:8], n)
		}
	}
	for name, u := range map[string]string{"non-member duyet3 (same role!)": s.duyet3, "the sender": s.sender} {
		if ids := listIDs(t, s.wiStack, o3aTenantA, member(u), "pool"); len(ids) != 0 {
			t.Fatalf("pool of %s = %v; want empty", name, ids)
		}
		if n := s.notifCount(t, o3aTenantA, u); n != 0 {
			t.Fatalf("bell of %s = %d; want none", name, n)
		}
	}
	// a disabled member is not pinged (a disabled account has no session to look into the pool with)
	if n := s.notifCount(t, o3aTenantA, s.ghost); n != 0 {
		t.Fatalf("bell of the disabled member = %d; want none", n)
	}
	// the count the home card shows
	if c, err := s.svc.Counts(ctx, o3aTenantA, member(s.duyet2)); err != nil || c.Pool != 1 {
		t.Fatalf("counts = %+v (%v); want pool 1", c, err)
	}
	// the approvals inbox is the same rule: a pool item awaits every member until it is claimed
	if rows, err := s.approval.ListPending(ctx, o3aTenantA, bpm.Actor{UserID: s.duyet2}); err != nil || len(rows) != 1 {
		t.Fatalf("approvals pending of member = %d (%v); want 1", len(rows), err)
	}
	if rows, err := s.approval.ListPending(ctx, o3aTenantA, bpm.Actor{UserID: s.duyet3, RoleIDs: []string{s.roleMgr}}); err != nil || len(rows) != 0 {
		t.Fatalf("approvals pending of a non-member = %d (%v); want 0", len(rows), err)
	}

	itemID := listIDs(t, s.wiStack, o3aTenantA, member(s.duyet1), "pool")[0]
	_, err := s.svc.Claim(ctx, o3aTenantA, itemID, member(s.duyet3))
	wantItemCode(t, err, "WORK_ITEM_NOT_FOUND")
	if _, err := s.svc.Claim(ctx, o3aTenantA, itemID, member(s.duyet1)); err != nil {
		t.Fatalf("claim by a member: %v", err)
	}
	_, err = s.svc.Claim(ctx, o3aTenantA, itemID, member(s.duyet2))
	wantItemCode(t, err, "WORK_ITEM_ALREADY_CLAIMED")
	if ids := listIDs(t, s.wiStack, o3aTenantA, member(s.duyet2), "pool"); len(ids) != 0 {
		t.Fatalf("pool of the other member after the claim = %v; want empty", ids)
	}
	if ids := listIDs(t, s.wiStack, o3aTenantA, member(s.duyet1), "mine"); len(ids) != 1 {
		t.Fatalf("mine of the claimer = %v; want the item", ids)
	}

	// deciding goes through the SAME approvals verbs: the other member is refused, the stranger gets nothing
	_, err = s.approval.DecideAt(ctx, o3aTenantA, inst.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: s.duyet2}, nil)
	wantItemCode(t, err, "WORK_ITEM_ALREADY_CLAIMED") // a candidate hears "taken"
	_, err = s.approval.DecideAt(ctx, o3aTenantA, inst.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: s.duyet3, RoleIDs: []string{s.roleMgr}}, nil)
	wantItemCode(t, err, "APPROVAL_NOT_AUTHORIZED") // a stranger hears nothing about the seat
	done, err := s.approval.DecideAt(ctx, o3aTenantA, inst.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: s.duyet1}, nil)
	if err != nil || done.Status != bpm.StatusApproved {
		t.Fatalf("claimer approves: %+v (%v); want approved", done, err)
	}
}

// T109/T110 (engine seam) — the person in the field holds the step (and gets the bell), the owner is
// reachable as the pseudo-field, an empty field uses the fallback, nothing at all is stuck-not-lost.
func TestRouting_FieldAndFallbackAndStuck(t *testing.T) {
	s := rtWire(t)
	ctx := context.Background()
	fb := &ApprovalOptionsInput{FallbackAssignee: &ApprovalApproverInput{Type: "user", ID: s.duyet3}}

	// a) field → the user it holds
	s.rtEntity(t, "rtfield")
	s.rtProcess(t, "rtfield", fb, ruleNode("duyet", "field", "assigned_to"))
	r1 := s.rtRecord(t, "rtfield", map[string]any{"assigned_to": s.owner})
	// b) empty field → the fallback
	r2 := s.rtRecord(t, "rtfield", nil)
	// c) the record owner pseudo-field on another entity (owner = created_by = the sender)
	s.rtEntity(t, "rtowner")
	s.rtProcess(t, "rtowner", fb, ruleNode("duyet", "field", "owner_id"))
	r4 := s.rtRecord(t, "rtowner", nil)
	s.drain(t)

	holder := func(entity, rid string) string {
		seats := s.seatsOf(t, s.onlyPendingInstance(t, o3aTenantA, entity, rid).ID)
		if len(seats) != 1 || seats[0].AssigneeType != bpm.ApproverUser {
			t.Fatalf("%s/%s seats = %+v; want ONE user seat", entity, rid[:8], seats)
		}
		return seats[0].AssigneeID
	}
	if got := holder("rtfield", r1); got != s.owner {
		t.Fatalf("field holder = %s; want the user the field holds (owner)", got)
	}
	if n := s.notifCount(t, o3aTenantA, s.owner); n != 1 {
		t.Fatalf("the field's user got %d notices; want 1", n)
	}
	if got := holder("rtfield", r2); got != s.duyet3 {
		t.Fatalf("empty field holder = %s; want the fallback duyet3", got)
	}
	if got := holder("rtowner", r4); got != s.sender {
		t.Fatalf("owner_id holder = %s; want the record owner (the sender)", got)
	}
	if n := s.notifCount(t, o3aTenantA, s.sender); n != 1 {
		t.Fatalf("the owner-routed holder got %d notices; want 1", n)
	}

	// d) no fallback and nobody in the field → stuck, not lost (a fresh stack: the first one's engine is drained)
	s2 := rtWire(t)
	s2.rtEntity(t, "rtstuck")
	proc := s2.rtProcess(t, "rtstuck", nil, ruleNode("duyet", "field", "assigned_to"))
	rid := s2.rtRecord(t, "rtstuck", nil)
	s2.drain(t)
	inst := s2.onlyPendingInstance(t, o3aTenantA, "rtstuck", rid)
	if seats := s2.seatsOf(t, inst.ID); len(seats) != 0 {
		t.Fatalf("stuck instance has seats %+v", seats)
	}
	if le := s2.lastError(t, inst.ID); !strings.Contains(le, "duyet") {
		t.Fatalf("last_error = %q; want a reason naming the step", le)
	}
	rows, err := s2.repo.ListProcessInstances(ctx, o3aTenantA, proc.ID, nil, "pending", false, 10)
	if err != nil || len(rows) != 1 || rows[0].LastError == "" {
		t.Fatalf("admin instance list = %+v (%v); want the stuck one with its reason", rows, err)
	}
	// Doctor's seat-drift count is about instances whose inbox rests on the projection fallback; a step
	// stuck on purpose (last_error) is not that
	drift, err := s2.repo.ListSeatDrift(ctx, o3aTenantA, 50)
	if err != nil {
		t.Fatalf("ListSeatDrift: %v", err)
	}
	for _, d := range drift {
		if d.InstanceID == inst.ID {
			t.Fatalf("the stuck instance is reported as seat drift: %+v", d)
		}
	}
	// nobody sees it in an inbox, an admin can still settle it
	if pend, err := s2.approval.ListPending(ctx, o3aTenantA, bpm.Actor{UserID: s2.duyet1, RoleIDs: []string{s2.roleMgr}}); err != nil || len(pend) != 0 {
		t.Fatalf("a stuck instance appears in an inbox: %d (%v)", len(pend), err)
	}
	fin, err := s2.approval.DecideAt(ctx, o3aTenantA, inst.ID, bpm.DecisionApprove, "xử lý tay", bpm.Actor{UserID: s2.sender, IsAdmin: true}, nil)
	if err != nil || fin.Status != bpm.StatusApproved {
		t.Fatalf("admin settles the stuck step: %+v (%v)", fin, err)
	}
}

// The decision stands when the NEXT step has nobody: the instance moves there pending, with no seat
// and the reason written WITH the transition (not only at arm); an admin settling it clears the
// reason. And a stuck Submit keeps its reason through the "submitted" history write that follows
// the insert (a history append is not a state change of the step).
func TestRouting_StuckReasonSurvivesTransitionsOnMySQL(t *testing.T) {
	s := rtWire(t)
	ctx := context.Background()
	s.rtEntity(t, "rtstuck2")
	s.rtProcess(t, "rtstuck2", nil,
		graphNode("one", "Bước 1", "user", s.owner, 0),
		ruleNode("two", "field", "assigned_to"),
	)
	rid := s.rtRecord(t, "rtstuck2", nil) // assigned_to stays empty
	s.drain(t)
	inst := s.onlyPendingInstance(t, o3aTenantA, "rtstuck2", rid)
	if le := s.lastError(t, inst.ID); le != "" {
		t.Fatalf("step 1 has a holder but last_error = %q", le)
	}
	moved, err := s.approval.DecideAt(ctx, o3aTenantA, inst.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: s.owner}, nil)
	if err != nil || moved.Status != bpm.StatusPending || moved.CurrentStep != 1 {
		t.Fatalf("approve step 1: %+v (%v); want pending on step 2 (the decision stands)", moved, err)
	}
	if seats := s.seatsOf(t, inst.ID); len(seats) != 0 {
		t.Fatalf("seats of the stuck step = %+v; want none", seats)
	}
	if le := s.lastError(t, inst.ID); !strings.Contains(le, "two") && !strings.Contains(le, "Duyệt two") {
		t.Fatalf("last_error after the decision = %q; want the reason written with the transition", le)
	}
	fin, err := s.approval.DecideAt(ctx, o3aTenantA, inst.ID, bpm.DecisionApprove, "xử lý tay", bpm.Actor{UserID: s.sender, IsAdmin: true}, nil)
	if err != nil || fin.Status != bpm.StatusApproved {
		t.Fatalf("admin settles: %+v (%v)", fin, err)
	}
	if le := s.lastError(t, inst.ID); le != "" {
		t.Fatalf("last_error after the instance finished = %q; want cleared", le)
	}

	// manual Submit into a step nobody holds: created stuck, and still stuck after the history write
	s.rtEntity(t, "rtstuck3")
	if _, err := s.approval.CreateProcess(ctx, o3aTenantA, CreateApprovalProcessInput{
		Name: "rtstuck3-p", EntityName: "rtstuck3", Trigger: "manual", Graph: graphNodes(ruleNode("a", "field", "assigned_to")),
		IsActive: boolPtr(true), ActorID: s.sender,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	rid3 := s.rtRecord(t, "rtstuck3", nil)
	sub, err := s.requester.Submit(ctx, o3aTenantA, "rtstuck3", rid3, map[string]any{"status": "draft"}, bpm.Actor{UserID: s.sender})
	if err != nil {
		t.Fatalf("Submit into a step nobody holds must create the approval: %v", err)
	}
	if le := s.lastError(t, sub.ID); le == "" {
		t.Fatal("a stuck Submit lost its reason (the history write cleared last_error)")
	}
	if got, err := s.repo.GetInstance(ctx, o3aTenantA, sub.ID); err != nil || len(got.History) != 1 || got.History[0].Action != bpm.HistorySubmitted || got.LastError == "" {
		t.Fatalf("stuck instance = %+v (%v); want the submitted mark AND the reason", got, err)
	}
}

// The submit path: the submitter holds the step; an API-style submit (no known user) falls to the
// record owner; and the bell goes to a role's holders.
func TestRouting_SubmitterViaManualSubmit(t *testing.T) {
	s := rtWire(t)
	ctx := context.Background()
	s.rtEntity(t, "rtsub")
	rec, err := s.approval.CreateProcess(ctx, o3aTenantA, CreateApprovalProcessInput{
		Name: "rtsub-p", EntityName: "rtsub", Trigger: "manual", Graph: graphNodes(ruleNode("gui", "submitter", "")),
		IsActive: boolPtr(true), ActorID: s.sender,
	})
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	_ = rec
	rid := s.rtRecord(t, "rtsub", nil)
	inst, err := s.requester.Submit(ctx, o3aTenantA, "rtsub", rid, map[string]any{"status": "draft"}, bpm.Actor{UserID: s.duyet1})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	seats := s.seatsOf(t, inst.ID)
	if len(seats) != 1 || seats[0].AssigneeType != bpm.ApproverUser || seats[0].AssigneeID != s.duyet1 {
		t.Fatalf("seats = %+v; want the person who pressed Submit (duyet1)", seats)
	}
	if n := s.notifCount(t, o3aTenantA, s.duyet1); n != 1 {
		t.Fatalf("notices of the submitter-holder = %d; want 1", n)
	}
}

// T111/T120 — a ROLE seat's holders all get the bell, each once (the hole at engine.go:287), and the
// decision that opens a role step tells that role's holders too.
func TestNotify_RoleAndTeamHoldersReceiveNotice(t *testing.T) {
	s := rtWire(t)
	ctx := context.Background()
	s.rtEntity(t, "rtrole")
	s.rtProcess(t, "rtrole", nil,
		graphNode("one", "Bước 1", "user", s.owner, 0),
		graphNode("two", "Giám đốc", "role", s.roleMgr, 0),
	)
	rid := s.rtRecord(t, "rtrole", nil)
	s.drain(t)
	inst := s.onlyPendingInstance(t, o3aTenantA, "rtrole", rid)
	if n := s.notifCount(t, o3aTenantA, s.owner); n != 1 {
		t.Fatalf("step-1 holder bell = %d; want 1", n)
	}
	for _, u := range []string{s.duyet1, s.duyet2, s.duyet3} {
		if n := s.notifCount(t, o3aTenantA, u); n != 0 {
			t.Fatalf("role holder %s got a bell before their step opened", u[:8])
		}
	}
	if _, err := s.approval.DecideAt(ctx, o3aTenantA, inst.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: s.owner}, nil); err != nil {
		t.Fatalf("approve step 1: %v", err)
	}
	for _, u := range []string{s.duyet1, s.duyet2, s.duyet3} {
		if n := s.notifCount(t, o3aTenantA, u); n != 1 {
			t.Fatalf("role holder %s bell = %d; want exactly 1 \"Cần phê duyệt\"", u[:8], n)
		}
	}
	var title string
	if err := s.sdb.Get(&title, `SELECT title FROM sys_notifications WHERE tenant_id = ? AND user_id = ?`, o3aTenantA, s.duyet1); err != nil || title != "Cần phê duyệt" {
		t.Fatalf("notice title = %q (%v)", title, err)
	}
	if n := s.notifCount(t, o3aTenantA, s.sender); n != 0 {
		t.Fatalf("the sender got %d notices of a step they hold nothing in", n)
	}
}

// FR-017 — the holder is resolved ONCE, when the step is entered, and frozen: moving the field
// afterwards does not move a step already open; the NEXT step is resolved when it is entered.
func TestRouting_ResolvedOnEntryAndFrozen(t *testing.T) {
	s := rtWire(t)
	ctx := context.Background()
	s.rtEntity(t, "rtfrozen")
	s.rtProcess(t, "rtfrozen", nil,
		ruleNode("a", "field", "assigned_to"),
		ruleNode("b", "field", "assigned_to"),
	)
	rid := s.rtRecord(t, "rtfrozen", map[string]any{"assigned_to": s.duyet1})
	s.drain(t)
	inst := s.onlyPendingInstance(t, o3aTenantA, "rtfrozen", rid)
	if seats := s.seatsOf(t, inst.ID); len(seats) != 1 || seats[0].AssigneeID != s.duyet1 || seats[0].NodeID != "a" {
		t.Fatalf("step a seats = %+v; want duyet1", seats)
	}
	// the field moves to duyet2 while step a is open
	if err := s.recordSvc.Update(ctx, bpmAdmin(s.sender), o3aTenantA, "rtfrozen", rid, map[string]any{"assigned_to": s.duyet2}, nil); err != nil {
		t.Fatalf("update record: %v", err)
	}
	if seats := s.seatsOf(t, inst.ID); len(seats) != 1 || seats[0].AssigneeID != s.duyet1 {
		t.Fatalf("seat after the field moved = %+v; want it frozen on duyet1", seats)
	}
	if _, err := s.approval.DecideAt(ctx, o3aTenantA, inst.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: s.duyet1}, nil); err != nil {
		t.Fatalf("approve a: %v", err)
	}
	if seats := s.seatsOf(t, inst.ID); len(seats) != 1 || seats[0].AssigneeID != s.duyet2 || seats[0].NodeID != "b" {
		t.Fatalf("step b seats = %+v; want the field's value AT ENTRY of b (duyet2)", seats)
	}
}

// T113 at the SQL seam + options persistence: a team of another tenant / a missing role is refused
// at save; options (the fallback) round-trip through the process row, its versions and the pin.
func TestProcessOptions_PersistedVersionedPinned(t *testing.T) {
	s := rtWire(t)
	ctx := context.Background()
	s.rtEntity(t, "rtopts")

	_, err := s.approval.CreateProcess(ctx, o3aTenantA, CreateApprovalProcessInput{
		Name: "x", EntityName: "rtopts", Trigger: "manual", Graph: graphNodes(teamNode("a", s.teamB)), IsActive: boolPtr(true),
	})
	wantItemCode(t, err, "APPROVAL_ASSIGNEE_NOT_FOUND") // B's team named by an A process
	_, err = s.approval.CreateProcess(ctx, o3aTenantA, CreateApprovalProcessInput{
		Name: "x", EntityName: "rtopts", Trigger: "manual", Graph: graphNodes(teamNode("a", s.team)), IsActive: boolPtr(true),
		Options: &ApprovalOptionsInput{FallbackAssignee: &ApprovalApproverInput{Type: "role", ID: uuid.NewString()}},
	})
	wantItemCode(t, err, "APPROVAL_ASSIGNEE_NOT_FOUND") // a fallback role that does not exist
	_, err = s.approval.CreateProcess(ctx, o3aTenantA, CreateApprovalProcessInput{
		Name: "x", EntityName: "rtopts", Trigger: "manual", Graph: graphNodes(ruleNode("a", "field", "status")), IsActive: boolPtr(true),
	})
	wantItemCode(t, err, "APPROVAL_ASSIGNEE_FIELD_INVALID") // a text field names nobody

	fb := &ApprovalOptionsInput{FallbackAssignee: &ApprovalApproverInput{Type: "role", ID: s.roleMgr}}
	rec, err := s.approval.CreateProcess(ctx, o3aTenantA, CreateApprovalProcessInput{
		Name: "opts", EntityName: "rtopts", Trigger: "manual", Graph: graphNodes(ruleNode("a", "submitter", "")), Options: fb,
		IsActive: boolPtr(true), ActorID: s.sender,
	})
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	if string(rec.Options) != fmt.Sprintf(`{"fallbackAssignee": {"id": "%s", "type": "role"}}`, s.roleMgr) &&
		string(rec.Options) != fmt.Sprintf(`{"fallbackAssignee":{"type":"role","id":"%s"}}`, s.roleMgr) {
		t.Fatalf("stored options = %s", rec.Options)
	}
	// saving the same definition again publishes nothing (version numbers count definitions)
	same := UpdateApprovalProcessInput{
		Name: "opts", EntityName: "rtopts", Trigger: "manual", Graph: graphNodes(ruleNode("a", "submitter", "")), Options: fb,
		IsActive: boolPtr(true), ActorID: s.sender,
	}
	again, err := s.approval.UpdateProcess(ctx, o3aTenantA, rec.ID, same)
	if err != nil || again.Version != rec.Version {
		t.Fatalf("re-save unchanged: version %d → %d (%v); want no new version", rec.Version, again.Version, err)
	}
	// changing only the fallback IS a new version, and the draft carries options through publish
	same.Options = &ApprovalOptionsInput{FallbackAssignee: &ApprovalApproverInput{Type: "user", ID: s.duyet1}}
	if _, err := s.approval.SaveDraft(ctx, o3aTenantA, rec.ID, same); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	pub, err := s.approval.PublishDraft(ctx, o3aTenantA, rec.ID, "đổi người dự phòng", s.sender)
	if err != nil || pub.Version != rec.Version+1 {
		t.Fatalf("publish: %+v (%v); want version %d", pub, err, rec.Version+1)
	}
	if !strings.Contains(string(pub.Process.Options), s.duyet1) {
		t.Fatalf("published options = %s; want the new fallback", pub.Process.Options)
	}
	// an instance pins the options it started under
	rid := s.rtRecord(t, "rtopts", nil)
	inst, err := s.requester.Submit(ctx, o3aTenantA, "rtopts", rid, map[string]any{}, bpm.Actor{UserID: s.sender})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if inst.Definition == nil || inst.Definition.Options == nil || inst.Definition.Options.FallbackAssignee == nil || inst.Definition.Options.FallbackAssignee.ID != s.duyet1 {
		t.Fatalf("pinned definition options = %+v; want the fallback it started under", inst.Definition)
	}
	// a body that omits the options of a process that has them (an old client — the previous Studio
	// builder never showed them) would erase the fallback silently: refused like an erased graph,
	// on the legacy save AND on the draft save; ?force=true is the deliberate "remove it"
	noOpts := same
	noOpts.Options = nil
	_, err = s.approval.UpdateProcess(ctx, o3aTenantA, rec.ID, noOpts)
	wantItemCode(t, err, "APPROVAL_PROCESS_V2_OVERWRITE")
	_, err = s.approval.SaveDraft(ctx, o3aTenantA, rec.ID, noOpts)
	wantItemCode(t, err, "APPROVAL_PROCESS_V2_OVERWRITE")
	got, err := s.approval.GetProcess(ctx, o3aTenantA, rec.ID)
	if err != nil || !strings.Contains(string(got.Options), s.duyet1) {
		t.Fatalf("the refused save changed the options: %s (%v)", got.Options, err)
	}
	noOpts.Force = true
	if forced, err := s.approval.UpdateProcess(ctx, o3aTenantA, rec.ID, noOpts); err != nil || len(forced.Options) != 0 {
		t.Fatalf("forced removal: options %s (%v); want none", forced.Options, err)
	}

	// an old process (no options) still reads and saves
	old, err := s.approval.CreateProcess(ctx, o3aTenantA, CreateApprovalProcessInput{
		Name: "plain", EntityName: "rtopts", Trigger: "manual", Steps: []ApprovalStepInput{{ApproverType: "role", ApproverID: s.roleMgr}},
		IsActive: boolPtr(false), ActorID: s.sender,
	})
	if err != nil || len(old.Options) != 0 {
		t.Fatalf("plain process: options %s (%v); want none", old.Options, err)
	}
}
