//go:build integration

// specs/043 S11 — `approval-process-edit-vs-pending` (#67, D7 phương án a), proven on MySQL: a save
// or delete never touches hồ sơ already running; the admin is told how many still run an older
// version and may move them across — per row, after a dry run, by stable step id.
//
// Run with:
//
//	TEST_DATABASE_URL=... go test -tags=integration -race ./internal/services/ -run 'TestImpact_|TestRepin_|TestPublish_ReturnsImpact|TestDelete_Stays204'
package services

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/bpm"
)

func repinStep(id, name, approver string) ApprovalStepInput {
	return ApprovalStepInput{ID: id, Name: name, ApproverType: "user", ApproverID: approver, SLASeconds: 3600}
}

// repinProcess creates a two-step manual process (verAp1 then verAp2, 1h deadlines) and returns
// its id.
func repinProcess(t *testing.T, st *bpmStack, tenant, entity string) string {
	t.Helper()
	return verProcess(t, st, tenant, entity, repinStep("s1", "Trưởng phòng", verAp1), repinStep("s2", "Kế toán", verAp2))
}

// repinSave is the legacy "save = publish" of a new step list over process `id`.
func repinSave(t *testing.T, st *bpmStack, tenant, id, entity string, steps ...ApprovalStepInput) {
	t.Helper()
	if _, err := st.approval.UpdateProcess(context.Background(), tenant, id, verBody(entity, "ver-"+entity, steps...)); err != nil {
		t.Fatalf("UpdateProcess: %v", err)
	}
}

type repinSeat struct {
	NodeID   string  `db:"node_id"`
	Assignee string  `db:"assignee_id"`
	From     *string `db:"delegated_from"`
	Entry    int64   `db:"entry_seq"`
}

func openSeatRows(t *testing.T, db *sqlx.DB, tenant, instance string) []repinSeat {
	t.Helper()
	var rows []repinSeat
	if err := db.Select(&rows, `SELECT node_id, assignee_id, delegated_from, entry_seq FROM sys_bpm_tasks
		WHERE tenant_id = ? AND instance_id = ? AND status = 'open' ORDER BY node_id`, tenant, instance); err != nil {
		t.Fatalf("read seats: %v", err)
	}
	return rows
}

func repinOne(t *testing.T, st *bpmStack, tenant, process, instance string, dry bool) bpm.RepinResult {
	t.Helper()
	out, err := st.approval.RepinInstances(context.Background(), tenant, process, bpm.RepinInput{InstanceIDs: []string{instance}, DryRun: dry, By: verSubmitter})
	if err != nil {
		t.Fatalf("RepinInstances: %v", err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("results = %+v; want one", out.Results)
	}
	return out.Results[0]
}

// TestImpact_CountsByProcessVersionAndNode (T171): after a save, impact counts the pending hồ sơ
// that still run another version, per step; a version-less (NULL) one counts as older.
func TestImpact_CountsByProcessVersionAndNode(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	tenant, entity := o3aTenantA, "impactcnt"
	pid := repinProcess(t, st, tenant, entity)
	a := verSubmit(t, st, tenant, entity) // stays on s1
	b := verSubmit(t, st, tenant, entity)
	if _, err := st.approval.Decide(ctx, tenant, b.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: verAp1}); err != nil { // b → s2
		t.Fatalf("decide: %v", err)
	}

	// A finished hồ sơ on v1 is history, not impact: it must appear in no count.
	done := verSubmit(t, st, tenant, entity)
	for _, who := range []string{verAp1, verAp2} {
		if _, err := st.approval.Decide(ctx, tenant, done.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: who}); err != nil {
			t.Fatalf("decide to completion: %v", err)
		}
	}

	im, err := st.approval.ProcessImpact(ctx, tenant, pid)
	if err != nil {
		t.Fatalf("ProcessImpact: %v", err)
	}
	if im.PendingTotal != 2 || im.PendingOnOldDefinition != 0 {
		t.Fatalf("before any save: %+v; want 2 pending, 0 older", im)
	}

	repinSave(t, st, tenant, pid, entity, repinStep("s1", "Trưởng phòng", verAp1), repinStep("s2", "Kế toán", verAp2), repinStep("s3", "Giám đốc", verAp3))
	im, err = st.approval.ProcessImpact(ctx, tenant, pid)
	if err != nil {
		t.Fatalf("ProcessImpact: %v", err)
	}
	if im.Version != 2 || im.PendingTotal != 2 || im.PendingOnOldDefinition != 2 {
		t.Fatalf("after v2: %+v; want version 2, 2 pending, 2 older", im)
	}
	got := map[string]int{}
	for _, n := range im.ByNode {
		got[n.NodeID] = n.Count
		if n.Name == "" {
			t.Errorf("node %q has no name", n.NodeID)
		}
	}
	if got["s1"] != 1 || got["s2"] != 1 || len(got) != 2 {
		t.Fatalf("byNode = %+v; want s1:1 and s2:1", im.ByNode)
	}

	// A hồ sơ with no recorded version (armed before versions) is older too.
	if _, err := st.sdb.Exec(`UPDATE sys_approval_instances SET process_version = NULL WHERE tenant_id = ? AND id = ?`, tenant, a.ID); err != nil {
		t.Fatal(err)
	}
	moved := repinOne(t, st, tenant, pid, b.ID, false)
	if !moved.OK {
		t.Fatalf("repin b: %+v", moved)
	}
	im, _ = st.approval.ProcessImpact(ctx, tenant, pid)
	if im.PendingOnOldDefinition != 1 || len(im.ByNode) != 1 || im.ByNode[0].NodeID != "s1" {
		t.Fatalf("after moving b: %+v; want only the NULL-version hồ sơ left on s1", im)
	}

	// Another tenant's process id is simply not found.
	if _, err := st.approval.ProcessImpact(ctx, o3aTenantB, pid); err == nil {
		t.Fatal("tenant B read tenant A's impact")
	}
}

// TestPublish_ReturnsImpact (T171): publishing a draft answers with the number of hồ sơ it left on
// the old version; the legacy save exposes the same through SaveImpact (PUT's `impact`).
func TestPublish_ReturnsImpact(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	tenant, entity := o3aTenantA, "pubimpact"
	pid := repinProcess(t, st, tenant, entity)
	verSubmit(t, st, tenant, entity)
	verSubmit(t, st, tenant, entity)

	if _, err := st.approval.SaveDraft(ctx, tenant, pid, verBody(entity, "ver-"+entity, repinStep("s1", "Trưởng phòng", verAp1), repinStep("s2", "Kế toán", verAp2), repinStep("s3", "Giám đốc", verAp3))); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	res, err := st.approval.PublishDraft(ctx, tenant, pid, "thêm bước", verSubmitter)
	if err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}
	if res.Impact == nil || res.Impact.PendingOnOldDefinition != 2 || res.Version != 2 {
		t.Fatalf("publish impact = %+v (version %d); want 2 older", res.Impact, res.Version)
	}
	if im := st.approval.SaveImpact(ctx, tenant, pid); im == nil || im.PendingOnOldDefinition != 2 {
		t.Fatalf("SaveImpact = %+v; want 2 older", im)
	}
}

// TestRepin_MapsByNodeID_RebuildsSeats_KeepsDelegateAndDue (T172): v2 inserts a step in front and
// gives s2 to a new approver. The hồ sơ on s2 lands on s2 (index moved), the NEW approver holds it,
// the old one no longer does, the deadline is the one it had, a delegated hồ sơ keeps its delegate,
// and the seat stays in the same visit.
func TestRepin_MapsByNodeID_RebuildsSeats_KeepsDelegateAndDue(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	tenant, entity := o3aTenantA, "repinmap"
	pid := repinProcess(t, st, tenant, entity)
	plain := verSubmit(t, st, tenant, entity)
	delegated := verSubmit(t, st, tenant, entity)
	for _, id := range []string{plain.ID, delegated.ID} {
		if _, err := st.approval.Decide(ctx, tenant, id, bpm.DecisionApprove, "ok", bpm.Actor{UserID: verAp1}); err != nil {
			t.Fatalf("decide: %v", err)
		}
	}
	if _, err := st.requester.Delegate(ctx, tenant, delegated.ID, verAp3, "nghỉ phép", bpm.Actor{UserID: verAp2}); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	before, _ := st.repo.GetInstance(ctx, tenant, plain.ID)
	entryBefore := openSeatRows(t, st.sdb, tenant, plain.ID)[0].Entry

	repinSave(t, st, tenant, pid, entity, repinStep("s9", "Thẩm định", verAp3), repinStep("s1", "Trưởng phòng", verAp1), repinStep("s2", "Kế toán", verSubmitter))

	r := repinOne(t, st, tenant, pid, plain.ID, false)
	if !r.OK || r.FromNode != "s2" || r.ToNode != "s2" || r.ToVersion != 2 {
		t.Fatalf("repin = %+v; want ok s2→s2 to v2", r)
	}
	after, _ := st.repo.GetInstance(ctx, tenant, plain.ID)
	if after.CurrentStep != 2 || after.ProcessVersion != 2 || after.Definition.Version != 2 {
		t.Fatalf("after: step %d version %d/%d; want step 2 on v2", after.CurrentStep, after.ProcessVersion, after.Definition.Version)
	}
	if after.SLADueAt == nil || !after.SLADueAt.Equal(*before.SLADueAt) {
		t.Fatalf("deadline %v → %v; want it kept", before.SLADueAt, after.SLADueAt)
	}
	if after.Seq != before.Seq+1 {
		t.Fatalf("seq %d → %d; want +1", before.Seq, after.Seq)
	}
	seats := openSeatRows(t, st.sdb, tenant, plain.ID)
	if len(seats) != 1 || seats[0].NodeID != "s2" || seats[0].Assignee != verSubmitter || seats[0].Entry != entryBefore {
		t.Fatalf("seats = %+v; want one s2 seat held by the new approver in the same visit (entry %d)", seats, entryBefore)
	}
	last := after.History[len(after.History)-1]
	if last.Action != bpm.HistoryRepinned || last.FromVersion != 1 || last.Version != 2 || last.Node != "s2" {
		t.Fatalf("history tail = %+v", last)
	}
	// The old approver can no longer decide; the new one can.
	if _, err := st.approval.Decide(ctx, tenant, plain.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: verAp2}); err == nil {
		t.Fatal("the previous approver still decides a repinned step")
	}
	if _, err := st.approval.Decide(ctx, tenant, plain.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: verSubmitter}); err != nil {
		t.Fatalf("new approver cannot decide: %v", err)
	}

	// The delegated one keeps its delegate.
	if r := repinOne(t, st, tenant, pid, delegated.ID, false); !r.OK {
		t.Fatalf("repin delegated: %+v", r)
	}
	seats = openSeatRows(t, st.sdb, tenant, delegated.ID)
	if len(seats) != 1 || seats[0].Assignee != verAp3 || seats[0].From == nil || *seats[0].From != verAp2 {
		t.Fatalf("delegated seats = %+v; want the delegate kept", seats)
	}
	if _, err := st.approval.Decide(ctx, tenant, delegated.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: verAp3}); err != nil {
		t.Fatalf("delegate cannot decide after repin: %v", err)
	}
}

// TestRepin_NodeMissingRefusedPerInstance (T173): a step v2 deleted is refused for the hồ sơ standing
// on it — and, a fresh step later getting a NEW id, never mapped onto it — while the others move.
func TestRepin_NodeMissingRefusedPerInstance(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	tenant, entity := o3aTenantA, "repinmiss"
	pid := repinProcess(t, st, tenant, entity)
	onS1 := verSubmit(t, st, tenant, entity)
	onS2 := verSubmit(t, st, tenant, entity)
	if _, err := st.approval.Decide(ctx, tenant, onS2.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: verAp1}); err != nil {
		t.Fatal(err)
	}
	repinSave(t, st, tenant, pid, entity, repinStep("s1", "Trưởng phòng", verAp1)) // s2 deleted
	repinSave(t, st, tenant, pid, entity, repinStep("s1", "Trưởng phòng", verAp1), ApprovalStepInput{Name: "Bước mới", ApproverType: "user", ApproverID: verAp3})

	out, err := st.approval.RepinInstances(ctx, tenant, pid, bpm.RepinInput{DryRun: false, By: verSubmitter})
	if err != nil {
		t.Fatalf("RepinInstances: %v", err)
	}
	byID := map[string]bpm.RepinResult{}
	for _, r := range out.Results {
		byID[r.InstanceID] = r
	}
	if r := byID[onS1.ID]; !r.OK {
		t.Fatalf("hồ sơ on s1: %+v; want moved", r)
	}
	if r := byID[onS2.ID]; r.OK || r.Reason != bpm.RepinNodeMissing {
		t.Fatalf("hồ sơ on the deleted s2: %+v; want %s", r, bpm.RepinNodeMissing)
	}
	left, _ := st.repo.GetInstance(ctx, tenant, onS2.ID)
	if left.ProcessVersion != 1 || left.CurrentStep != 1 {
		t.Fatalf("the refused hồ sơ moved: version %d step %d", left.ProcessVersion, left.CurrentStep)
	}
	// ...and it is still decidable by its own approver, on the old version.
	if _, err := st.approval.Decide(ctx, tenant, onS2.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: verAp2}); err != nil {
		t.Fatalf("refused hồ sơ no longer decidable: %v", err)
	}
	// Another process's instance, or a made-up id, is "not found" — not moved, not leaked.
	other := repinProcess(t, st, tenant, "repinmiss2")
	foreign := verSubmit(t, st, tenant, "repinmiss2")
	_ = other
	if r := repinOne(t, st, tenant, pid, foreign.ID, false); r.OK || r.Reason != bpm.RepinNotFound {
		t.Fatalf("another process's hồ sơ: %+v; want %s", r, bpm.RepinNotFound)
	}
}

// TestRepin_DryRunEqualsApply (T174): the dry run reports exactly what the apply then does and
// writes nothing.
func TestRepin_DryRunEqualsApply(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	tenant, entity := o3aTenantA, "repindry"
	pid := repinProcess(t, st, tenant, entity)
	a := verSubmit(t, st, tenant, entity)
	b := verSubmit(t, st, tenant, entity)
	if _, err := st.approval.Decide(ctx, tenant, b.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: verAp1}); err != nil {
		t.Fatal(err)
	}
	repinSave(t, st, tenant, pid, entity, repinStep("s1", "Trưởng phòng", verAp1)) // b's step is gone

	seqBefore, _ := st.repo.GetInstance(ctx, tenant, a.ID)
	dry, err := st.approval.RepinInstances(ctx, tenant, pid, bpm.RepinInput{DryRun: true, By: verSubmitter})
	if err != nil {
		t.Fatal(err)
	}
	if seqNow, _ := st.repo.GetInstance(ctx, tenant, a.ID); seqNow.Seq != seqBefore.Seq || seqNow.ProcessVersion != 1 {
		t.Fatal("a dry run wrote")
	}
	real, err := st.approval.RepinInstances(ctx, tenant, pid, bpm.RepinInput{DryRun: false, By: verSubmitter})
	if err != nil {
		t.Fatal(err)
	}
	dj, _ := json.Marshal(dry.Results)
	rj, _ := json.Marshal(real.Results)
	if string(dj) != string(rj) {
		t.Fatalf("dry run %s\napply   %s", dj, rj)
	}
	if again, _ := st.approval.RepinInstances(ctx, tenant, pid, bpm.RepinInput{DryRun: false, By: verSubmitter}); len(again.Results) != 1 { // only b is still older
		t.Fatalf("second apply = %+v; want only the refused hồ sơ as a candidate", again.Results)
	}
}

// TestRepin_ConcurrentDecisionLoses (T174): a decision racing a repin never leaves a half-moved
// hồ sơ. Whichever wins, the pin, the version column, the step and the single open seat agree.
func TestRepin_ConcurrentDecisionLoses(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	tenant, entity := o3aTenantA, "repinrace"
	pid := repinProcess(t, st, tenant, entity)
	const rounds = 12
	insts := make([]*bpm.Instance, rounds)
	for i := range insts {
		insts[i] = verSubmit(t, st, tenant, entity) // all armed on v1
	}
	repinSave(t, st, tenant, pid, entity, repinStep("s0", "Mới", verAp3), repinStep("s1", "Trưởng phòng", verAp1), repinStep("s2", "Kế toán", verAp2))
	for i, inst := range insts {
		var wg sync.WaitGroup
		var repinRes bpm.RepinResult
		wg.Add(2)
		go func() {
			defer wg.Done()
			out, err := st.approval.RepinInstances(ctx, tenant, pid, bpm.RepinInput{InstanceIDs: []string{inst.ID}, By: verSubmitter, DryRun: false})
			if err != nil {
				t.Errorf("repin: %v", err)
				return
			}
			repinRes = out.Results[0]
		}()
		go func() {
			defer wg.Done()
			// err is expected on the loser; the invariants below are what matter.
			_, _ = st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: verAp1})
		}()
		wg.Wait()

		got, err := st.repo.GetInstance(ctx, tenant, inst.ID)
		if err != nil {
			t.Fatal(err)
		}
		var colVersion *int
		if err := st.sdb.Get(&colVersion, `SELECT process_version FROM sys_approval_instances WHERE tenant_id = ? AND id = ?`, tenant, inst.ID); err != nil {
			t.Fatal(err)
		}
		if repinRes.OK && (got.Definition.Version != 2 || colVersion == nil || *colVersion != 2) {
			t.Fatalf("round %d: repin ok but pin v%d column %v", i, got.Definition.Version, colVersion)
		}
		if !repinRes.OK && repinRes.Reason != bpm.RepinConflict && repinRes.Reason != bpm.RepinNodeMissing {
			t.Fatalf("round %d: repin %+v; want ok or a clean refusal", i, repinRes)
		}
		if got.Status == bpm.StatusPending {
			seats := openSeatRows(t, st.sdb, tenant, inst.ID)
			node, _ := got.Definition.StepRef(got.CurrentStep)
			if len(seats) != 1 || seats[0].NodeID != node {
				t.Fatalf("round %d: seats %+v on step %d (%s) of v%d", i, seats, got.CurrentStep, node, got.Definition.Version)
			}
		}
	}
}

// TestDelete_Stays204_ImpactBeforeDelete_PendingStillDecidable (T175): the delete itself is
// unchanged (no error, no count in its answer); the number is read first through impact; a hồ sơ
// already running is still decided by its approver afterwards; impact of a deleted process 404s.
func TestDelete_Stays204_ImpactBeforeDelete_PendingStillDecidable(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	tenant, entity := o3aTenantA, "repindel"
	pid := repinProcess(t, st, tenant, entity)
	inst := verSubmit(t, st, tenant, entity)

	im, err := st.approval.ProcessImpact(ctx, tenant, pid)
	if err != nil || im.PendingTotal != 1 {
		t.Fatalf("impact before delete = %+v, %v; want 1 pending", im, err)
	}
	if err := st.approval.DeleteProcess(ctx, tenant, pid); err != nil {
		t.Fatalf("DeleteProcess: %v", err)
	}
	if _, err := st.approval.ProcessImpact(ctx, tenant, pid); err == nil {
		t.Fatal("impact of a deleted process answered")
	}
	if _, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: verAp1}); err != nil {
		t.Fatalf("pending hồ sơ not decidable after the process was deleted: %v", err)
	}
}

// TestRepin_LegacyNullSnapshot (T175): a hồ sơ with no pin (NULL snapshot and version) is mapped by
// the step number it stands on and gets pinned by the repin.
func TestRepin_LegacyNullSnapshot(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	tenant, entity := o3aTenantA, "repinnull"
	pid := repinProcess(t, st, tenant, entity)
	inst := verSubmit(t, st, tenant, entity)
	if _, err := st.sdb.Exec(`UPDATE sys_approval_instances SET process_snapshot = NULL, process_version = NULL WHERE tenant_id = ? AND id = ?`, tenant, inst.ID); err != nil {
		t.Fatal(err)
	}
	repinSave(t, st, tenant, pid, entity, repinStep("s1", "Trưởng phòng", verAp1), repinStep("s2", "Kế toán", verAp2), repinStep("s3", "Giám đốc", verAp3))
	r := repinOne(t, st, tenant, pid, inst.ID, false)
	if !r.OK || r.FromNode != "s1" || r.ToNode != "s1" || r.FromVersion != 0 {
		t.Fatalf("repin = %+v; want ok s1→s1 from the unknown version", r)
	}
	got, _ := st.repo.GetInstance(ctx, tenant, inst.ID)
	if got.Definition == nil || got.Definition.Version != 2 || len(got.Definition.Chain()) != 3 {
		t.Fatalf("pin after repin = %+v; want the 3-step v2", got.Definition)
	}
}

// TestRepin_ReenqueuesSLADue (T292): a repin raises seq and keeps the deadline, so the durable
// breach job for the new seq must exist (the job of the old seq is ignored by its handler).
func TestRepin_ReenqueuesSLADue(t *testing.T) {
	st := bpmWire(t)
	st.withDueJobs()
	ctx := context.Background()
	tenant, entity := o3aTenantA, "repinjob"
	pid := repinProcess(t, st, tenant, entity)
	inst := verSubmit(t, st, tenant, entity)
	repinSave(t, st, tenant, pid, entity, repinStep("s1", "Trưởng phòng", verAp1), repinStep("s2", "Kế toán", verAp2), repinStep("s3", "Giám đốc", verAp3))

	if r := repinOne(t, st, tenant, pid, inst.ID, false); !r.OK {
		t.Fatalf("repin: %+v", r)
	}
	got, _ := st.repo.GetInstance(ctx, tenant, inst.ID)
	if got.SLADueAt == nil {
		t.Fatal("fixture lost its deadline")
	}
	_, seqs := dueJobsOf(t, st, tenant, inst.ID)
	n := 0
	for _, q := range seqs {
		if q == got.Seq {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d jobs for seq %d (all %v); want exactly one", n, got.Seq, seqs)
	}
}

// TestRepin_CursorConvergesPastRefused: more refused hồ sơ than one batch must not starve the
// movable one behind them — a loop on nextAfter visits every candidate once and ends at remaining 0.
func TestRepin_CursorConvergesPastRefused(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	tenant, entity := o3aTenantA, "repincursor"
	pid := repinProcess(t, st, tenant, entity)
	ids := map[string]bool{}
	for i := 0; i < bpm.MaxRepinBatch; i++ {
		inst := verSubmit(t, st, tenant, entity)
		if _, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: verAp1}); err != nil {
			t.Fatal(err)
		}
		ids[inst.ID] = true
	}
	movable := verSubmit(t, st, tenant, entity)
	ids[movable.ID] = true
	repinSave(t, st, tenant, pid, entity, repinStep("s1", "Trưởng phòng", verAp1)) // s2 gone: 200 refused

	seen, after := map[string]bool{}, ""
	for call := 0; ; call++ {
		if call > 3 {
			t.Fatal("cursor loop does not converge")
		}
		out, err := st.approval.RepinInstances(ctx, tenant, pid, bpm.RepinInput{DryRun: false, By: verSubmitter, After: after})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range out.Results {
			if seen[r.InstanceID] {
				t.Fatalf("%s visited twice", r.InstanceID)
			}
			seen[r.InstanceID] = true
		}
		if out.NextAfter == "" {
			if out.Remaining != 0 {
				t.Fatalf("remaining %d with no cursor", out.Remaining)
			}
			break
		}
		after = out.NextAfter
	}
	if len(seen) != len(ids) {
		t.Fatalf("visited %d of %d", len(seen), len(ids))
	}
	if got, _ := st.repo.GetInstance(ctx, tenant, movable.ID); got.ProcessVersion != 2 {
		t.Fatalf("movable hồ sơ still on v%d", got.ProcessVersion)
	}
}
