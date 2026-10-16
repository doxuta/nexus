//go:build integration

// specs/043 S3a2 — graph-form approval processes against real MySQL (T054, T059, T062):
//   - the API stores a graph beside steps '[]' (D-2: a binary older than S3a2 finds no step and
//     skips the process instead of misreading it);
//   - the engine arms, seats, lists and decides it exactly like the linear chain it equals today;
//   - a linear-only save cannot erase a stored graph (409 APPROVAL_PROCESS_V2_OVERWRITE) unless
//     forced — the guard is IN the UPDATE, so it holds against a concurrent graph save;
//   - an unchanged save is a success, not "process not found" (MySQL reports 0 affected rows).
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run 'TestGraphProcess_|TestUpdateProcess_'
package services

import (
	"context"
	"encoding/json"
	"testing"

	"nexus/platform/internal/bpm"
)

func graphNodes(nodes ...ApprovalNodeInput) *ApprovalGraphInput {
	return &ApprovalGraphInput{Nodes: nodes}
}

func graphNode(id, name, approverType, approverID string, sla int) ApprovalNodeInput {
	return ApprovalNodeInput{Kind: "approval", ApprovalStepInput: ApprovalStepInput{
		ID: id, Name: name, ApproverType: approverType, ApproverID: approverID, SLASeconds: sla,
	}}
}

type processColumns struct {
	Steps json.RawMessage  `db:"steps"`
	Graph *json.RawMessage `db:"graph"`
}

func processRow(t *testing.T, st *bpmStack, tenant, id string) processColumns {
	t.Helper()
	var r processColumns
	if err := st.sdb.Get(&r, `SELECT steps, graph FROM sys_approval_processes WHERE tenant_id = ? AND id = ?`, tenant, id); err != nil {
		t.Fatalf("read process %s: %v", id, err)
	}
	return r
}

func TestGraphProcess_StoredRunAndGuarded(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	st.createEntity(t, ctx, tenant, "graphq", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	g := graphNodes(
		graphNode("truong-phong", "Trưởng phòng", "user", seatAp1, 3600),
		graphNode("", "Ban giám đốc", "role", seatRoleC, 0),
	)
	proc, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: "graph", EntityName: "graphq", Trigger: "after_create", Graph: g, IsActive: boolPtr(true), ActorID: seatSubmitter,
	})
	if err != nil {
		t.Fatalf("CreateProcess graph: %v", err)
	}
	row := processRow(t, st, tenant, proc.ID)
	if string(row.Steps) != "[]" || row.Graph == nil {
		t.Fatalf("stored steps=%s graph=%v; want [] beside the graph", row.Steps, row.Graph)
	}
	var stored bpm.Graph
	if err := json.Unmarshal(*row.Graph, &stored); err != nil || len(stored.Nodes) != 2 || stored.Nodes[1].ID != "s2" {
		t.Fatalf("stored graph %s (%v); want 2 nodes, the second id assigned s2", *row.Graph, err)
	}
	if len(proc.Graph) == 0 {
		t.Fatalf("GET of the created process carries no graph")
	}

	// Run: arm → seat on the node id → inbox → decide → next node → decide → approved.
	rid, err := st.recordSvc.Create(ctx, bpmAdmin(seatSubmitter), tenant, seatSubmitter, "graphq", map[string]any{"status": "draft"})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}
	st.drain(t)
	inst := st.onlyPendingInstance(t, tenant, "graphq", rid)
	pinRow := instState(t, st.sdb, tenant, inst.ID)
	var pin struct {
		Steps []bpm.Step `json:"steps"`
		Graph *bpm.Graph `json:"graph"`
	}
	if err := json.Unmarshal(*pinRow.Snapshot, &pin); err != nil || pin.Steps == nil || len(pin.Steps) != 0 || pin.Graph == nil {
		t.Fatalf("pin %s (%v); want steps [] and the graph — an older binary must find no step", *pinRow.Snapshot, err)
	}
	if open := openSeats(seatsOf(t, st.sdb, tenant, inst.ID)); len(open) != 1 || open[0].NodeID != "truong-phong" || open[0].AssigneeID != seatAp1 {
		t.Fatalf("first seat %+v; want truong-phong held by ap1", open)
	}
	pending, err := st.approval.ListPending(ctx, tenant, bpm.Actor{UserID: seatAp1})
	if err != nil || len(pending) != 1 || pending[0].ID != inst.ID {
		t.Fatalf("ap1 inbox %v (%v); want the graph instance", instanceIDs(pending), err)
	}
	if _, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: seatAp1}); err != nil {
		t.Fatalf("approve node 1: %v", err)
	}
	if open := openSeats(seatsOf(t, st.sdb, tenant, inst.ID)); len(open) != 1 || open[0].NodeID != "s2" || open[0].AssigneeID != seatRoleC {
		t.Fatalf("second seat %+v; want s2 held by the role", open)
	}
	done, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: seatRoleUser, RoleIDs: []string{seatRoleC}})
	if err != nil || done.Status != bpm.StatusApproved {
		t.Fatalf("approve node 2: %v status %v; want approved", err, done)
	}

	// Guard: a linear-only save is refused and leaves the graph where it was.
	linear := UpdateApprovalProcessInput{
		Name: "graph", EntityName: "graphq", Trigger: "after_create",
		Steps: []ApprovalStepInput{{ApproverType: "user", ApproverID: seatAp1}}, IsActive: boolPtr(true), ActorID: seatSubmitter,
	}
	_, err = st.approval.UpdateProcess(ctx, tenant, proc.ID, linear)
	wantApprovalCode(t, err, "APPROVAL_PROCESS_V2_OVERWRITE")
	if after := processRow(t, st, tenant, proc.ID); after.Graph == nil || string(after.Steps) != "[]" {
		t.Fatalf("the refused save changed the row: steps=%s graph=%v", after.Steps, after.Graph)
	}

	// The same graph saved twice: the second changes nothing and must not read as "not found".
	same := UpdateApprovalProcessInput{Name: "graph", EntityName: "graphq", Trigger: "after_create", Graph: g, IsActive: boolPtr(true), ActorID: seatSubmitter}
	for i := 1; i <= 2; i++ {
		if _, err := st.approval.UpdateProcess(ctx, tenant, proc.ID, same); err != nil {
			t.Fatalf("graph save #%d: %v", i, err)
		}
	}

	// Forced: the linear form replaces the graph.
	linear.Force = true
	if _, err := st.approval.UpdateProcess(ctx, tenant, proc.ID, linear); err != nil {
		t.Fatalf("forced linear save: %v", err)
	}
	if after := processRow(t, st, tenant, proc.ID); after.Graph != nil || string(after.Steps) == "[]" {
		t.Fatalf("forced save left steps=%s graph=%v; want the linear chain and graph NULL", after.Steps, after.Graph)
	}
	// A linear process takes a graph without force (upgrading erases nothing), and another tenant
	// cannot touch it at all.
	if _, err := st.approval.UpdateProcess(ctx, tenant, proc.ID, same); err != nil {
		t.Fatalf("linear → graph: %v", err)
	}
	if _, err := st.repo.GetProcess(ctx, o3aTenantB, proc.ID); err != bpm.ErrProcessNotFound {
		t.Fatalf("tenant B read tenant A's process: %v", err)
	}
	if err := st.repo.UpdateProcess(ctx, o3aTenantB, proc.ID, bpm.UpdateProcessInput{
		Name: "x", EntityName: "graphq", Trigger: bpm.TriggerManual, Steps: []byte(`[]`), Force: true,
	}); err != bpm.ErrProcessNotFound {
		t.Fatalf("tenant B forced an update of tenant A's process: %v; want ErrProcessNotFound", err)
	}
	if after := processRow(t, st, tenant, proc.ID); after.Graph == nil {
		t.Fatalf("tenant B's call reached tenant A's graph")
	}
}

// TestUpdateProcess_UnchangedSaveIsNotNotFound: saving a linear process with nothing changed
// (Studio's "Lưu" twice) is a success. MySQL reports 0 affected rows for an unchanged row, and the
// update used to read that as "no such process" — 404 on a process that is right there.
func TestUpdateProcess_UnchangedSaveIsNotNotFound(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	st.createEntity(t, ctx, tenant, "graphsame", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	in := CreateApprovalProcessInput{Name: "same", EntityName: "graphsame", Trigger: "manual",
		Steps: []ApprovalStepInput{{ID: "s1", ApproverType: "user", ApproverID: seatAp1}}, IsActive: boolPtr(true), ActorID: seatSubmitter}
	proc, err := st.approval.CreateProcess(ctx, tenant, in)
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	up := UpdateApprovalProcessInput{Name: in.Name, EntityName: in.EntityName, Trigger: in.Trigger, Steps: in.Steps,
		IsActive: in.IsActive, ActorID: in.ActorID}
	for i := 1; i <= 2; i++ {
		if _, err := st.approval.UpdateProcess(ctx, tenant, proc.ID, up); err != nil {
			t.Fatalf("unchanged save #%d: %v", i, err)
		}
	}
	if _, err := st.approval.UpdateProcess(ctx, tenant, "00000000-0000-0000-0000-00000000dead", up); err == nil {
		t.Fatalf("an unknown id saved without error")
	} else {
		wantApprovalCode(t, err, "APPROVAL_PROCESS_NOT_FOUND")
	}
}
