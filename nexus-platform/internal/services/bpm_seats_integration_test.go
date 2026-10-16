//go:build integration

// specs/043 S3a — `bpm-seats-dual-write`: Migration A (sys_bpm_tasks + seq + backfill) and the
// seat DUAL WRITE, proven against real MySQL.
//
// What S3a promises (tasks.md T048–T057, T250):
//   - every state write of an approval instance — arm, Submit, Decide, Recall, Delegate, escalate —
//     mirrors itself onto sys_bpm_tasks IN THE SAME TRANSACTION, and bumps sys_approval_instances.seq
//     (the S3b CAS key: a rolling S3a→S3b deploy must see every step the S3a binary moved);
//   - authority is still the old CAS (current_step + escalated) and the inbox still reads the old
//     projection — seats are written, nobody reads them yet;
//   - Migration A pins every pending pre-000065 instance (from the process row EVEN IF soft-deleted,
//     on the wall clock) and gives every pending instance exactly one open seat, on the node id its
//     PIN names (not s{current_step+1}), with the deadline basis S1 recorded.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run 'TestSeats_|TestSeq_|TestArm_|TestMigrationA_'
package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/bpm"
)

// ---------------------------------------------------------------------------------------------
// Row readers — the tests judge the TABLES, not the Go values the engine returned.
// ---------------------------------------------------------------------------------------------

type seatRow struct {
	ID               string     `db:"id"`
	TenantID         string     `db:"tenant_id"`
	InstanceID       string     `db:"instance_id"`
	ProcessID        string     `db:"process_id"`
	EntityName       string     `db:"entity_name"`
	RecordID         string     `db:"record_id"`
	NodeID           string     `db:"node_id"`
	EntrySeq         int        `db:"entry_seq"`
	Kind             string     `db:"kind"`
	AssigneeType     string     `db:"assignee_type"`
	AssigneeID       string     `db:"assignee_id"`
	Status           string     `db:"status"`
	Outcome          *string    `db:"outcome"`
	DecidedBy        *string    `db:"decided_by"`
	Comment          *string    `db:"comment"`
	DueAt            *time.Time `db:"due_at"`
	DueBasis         *string    `db:"due_basis"`
	BreachNotifiedAt *time.Time `db:"breach_notified_at"`
	DelegatedFrom    *string    `db:"delegated_from"`
	CompletedAt      *time.Time `db:"completed_at"`
}

// seatsOf returns every seat of one instance, oldest visit first, the open seat of a visit last.
func seatsOf(t *testing.T, db *sqlx.DB, tenant, instID string) []seatRow {
	t.Helper()
	var rows []seatRow
	if err := db.Select(&rows, `SELECT id, tenant_id, instance_id, process_id, entity_name, record_id, node_id,
		entry_seq, kind, assignee_type, assignee_id, status, outcome, decided_by, comment, due_at, due_basis,
		breach_notified_at, delegated_from, completed_at
		FROM sys_bpm_tasks WHERE tenant_id = ? AND instance_id = ?
		ORDER BY entry_seq ASC, (status = 'open') ASC, created_at ASC, id ASC`, tenant, instID); err != nil {
		t.Fatalf("read seats of %s: %v", instID, err)
	}
	return rows
}

func openSeats(rows []seatRow) []seatRow {
	var out []seatRow
	for _, r := range rows {
		if r.Status == "open" {
			out = append(out, r)
		}
	}
	return out
}

type instRow struct {
	Seq          int              `db:"seq"`
	Status       string           `db:"status"`
	CurrentStep  int              `db:"current_step"`
	ApproverType *string          `db:"current_approver_type"`
	ApproverID   *string          `db:"current_approver_id"`
	SLADueAt     *time.Time       `db:"sla_due_at"`
	Escalated    bool             `db:"escalated"`
	Snapshot     *json.RawMessage `db:"process_snapshot"`
}

func instState(t *testing.T, db *sqlx.DB, tenant, id string) instRow {
	t.Helper()
	var r instRow
	if err := db.Get(&r, `SELECT seq, status, current_step, current_approver_type, current_approver_id,
		sla_due_at, escalated, process_snapshot FROM sys_approval_instances WHERE tenant_id = ? AND id = ?`, tenant, id); err != nil {
		t.Fatalf("read instance %s: %v", id, err)
	}
	return r
}

// pinnedNode is the stable id of the step the instance stands on, read off its pin.
func pinnedNode(t *testing.T, r instRow) string {
	t.Helper()
	if r.Snapshot == nil {
		t.Fatalf("instance has no pinned definition")
	}
	var def bpm.Definition
	if err := json.Unmarshal(*r.Snapshot, &def); err != nil {
		t.Fatalf("decode pin: %v", err)
	}
	steps := bpm.NormalizeSteps(def.Steps)
	if r.CurrentStep >= len(steps) {
		t.Fatalf("current_step %d outside the %d pinned steps", r.CurrentStep, len(steps))
	}
	return steps[r.CurrentStep].ID
}

func sameInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func str(p *string) string {
	if p == nil {
		return "<NULL>"
	}
	return *p
}

// assertMirror is the S3a invariant, judged on the rows: a pending instance has EXACTLY one open
// seat, on the node its pin names, holding what the old projection says, with the instance's
// deadline, flagged breached iff the instance is escalated, entered at or before the current seq;
// a terminal instance has none.
func assertMirror(t *testing.T, db *sqlx.DB, tenant, instID, stage string) {
	t.Helper()
	inst := instState(t, db, tenant, instID)
	open := openSeats(seatsOf(t, db, tenant, instID))
	if inst.Status != string(bpm.StatusPending) {
		if len(open) != 0 {
			t.Fatalf("[%s] %s instance still has %d open seat(s): %+v", stage, inst.Status, len(open), open)
		}
		return
	}
	if len(open) != 1 {
		t.Fatalf("[%s] pending instance has %d open seats; want exactly 1: %+v", stage, len(open), open)
	}
	s := open[0]
	if s.AssigneeType != str(inst.ApproverType) || s.AssigneeID != str(inst.ApproverID) {
		t.Fatalf("[%s] open seat holder %s/%s ≠ instance projection %s/%s", stage, s.AssigneeType, s.AssigneeID, str(inst.ApproverType), str(inst.ApproverID))
	}
	if want := pinnedNode(t, inst); s.NodeID != want {
		t.Fatalf("[%s] open seat node %q; the pin says the instance stands on %q", stage, s.NodeID, want)
	}
	if !sameInstant(s.DueAt, inst.SLADueAt) {
		t.Fatalf("[%s] seat due %v ≠ instance sla_due_at %v", stage, s.DueAt, inst.SLADueAt)
	}
	if (s.BreachNotifiedAt != nil) != inst.Escalated {
		t.Fatalf("[%s] seat breach_notified_at=%v but instance escalated=%v", stage, s.BreachNotifiedAt, inst.Escalated)
	}
	// A visit is entered at or before the instance's current seq (a backfilled seat: 0 = 0).
	if s.EntrySeq > inst.Seq {
		t.Fatalf("[%s] seat entry_seq %d ahead of instance seq %d", stage, s.EntrySeq, inst.Seq)
	}
	if s.Kind != "approval" {
		t.Fatalf("[%s] seat kind %q; want approval", stage, s.Kind)
	}
}

// ---------------------------------------------------------------------------------------------
// One scenario, every write path. Both TestSeats_EveryWritePathMirrors and
// TestSeq_BumpedOnEveryStateWrite walk it and judge different columns after each stage.
// ---------------------------------------------------------------------------------------------

const (
	seatSubmitter = "11111111-0000-0000-0000-000000000a01"
	seatAp1       = "22222222-0000-0000-0000-000000000a02"
	seatAp2       = "33333333-0000-0000-0000-000000000a03"
	seatAp3       = "44444444-0000-0000-0000-000000000a04"
	seatAp4       = "55555555-0000-0000-0000-000000000a05"
	seatRoleC     = "66666666-0000-0000-0000-000000000a06"
	seatRoleUser  = "77777777-0000-0000-0000-000000000a07"
)

// seatStage is one write the scenario performed: which instance, which path, and how many state
// writes that path is (Submit = the insert + its "submitted" history write).
type seatStage struct {
	name   string
	instID string
	writes int
}

func runSeatScenario(t *testing.T, st *bpmStack, after func(stage seatStage)) {
	t.Helper()
	ctx := context.Background()
	const tenant = o3aTenantA
	st.createEntity(t, ctx, tenant, "seatq", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	st.createEntity(t, ctx, tenant, "seatm", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	if _, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: "seats-3", EntityName: "seatq", Trigger: "after_create",
		Steps: []ApprovalStepInput{
			{Name: "Trưởng phòng", ApproverType: "user", ApproverID: seatAp1, SLASeconds: 60},
			{Name: "Kế toán", ApproverType: "user", ApproverID: seatAp2, SLASeconds: 60},
			{Name: "Ban giám đốc", ApproverType: "role", ApproverID: seatRoleC},
		},
		IsActive: boolPtr(true), ActorID: seatSubmitter,
	}); err != nil {
		t.Fatalf("CreateProcess seats-3: %v", err)
	}
	if _, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: "seats-manual", EntityName: "seatm", Trigger: "manual",
		Steps:    []ApprovalStepInput{{Name: "Trưởng phòng", ApproverType: "user", ApproverID: seatAp1, SLASeconds: 3600}},
		IsActive: boolPtr(true), ActorID: seatSubmitter,
	}); err != nil {
		t.Fatalf("CreateProcess seats-manual: %v", err)
	}

	// arm ×2 (the after-commit engine path).
	r1, err := st.recordSvc.Create(ctx, bpmAdmin(seatSubmitter), tenant, seatSubmitter, "seatq", map[string]any{"status": "draft"})
	if err != nil {
		t.Fatalf("create r1: %v", err)
	}
	r2, err := st.recordSvc.Create(ctx, bpmAdmin(seatSubmitter), tenant, seatSubmitter, "seatq", map[string]any{"status": "draft"})
	if err != nil {
		t.Fatalf("create r2: %v", err)
	}
	st.drain(t)
	a := st.onlyPendingInstance(t, tenant, "seatq", r1).ID
	b := st.onlyPendingInstance(t, tenant, "seatq", r2).ID
	after(seatStage{"arm", a, 1})
	after(seatStage{"arm-2", b, 1})

	decide := func(id string, kind bpm.DecisionKind, note string, actor bpm.Actor) {
		t.Helper()
		if _, err := st.approval.Decide(ctx, tenant, id, kind, note, actor); err != nil {
			t.Fatalf("%s by %s: %v", kind, actor.UserID, err)
		}
	}
	delegate := func(id, from, to, note string) {
		t.Helper()
		if _, err := st.requester.Delegate(ctx, tenant, id, to, note, bpm.Actor{UserID: from}); err != nil {
			t.Fatalf("delegate %s→%s: %v", from, to, err)
		}
	}

	// b is rejected before the breach scan below, so the scan touches only a.
	decide(b, bpm.DecisionReject, "thiếu chứng từ", bpm.Actor{UserID: seatAp1})
	after(seatStage{"reject", b, 1})

	decide(a, bpm.DecisionApprove, "ok", bpm.Actor{UserID: seatAp1})
	after(seatStage{"decide-advance", a, 1})
	delegate(a, seatAp2, seatAp3, "nghỉ phép")
	after(seatStage{"delegate", a, 1})
	st.sla.RunBreachesForTest(ctx, time.Now().UTC().Add(2*time.Hour))
	after(seatStage{"escalate", a, 1})
	delegate(a, seatAp3, seatAp4, "chuyển tiếp")
	after(seatStage{"delegate-escalated", a, 1})
	decide(a, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp4})
	after(seatStage{"decide-to-role", a, 1})
	decide(a, bpm.DecisionApprove, "duyệt cuối", bpm.Actor{UserID: seatRoleUser, RoleIDs: []string{seatRoleC}})
	after(seatStage{"decide-final", a, 1})

	// The manual verbs: Submit (insert + "submitted" history write), Recall.
	r3, err := st.recordSvc.Create(ctx, bpmAdmin(seatSubmitter), tenant, seatSubmitter, "seatm", map[string]any{"status": "draft"})
	if err != nil {
		t.Fatalf("create r3: %v", err)
	}
	sub, err := st.requester.Submit(ctx, tenant, "seatm", r3, map[string]any{"status": "draft"}, bpm.Actor{UserID: seatSubmitter})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	after(seatStage{"submit", sub.ID, 2})
	if _, err := st.requester.Recall(ctx, tenant, sub.ID, "gửi nhầm", bpm.Actor{UserID: seatSubmitter}); err != nil {
		t.Fatalf("recall: %v", err)
	}
	after(seatStage{"recall", sub.ID, 1})
}

// TestSeats_EveryWritePathMirrors (T051): after every write path the seat table says what the
// instance says — and the closed seats keep who did what.
func TestSeats_EveryWritePathMirrors(t *testing.T) {
	st := bpmWire(t)
	const tenant = o3aTenantA
	runSeatScenario(t, st, func(s seatStage) {
		assertMirror(t, st.sdb, tenant, s.instID, s.name)
		seats := seatsOf(t, st.sdb, tenant, s.instID)
		last := seats[len(seats)-1]
		switch s.name {
		case "arm":
			if len(seats) != 1 || last.NodeID != "s1" || last.AssigneeID != seatAp1 || last.DueAt == nil ||
				last.DueBasis == nil || *last.DueBasis != string(bpm.SLABasisWall) {
				t.Fatalf("[arm] seats %+v; want one open s1 seat for ap1 with a wall-clock due", seats)
			}
			if last.ProcessID == "" || last.EntityName != "seatq" || last.RecordID == "" {
				t.Fatalf("[arm] seat does not carry its instance's process/entity/record: %+v", last)
			}
		case "decide-advance":
			if len(seats) != 2 || seats[0].Status != "done" || str(seats[0].Outcome) != "approved" ||
				str(seats[0].DecidedBy) != seatAp1 || str(seats[0].Comment) != "ok" || seats[0].CompletedAt == nil {
				t.Fatalf("[decide-advance] step-1 seat not closed as ap1's approval: %+v", seats[0])
			}
			if last.NodeID != "s2" || last.AssigneeID != seatAp2 || last.EntrySeq <= seats[0].EntrySeq {
				t.Fatalf("[decide-advance] next seat %+v; want s2 for ap2 on a NEW visit", last)
			}
		case "delegate":
			prev := seats[len(seats)-2]
			if prev.Status != "done" || str(prev.Outcome) != "reassigned" || str(prev.DecidedBy) != seatAp2 {
				t.Fatalf("[delegate] handed-on seat %+v; want done/reassigned by ap2", prev)
			}
			if last.AssigneeID != seatAp3 || str(last.DelegatedFrom) != seatAp2 || last.NodeID != "s2" ||
				last.EntrySeq != prev.EntrySeq || !sameInstant(last.DueAt, prev.DueAt) || str(last.DueBasis) != str(prev.DueBasis) {
				t.Fatalf("[delegate] new seat %+v; want ap3 on the SAME visit, same due+basis, from ap2 (prev %+v)", last, prev)
			}
		case "escalate":
			if last.BreachNotifiedAt == nil {
				t.Fatalf("[escalate] open seat not stamped breached: %+v", last)
			}
		case "delegate-escalated":
			if last.AssigneeID != seatAp4 || last.BreachNotifiedAt == nil {
				t.Fatalf("[delegate-escalated] the breach flag did not follow the delegation: %+v", last)
			}
		case "decide-to-role":
			if last.NodeID != "s3" || last.AssigneeType != "role" || last.AssigneeID != seatRoleC || last.BreachNotifiedAt != nil || last.DueAt != nil {
				t.Fatalf("[decide-to-role] seat %+v; want a fresh role seat on s3 without SLA", last)
			}
		case "decide-final":
			if last.Status != "done" || str(last.Outcome) != "approved" || str(last.DecidedBy) != seatRoleUser {
				t.Fatalf("[decide-final] last seat %+v; want done/approved by the role holder", last)
			}
		case "reject":
			if len(seats) != 1 || last.Status != "done" || str(last.Outcome) != "rejected" || str(last.Comment) != "thiếu chứng từ" {
				t.Fatalf("[reject] seat %+v; want done/rejected with the reason", last)
			}
		case "submit":
			if len(seats) != 1 || last.Status != "open" || last.NodeID != "s1" || last.AssigneeID != seatAp1 {
				t.Fatalf("[submit] seats %+v; want one open s1 seat for ap1", seats)
			}
		case "recall":
			if last.Status != "cancelled" || str(last.Outcome) != "recalled" || last.DecidedBy != nil || last.CompletedAt == nil {
				t.Fatalf("[recall] seat %+v; want cancelled/recalled, decided by nobody", last)
			}
		}
	})
}

// TestSeq_BumpedOnEveryStateWrite (T250): every write bumps seq by exactly its number of state
// writes; a write that LOSES its CAS bumps nothing. S3b's CAS on seq relies on this to see every
// step an S3a binary moved during a rolling deploy.
func TestSeq_BumpedOnEveryStateWrite(t *testing.T) {
	st := bpmWire(t)
	const tenant = o3aTenantA
	ctx := context.Background()
	last := map[string]int{}
	runSeatScenario(t, st, func(s seatStage) {
		got := instState(t, st.sdb, tenant, s.instID).Seq
		if want := last[s.instID] + s.writes; got != want {
			t.Fatalf("[%s] seq %d; want %d (previous %d + %d state write(s))", s.name, got, want, last[s.instID], s.writes)
		}
		last[s.instID] = got
	})

	// A lost CAS writes nothing: a stale decision (wrong expected step) and a second breach claim.
	var id string
	if err := st.sdb.Get(&id, `SELECT id FROM sys_approval_instances WHERE tenant_id = ? AND status = 'approved' LIMIT 1`, tenant); err != nil {
		t.Fatalf("locate a finished instance: %v", err)
	}
	before := instState(t, st.sdb, tenant, id).Seq
	err := st.repo.DecideTaskTx(ctx, tenant, id, bpm.TaskGuard{ExpectSeq: int64(before)}, bpm.InstanceTransition{Status: bpm.StatusPending}, []byte(`[]`))
	if !errors.Is(err, bpm.ErrNotPending) {
		t.Fatalf("stale DecideTaskTx on a finished instance: err=%v; want a clean ErrNotPending", err)
	}
	won, err := st.repo.ClaimBreach(ctx, tenant, id, int64(before), []byte(`[]`))
	if err != nil || won {
		t.Fatalf("ClaimBreach on a finished instance: won=%v err=%v; want a clean loss", won, err)
	}
	if got := instState(t, st.sdb, tenant, id).Seq; got != before {
		t.Fatalf("a LOST CAS bumped seq %d → %d", before, got)
	}
}

// casView is everything a lost CAS must leave alone, one line per row: the instance's state and
// every seat of it (version included — a seat touched and put back would still bump it).
func casView(t *testing.T, db *sqlx.DB, tenant, id string) string {
	t.Helper()
	var inst string
	if err := db.Get(&inst, `SELECT CONCAT_WS('|', seq, status, current_step, escalated,
		IFNULL(current_approver_id, '-'), IFNULL(sla_due_at, '-'), MD5(CAST(history AS CHAR)))
		FROM sys_approval_instances WHERE tenant_id = ? AND id = ?`, tenant, id); err != nil {
		t.Fatalf("read instance %s: %v", id, err)
	}
	var seats []string
	if err := db.Select(&seats, `SELECT CONCAT_WS('|', id, node_id, entry_seq, assignee_id, status,
		IFNULL(outcome, '-'), IFNULL(decided_by, '-'), IFNULL(comment, '-'), IFNULL(breach_notified_at, '-'),
		IFNULL(completed_at, '-'), version)
		FROM sys_bpm_tasks WHERE tenant_id = ? AND instance_id = ? ORDER BY id`, tenant, id); err != nil {
		t.Fatalf("read seats of %s: %v", id, err)
	}
	return "instance " + inst + "\nseats " + strings.Join(seats, "\n      ")
}

// TestSeats_LostCASOnPendingWritesNoSeat (T250, seat half): "a lost CAS writes nothing anywhere"
// judged where there IS something to damage — a PENDING instance with an open, unstamped seat —
// and with a seat write that WOULD close and open seats had the CAS won. TestSeq_ only proves
// the seq half, on a finished instance with no open seat, where writing seats before the CAS
// check could not show. The positive controls at the end prove the same calls do write when
// they win, so a green run is the guard, not an inert seat write.
func TestSeats_LostCASOnPendingWritesNoSeat(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	st.createEntity(t, ctx, tenant, "seatcas", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	if _, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: "cas-2", EntityName: "seatcas", Trigger: "after_create",
		Steps: []ApprovalStepInput{
			{Name: "Trưởng phòng", ApproverType: "user", ApproverID: seatAp1, SLASeconds: 60},
			{Name: "Kế toán", ApproverType: "user", ApproverID: seatAp2, SLASeconds: 60},
		},
		IsActive: boolPtr(true), ActorID: seatSubmitter,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	rid, err := st.recordSvc.Create(ctx, bpmAdmin(seatSubmitter), tenant, seatSubmitter, "seatcas", map[string]any{"status": "draft"})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}
	st.drain(t)
	id := st.onlyPendingInstance(t, tenant, "seatcas", rid).ID
	if open := openSeats(seatsOf(t, st.sdb, tenant, id)); len(open) != 1 || open[0].BreachNotifiedAt != nil {
		t.Fatalf("setup: want one open unstamped seat, got %+v", open)
	}

	// What a winning step-1 approval would write: close s1 as ap1's approval, open s2 for ap2.
	advance := bpm.InstanceTransition{
		CurrentStep: 1, Status: bpm.StatusPending,
		CurrentApproverType: bpm.ApproverUser, CurrentApproverID: seatAp2,
		Seats: bpm.SeatWrite{Close: bpm.SeatApproved, Node: "s1", By: seatAp1, Comment: "stale",
			Open: []bpm.Seat{{NodeID: "s2", AssigneeType: bpm.ApproverUser, AssigneeID: seatAp2}}},
	}
	// The CAS key is the seq (S3b): a lost write must leave the instance AND every seat untouched.
	seqNow := int64(instState(t, st.sdb, tenant, id).Seq)
	advanceWith := func(seq int64) func() (bool, error) {
		return func() (bool, error) {
			err := st.repo.DecideTaskTx(ctx, tenant, id, bpm.TaskGuard{ExpectSeq: seq}, advance, []byte(`[]`))
			if errors.Is(err, bpm.ErrConcurrent) {
				return false, nil
			}
			return err == nil, err
		}
	}
	lose := func(stage string, call func() (bool, error)) {
		t.Helper()
		before := casView(t, st.sdb, tenant, id)
		won, err := call()
		if err != nil || won {
			t.Fatalf("[%s] won=%v err=%v; want a clean loss", stage, won, err)
		}
		if after := casView(t, st.sdb, tenant, id); after != before {
			t.Fatalf("[%s] a LOST CAS wrote:\nbefore %s\nafter  %s", stage, before, after)
		}
	}

	lose("advance, stale seq", advanceWith(seqNow-1))
	lose("advance, seq from the future", advanceWith(seqNow+1))
	lose("claim, stale seq", func() (bool, error) {
		return st.repo.ClaimBreach(ctx, tenant, id, seqNow-1, []byte(`[]`))
	})

	// A pre-S3a binary (rolling deploy) claims the breach: escalated=1, seat left unstamped, seq
	// untouched. A second claim from this binary must lose AND leave that seat alone — the one
	// state where the seat stamp's own "breach_notified_at IS NULL" filter would not hide an
	// ungated stamp.
	if _, err := st.sdb.Exec(`UPDATE sys_approval_instances SET escalated = 1 WHERE tenant_id = ? AND id = ?`, tenant, id); err != nil {
		t.Fatalf("old-binary escalate: %v", err)
	}
	lose("claim, already escalated", func() (bool, error) {
		return st.repo.ClaimBreach(ctx, tenant, id, seqNow, []byte(`[]`))
	})

	// Positive controls: the very same calls, once their CAS matches, do write seats.
	if won, err := advanceWith(seqNow)(); err != nil || !won {
		t.Fatalf("control advance: won=%v err=%v; want a win", won, err)
	}
	seats := seatsOf(t, st.sdb, tenant, id)
	if open := openSeats(seats); len(seats) != 2 || seats[0].Status != "done" || len(open) != 1 || open[0].NodeID != "s2" {
		t.Fatalf("control advance: seats %+v; want s1 done + s2 open", seats)
	}
	if won, err := st.repo.ClaimBreach(ctx, tenant, id, seqNow+1, []byte(`[]`)); err != nil || !won {
		t.Fatalf("control claim: won=%v err=%v; want a win", won, err)
	}
	if open := openSeats(seatsOf(t, st.sdb, tenant, id)); len(open) != 1 || open[0].BreachNotifiedAt == nil {
		t.Fatalf("control claim: open seat %+v; want it stamped", open)
	}
}

// TestSeats_LongNoteStillDecides: a decision/delegation note has no length cap (only the 1 MiB
// request body) and before S3a lived only in the JSON history, so a long one went through. The
// seat keeps the note too; if its column were smaller than a note the old path accepted, strict
// mode would fail the whole transaction (1406) and the decision would be lost with a 500.
func TestSeats_LongNoteStillDecides(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	st.createEntity(t, ctx, tenant, "seatnote", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	if _, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: "long-note", EntityName: "seatnote", Trigger: "after_create",
		Steps: []ApprovalStepInput{
			{Name: "Trưởng phòng", ApproverType: "user", ApproverID: seatAp1},
			{Name: "Kế toán", ApproverType: "user", ApproverID: seatAp2},
		},
		IsActive: boolPtr(true), ActorID: seatSubmitter,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	rid, err := st.recordSvc.Create(ctx, bpmAdmin(seatSubmitter), tenant, seatSubmitter, "seatnote", map[string]any{"status": "draft"})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}
	st.drain(t)
	id := st.onlyPendingInstance(t, tenant, "seatnote", rid).ID

	// 22 000 × "ệ" = 66 000 bytes: past TEXT's 65 535, far under the body limit.
	long := strings.Repeat("ệ", 22000)
	if _, err := st.requester.Delegate(ctx, tenant, id, seatAp3, long, bpm.Actor{UserID: seatAp1}); err != nil {
		t.Fatalf("delegate with a %d-byte note: %v", len(long), err)
	}
	if _, err := st.approval.Decide(ctx, tenant, id, bpm.DecisionApprove, long, bpm.Actor{UserID: seatAp3}); err != nil {
		t.Fatalf("approve with a %d-byte note: %v", len(long), err)
	}
	if _, err := st.approval.Decide(ctx, tenant, id, bpm.DecisionReject, long, bpm.Actor{UserID: seatAp2}); err != nil {
		t.Fatalf("reject with a %d-byte note: %v", len(long), err)
	}
	if got := instState(t, st.sdb, tenant, id).Status; got != string(bpm.StatusRejected) {
		t.Fatalf("instance status %q; want rejected", got)
	}
	seats := seatsOf(t, st.sdb, tenant, id)
	if len(seats) != 3 {
		t.Fatalf("%d seats; want 3 (reassigned, approved, rejected)", len(seats))
	}
	for i, s := range seats {
		if s.Status != "done" || s.Comment == nil || *s.Comment != long {
			t.Fatalf("seat %d (%s/%s) lost its note: %d bytes kept", i, s.Status, str(s.Outcome), len(str(s.Comment)))
		}
	}
}

// ---------------------------------------------------------------------------------------------
// D-5: the projection current_approver_* is ENUM('user','role'). A step held by anything else
// (team pool, queue — S6a) must arm with a NULL projection, not die on error 1265 on the
// fail-open arming path (where the failure would be a log line and a record that never enters
// approval).
// ---------------------------------------------------------------------------------------------

// insertRawProcess writes a process row directly — the validator (rightly) refuses a team step
// until S6a; the engine must still not choke on one (a pack, a later binary, a hand edit).
func insertRawProcess(t *testing.T, db *sqlx.DB, tenant, entity, trigger string, steps string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := db.Exec(`INSERT INTO sys_approval_processes (id, tenant_id, name, entity_name, trigger_event, steps, is_active)
		VALUES (?, ?, ?, ?, ?, ?, 1)`, id, tenant, "raw-"+entity, entity, trigger, steps); err != nil {
		t.Fatalf("insert raw process: %v", err)
	}
	return id
}

func TestArm_TeamPoolSeat_ProjectionNull(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const team = "88888888-0000-0000-0000-000000000a08"
	st.createEntity(t, ctx, tenant, "seatteam", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	insertRawProcess(t, st.sdb, tenant, "seatteam", "after_create",
		`[{"id":"pool","name":"Ban duyệt","approverType":"team","approverId":"`+team+`","slaSeconds":600}]`)
	rid, err := st.recordSvc.Create(ctx, bpmAdmin(seatSubmitter), tenant, seatSubmitter, "seatteam", map[string]any{"status": "draft"})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}
	st.drain(t)
	if n := st.instanceCount(t, tenant, "seatteam", rid); n != 1 {
		t.Fatalf("team step armed %d instance(s); want 1 (the arm died on the ENUM projection?)", n)
	}
	inst := st.onlyPendingInstance(t, tenant, "seatteam", rid)
	row := instState(t, st.sdb, tenant, inst.ID)
	if row.ApproverType != nil || row.ApproverID != nil {
		t.Fatalf("team seat projected onto current_approver_*: %s/%s; want NULL/NULL (D-5)", str(row.ApproverType), str(row.ApproverID))
	}
	seats := seatsOf(t, st.sdb, tenant, inst.ID)
	if len(seats) != 1 || seats[0].Status != "open" || seats[0].AssigneeType != "team" || seats[0].AssigneeID != team || seats[0].NodeID != "pool" {
		t.Fatalf("seats %+v; want one open team seat on node pool", seats)
	}
}

func TestArm_SingleUserSeat_ProjectionFilled(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	st.createEntity(t, ctx, tenant, "seatuser", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	if _, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: "one-user", EntityName: "seatuser", Trigger: "after_create",
		Steps:    []ApprovalStepInput{{Name: "Trưởng phòng", ApproverType: "user", ApproverID: seatAp1}},
		IsActive: boolPtr(true), ActorID: seatSubmitter,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	rid, err := st.recordSvc.Create(ctx, bpmAdmin(seatSubmitter), tenant, seatSubmitter, "seatuser", map[string]any{"status": "draft"})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}
	st.drain(t)
	inst := st.onlyPendingInstance(t, tenant, "seatuser", rid)
	row := instState(t, st.sdb, tenant, inst.ID)
	if str(row.ApproverType) != "user" || str(row.ApproverID) != seatAp1 {
		t.Fatalf("single user seat projection %s/%s; want user/%s", str(row.ApproverType), str(row.ApproverID), seatAp1)
	}
	assertMirror(t, st.sdb, tenant, inst.ID, "arm-single-user")
}

// TestSeats_CrossTenantInvisible (T057): a seat always carries its instance's tenant, and no
// write path run under another tenant reaches it — not through the engine, not through the
// repository's own statements.
func TestSeats_CrossTenantInvisible(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const userB = "bbbb1111-0000-0000-0000-000000000a09"
	st.createEntity(t, ctx, o3aTenantA, "seatx", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	if _, err := st.approval.CreateProcess(ctx, o3aTenantA, CreateApprovalProcessInput{
		Name: "a-seat", EntityName: "seatx", Trigger: "after_create",
		Steps:    []ApprovalStepInput{{Name: "A", ApproverType: "user", ApproverID: seatAp1, SLASeconds: 1}},
		IsActive: boolPtr(true), ActorID: seatSubmitter,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	rid, err := st.recordSvc.Create(ctx, bpmAdmin(seatSubmitter), o3aTenantA, seatSubmitter, "seatx", map[string]any{"status": "draft"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	st.drain(t)
	idA := st.onlyPendingInstance(t, o3aTenantA, "seatx", rid).ID
	before := seatsOf(t, st.sdb, o3aTenantA, idA)
	if len(before) != 1 || before[0].TenantID != o3aTenantA {
		t.Fatalf("tenant A seat %+v", before)
	}

	// Tenant B — even an admin of B — names A's instance id on every path.
	adminB := bpm.Actor{UserID: userB, IsAdmin: true}
	if _, err := st.approval.Decide(ctx, o3aTenantB, idA, bpm.DecisionApprove, "", adminB); err == nil {
		t.Fatalf("tenant B decided tenant A's instance")
	}
	if _, err := st.requester.Delegate(ctx, o3aTenantB, idA, userB, "", adminB); !errors.Is(err, bpm.ErrInstanceNotFound) {
		t.Fatalf("tenant B delegate on A's instance: %v; want ErrInstanceNotFound", err)
	}
	if _, err := st.requester.Recall(ctx, o3aTenantB, idA, "", adminB); !errors.Is(err, bpm.ErrInstanceNotFound) {
		t.Fatalf("tenant B recall on A's instance: %v; want ErrInstanceNotFound", err)
	}
	// The repository's own statements, called with B's tenant and a seat write aimed at A.
	seqA := int64(instState(t, st.sdb, o3aTenantA, idA).Seq)
	err = st.repo.DecideTaskTx(ctx, o3aTenantB, idA, bpm.TaskGuard{ExpectSeq: seqA}, bpm.InstanceTransition{
		Status: bpm.StatusCancelled,
		Seats:  bpm.SeatWrite{Close: bpm.SeatRecalled, By: userB},
	}, []byte(`[]`))
	if !errors.Is(err, bpm.ErrInstanceNotFound) {
		t.Fatalf("DecideTaskTx under tenant B on A's id: err=%v; want ErrInstanceNotFound", err)
	}
	if won, err := st.repo.ClaimBreach(ctx, o3aTenantB, idA, seqA, []byte(`[]`)); err != nil || won {
		t.Fatalf("ClaimBreach under tenant B on A's id: won=%v err=%v", won, err)
	}

	after := seatsOf(t, st.sdb, o3aTenantA, idA)
	if len(after) != 1 || after[0].Status != "open" || after[0].BreachNotifiedAt != nil || after[0].AssigneeID != seatAp1 {
		t.Fatalf("tenant B's calls reached tenant A's seat: %+v", after)
	}
	var leaked int
	if err := st.sdb.Get(&leaked, `SELECT COUNT(*) FROM sys_bpm_tasks t JOIN sys_approval_instances i ON i.id = t.instance_id
		WHERE t.tenant_id <> i.tenant_id`); err != nil {
		t.Fatalf("count cross-tenant seats: %v", err)
	}
	if leaked != 0 {
		t.Fatalf("%d seat(s) carry a tenant other than their instance's", leaked)
	}
	var visibleToB int
	if err := st.sdb.Get(&visibleToB, `SELECT COUNT(*) FROM sys_bpm_tasks WHERE tenant_id = ?`, o3aTenantB); err != nil {
		t.Fatalf("count B seats: %v", err)
	}
	if visibleToB != 0 {
		t.Fatalf("tenant B has %d seat(s) though it never armed anything", visibleToB)
	}
}

// TestSeats_PreS3aWritesHealOnNextTouch (D-6, write side): a binary older than S3a — during a
// rolling deploy, or after deploy.sh rolls the code back on this schema — arms and moves instances
// without touching seats or seq. The next write of THIS binary must leave exactly the right open
// seat: a seatless instance gets one, a seat left open on a step the instance has left closes as
// superseded (decided by nobody), and a delegation of a seatless step does not invent a visit.
func TestSeats_PreS3aWritesHealOnNextTouch(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	st.createEntity(t, ctx, tenant, "seatold", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	proc, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: "old-binary", EntityName: "seatold", Trigger: "after_create",
		Steps: []ApprovalStepInput{
			{Name: "A", ApproverType: "user", ApproverID: seatAp1},
			{Name: "B", ApproverType: "user", ApproverID: seatAp2},
		},
		IsActive: boolPtr(true), ActorID: seatSubmitter,
	})
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	pin := `{"steps":[{"id":"s1","name":"A","approverType":"user","approverId":"` + seatAp1 + `"},{"id":"s2","name":"B","approverType":"user","approverId":"` + seatAp2 + `"}]}`

	// 1) Armed by an old binary: seq 0, no seat. Decided here: the next step's seat appears.
	seatless := plantInstance(t, st.sdb, legacyInstance{tenant: tenant, process: proc.ID, entity: "seatold",
		approverType: "user", approverID: seatAp1, snapshot: pin})
	if _, err := st.approval.Decide(ctx, tenant, seatless, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp1}); err != nil {
		t.Fatalf("decide a seatless instance: %v", err)
	}
	assertMirror(t, st.sdb, tenant, seatless, "seatless-decided")

	// 2) Armed by an old binary at step 0, then delegated here: the seat opens on the visit the
	// instance stood at (seq before this write = 0), not on a visit of its own.
	seatless2 := plantInstance(t, st.sdb, legacyInstance{tenant: tenant, process: proc.ID, entity: "seatold",
		approverType: "user", approverID: seatAp1, snapshot: pin})
	if _, err := st.requester.Delegate(ctx, tenant, seatless2, seatAp3, "", bpm.Actor{UserID: seatAp1}); err != nil {
		t.Fatalf("delegate a seatless instance: %v", err)
	}
	assertMirror(t, st.sdb, tenant, seatless2, "seatless-delegated")
	if open := openSeats(seatsOf(t, st.sdb, tenant, seatless2)); open[0].EntrySeq != 0 {
		t.Fatalf("delegated seatless seat entry_seq %d; want 0 (the visit it stood at)", open[0].EntrySeq)
	}

	// 3) Armed here (seat s1), then approved by an old binary (step 1, no seat write, no seq bump):
	// s1 is left open on a step the instance has left. Delegating s2 here supersedes it.
	rid, err := st.recordSvc.Create(ctx, bpmAdmin(seatSubmitter), tenant, seatSubmitter, "seatold", map[string]any{"status": "draft"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	st.drain(t)
	stale := st.onlyPendingInstance(t, tenant, "seatold", rid).ID
	if _, err := st.sdb.Exec(`UPDATE sys_approval_instances SET current_step = 1, current_approver_id = ?
		WHERE tenant_id = ? AND id = ?`, seatAp2, tenant, stale); err != nil {
		t.Fatalf("old-binary approve: %v", err)
	}
	if _, err := st.requester.Delegate(ctx, tenant, stale, seatAp3, "", bpm.Actor{UserID: seatAp2}); err != nil {
		t.Fatalf("delegate over a stale seat: %v", err)
	}
	assertMirror(t, st.sdb, tenant, stale, "stale-healed")
	seats := seatsOf(t, st.sdb, tenant, stale)
	var superseded int
	for _, s := range seats {
		if s.NodeID == "s1" {
			if s.Status != "cancelled" || str(s.Outcome) != "superseded" || s.DecidedBy != nil {
				t.Fatalf("stale s1 seat %+v; want cancelled/superseded, decided by nobody", s)
			}
			superseded++
		}
	}
	if superseded != 1 {
		t.Fatalf("seats %+v; want the stale s1 seat closed exactly once", seats)
	}
}

// ---------------------------------------------------------------------------------------------
// Migration A on a database that already holds pre-A rows. The migration is located by NAME
// (…_bpm_runtime_v2), not number, so a renumber at merge time does not break these tests.
// ---------------------------------------------------------------------------------------------

const migASuffix = "_bpm_runtime_v2"

func migFiles(t *testing.T, suffix string) []string {
	t.Helper()
	entries, err := os.ReadDir(o3aMigrationsDir)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), suffix) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func migAFile(t *testing.T, dir string) string {
	t.Helper()
	for _, f := range migFiles(t, "."+dir+".sql") {
		if strings.HasSuffix(f, migASuffix+"."+dir+".sql") {
			return f
		}
	}
	t.Fatalf("Migration A (%s.%s.sql) not found in %s", migASuffix, dir, o3aMigrationsDir)
	return ""
}

func execMigFile(t *testing.T, db *sqlx.DB, name string) error {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(o3aMigrationsDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	_, err = db.Exec(string(b))
	return err
}

// preMigADB is a throwaway database migrated to just BEFORE Migration A, so a test can plant the
// rows production holds today and then run A over them. Files are applied as one multi-statement
// Exec each (stored-procedure bodies intact), the same way ittest and the 000012 test do.
func preMigADB(t *testing.T) *sqlx.DB {
	t.Helper()
	admin, err := sqlx.Connect("mysql", o3aAdminDSN(t))
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping", err)
	}
	dbName := o3aUniqueDBName()
	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`"); err != nil {
		t.Fatalf("pre-drop: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("create db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
		_ = admin.Close()
	})
	dsn := o3aDSN(t, dbName)
	if !strings.Contains(dsn, "multiStatements") {
		if strings.Contains(dsn, "?") {
			dsn += "&multiStatements=true"
		} else {
			dsn += "?multiStatements=true"
		}
	}
	db, err := sqlx.Connect("mysql", dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", dbName, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	stop := migAFile(t, "up")
	for _, f := range migFiles(t, ".up.sql") {
		if f >= stop {
			break
		}
		if err := execMigFile(t, db, f); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	return db
}

// applyMigAAndRest runs Migration A and every migration after it (a later slice's schema must not
// be missing when the engine runs over this database).
func applyMigAAndRest(t *testing.T, db *sqlx.DB) {
	t.Helper()
	start := migAFile(t, "up")
	for _, f := range migFiles(t, ".up.sql") {
		if f < start {
			continue
		}
		if err := execMigFile(t, db, f); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
}

// legacyInstance plants a pending instance the way the pre-A binary left it.
type legacyInstance struct {
	id, tenant, process, entity, record string
	step                                int
	approverType, approverID            string // "" → NULL
	snapshot                            string // "" → NULL
	history                             string // "" → []
	due                                 *time.Time
	escalated                           bool
	status                              string // "" → pending
}

func plantInstance(t *testing.T, db *sqlx.DB, in legacyInstance) string {
	t.Helper()
	if in.id == "" {
		in.id = uuid.NewString()
	}
	if in.record == "" {
		in.record = uuid.NewString()
	}
	if in.history == "" {
		in.history = "[]"
	}
	if in.status == "" {
		in.status = "pending"
	}
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	if _, err := db.Exec(`INSERT INTO sys_approval_instances
		(id, tenant_id, process_id, process_snapshot, entity_name, record_id, current_step, status,
		 current_approver_type, current_approver_id, sla_due_at, escalated, history, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.id, in.tenant, in.process, nullable(in.snapshot), in.entity, in.record, in.step, in.status,
		nullable(in.approverType), nullable(in.approverID), in.due, in.escalated, in.history, seatSubmitter); err != nil {
		t.Fatalf("plant instance: %v", err)
	}
	return in.id
}

// TestMigrationA_UpDownUp_Idempotent (T048): up twice is harmless, down twice is harmless, up
// again rebuilds exactly the same seats; the pins the backfill wrote survive down (they are the
// only record of which chain those instances executed — see the down file's header).
func TestMigrationA_UpDownUp_Idempotent(t *testing.T) {
	db := preMigADB(t)
	const tenant = o3aTenantA
	proc := insertRawProcess(t, db, tenant, "quotes", "after_create",
		`[{"name":"A","approverType":"user","approverId":"`+seatAp1+`","slaSeconds":60},{"name":"B","approverType":"user","approverId":"`+seatAp2+`"}]`)
	legacy := plantInstance(t, db, legacyInstance{tenant: tenant, process: proc, entity: "quotes", approverType: "user", approverID: seatAp1})
	plantInstance(t, db, legacyInstance{tenant: tenant, process: proc, entity: "quotes", step: 1, approverType: "user", approverID: seatAp2,
		snapshot: `{"steps":[{"id":"s1","name":"A","approverType":"user","approverId":"` + seatAp1 + `"},{"id":"s2","name":"B","approverType":"user","approverId":"` + seatAp2 + `"}]}`})
	plantInstance(t, db, legacyInstance{tenant: tenant, process: proc, entity: "quotes", status: "approved", approverType: "user", approverID: seatAp1})

	up, down := migAFile(t, "up"), migAFile(t, "down")
	schema := func() (table, cols int) {
		t.Helper()
		if err := db.Get(&table, `SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_bpm_tasks'`); err != nil {
			t.Fatalf("table probe: %v", err)
		}
		if err := db.Get(&cols, `SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND
			((TABLE_NAME = 'sys_approval_instances' AND COLUMN_NAME IN ('seq','context','last_error','sla_paused_at','sla_remaining_seconds'))
			 OR (TABLE_NAME = 'sys_approval_processes' AND COLUMN_NAME IN ('graph','options')))`); err != nil {
			t.Fatalf("column probe: %v", err)
		}
		return table, cols
	}
	openSeatCount := func() int {
		t.Helper()
		var n int
		if err := db.Get(&n, `SELECT COUNT(*) FROM sys_bpm_tasks WHERE tenant_id = ? AND status = 'open'`, tenant); err != nil {
			t.Fatalf("count seats: %v", err)
		}
		return n
	}

	for i := 1; i <= 2; i++ {
		if err := execMigFile(t, db, up); err != nil {
			t.Fatalf("up #%d: %v", i, err)
		}
		if tbl, cols := schema(); tbl != 1 || cols != 7 {
			t.Fatalf("after up #%d: table=%d cols=%d; want 1/7", i, tbl, cols)
		}
		if n := openSeatCount(); n != 2 {
			t.Fatalf("after up #%d: %d open seats; want 2 (one per pending instance, never doubled)", i, n)
		}
	}
	var idx int
	if err := db.Get(&idx, `SELECT COUNT(DISTINCT INDEX_NAME) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE()
		AND TABLE_NAME = 'sys_bpm_tasks' AND INDEX_NAME IN ('idx_bt_inbox','idx_bt_claimed','idx_bt_visit','idx_bt_record','idx_bt_breach','idx_bt_done','idx_bt_process')`); err != nil {
		t.Fatalf("index probe: %v", err)
	}
	if idx != 7 {
		t.Fatalf("sys_bpm_tasks has %d of the 7 planned indexes", idx)
	}
	var notTenantFirst int
	if err := db.Get(&notTenantFirst, `SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE()
		AND TABLE_NAME = 'sys_bpm_tasks' AND INDEX_NAME <> 'PRIMARY' AND SEQ_IN_INDEX = 1 AND COLUMN_NAME <> 'tenant_id'`); err != nil {
		t.Fatalf("index lead probe: %v", err)
	}
	if notTenantFirst != 0 {
		t.Fatalf("%d sys_bpm_tasks index(es) do not lead with tenant_id", notTenantFirst)
	}

	for i := 1; i <= 2; i++ {
		if err := execMigFile(t, db, down); err != nil {
			t.Fatalf("down #%d: %v", i, err)
		}
		if tbl, cols := schema(); tbl != 0 || cols != 0 {
			t.Fatalf("after down #%d: table=%d cols=%d; want 0/0", i, tbl, cols)
		}
	}
	var pinned int
	if err := db.Get(&pinned, `SELECT COUNT(*) FROM sys_approval_instances WHERE id = ? AND process_snapshot IS NOT NULL`, legacy); err != nil {
		t.Fatalf("pin probe: %v", err)
	}
	if pinned != 1 {
		t.Fatalf("down un-pinned the backfilled instance — it would read the live process again")
	}

	if err := execMigFile(t, db, up); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n := openSeatCount(); n != 2 {
		t.Fatalf("after up→down→up: %d open seats; want 2", n)
	}
}

// TestMigrationA_SnapshotBackfillThenDeleteProcessStillDecides (T050): a pending instance from
// before 000065 (no pin) decided through the LIVE process — so a soft-deleted process made it
// undecidable (GetProcess filters deleted_at). The backfill pins it from the process row WITHOUT
// that filter; afterwards the Decider never needs the live process again.
func TestMigrationA_SnapshotBackfillThenDeleteProcessStillDecides(t *testing.T) {
	db := preMigADB(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	steps := `[{"name":"A","approverType":"user","approverId":"` + seatAp1 + `"},{"name":"B","approverType":"user","approverId":"` + seatAp2 + `"}]`
	deletedBefore := insertRawProcess(t, db, tenant, "quotes", "after_create", steps)
	deletedAfter := insertRawProcess(t, db, tenant, "quotes", "after_create", steps)
	if _, err := db.Exec(`UPDATE sys_approval_processes SET deleted_at = NOW(3) WHERE tenant_id = ? AND id = ?`, tenant, deletedBefore); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	early := plantInstance(t, db, legacyInstance{tenant: tenant, process: deletedBefore, entity: "quotes", approverType: "user", approverID: seatAp1})
	late := plantInstance(t, db, legacyInstance{tenant: tenant, process: deletedAfter, entity: "quotes", approverType: "user", approverID: seatAp1})

	applyMigAAndRest(t, db)
	if _, err := db.Exec(`UPDATE sys_approval_processes SET deleted_at = NOW(3) WHERE tenant_id = ? AND id = ?`, tenant, deletedAfter); err != nil {
		t.Fatalf("soft-delete after migration: %v", err)
	}

	repo := bpm.NewSQLRepository(db, nil)
	decider := bpm.NewDecider(repo, repo, repo, nil, nil)
	for name, id := range map[string]string{"process deleted before the migration": early, "process deleted after the migration": late} {
		inst, err := decider.Decide(ctx, tenant, id, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp1})
		if err != nil {
			t.Fatalf("%s: decide: %v", name, err)
		}
		if inst.Status != bpm.StatusPending || inst.CurrentStep != 1 || inst.CurrentApproverID != seatAp2 {
			t.Fatalf("%s: after step 1: %s@%d approver %s; want pending@1/%s", name, inst.Status, inst.CurrentStep, inst.CurrentApproverID, seatAp2)
		}
		assertMirror(t, db, tenant, id, name)
	}
}

// TestMigrationA_BackfillUsesPinnedNodeID (T055): a seat names the step its instance's PIN stands
// on. After S0 an admin may reorder or insert steps, so s{current_step+1} can name the wrong step
// for an instance started after that edit. It also carries the deadline basis S1 recorded.
func TestMigrationA_BackfillUsesPinnedNodeID(t *testing.T) {
	db := preMigADB(t)
	const tenant = o3aTenantA
	legacySteps := `[{"name":"A","approverType":"user","approverId":"` + seatAp1 + `"},{"name":"B","approverType":"user","approverId":"` + seatAp2 + `"},{"name":"C","approverType":"role","approverId":"` + seatRoleC + `"}]`
	proc := insertRawProcess(t, db, tenant, "quotes", "after_create", legacySteps)
	due := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)

	cases := []struct {
		name, wantNode, wantBasis string
		in                        legacyInstance
		wantBreach                bool
	}{
		{name: "reordered pin: step index 1 is s1", wantNode: "s1", wantBasis: "business", in: legacyInstance{
			step: 1, approverType: "user", approverID: seatAp1, due: &due,
			snapshot: `{"steps":[{"id":"s3","name":"C","approverType":"role","approverId":"` + seatRoleC + `","slaClock":"business"},{"id":"s1","name":"A","approverType":"user","approverId":"` + seatAp1 + `","slaClock":"business"}],"firstDueBasis":"wall_no_calendar"}`,
			history:  `[{"step":0,"action":"approved","actor":"x","slaBasis":"business","at":"2026-09-29T01:00:00Z"}]`,
		}},
		{name: "pre-S0 pin: positional id", wantNode: "s3", wantBasis: "wall_no_calendar", in: legacyInstance{
			step: 2, approverType: "role", approverID: seatRoleC, due: &due,
			snapshot: `{"steps":` + legacySteps + `,"firstDueBasis":"wall_no_calendar"}`,
			history:  `[{"step":0,"action":"approved","actor":"x","at":"2026-09-29T01:00:00Z"},{"step":1,"action":"escalated","actor":"","at":"2026-09-29T02:00:00Z"}]`,
		}},
		{name: "no pin (pre-000065): backfilled from the process", wantNode: "s2", wantBasis: "wall", wantBreach: true, in: legacyInstance{
			step: 1, approverType: "user", approverID: seatAp2, due: &due, escalated: true,
		}},
		{name: "no SLA: no basis", wantNode: "s1", in: legacyInstance{
			step: 0, approverType: "user", approverID: seatAp1,
			snapshot: `{"steps":` + legacySteps + `,"firstDueBasis":"business"}`,
		}},
	}
	ids := make([]string, len(cases))
	for i := range cases {
		in := cases[i].in
		in.tenant, in.process, in.entity = tenant, proc, "quotes"
		ids[i] = plantInstance(t, db, in)
	}
	noApprover := plantInstance(t, db, legacyInstance{tenant: tenant, process: proc, entity: "quotes"})
	finished := plantInstance(t, db, legacyInstance{tenant: tenant, process: proc, entity: "quotes", status: "rejected", approverType: "user", approverID: seatAp1})
	otherTenant := plantInstance(t, db, legacyInstance{tenant: o3aTenantB, process: proc, entity: "quotes", approverType: "user", approverID: seatAp1})

	applyMigAAndRest(t, db)

	for i, c := range cases {
		seats := seatsOf(t, db, tenant, ids[i])
		if len(seats) != 1 {
			t.Fatalf("%s: %d seats; want 1", c.name, len(seats))
		}
		s := seats[0]
		if s.NodeID != c.wantNode || s.Status != "open" || s.EntrySeq != 0 || s.Kind != "approval" {
			t.Fatalf("%s: seat %+v; want open node %s at entry_seq 0", c.name, s, c.wantNode)
		}
		if got := s.DueBasis; (c.wantBasis == "" && got != nil) || (c.wantBasis != "" && str(got) != c.wantBasis) {
			t.Fatalf("%s: due_basis %s; want %q", c.name, str(got), c.wantBasis)
		}
		if (s.BreachNotifiedAt != nil) != c.wantBreach {
			t.Fatalf("%s: breach_notified_at %v; want set=%v", c.name, s.BreachNotifiedAt, c.wantBreach)
		}
		if s.ProcessID != proc || s.EntityName != "quotes" || s.TenantID != tenant {
			t.Fatalf("%s: seat does not carry its instance's process/entity/tenant: %+v", c.name, s)
		}
		assertMirror(t, db, tenant, ids[i], c.name)
	}
	if n := len(seatsOf(t, db, tenant, noApprover)); n != 0 {
		t.Fatalf("an instance with no projected approver got %d seat(s); the D-6 fallback owns it", n)
	}
	if n := len(seatsOf(t, db, tenant, finished)); n != 0 {
		t.Fatalf("a finished instance got %d seat(s)", n)
	}
	if s := seatsOf(t, db, o3aTenantB, otherTenant); len(s) != 1 || s[0].TenantID != o3aTenantB {
		t.Fatalf("tenant B's pending instance seat %+v; want one seat in tenant B", s)
	}
}

// TestMigrationA_BackfilledSnapshotPinsWallClock (T055, D-9): an instance from before 000065 was
// timed on the wall clock and must finish on it — even though the live process it is pinned
// from was saved after S1 with slaClock "business". A pin the migration did NOT write is left
// exactly as it was.
func TestMigrationA_BackfilledSnapshotPinsWallClock(t *testing.T) {
	db := preMigADB(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	steps := `[{"id":"s1","name":"A","approverType":"user","approverId":"` + seatAp1 + `","slaSeconds":600,"slaClock":"business"},` +
		`{"id":"s2","name":"B","approverType":"user","approverId":"` + seatAp2 + `","slaSeconds":3600,"slaClock":"business"}]`
	proc := insertRawProcess(t, db, tenant, "quotes", "after_create", steps)
	legacy := plantInstance(t, db, legacyInstance{tenant: tenant, process: proc, entity: "quotes", approverType: "user", approverID: seatAp1})
	ownPin := `{"steps":` + steps + `,"firstDueBasis":"business"}`
	modern := plantInstance(t, db, legacyInstance{tenant: tenant, process: proc, entity: "quotes", approverType: "user", approverID: seatAp1, snapshot: ownPin})

	applyMigAAndRest(t, db)

	var raw json.RawMessage
	if err := db.Get(&raw, `SELECT process_snapshot FROM sys_approval_instances WHERE tenant_id = ? AND id = ?`, tenant, legacy); err != nil {
		t.Fatalf("read backfilled pin: %v", err)
	}
	var pin struct {
		Steps      []bpm.Step `json:"steps"`
		Backfilled bool       `json:"backfilled"`
	}
	if err := json.Unmarshal(raw, &pin); err != nil {
		t.Fatalf("decode backfilled pin: %v", err)
	}
	if !pin.Backfilled || len(pin.Steps) != 2 {
		t.Fatalf("backfilled pin %s; want backfilled=true with both steps", raw)
	}
	for i, s := range pin.Steps {
		if s.SLAClock != bpm.SLAClockWall || s.SLASeconds == 0 || s.ID == "" {
			t.Fatalf("backfilled step %d = %+v; want slaClock wall, SLA and id kept", i, s)
		}
	}
	var modernPin json.RawMessage
	if err := db.Get(&modernPin, `SELECT process_snapshot FROM sys_approval_instances WHERE tenant_id = ? AND id = ?`, tenant, modern); err != nil {
		t.Fatalf("read own pin: %v", err)
	}
	if strings.Contains(string(modernPin), `"wall"`) || strings.Contains(string(modernPin), "backfilled") {
		t.Fatalf("the migration rewrote a pin it did not write: %s", modernPin)
	}

	// Deciding step 1 opens step 2 on the WALL clock, although a calendar is wired and would answer.
	calc := &countingCalc{}
	repo := bpm.NewSQLRepository(db, nil)
	decider := bpm.NewDecider(repo, repo, repo, nil, nil).WithDeadlines(calc)
	start := time.Now().UTC()
	inst, err := decider.Decide(ctx, tenant, legacy, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp1})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if calc.calls != 0 {
		t.Fatalf("the business calendar was consulted %d time(s) for a pre-000065 instance", calc.calls)
	}
	if inst.SLADueAt == nil || inst.SLADueAt.Before(start.Add(time.Hour-time.Second)) || inst.SLADueAt.After(time.Now().UTC().Add(time.Hour+time.Second)) {
		t.Fatalf("step-2 due %v; want now + 1h on the wall clock", inst.SLADueAt)
	}
	if got := inst.History[len(inst.History)-1].SLABasis; got != bpm.SLABasisWall {
		t.Fatalf("decision recorded basis %q; want wall", got)
	}
	seats := seatsOf(t, db, tenant, legacy)
	if open := openSeats(seats); len(open) != 1 || str(open[0].DueBasis) != "wall" {
		t.Fatalf("step-2 seat %+v; want an open seat on the wall basis", open)
	}
}

// countingCalc is a DeadlineCalculator that would answer "business" — the test proves it is never asked.
type countingCalc struct{ calls int }

func (c *countingCalc) DueAt(_ context.Context, _, _ string, from time.Time, d time.Duration) (time.Time, bpm.SLABasis, error) {
	c.calls++
	return from.Add(100 * d), bpm.SLABasisBusiness, nil
}
