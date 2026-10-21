//go:build integration

// specs/043 S3b — `bpm-seat-authority` (tasks T067–T071): authority to decide moves from the
// instance's current_step/escalated CAS and its current_approver_* projection to the work seats +
// the seq counter, proven against real MySQL.
//
// What S3b promises:
//   - a decision is ONE transaction that locks the instance row, checks the seq the caller READ
//     (a stale read — even one that "looks the same", the ABA — loses with ErrConcurrent) and that
//     the actor holds an open seat of the visit, then writes the instance and its seats;
//   - two tabs pressing Approve on one seat advance the step once; the loser gets the conflict;
//   - an instance the seats have never been written for (armed by a pre-S3a binary) gets the seat
//     the moment it is touched, on the same node id Migration A's backfill would have written;
//   - the SLA sweep walks tenants one by one, every query pinned to the tenant, and an instance
//     escalates exactly once however many sweeps race.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run 'TestDecide_|TestDelegate_|TestRecall_|TestClaimBreach_|TestSLAScheduler_|TestSeatNodeID_'
package services

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/jobs"
)

// racyStore is the real repository with a seam between the READ of an instance and the WRITE of a
// decision about it: onRead runs after every GetInstance, in the caller's goroutine. A test uses
// it to let another writer (or another tab) get in exactly there — the window a lost update lives in.
type racyStore struct {
	*bpm.SQLRepository
	onRead func(inst *bpm.Instance)
}

func (s *racyStore) GetInstance(ctx context.Context, tenantID, id string) (*bpm.Instance, error) {
	inst, err := s.SQLRepository.GetInstance(ctx, tenantID, id)
	if err == nil && s.onRead != nil {
		s.onRead(inst)
	}
	return inst, err
}

func (s *bpmStack) racyDecider(onRead func(*bpm.Instance)) (*bpm.Decider, *racyStore) {
	rs := &racyStore{SQLRepository: s.repo, onRead: onRead}
	return bpm.NewDecider(rs, s.repo, s.repo, nil, nil), rs
}

// armTwoStep creates a two-step process (seatAp1 then seatAp2, both with a 1h deadline) on a fresh
// entity and arms one instance on it through the real after-create path.
func armTwoStep(t *testing.T, st *bpmStack, entity string) (tenant, instID string) {
	t.Helper()
	tenant, ids := armTwoStepN(t, st, entity, 1)
	return tenant, ids[0]
}

// armTwoStepN is armTwoStep for n records of the entity, armed together.
func armTwoStepN(t *testing.T, st *bpmStack, entity string, n int) (tenant string, instIDs []string) {
	t.Helper()
	ctx := context.Background()
	tenant = o3aTenantA
	st.createEntity(t, ctx, tenant, entity, []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	if _, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: entity + "-2", EntityName: entity, Trigger: "after_create",
		Steps: []ApprovalStepInput{
			{Name: "Trưởng phòng", ApproverType: "user", ApproverID: seatAp1, SLASeconds: 3600},
			{Name: "Kế toán", ApproverType: "user", ApproverID: seatAp2, SLASeconds: 3600},
		},
		IsActive: boolPtr(true), ActorID: seatSubmitter,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	var rids []string
	for range n {
		rid, err := st.recordSvc.Create(ctx, bpmAdmin(seatSubmitter), tenant, seatSubmitter, entity, map[string]any{"status": "draft"})
		if err != nil {
			t.Fatalf("create record: %v", err)
		}
		rids = append(rids, rid)
	}
	st.drain(t)
	for _, rid := range rids {
		instIDs = append(instIDs, st.onlyPendingInstance(t, tenant, entity, rid).ID)
	}
	return tenant, instIDs
}

func approvedEntries(inst *bpm.Instance) int {
	n := 0
	for _, h := range inst.History {
		if h.Action == bpm.HistoryApproved {
			n++
		}
	}
	return n
}

// TestDecide_StaleReadAfterDelegate_Loses (T067, ABA): approver A reads the step, hands it to B,
// and only then does the stale Approve land. current_step and escalated are exactly what A read,
// so the old CAS let A decide a step A had already given away; the seq has moved, so now it loses.
func TestDecide_StaleReadAfterDelegate_Loses(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	tenant, id := armTwoStep(t, st, "seataba")

	var once sync.Once
	decider, _ := st.racyDecider(func(*bpm.Instance) {
		once.Do(func() {
			if _, err := st.requester.Delegate(ctx, tenant, id, seatAp3, "nghỉ phép", bpm.Actor{UserID: seatAp1}); err != nil {
				t.Errorf("delegate in the race window: %v", err)
			}
		})
	})
	_, err := decider.Decide(ctx, tenant, id, bpm.DecisionApprove, "duyệt", bpm.Actor{UserID: seatAp1})
	if !errors.Is(err, bpm.ErrConcurrent) {
		t.Fatalf("a stale approve after the step was handed on: err = %v; want ErrConcurrent", err)
	}

	inst, getErr := st.repo.GetInstance(ctx, tenant, id)
	if getErr != nil {
		t.Fatalf("GetInstance: %v", getErr)
	}
	if inst.Status != bpm.StatusPending || inst.CurrentStep != 0 || inst.CurrentApproverID != seatAp3 || approvedEntries(inst) != 0 {
		t.Fatalf("the stale approve left a mark: status=%s step=%d approver=%s history=%+v", inst.Status, inst.CurrentStep, inst.CurrentApproverID, inst.History)
	}
	if open := openSeats(seatsOf(t, st.sdb, tenant, id)); len(open) != 1 || open[0].AssigneeID != seatAp3 || open[0].NodeID != "s1" {
		t.Fatalf("open seats after the lost decision: %+v; want only the delegate's seat on s1", open)
	}
}

// TestDecide_SameSeatTwoTabs_SecondGets409 (T068, -race): one approver, two tabs, both read the
// step before either writes. The step advances once; the other tab is refused with the conflict
// the API answers as 409 APPROVAL_CONFLICT.
func TestDecide_SameSeatTwoTabs_SecondGets409(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	tenant, id := armTwoStep(t, st, "seattabs")

	var arrived int32
	var both sync.WaitGroup
	both.Add(2)
	decider, rs := st.racyDecider(nil)
	rs.onRead = func(*bpm.Instance) {
		if atomic.AddInt32(&arrived, 1) <= 2 { // the two tabs' reads; later reads pass through
			both.Done()
			both.Wait()
		}
	}
	svc := NewApprovalService(st.repo, st.repo, decider)

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.Decide(ctx, tenant, id, bpm.DecisionApprove, "duyệt", bpm.Actor{UserID: seatAp1})
		}(i)
	}
	wg.Wait()

	wins, conflicts := 0, 0
	for _, err := range errs {
		var ve *ValidationError
		switch {
		case err == nil:
			wins++
		case errors.As(err, &ve) && ve.Code == "APPROVAL_CONFLICT":
			conflicts++
		default:
			t.Fatalf("a tab got %v; want success or APPROVAL_CONFLICT", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d; want exactly one of each (errs: %v)", wins, conflicts, errs)
	}

	inst, err := st.repo.GetInstance(ctx, tenant, id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if inst.CurrentStep != 1 || inst.Status != bpm.StatusPending || approvedEntries(inst) != 1 {
		t.Fatalf("two tabs moved the instance to step %d (%s) with %d approvals; want step 1 pending, one approval", inst.CurrentStep, inst.Status, approvedEntries(inst))
	}
	assertMirror(t, st.sdb, tenant, id, "two-tabs")
	if got := instState(t, st.sdb, tenant, id).Seq; got != 2 {
		t.Fatalf("seq = %d; want 2 (the arm + the one decision that won)", got)
	}
}

// TestDecide_SingleSeatDeadlockVictim (#37) is TestParallel_ConcurrentLastTwoApprovers_DeadlockVictim
// for a single-seat approval. DecideTaskTx shares writeSeats with the vote, so its seat writes can
// wait on the seat of the instance whose id follows (an UPDATE that reads it, or the next step's
// seat going into the gap just before it in idx_bt_visit) while that instance's own writer waits on
// this instance's row. InnoDB rolls one of them back (1213), and the approver, a plain seat holder,
// got a 500 and had to click again.
//
// A second connection plays the neighbour: it locks the neighbour's seat, waits until the approval
// waits on it, then asks for a lock the approval holds (its instance row). It has written more rows,
// so InnoDB rolls the approval back every time. The approval must run again, and the step must
// advance exactly once. writeSeats no longer takes that range lock (#37, see
// TestDecide_BesideHeldNeighbour_NoRangeLock); the neighbour here still does, so this test pins the
// re-run that stays as a backstop.
func TestDecide_SingleSeatDeadlockVictim(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	tenant, ids := armTwoStepN(t, st, "seatlock", 2)
	id, next := ids[0], ids[1] // next: the instance whose seats follow id's in idx_bt_visit
	if next < id {
		id, next = next, id
	}

	if _, err := st.sdb.ExecContext(ctx, `CREATE TABLE it_deadlock_ballast (n INT NOT NULL PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	nb, err := st.sdb.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nb.Rollback() }()
	// InnoDB rolls back the transaction that wrote less: the ballast makes that the approval.
	if _, err := nb.ExecContext(ctx, `INSERT INTO it_deadlock_ballast (n)
		WITH RECURSIVE s (n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM s WHERE n < 200) SELECT n FROM s`); err != nil {
		t.Fatalf("ballast: %v", err)
	}
	var held []string
	if err := nb.SelectContext(ctx, &held, `SELECT id FROM sys_bpm_tasks FORCE INDEX (idx_bt_visit)
		WHERE tenant_id = ? AND instance_id = ? FOR UPDATE`, tenant, next); err != nil || len(held) != 1 {
		t.Fatalf("hold the neighbour's visit: %d seats, err %v; want its 1 seat", len(held), err)
	}

	errs := make(chan error, 1)
	go func() {
		_, err := st.approval.DecideAt(ctx, tenant, id, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp1}, nil)
		errs <- err
	}()
	waitUntil(t, "the approval to wait on the neighbour's locks", func() bool {
		var n int
		err := st.sdb.GetContext(ctx, &n, `SELECT COUNT(*) FROM performance_schema.data_locks
			WHERE object_schema = DATABASE() AND object_name = 'sys_bpm_tasks' AND lock_status = 'WAITING'`)
		return err == nil && n > 0
	})
	// The neighbour now waits on the approval: a deadlock, and the approval is the smaller transaction.
	var locked string
	if err := nb.GetContext(ctx, &locked, `SELECT id FROM sys_approval_instances WHERE tenant_id = ? AND id = ? FOR UPDATE`, tenant, id); err != nil {
		t.Fatalf("the neighbour lost the deadlock instead of the approval: %v", err)
	}
	if err := nb.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatalf("the approval failed: %v (a deadlock victim must decide again)", err)
	}

	inst, err := st.repo.GetInstance(ctx, tenant, id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if inst.Status != bpm.StatusPending || inst.CurrentStep != 1 || inst.CurrentApproverID != seatAp2 || approvedEntries(inst) != 1 {
		t.Fatalf("after the deadlock: %s@%d approver %s, %d approvals; want pending on step 1 (%s), one approval",
			inst.Status, inst.CurrentStep, inst.CurrentApproverID, approvedEntries(inst), seatAp2)
	}
	assertMirror(t, st.sdb, tenant, id, "after the deadlock")
	if got := instState(t, st.sdb, tenant, id).Seq; got != 2 {
		t.Fatalf("seq = %d; want 2 (the arm + the one decision)", got)
	}
}

// heldQueue is the breach-job queue of a repository whose write a test holds open: the first job a
// write parks (inside its transaction, after its seat writes) closes held, then waits for release.
// It enqueues nothing.
type heldQueue struct {
	held, release chan struct{}
	once          sync.Once
}

func (q *heldQueue) EnqueueExec(context.Context, sqlx.ExecerContext, jobs.Job) (string, error) {
	q.once.Do(func() { close(q.held) })
	<-q.release
	return "", nil
}

// holdNeighbour has the real repository approve instance id as actor (a single-seat decision or a
// vote) and holds that transaction open right after its seat writes, where it parks the next step's
// breach job. Every lock it then keeps on sys_bpm_tasks must be a record lock: a next-key or gap
// lock on a seat index is what a neighbouring instance's seat write waits on (#35, #37).
// release lets the write commit and returns its error; call it once.
func (s *bpmStack) holdNeighbour(t *testing.T, tenant, id string, actor bpm.Actor) (release func() error) {
	t.Helper()
	q := &heldQueue{held: make(chan struct{}), release: make(chan struct{})}
	repo := bpm.NewSQLRepository(s.sdb, nil).WithSLADueJobs(q)
	errs := make(chan error, 1)
	go func() {
		_, err := bpm.NewDecider(repo, repo, repo, nil, nil).Decide(context.Background(), tenant, id, bpm.DecisionApprove, "", actor)
		errs <- err
	}()
	var let sync.Once
	release = func() error { let.Do(func() { close(q.release) }); return <-errs }
	t.Cleanup(func() { let.Do(func() { close(q.release) }) })
	select {
	case <-q.held:
	case err := <-errs:
		t.Fatalf("the neighbour's write ended before it parked its breach job: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the neighbour's write never reached its breach job")
	}
	var locks []string
	if err := s.sdb.Select(&locks, `SELECT CONCAT(index_name, ' ', lock_mode, ' ', COALESCE(lock_data, ''))
		FROM performance_schema.data_locks
		WHERE object_schema = DATABASE() AND object_name = 'sys_bpm_tasks' AND lock_type = 'RECORD'`); err != nil {
		t.Fatalf("read the held write's seat locks: %v", err)
	}
	if len(locks) == 0 {
		t.Fatal("the held write keeps no seat lock: it is not held after its seat writes")
	}
	for _, l := range locks {
		if !strings.Contains(l, " X,REC_NOT_GAP ") {
			t.Errorf("the held write keeps a range lock on a seat index: %s; want record locks only (X,REC_NOT_GAP)", l)
		}
	}
	return release
}

// besideHeld runs do while a neighbour's write is held (holdNeighbour) and requires it to finish
// without waiting on a lock: the only transaction it could wait on is the neighbour's, the first
// half of the deadlock of #35 and #37. The neighbour is then released and must commit too.
func (s *bpmStack) besideHeld(t *testing.T, release func() error, do func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- do() }()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("beside the held neighbour: %v", err)
			}
			if err := release(); err != nil {
				t.Fatalf("the released neighbour: %v", err)
			}
			return
		case <-deadline:
			t.Fatal("neither finished nor waited on a lock in 20s")
		case <-time.After(10 * time.Millisecond):
		}
		var waits []string
		if err := s.sdb.Select(&waits, `SELECT CONCAT(object_name, ' ', COALESCE(index_name, ''), ' ', lock_mode, ' ', COALESCE(lock_data, ''))
			FROM performance_schema.data_locks WHERE object_schema = DATABASE() AND lock_status = 'WAITING'`); err != nil {
			t.Fatalf("read lock waits: %v", err)
		}
		if len(waits) > 0 {
			nerr := release()
			t.Fatalf("waited on the held neighbour: %v (after its release: %v; the neighbour: %v)", waits, <-done, nerr)
		}
	}
}

// TestDecide_BesideHeldNeighbour_NoRangeLock (#37, the root fix): the cycle of
// TestDecide_SingleSeatDeadlockVictim needs a seat write that waits on a NEIGHBOURING instance's
// seats. Here a real decision on the neighbour is held open after its seat writes. It must keep only
// record locks on sys_bpm_tasks, and the approval of the instance whose seats come before the
// neighbour's in idx_bt_visit must go through without waiting on it. Both then advanced once.
func TestDecide_BesideHeldNeighbour_NoRangeLock(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	tenant, ids := armTwoStepN(t, st, "seatheld", 2)
	id, next := ids[0], ids[1] // next: the instance whose seats follow id's in idx_bt_visit
	if next < id {
		id, next = next, id
	}

	release := st.holdNeighbour(t, tenant, next, bpm.Actor{UserID: seatAp1})
	st.besideHeld(t, release, func() error {
		_, err := st.approval.DecideAt(ctx, tenant, id, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp1}, nil)
		return err
	})

	for _, x := range []string{id, next} {
		inst, err := st.repo.GetInstance(ctx, tenant, x)
		if err != nil {
			t.Fatalf("GetInstance: %v", err)
		}
		if inst.Status != bpm.StatusPending || inst.CurrentStep != 1 || inst.CurrentApproverID != seatAp2 || approvedEntries(inst) != 1 {
			t.Fatalf("%s: %s@%d approver %s, %d approvals; want pending on step 1 (%s), one approval",
				x, inst.Status, inst.CurrentStep, inst.CurrentApproverID, approvedEntries(inst), seatAp2)
		}
		assertMirror(t, st.sdb, tenant, x, "beside the held neighbour")
		if got := instState(t, st.sdb, tenant, x).Seq; got != 2 {
			t.Fatalf("%s: seq = %d; want 2 (the arm + the one decision)", x, got)
		}
	}
}

// TestDecide_LateSecondClickOnNextStep_SecondGets409 (review finding on T068, MySQL): the same
// person holds BOTH steps (an admin may decide any), tab 2 still shows step 0 and clicks Approve
// AFTER tab 1 committed — no race, nothing for the seq lock to catch, because the server's own
// read already says step 1. The step the screen SAW (expectedStep) is the only thing that tells
// the click is stale: 409, and step 1 stays pending, unapproved.
func TestDecide_LateSecondClickOnNextStep_SecondGets409(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	tenant, id := armTwoStep(t, st, "seatlate")
	svc := NewApprovalService(st.repo, st.repo, bpm.NewDecider(st.repo, st.repo, st.repo, nil, nil))
	admin := bpm.Actor{UserID: seatAp3, IsAdmin: true}
	saw := 0

	if _, err := svc.DecideAt(ctx, tenant, id, bpm.DecisionApprove, "tab 1", admin, &saw); err != nil {
		t.Fatalf("tab 1: %v", err)
	}
	_, err := svc.DecideAt(ctx, tenant, id, bpm.DecisionApprove, "tab 2", admin, &saw)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "APPROVAL_CONFLICT" {
		t.Fatalf("tab 2 (late, still on step 0): err = %v; want APPROVAL_CONFLICT", err)
	}
	inst, getErr := st.repo.GetInstance(ctx, tenant, id)
	if getErr != nil {
		t.Fatalf("GetInstance: %v", getErr)
	}
	if inst.CurrentStep != 1 || inst.Status != bpm.StatusPending || approvedEntries(inst) != 1 {
		t.Fatalf("the late click moved the instance: step=%d status=%s approvals=%d; want step 1 pending, one approval", inst.CurrentStep, inst.Status, approvedEntries(inst))
	}
}

// TestDecide_SeatHolderWithNullProjection_Decides: authority is the SEAT. A holder of the open seat
// of a step whose current_approver_* projection is NULL (a step that is not exactly one user/role
// seat, D-5) saw the row in the inbox since S3a2 but could not decide it; now they can, and a
// stranger still cannot.
func TestDecide_SeatHolderWithNullProjection_Decides(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	tenant, id := armTwoStep(t, st, "seatnull")
	if _, err := st.sdb.Exec(`UPDATE sys_approval_instances SET current_approver_type = NULL, current_approver_id = NULL
		WHERE tenant_id = ? AND id = ?`, tenant, id); err != nil {
		t.Fatalf("null the projection: %v", err)
	}

	approval := NewApprovalService(st.repo, st.repo, bpm.NewDecider(st.repo, st.repo, st.repo, nil, nil))
	_, err := approval.Decide(ctx, tenant, id, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp3})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "APPROVAL_NOT_AUTHORIZED" {
		t.Fatalf("a stranger on a NULL-projection step: err = %v; want APPROVAL_NOT_AUTHORIZED", err)
	}
	if got := instState(t, st.sdb, tenant, id).CurrentStep; got != 0 {
		t.Fatalf("the refused stranger moved the instance to step %d", got)
	}
	if _, err := approval.Decide(ctx, tenant, id, bpm.DecisionApprove, "ok", bpm.Actor{UserID: seatAp1}); err != nil {
		t.Fatalf("the seat holder on a NULL-projection step: %v", err)
	}
	if got := instState(t, st.sdb, tenant, id).CurrentStep; got != 1 {
		t.Fatalf("current_step = %d; want 1 after the seat holder approved", got)
	}
	assertMirror(t, st.sdb, tenant, id, "null-projection-decided")
}

// sqlNodeID is the expression Migration A's backfill uses for a seat's node id (plan §Migration A
// 4(b)) — the Go seatNodeID must answer the same for every well-formed pin.
func sqlNodeID(t *testing.T, st *bpmStack, tenant, id string) string {
	t.Helper()
	var node string
	if err := st.sdb.Get(&node, `SELECT COALESCE(JSON_UNQUOTE(JSON_EXTRACT(process_snapshot, CONCAT('$.steps[', current_step, '].id'))),
		CONCAT('s', current_step + 1)) FROM sys_approval_instances WHERE tenant_id = ? AND id = ?`, tenant, id); err != nil {
		t.Fatalf("backfill expression: %v", err)
	}
	return node
}

// TestDecide_SeatlessInstance_CreatesSeatOnTouch (T070, D-6) + TestSeatNodeID_GoMatchesMigrationSQL:
// an instance armed by a pre-S3a binary has no seat. Deciding it leaves the seat a decision
// leaves — done, by the actor, on the node the pin names — instead of a decision no table shows.
// Run for a pin WITH ids and a LEGACY pin without them: the node the engine creates equals what
// the migration's SQL computes for the same row.
func TestDecide_SeatlessInstance_CreatesSeatOnTouch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stripIDs bool
		want     string
	}{
		{"pin with ids", false, "s1"},
		{"legacy pin without ids", true, "s1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := bpmWire(t)
			ctx := context.Background()
			tenant, id := armTwoStep(t, st, "seatless")
			if tc.stripIDs {
				if _, err := st.sdb.Exec(`UPDATE sys_approval_instances SET process_snapshot = JSON_REMOVE(process_snapshot, '$.steps[0].id', '$.steps[1].id')
					WHERE tenant_id = ? AND id = ?`, tenant, id); err != nil {
					t.Fatalf("strip pinned ids: %v", err)
				}
			}
			if _, err := st.sdb.Exec(`DELETE FROM sys_bpm_tasks WHERE tenant_id = ? AND instance_id = ?`, tenant, id); err != nil {
				t.Fatalf("drop the seats (a pre-S3a arming): %v", err)
			}
			if got := sqlNodeID(t, st, tenant, id); got != tc.want {
				t.Fatalf("setup: the migration's expression says %q; want %q", got, tc.want)
			}

			approval := NewApprovalService(st.repo, st.repo, bpm.NewDecider(st.repo, st.repo, st.repo, nil, nil))
			if _, err := approval.Decide(ctx, tenant, id, bpm.DecisionApprove, "duyệt", bpm.Actor{UserID: seatAp1}); err != nil {
				t.Fatalf("decide a seatless instance: %v", err)
			}

			seats := seatsOf(t, st.sdb, tenant, id)
			var done []seatRow
			for _, s := range seats {
				if s.Status == "done" {
					done = append(done, s)
				}
			}
			if len(done) != 1 {
				t.Fatalf("seats %+v; want exactly one done seat (the one created on touch and closed by the decision)", seats)
			}
			d := done[0]
			if d.NodeID != tc.want || str(d.DecidedBy) != seatAp1 || str(d.Outcome) != "approved" || d.AssigneeID != seatAp1 {
				t.Fatalf("done seat %+v; want %s approved by %s", d, tc.want, seatAp1)
			}
			assertMirror(t, st.sdb, tenant, id, "seatless-decided")
		})
	}
}

// TestDelegate_MovesSeatKeepsDue (T071): a delegation hands the seat on — the delegator's closes
// as reassigned, the target's opens on the same visit with the SAME deadline and delegated_from —
// and does not buy time.
func TestDelegate_MovesSeatKeepsDue(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	tenant, id := armTwoStep(t, st, "seatdeleg")
	before := openSeats(seatsOf(t, st.sdb, tenant, id))
	if len(before) != 1 || before[0].DueAt == nil {
		t.Fatalf("setup: want one open seat with a deadline, got %+v", before)
	}
	if _, err := st.requester.Delegate(ctx, tenant, id, seatAp3, "nghỉ phép", bpm.Actor{UserID: seatAp1}); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	seats := seatsOf(t, st.sdb, tenant, id)
	open := openSeats(seats)
	if len(open) != 1 {
		t.Fatalf("seats after delegating: %+v; want one open", seats)
	}
	o := open[0]
	if o.AssigneeID != seatAp3 || str(o.DelegatedFrom) != seatAp1 || !sameInstant(o.DueAt, before[0].DueAt) || o.EntrySeq != before[0].EntrySeq {
		t.Fatalf("delegate's seat %+v; want %s, delegated from %s, due %v, visit %d (as before)", o, seatAp3, seatAp1, before[0].DueAt, before[0].EntrySeq)
	}
	if seats[0].Status != "done" || str(seats[0].Outcome) != "reassigned" || str(seats[0].DecidedBy) != seatAp1 {
		t.Fatalf("delegator's seat %+v; want done/reassigned by %s", seats[0], seatAp1)
	}
}

// TestRecall_ClosesAllSeats (T071): a recall withdraws EVERY open seat, however many the step has.
func TestRecall_ClosesAllSeats(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	tenant, id := armTwoStep(t, st, "seatrecall")
	// A second open seat on the same step (what a parallel step opens, S4), planted raw.
	if _, err := st.sdb.Exec(`INSERT INTO sys_bpm_tasks (id, tenant_id, instance_id, process_id, entity_name, record_id, node_id,
		entry_seq, kind, assignee_type, assignee_id, status, version, created_at, updated_at)
		SELECT UUID(), tenant_id, instance_id, process_id, entity_name, record_id, node_id, entry_seq, kind, 'user', ?, 'open', 0, NOW(3), NOW(3)
		FROM sys_bpm_tasks WHERE tenant_id = ? AND instance_id = ? AND status = 'open'`, seatAp3, tenant, id); err != nil {
		t.Fatalf("plant a second seat: %v", err)
	}
	if n := len(openSeats(seatsOf(t, st.sdb, tenant, id))); n != 2 {
		t.Fatalf("setup: %d open seats; want 2", n)
	}
	var inst *bpm.Instance
	inst, err := st.requester.Recall(ctx, tenant, id, "gửi nhầm", bpm.Actor{UserID: seatSubmitter})
	if err != nil || inst.Status != bpm.StatusCancelled {
		t.Fatalf("recall: inst=%+v err=%v", inst, err)
	}
	for _, s := range seatsOf(t, st.sdb, tenant, id) {
		if s.Status != "cancelled" || str(s.Outcome) != "recalled" || s.DecidedBy != nil {
			t.Fatalf("seat after recall %+v; want cancelled/recalled, decided by nobody", s)
		}
	}
}

// breach arms one instance whose only step is already past its deadline and returns its id.
func armBreached(t *testing.T, st *bpmStack, tenant, entity string) string {
	t.Helper()
	ctx := context.Background()
	st.createEntity(t, ctx, tenant, entity, []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	if _, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: entity + "-sla", EntityName: entity, Trigger: "after_create",
		Steps:    []ApprovalStepInput{{Name: "OnCall", ApproverType: "user", ApproverID: seatAp1, SLASeconds: 1}},
		IsActive: boolPtr(true), ActorID: seatSubmitter,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	rid, err := st.recordSvc.Create(ctx, bpmAdmin(seatSubmitter), tenant, seatSubmitter, entity, map[string]any{"status": "draft"})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}
	return rid
}

func escalatedEntries(inst *bpm.Instance) int {
	n := 0
	for _, h := range inst.History {
		if h.Action == bpm.HistoryEscalated {
			n++
		}
	}
	return n
}

// TestClaimBreach_PerSeatOnce (T069, -race): several sweeps racing over one breached step escalate
// it once — one history entry, one notice, the seat stamped once — across BOTH tenants of a sweep.
func TestClaimBreach_PerSeatOnce(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	ridA := armBreached(t, st, o3aTenantA, "seatsla")
	ridB := armBreached(t, st, o3aTenantB, "seatslab")
	st.drain(t)
	idA := st.onlyPendingInstance(t, o3aTenantA, "seatsla", ridA).ID
	idB := st.onlyPendingInstance(t, o3aTenantB, "seatslab", ridB).ID

	notifier := bpm.NewSLAScheduler(bpm.SLASchedulerConfig{Interval: time.Minute}, st.repo, st.repo, nil, nil)
	now := time.Now().UTC().Add(time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); notifier.RunBreachesForTest(ctx, now) }()
	}
	wg.Wait()

	for _, c := range []struct{ tenant, id string }{{o3aTenantA, idA}, {o3aTenantB, idB}} {
		inst, err := st.repo.GetInstance(ctx, c.tenant, c.id)
		if err != nil {
			t.Fatalf("GetInstance: %v", err)
		}
		if !inst.Escalated || escalatedEntries(inst) != 1 {
			t.Fatalf("tenant %s: escalated=%v with %d escalation entries; want exactly one", c.tenant, inst.Escalated, escalatedEntries(inst))
		}
		if open := openSeats(seatsOf(t, st.sdb, c.tenant, c.id)); len(open) != 1 || open[0].BreachNotifiedAt == nil {
			t.Fatalf("tenant %s: open seats %+v; want the one seat stamped breached", c.tenant, open)
		}
		if got := st.notifCount(t, c.tenant, seatAp1); got != 2 { // arm notice + ONE breach notice
			t.Fatalf("tenant %s: %d notices for the approver; want 2 (arm + one breach)", c.tenant, got)
		}
	}
}

// TestSLAScheduler_IteratesTenantsPinned (T074): the sweep reads the tenant registry and then one
// pinned query per tenant. A breached instance of a tenant that is not active is left alone — a
// suspended customer is not pinged; the old scan escalated every tenant's rows.
func TestSLAScheduler_IteratesTenantsPinned(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	ridA := armBreached(t, st, o3aTenantA, "seatslaact")
	ridB := armBreached(t, st, o3aTenantB, "seatslasus")
	st.drain(t)
	idA := st.onlyPendingInstance(t, o3aTenantA, "seatslaact", ridA).ID
	idB := st.onlyPendingInstance(t, o3aTenantB, "seatslasus", ridB).ID
	if _, err := st.sdb.Exec(`UPDATE tenants SET status = 'suspended' WHERE id = ?`, o3aTenantB); err != nil {
		t.Fatalf("suspend tenant B: %v", err)
	}

	bpm.NewSLAScheduler(bpm.SLASchedulerConfig{Interval: time.Minute}, st.repo, st.repo, nil, nil).
		RunBreachesForTest(ctx, time.Now().UTC().Add(time.Hour))

	a, _ := st.repo.GetInstance(ctx, o3aTenantA, idA)
	b, _ := st.repo.GetInstance(ctx, o3aTenantB, idB)
	if a == nil || b == nil {
		t.Fatalf("instances vanished: a=%v b=%v", a, b)
	}
	if !a.Escalated {
		t.Fatalf("the active tenant's breached instance was not escalated")
	}
	if b.Escalated {
		t.Fatalf("a suspended tenant's instance was escalated by the sweep")
	}
}
