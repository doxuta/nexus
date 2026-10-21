//go:build integration

// specs/043 S4 — `bpm-parallel-approval` (tasks T079–T082, SC-003): a step held by several people,
// proven against real MySQL with -race.
//
// What S4 promises:
//   - a step with `approvers` opens one seat per approver on ONE visit; `all` needs every vote (a
//     rejection ends it), `any` lets the first decision decide; the votes are counted in the
//     transaction that records them;
//   - two last approvers pressing together lose no vote and advance the step exactly once
//     (SC-003: 50 rounds under -race), and neither of them is told "conflict";
//   - one person never decides two seats of a visit (409); an admin without a seat gets 409 and has
//     the explicit override (note mandatory, history marked), a single-seat step is unchanged;
//   - the outcome is written on the record only when the step is over; finishing a step supersedes
//     the sibling seats; a delegation hands on only the delegator's own seat; a voter keeps read
//     access to the approval they voted on.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run 'TestParallel_|TestAdmin_SingleSeat'
package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"nexus/platform/internal/bpm"
)

// parallelFixture is `rounds` records of a fresh entity, each armed on a process whose first step
// is a council of `approvers` (policy `mode`) and whose second step is the single user seatAp4.
type parallelFixture struct {
	st      *bpmStack
	tenant  string
	entity  string
	records []string
	insts   []string
}

func armCouncils(t *testing.T, entity, mode string, rounds int, approvers ...ApprovalApproverInput) *parallelFixture {
	t.Helper()
	return armCouncilsWith(t, entity, &ApprovalCompletionInput{Mode: mode}, rounds, approvers...)
}

// armCouncilsWith is armCouncils for a policy that carries more than a mode (a quorum percent).
func armCouncilsWith(t *testing.T, entity string, completion *ApprovalCompletionInput, rounds int, approvers ...ApprovalApproverInput) *parallelFixture {
	t.Helper()
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	st.createEntity(t, ctx, tenant, entity, []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	if _, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: entity + "-council", EntityName: entity, Trigger: "after_create",
		Steps: []ApprovalStepInput{
			{Name: "Hội đồng", Approvers: approvers, Completion: completion, SLASeconds: 3600},
			{Name: "Giám đốc", ApproverType: "user", ApproverID: seatAp4, SLASeconds: 3600},
		},
		Outcome:  &ApprovalOutcomeInput{StatusField: "status", ApprovedValue: "approved", RejectedValue: "rejected"},
		IsActive: boolPtr(true), ActorID: seatSubmitter,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	f := &parallelFixture{st: st, tenant: tenant, entity: entity}
	for i := 0; i < rounds; i++ {
		rid, err := st.recordSvc.Create(ctx, bpmAdmin(seatSubmitter), tenant, seatSubmitter, entity, map[string]any{"status": "draft"})
		if err != nil {
			t.Fatalf("create record %d: %v", i, err)
		}
		f.records = append(f.records, rid)
	}
	st.drain(t)
	for _, rid := range f.records {
		f.insts = append(f.insts, st.onlyPendingInstance(t, tenant, entity, rid).ID)
	}
	return f
}

func users(ids ...string) []ApprovalApproverInput {
	out := make([]ApprovalApproverInput, len(ids))
	for i, id := range ids {
		out[i] = ApprovalApproverInput{Type: "user", ID: id}
	}
	return out
}

func (f *parallelFixture) decide(t *testing.T, id string, kind bpm.DecisionKind, note string, actor bpm.Actor) (*bpm.Instance, error) {
	t.Helper()
	return f.st.approval.DecideAt(context.Background(), f.tenant, id, kind, note, actor, nil)
}

func seatsByNode(t *testing.T, f *parallelFixture, id, node string) (open, approved, rejected, superseded int) {
	t.Helper()
	for _, s := range seatsOf(t, f.st.sdb, f.tenant, id) {
		if s.NodeID != node {
			continue
		}
		switch {
		case s.Status == "open":
			open++
		case s.Outcome != nil && *s.Outcome == "approved":
			approved++
		case s.Outcome != nil && *s.Outcome == "rejected":
			rejected++
		case s.Outcome != nil && *s.Outcome == "superseded":
			superseded++
		}
	}
	return open, approved, rejected, superseded
}

// TestParallel_AllRequiresEveryone_MySQL: 3 seats, `all`. The step stays open through two votes
// (each voter's work leaves their own inbox only), the record keeps its status, and the third
// vote moves the instance to the next step, writing nothing yet (that step is not the last).
func TestParallel_AllRequiresEveryone_MySQL(t *testing.T) {
	f := armCouncils(t, "parall", "all", 1, users(seatAp1, seatAp2, seatAp3)...)
	ctx := context.Background()
	id := f.insts[0]

	if open, _, _, _ := seatsByNode(t, f, id, "s1"); open != 3 {
		t.Fatalf("seats at arm: open=%d; want 3 on node s1", open)
	}
	inst := f.st.onlyPendingInstance(t, f.tenant, f.entity, f.records[0])
	if inst.CurrentApproverType != "" || inst.CurrentApproverID != "" {
		t.Fatalf("projection of a 3-seat step = %s/%s; want NULL (D-5)", inst.CurrentApproverType, inst.CurrentApproverID)
	}
	for _, ap := range []string{seatAp1, seatAp2, seatAp3} {
		rows, err := f.st.repo.ListPendingForUser(ctx, f.tenant, ap, nil, nil)
		if err != nil || len(rows) != 1 {
			t.Fatalf("inbox of %s = %d rows (err %v); want the approval", ap, len(rows), err)
		}
	}

	for i, voter := range []string{seatAp1, seatAp2} {
		got, err := f.decide(t, id, bpm.DecisionApprove, "ok", bpm.Actor{UserID: voter})
		if err != nil {
			t.Fatalf("vote %d: %v", i+1, err)
		}
		if got.Status != bpm.StatusPending || got.CurrentStep != 0 {
			t.Fatalf("after %d of 3: %s@%d; want pending on step 0", i+1, got.Status, got.CurrentStep)
		}
		if rows, _ := f.st.repo.ListPendingForUser(ctx, f.tenant, voter, nil, nil); len(rows) != 0 {
			t.Fatalf("%s still has the work after voting", voter)
		}
	}
	if v := f.st.recordField(t, f.tenant, f.entity, f.records[0], "status"); v != "draft" {
		t.Fatalf("record status after 2 of 3 votes = %v; want untouched", v)
	}
	// Partial votes are on the timeline but do not close the step.
	cur, _ := f.st.repo.GetInstance(ctx, f.tenant, id)
	partial := 0
	for _, h := range cur.History {
		if h.Action == bpm.HistoryApproved && h.Partial {
			partial++
		}
	}
	if partial != 2 {
		t.Fatalf("partial vote entries = %d; want 2 (history %+v)", partial, cur.History)
	}

	got, err := f.decide(t, id, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp3})
	if err != nil {
		t.Fatalf("last vote: %v", err)
	}
	if got.Status != bpm.StatusPending || got.CurrentStep != 1 || got.CurrentApproverID != seatAp4 {
		t.Fatalf("after 3 of 3: %s@%d approver %s; want pending on the next step (%s)", got.Status, got.CurrentStep, got.CurrentApproverID, seatAp4)
	}
	if open, approved, _, _ := seatsByNode(t, f, id, "s1"); open != 0 || approved != 3 {
		t.Fatalf("council seats: open=%d approved=%d; want 0 open, 3 approved", open, approved)
	}
	if open, _, _, _ := seatsByNode(t, f, id, "s2"); open != 1 {
		t.Fatalf("next step seats: open=%d; want 1", open)
	}
	if v := f.st.recordField(t, f.tenant, f.entity, f.records[0], "status"); v != "draft" {
		t.Fatalf("record status after the first step = %v; want untouched until the last step", v)
	}
}

// TestParallel_AllOneRejectEnds_MySQL / AnyCancelsSiblings_MySQL: the two early-exit paths, with the
// sibling seats superseded and the outcome written once.
func TestParallel_AllOneRejectEnds_MySQL(t *testing.T) {
	f := armCouncils(t, "parrej", "all", 1, users(seatAp1, seatAp2, seatAp3)...)
	id := f.insts[0]
	if _, err := f.decide(t, id, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp1}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.decide(t, id, bpm.DecisionReject, "", bpm.Actor{UserID: seatAp2}); err == nil {
		t.Fatalf("a rejection without a reason was accepted (FR-027)")
	}
	got, err := f.decide(t, id, bpm.DecisionReject, "thiếu hồ sơ", bpm.Actor{UserID: seatAp2})
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if got.Status != bpm.StatusRejected {
		t.Fatalf("status = %s; want rejected", got.Status)
	}
	if open, approved, rejected, superseded := seatsByNode(t, f, id, "s1"); open != 0 || approved != 1 || rejected != 1 || superseded != 1 {
		t.Fatalf("seats: open=%d approved=%d rejected=%d superseded=%d; want 0/1/1/1", open, approved, rejected, superseded)
	}
	if v := f.st.recordField(t, f.tenant, f.entity, f.records[0], "status"); v != "rejected" {
		t.Fatalf("record status = %v; want the rejected value", v)
	}
	// The voter who was cut off finds the work gone from their inbox.
	if rows, _ := f.st.repo.ListPendingForUser(context.Background(), f.tenant, seatAp3, nil, nil); len(rows) != 0 {
		t.Fatalf("the third approver still has a finished approval in the inbox")
	}
}

func TestParallel_AnyCancelsSiblings_MySQL(t *testing.T) {
	f := armCouncils(t, "parany", "any", 1, users(seatAp1, seatAp2, seatAp3)...)
	id := f.insts[0]
	got, err := f.decide(t, id, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp2})
	if err != nil {
		t.Fatalf("first vote: %v", err)
	}
	if got.CurrentStep != 1 {
		t.Fatalf("after the first vote: step %d; want the next step", got.CurrentStep)
	}
	if open, approved, _, superseded := seatsByNode(t, f, id, "s1"); open != 0 || approved != 1 || superseded != 2 {
		t.Fatalf("seats: open=%d approved=%d superseded=%d; want 0/1/2", open, approved, superseded)
	}
	_, err = f.decide(t, id, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp1})
	wantApprovalCode(t, err, "APPROVAL_NOT_AUTHORIZED") // a sibling voting after the step ended
}

// TestParallel_ConcurrentLastTwoApprovers_OneAdvance (SC-003, -race, 50 rounds): the first approver
// has voted; the last two press together. Both succeed (no vote lost, nobody told "conflict"), the
// step advances exactly once, and the history keeps all three votes.
func TestParallel_ConcurrentLastTwoApprovers_OneAdvance(t *testing.T) {
	const rounds = 50
	f := armCouncils(t, "parrace", "all", rounds, users(seatAp1, seatAp2, seatAp3)...)

	for _, id := range f.insts {
		if _, err := f.decide(t, id, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp1}); err != nil {
			t.Fatalf("first vote: %v", err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, rounds*2)
	for _, id := range f.insts {
		for _, voter := range []string{seatAp2, seatAp3} {
			wg.Add(1)
			go func(id, voter string) {
				defer wg.Done()
				if _, err := f.decide(t, id, bpm.DecisionApprove, "", bpm.Actor{UserID: voter}); err != nil {
					errs <- err
				}
			}(id, voter)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a concurrent last vote failed: %v (no vote may be lost to a sibling)", err)
	}

	for i, id := range f.insts {
		wantAdvancedOnce(t, f, fmt.Sprintf("round %d", i), id)
	}
}

// wantAdvancedOnce: the three-seat council of instance id counted every vote (the first two
// partial) and opened the next step exactly once.
func wantAdvancedOnce(t *testing.T, f *parallelFixture, label, id string) {
	t.Helper()
	inst, err := f.st.repo.GetInstance(context.Background(), f.tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Status != bpm.StatusPending || inst.CurrentStep != 1 || inst.CurrentApproverID != seatAp4 {
		t.Fatalf("%s: %s@%d approver %s; want pending on step 1 (advanced once)", label, inst.Status, inst.CurrentStep, inst.CurrentApproverID)
	}
	votes, partial := 0, 0
	for _, h := range inst.History {
		if h.Action == bpm.HistoryApproved {
			votes++
			if h.Partial {
				partial++
			}
		}
	}
	if votes != 3 || partial != 2 {
		t.Fatalf("%s: history has %d votes (%d partial); want 3 (2 partial) — %+v", label, votes, partial, inst.History)
	}
	if open, approved, _, _ := seatsByNode(t, f, id, "s1"); open != 0 || approved != 3 {
		t.Fatalf("%s council seats open=%d approved=%d; want 0/3", label, open, approved)
	}
	if open, _, _, _ := seatsByNode(t, f, id, "s2"); open != 1 {
		t.Fatalf("%s next-step seats open=%d; want exactly 1 (the step advanced once)", label, open)
	}
}

// TestParallel_ConcurrentLastTwoApprovers_DeadlockVictim (#35), without luck. The two last approvers
// (plain seat holders, not admins) press together; the vote that finishes the step inserts the next
// step's seat into idx_bt_visit, in the gap just before the seats of the instance whose id follows.
// In CI that neighbour's own step write held the gap (next-key locks of its seat UPDATEs) and waited
// for a primary-key gap the vote held; InnoDB rolled back the smaller transaction, the vote, and the
// step never advanced ("open seat s2: Error 1213 … Deadlock", then "pending@0").
//
// A second connection plays the neighbour: it holds the neighbour's visit range, waits until the
// finishing vote waits on it, then asks for a lock the vote holds (its instance row). It has written
// more rows than any vote, so InnoDB rolls the vote back every time. The vote must run again, and
// the step must still advance exactly once. writeSeats no longer takes that range lock (#37); the
// neighbour here still does, so this test pins the re-run that stays as a backstop.
func TestParallel_ConcurrentLastTwoApprovers_DeadlockVictim(t *testing.T) {
	f := armCouncils(t, "parlock", "all", 2, users(seatAp1, seatAp2, seatAp3)...)
	ctx := context.Background()
	id, next := f.insts[0], f.insts[1] // next: the instance whose seats follow id's in idx_bt_visit
	if next < id {
		id, next = next, id
	}
	if _, err := f.decide(t, id, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp1}); err != nil {
		t.Fatalf("first vote: %v", err)
	}

	if _, err := f.st.sdb.ExecContext(ctx, `CREATE TABLE it_deadlock_ballast (n INT NOT NULL PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	nb, err := f.st.sdb.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nb.Rollback() }()
	// InnoDB rolls back the transaction that wrote less: the ballast makes that the vote.
	if _, err := nb.ExecContext(ctx, `INSERT INTO it_deadlock_ballast (n)
		WITH RECURSIVE s (n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM s WHERE n < 200) SELECT n FROM s`); err != nil {
		t.Fatalf("ballast: %v", err)
	}
	var held []string
	if err := nb.SelectContext(ctx, &held, `SELECT id FROM sys_bpm_tasks FORCE INDEX (idx_bt_visit)
		WHERE tenant_id = ? AND instance_id = ? FOR UPDATE`, f.tenant, next); err != nil || len(held) != 3 {
		t.Fatalf("hold the neighbour's visit: %d seats, err %v; want its 3 seats", len(held), err)
	}

	errs := make(chan error, 2)
	for _, voter := range []string{seatAp2, seatAp3} {
		go func(voter string) {
			_, err := f.decide(t, id, bpm.DecisionApprove, "", bpm.Actor{UserID: voter})
			errs <- err
		}(voter)
	}
	// The first vote is partial and commits; the one that finishes the step waits on the neighbour.
	waitUntil(t, "the finishing vote to wait on the neighbour's locks", func() bool {
		var n int
		err := f.st.sdb.GetContext(ctx, &n, `SELECT COUNT(*) FROM performance_schema.data_locks
			WHERE object_schema = DATABASE() AND object_name = 'sys_bpm_tasks' AND lock_status = 'WAITING'`)
		return err == nil && n > 0
	})
	// The neighbour now waits on the vote: a deadlock, and the vote is the smaller transaction.
	var locked string
	if err := nb.GetContext(ctx, &locked, `SELECT id FROM sys_approval_instances WHERE tenant_id = ? AND id = ? FOR UPDATE`, f.tenant, id); err != nil {
		t.Fatalf("the neighbour lost the deadlock instead of the vote: %v", err)
	}
	if err := nb.Rollback(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("a last vote failed: %v (a deadlock victim must vote again)", err)
		}
	}
	wantAdvancedOnce(t, f, "after the deadlock", id)
}

// TestParallel_FinishingVoteBesideHeldNeighbour_NoRangeLock (#35, #37, the root fix) is
// TestDecide_BesideHeldNeighbour_NoRangeLock for the vote: the finishing vote of the neighbour is
// held open after its seat writes and keeps only record locks on sys_bpm_tasks, and the finishing
// vote of the instance whose seats come before it in idx_bt_visit goes through without waiting.
func TestParallel_FinishingVoteBesideHeldNeighbour_NoRangeLock(t *testing.T) {
	f := armCouncils(t, "parheld", "all", 2, users(seatAp1, seatAp2, seatAp3)...)
	id, next := f.insts[0], f.insts[1] // next: the instance whose seats follow id's in idx_bt_visit
	if next < id {
		id, next = next, id
	}
	for _, x := range []string{id, next} {
		for _, voter := range []string{seatAp1, seatAp2} {
			if _, err := f.decide(t, x, bpm.DecisionApprove, "", bpm.Actor{UserID: voter}); err != nil {
				t.Fatalf("partial vote: %v", err)
			}
		}
	}

	release := f.st.holdNeighbour(t, f.tenant, next, bpm.Actor{UserID: seatAp3})
	f.st.besideHeld(t, release, func() error {
		_, err := f.decide(t, id, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp3})
		return err
	})
	wantAdvancedOnce(t, f, "the vote beside the held neighbour", id)
	wantAdvancedOnce(t, f, "the held neighbour", next)
}

// TestParallel_DelegateThenFinishingVote_NoOrphanSeat (#37 review, forced): writeSeats reads the open
// seats WITHOUT a lock, so it must still see a seat a concurrent writer of the same instance
// inserted. On an `any` council, seatAp2 hands their seat to seatAp4, a new open s1 seat, and that
// delegation is held open after its seat writes; seatAp3's vote, which finishes the step and
// supersedes every sibling, queues on the instance row meanwhile. The handed-on seat must be
// superseded, not left open forever. Red when a consistent read precedes the instance lock (the
// snapshot is then fixed before the delegation commits).
func TestParallel_DelegateThenFinishingVote_NoOrphanSeat(t *testing.T) {
	f := armCouncils(t, "pardeleg", "any", 1, users(seatAp1, seatAp2, seatAp3)...)
	ctx := context.Background()
	id := f.insts[0]

	release := f.st.holdWrite(t, func(repo *bpm.SQLRepository) error {
		_, err := bpm.NewRequester(repo, repo, repo, repo, nil).Delegate(ctx, f.tenant, id, seatAp4, "nghỉ phép", bpm.Actor{UserID: seatAp2})
		return err
	})
	vote := make(chan error, 1)
	go func() {
		_, err := f.decide(t, id, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp3})
		vote <- err
	}()
	f.st.waitLockWait(t, "sys_approval_instances")
	if err := release(); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if err := <-vote; err != nil {
		t.Fatalf("finishing vote: %v", err)
	}

	inst, err := f.st.repo.GetInstance(ctx, f.tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Status != bpm.StatusPending || inst.CurrentStep != 1 || inst.CurrentApproverID != seatAp4 {
		t.Fatalf("%s@%d approver %s; want pending on step 1 (%s)", inst.Status, inst.CurrentStep, inst.CurrentApproverID, seatAp4)
	}
	delegations := 0
	for _, h := range inst.History {
		if h.Action == bpm.HistoryDelegated {
			delegations++
		}
	}
	if delegations != 1 {
		t.Fatalf("delegations in history = %d; want 1", delegations)
	}
	if open, approved, _, superseded := seatsByNode(t, f, id, "s1"); open != 0 || approved != 1 || superseded != 2 {
		for _, s := range seatsOf(t, f.st.sdb, f.tenant, id) {
			t.Logf("seat %s node=%s %s/%s status=%s outcome=%s", s.ID, s.NodeID, s.AssigneeType, s.AssigneeID, s.Status, str(s.Outcome))
		}
		t.Fatalf("s1 seats open=%d approved=%d superseded=%d; want 0/1/2 (seatAp1 and the handed-on seat superseded)", open, approved, superseded)
	}
	if open, _, _, _ := seatsByNode(t, f, id, "s2"); open != 1 {
		t.Fatalf("s2 open seats = %d; want 1", open)
	}
	if got := instState(t, f.st.sdb, f.tenant, id).Seq; got != 3 {
		t.Fatalf("seq = %d; want 3 (the arm + the delegation + the vote)", got)
	}
}

// TestParallel_SameActorTwoSeats409_MySQL: the person named AND holding the seated role votes once.
func TestParallel_SameActorTwoSeats409_MySQL(t *testing.T) {
	f := armCouncils(t, "parsame", "all", 1,
		ApprovalApproverInput{Type: "user", ID: seatAp1}, ApprovalApproverInput{Type: "role", ID: seatRoleC}, ApprovalApproverInput{Type: "user", ID: seatAp3})
	id := f.insts[0]
	both := bpm.Actor{UserID: seatAp1, RoleIDs: []string{seatRoleC}}
	if _, err := f.decide(t, id, bpm.DecisionApprove, "", both); err != nil {
		t.Fatalf("first vote: %v", err)
	}
	_, err := f.decide(t, id, bpm.DecisionApprove, "", both)
	wantApprovalCode(t, err, "APPROVAL_ALREADY_DECIDED_BY_ACTOR") // second vote on the other seat; 409 over HTTP
	if open, approved, _, _ := seatsByNode(t, f, id, "s1"); open != 2 || approved != 1 {
		t.Fatalf("seats: open=%d approved=%d; want 2 open, 1 approved (the refused vote wrote nothing)", open, approved)
	}
	// A different holder of the role still takes the role seat.
	if _, err := f.decide(t, id, bpm.DecisionApprove, "", bpm.Actor{UserID: seatRoleUser, RoleIDs: []string{seatRoleC}}); err != nil {
		t.Fatalf("another role holder: %v", err)
	}
}

// TestParallel_AdminOnMultiSeat409_OverrideWorks_MySQL (+ the single-seat admin is unchanged).
func TestParallel_AdminOnMultiSeat409_OverrideWorks_MySQL(t *testing.T) {
	f := armCouncils(t, "paradm", "all", 2, users(seatAp1, seatAp2, seatAp3)...)
	ctx := context.Background()
	admin := bpm.Actor{UserID: "99999999-0000-0000-0000-000000000a99", IsAdmin: true}
	id := f.insts[0]

	_, err := f.decide(t, id, bpm.DecisionApprove, "", admin)
	wantApprovalCode(t, err, "APPROVAL_ADMIN_OVERRIDE_REQUIRED") // admin approving a council step
	if open, _, _, _ := seatsByNode(t, f, id, "s1"); open != 3 {
		t.Fatalf("the refused admin decision touched the seats (open=%d)", open)
	}
	// The override needs its reason.
	if _, err := f.st.approval.OverrideApproval(ctx, f.tenant, id, bpm.DecisionApprove, " ", admin, nil); err == nil {
		t.Fatalf("override without a note accepted")
	}
	_, err = f.st.approval.OverrideApproval(ctx, f.tenant, id, bpm.DecisionApprove, "hội đồng họp bất thường", bpm.Actor{UserID: seatAp1}, nil)
	wantApprovalCode(t, err, "APPROVAL_NOT_AUTHORIZED") // a seat holder is not an admin
	got, err := f.st.approval.OverrideApproval(ctx, f.tenant, id, bpm.DecisionApprove, "hội đồng họp bất thường", admin, nil)
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if got.CurrentStep != 1 || got.Status != bpm.StatusPending {
		t.Fatalf("after the override: %s@%d; want the next step", got.Status, got.CurrentStep)
	}
	last := got.History[len(got.History)-1]
	if last.Via != "override" || last.Comment != "hội đồng họp bất thường" || last.Actor != admin.UserID {
		t.Fatalf("history entry = %+v; want via=override with the reason and the admin", last)
	}
	if open, approved, _, superseded := seatsByNode(t, f, id, "s1"); open != 0 || approved != 0 || superseded != 3 {
		t.Fatalf("council seats: open=%d approved=%d superseded=%d; want all 3 superseded", open, approved, superseded)
	}
	// Override of a reject on the other record writes the rejected value.
	if _, err := f.st.approval.OverrideApproval(ctx, f.tenant, f.insts[1], bpm.DecisionReject, "hồ sơ giả", admin, nil); err != nil {
		t.Fatalf("override reject: %v", err)
	}
	if v := f.st.recordField(t, f.tenant, f.entity, f.records[1], "status"); v != "rejected" {
		t.Fatalf("record status after an override reject = %v; want rejected", v)
	}
}

// A single-seat step is decided by an admin exactly as before S4.
func TestAdmin_SingleSeatUnchanged_MySQL(t *testing.T) {
	st := bpmWire(t)
	tenant, id := armTwoStep(t, st, "paradm1")
	admin := bpm.Actor{UserID: "99999999-0000-0000-0000-000000000a99", IsAdmin: true}
	got, err := st.approval.DecideAt(context.Background(), tenant, id, bpm.DecisionApprove, "", admin, nil)
	if err != nil {
		t.Fatalf("admin on a single-seat step: %v", err)
	}
	if got.CurrentStep != 1 || got.Status != bpm.StatusPending {
		t.Fatalf("%s@%d; want advanced to step 1", got.Status, got.CurrentStep)
	}
}

// TestParallel_DelegateMovesOnlyOwnSeat_MySQL: delegating on a council hands on the delegator's
// seat and nobody else's; the projection stays empty; the delegate then votes.
func TestParallel_DelegateMovesOnlyOwnSeat_MySQL(t *testing.T) {
	f := armCouncils(t, "pardel", "all", 1, users(seatAp1, seatAp2)...)
	ctx := context.Background()
	id := f.insts[0]
	if _, err := f.st.requester.Delegate(ctx, f.tenant, id, seatAp3, "nghỉ phép", bpm.Actor{UserID: seatAp1}); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	rows := seatsOf(t, f.st.sdb, f.tenant, id)
	open := openSeats(rows)
	holders := map[string]bool{}
	for _, s := range open {
		holders[s.AssigneeID] = true
	}
	if len(open) != 2 || !holders[seatAp2] || !holders[seatAp3] || holders[seatAp1] {
		t.Fatalf("open seats after delegation: %+v; want ap2 and ap3 only", open)
	}
	cur, _ := f.st.repo.GetInstance(ctx, f.tenant, id)
	if cur.CurrentApproverType != "" {
		t.Fatalf("projection after delegating on a council = %s/%s; want NULL", cur.CurrentApproverType, cur.CurrentApproverID)
	}
	if _, err := f.decide(t, id, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp1}); err == nil {
		t.Fatalf("the delegator voted after handing their seat on")
	}
	for _, v := range []string{seatAp3, seatAp2} {
		if _, err := f.decide(t, id, bpm.DecisionApprove, "", bpm.Actor{UserID: v}); err != nil {
			t.Fatalf("vote by %s: %v", v, err)
		}
	}
	cur, _ = f.st.repo.GetInstance(ctx, f.tenant, id)
	if cur.CurrentStep != 1 {
		t.Fatalf("step after both remaining seats voted = %d; want 1", cur.CurrentStep)
	}
	// An admin without a seat cannot pick whose seat to move.
	g := armCouncils(t, "pardel2", "all", 1, users(seatAp1, seatAp2)...)
	if _, err := g.st.requester.Delegate(ctx, g.tenant, g.insts[0], seatAp3, "", bpm.Actor{UserID: "99999999-0000-0000-0000-000000000a99", IsAdmin: true}); !errors.Is(err, bpm.ErrAdminOverrideRequired) {
		t.Fatalf("admin delegating a council step without a seat: %v; want ErrAdminOverrideRequired", err)
	}
}

// TestParallel_DelegateRefusesCouncilMember_MySQL: handing a council seat to a colleague who sits on
// the same council (or already voted) would strand an `all` step on a seat nobody can use; it is
// refused and nothing moves. A stale override (a screen that saw an earlier step) is refused too.
func TestParallel_DelegateRefusesCouncilMember_MySQL(t *testing.T) {
	f := armCouncils(t, "pardelc", "all", 1, users(seatAp1, seatAp2, seatAp3)...)
	ctx := context.Background()
	id := f.insts[0]
	if _, err := f.st.requester.Delegate(ctx, f.tenant, id, seatAp2, "", bpm.Actor{UserID: seatAp1}); !errors.Is(err, bpm.ErrDelegateTargetSeated) {
		t.Fatalf("delegate to a seat holder: %v; want ErrDelegateTargetSeated", err)
	}
	if _, err := f.decide(t, id, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp2}); err != nil {
		t.Fatalf("ap2 votes: %v", err)
	}
	if _, err := f.st.requester.Delegate(ctx, f.tenant, id, seatAp2, "", bpm.Actor{UserID: seatAp1}); !errors.Is(err, bpm.ErrDelegateTargetSeated) {
		t.Fatalf("delegate to someone who voted: %v; want ErrDelegateTargetSeated", err)
	}
	if got := len(openSeats(seatsOf(t, f.st.sdb, f.tenant, id))); got != 2 {
		t.Fatalf("open seats after two refused delegations = %d; want 2 (ap1, ap3)", got)
	}
	// The last seats vote: the step ends; an override from the screen that saw step 0 is stale.
	for _, v := range []string{seatAp1, seatAp3} {
		if _, err := f.decide(t, id, bpm.DecisionApprove, "", bpm.Actor{UserID: v}); err != nil {
			t.Fatalf("vote by %s: %v", v, err)
		}
	}
	zero := 0
	_, err := f.st.approval.OverrideApproval(ctx, f.tenant, id, bpm.DecisionReject, "tab cũ", bpm.Actor{UserID: "99999999-0000-0000-0000-000000000a99", IsAdmin: true}, &zero)
	wantApprovalCode(t, err, "APPROVAL_CONFLICT")
}

// TestParallel_DetailShowsSeatsAndCompletion_MySQL: GET /approvals/:id payload — seats[] and the
// "1/3" tally — and a voter keeps read access to what they voted on, a stranger does not.
func TestParallel_DetailShowsSeatsAndCompletion_MySQL(t *testing.T) {
	f := armCouncils(t, "pardet", "all", 1, users(seatAp1, seatAp2, seatAp3)...)
	ctx := context.Background()
	id := f.insts[0]
	if _, err := f.decide(t, id, bpm.DecisionApprove, "đồng ý", bpm.Actor{UserID: seatAp1}); err != nil {
		t.Fatal(err)
	}
	d, err := f.st.approval.GetApprovalDetail(ctx, RecordScope{UserID: seatAp1}, f.tenant, id, bpm.Actor{UserID: seatAp1})
	if err != nil {
		t.Fatalf("the approver who already voted cannot open their approval: %v", err)
	}
	if d.Completion == nil || d.Completion.Mode != "all" || d.Completion.Total != 3 || d.Completion.Approved != 1 || d.Completion.Open != 2 || d.Completion.Required != 3 {
		t.Fatalf("completion = %+v; want all, 1 of 3 approved, 2 open, 3 required", d.Completion)
	}
	if len(d.Seats) != 3 {
		t.Fatalf("seats = %+v; want 3", d.Seats)
	}
	done := 0
	for _, s := range d.Seats {
		if s.Status == "done" && s.Outcome == "approved" && s.DecidedBy == seatAp1 && s.Comment == "đồng ý" && s.DecidedAt != nil {
			done++
		}
	}
	if done != 1 {
		t.Fatalf("seats %+v; want exactly one done seat by ap1 with the comment", d.Seats)
	}
	// "Is it mine" is the server's answer (the single-holder projection is empty on a council):
	// the voter no longer awaits it, the approver who has not voted yet does.
	if d.AwaitsMe {
		t.Fatalf("awaitsMe for the approver who already voted = true; want false")
	}
	d2, err := f.st.approval.GetApprovalDetail(ctx, RecordScope{UserID: seatAp2}, f.tenant, id, bpm.Actor{UserID: seatAp2})
	if err != nil || !d2.AwaitsMe {
		t.Fatalf("detail for the approver still to vote: err=%v awaitsMe=%v; want true", err, d2 != nil && d2.AwaitsMe)
	}
	// A stranger (not seated, not admin, no record visibility wired here) gets the no-oracle 404.
	_, err = f.st.approval.GetApprovalDetail(ctx, RecordScope{UserID: seatRoleUser}, f.tenant, id, bpm.Actor{UserID: seatRoleUser})
	wantApprovalCode(t, err, "APPROVAL_INSTANCE_NOT_FOUND")
}

// Tenant isolation: another tenant's id is "not found" for the vote transaction itself.
func TestParallel_VoteIsTenantPinned_MySQL(t *testing.T) {
	f := armCouncils(t, "partnt", "all", 1, users(seatAp1, seatAp2)...)
	_, err := f.st.repo.VoteTaskTx(context.Background(), o3aTenantB, f.insts[0], bpm.VoteGuard{
		Actor: bpm.Actor{UserID: seatAp1}, Node: "s1", SeatID: "x", Kind: bpm.SeatApproved, Mode: bpm.CompletionAll,
	}, bpm.InstanceTransition{})
	if !errors.Is(err, bpm.ErrInstanceNotFound) {
		t.Fatalf("cross-tenant vote: %v; want ErrInstanceNotFound", err)
	}
}

// The last safety net of a vote prepared WITHOUT routing the next step (VoteGuard.Unrouted): when a
// sibling's vote landed first and this one finishes the step, the transaction must write nothing
// and say the route is missing — otherwise an empty InstanceTransition would be applied to the
// instance. Deterministic: ap1 votes through the engine, ap2's vote is handed in directly.
func TestParallel_UnroutedVoteThatFinishesTheStepWritesNothing_MySQL(t *testing.T) {
	f := armCouncils(t, "parout", "all", 1, users(seatAp1, seatAp2)...)
	ctx := context.Background()
	id := f.insts[0]
	if _, err := f.decide(t, id, bpm.DecisionApprove, "ok", bpm.Actor{UserID: seatAp1}); err != nil {
		t.Fatalf("first vote: %v", err)
	}
	var seat string
	for _, s := range openSeats(seatsOf(t, f.st.sdb, f.tenant, id)) {
		if s.AssigneeID == seatAp2 {
			seat = s.ID
		}
	}
	if seat == "" {
		t.Fatal("no open seat for the second approver")
	}
	before := seatsOf(t, f.st.sdb, f.tenant, id)

	_, err := f.st.repo.VoteTaskTx(ctx, f.tenant, id, bpm.VoteGuard{
		Actor: bpm.Actor{UserID: seatAp2}, ExpectStep: 0, Node: "s1", SeatID: seat,
		Kind: bpm.SeatApproved, Mode: bpm.CompletionAll, Unrouted: true,
	}, bpm.InstanceTransition{})
	if err == nil || !strings.Contains(err.Error(), "not routed") {
		t.Fatalf("unrouted vote that finishes the step: %v; want the route-needed refusal", err)
	}
	inst := f.st.onlyPendingInstance(t, f.tenant, f.entity, f.records[0])
	if inst.CurrentStep != 0 || inst.Status != bpm.StatusPending {
		t.Fatalf("instance after the refused vote: %s@%d; want pending on step 0", inst.Status, inst.CurrentStep)
	}
	after := seatsOf(t, f.st.sdb, f.tenant, id)
	if len(after) != len(before) {
		t.Fatalf("seats %d -> %d; want unchanged", len(before), len(after))
	}
	if open, _, _, _ := seatsByNode(t, f, id, "s1"); open != 1 {
		t.Fatalf("open seats on s1 = %d; want the voter's seat still open", open)
	}
}
