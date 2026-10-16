//go:build integration

// specs/084 — field_update fanned out to every child record, END-TO-END on a real MySQL (wfWire:
// RecordService → AsyncDispatcher → WorkflowRecordMutator → dynrepo): a NON-ADMIN rep closes a
// class and every enrollment OF THAT CLASS closes — enrollments of another class and of another
// tenant do not — and a run over the budget stops, says how many it updated, and leaves the rest.
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -p 4 ./internal/services/ -run WorkflowChildren
package services

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/workflow"
)

func childrenWorld(t *testing.T) (st *wfStack, tenant, admin string) {
	t.Helper()
	st = wfWire(t)
	ctx := context.Background()
	tenant, admin = o3aTenantA, "11111111-0000-0000-0000-00000000a001"
	st.createEntity(t, ctx, tenant, "ch_classes", []FieldInput{
		{Name: "status", Label: "Trạng thái", FieldType: "picklist", Options: map[string]any{"values": []any{"open", "closed"}}},
	})
	st.createEntity(t, ctx, tenant, "ch_enrollments", []FieldInput{
		{Name: "status", Label: "Trạng thái", FieldType: "picklist", Options: map[string]any{"values": []any{"active", "closed"}}},
		{Name: "class", Label: "Lớp", FieldType: "relation", Options: map[string]any{"relatedEntity": "ch_classes"}},
	})
	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name: "close-class-closes-enrollments", EntityName: "ch_classes", Trigger: "after_update",
		Rules:   []WorkflowRuleInput{{Field: "status", Op: "changedTo", Value: "closed"}},
		Actions: []WorkflowActionInput{{Type: "field_update", ChildEntity: "ch_enrollments", ChildVia: "class", Field: "status", Value: "closed"}},
		ActorID: admin,
	}); err != nil {
		t.Fatalf("CreateWorkflow(children field_update): %v", err)
	}
	return st, tenant, admin
}

func enrollmentStatus(t *testing.T, st *wfStack, tenant, id string) string {
	t.Helper()
	var s string
	if err := st.sdb.Get(&s, "SELECT JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.status')) FROM obj_ch_enrollments WHERE tenant_id = ? AND id = ?", tenant, id); err != nil {
		t.Fatalf("read enrollment %s: %v", id, err)
	}
	return s
}

func TestWorkflowChildren_ClosingAClassClosesOnlyItsEnrollments(t *testing.T) {
	st, tenant, admin := childrenWorld(t)
	ctx := context.Background()
	const rep = "11111111-0000-0000-0000-00000000a002"
	repScope := RecordScope{UserID: rep, AccessLevel: "owner"}
	adminScope := RecordScope{IsAdmin: true}

	classA, err := st.recordSvc.Create(ctx, repScope, tenant, rep, "ch_classes", map[string]any{"status": "open"})
	if err != nil {
		t.Fatalf("rep creates class A: %v", err)
	}
	classB, err := st.recordSvc.Create(ctx, adminScope, tenant, admin, "ch_classes", map[string]any{"status": "open"})
	if err != nil {
		t.Fatalf("class B: %v", err)
	}
	var inA []string
	for i := 0; i < 3; i++ {
		id, err := st.recordSvc.Create(ctx, adminScope, tenant, admin, "ch_enrollments", map[string]any{"status": "active", "class": classA})
		if err != nil {
			t.Fatalf("enrollment in A: %v", err)
		}
		inA = append(inA, id)
	}
	inB, err := st.recordSvc.Create(ctx, adminScope, tenant, admin, "ch_enrollments", map[string]any{"status": "active", "class": classB})
	if err != nil {
		t.Fatalf("enrollment in B: %v", err)
	}
	// Precondition: the rep cannot edit the admin's enrollment by hand — that is why the rule exists.
	if err := st.recordSvc.Update(ctx, repScope, tenant, "ch_enrollments", inA[0], map[string]any{"status": "closed"}, nil); err == nil {
		t.Fatalf("precondition: the rep could edit the admin's enrollment directly — the test proves nothing")
	}
	// A tenant-B enrollment whose relation (forced past the write-path FK check) holds class A's id.
	store := dynrepo.New(st.sdb)
	foreign, err := store.InsertRecord(ctx, "ch_enrollments", o3aTenantB, admin, map[string]any{"status": "active", "class": classA}, nil)
	if err != nil {
		t.Fatalf("insert tenant-B enrollment: %v", err)
	}

	if err := st.recordSvc.Update(ctx, repScope, tenant, "ch_classes", classA, map[string]any{"status": "closed"}, nil); err != nil {
		t.Fatalf("rep closes class A: %v", err)
	}
	st.drain(t)

	for _, id := range inA {
		if got := enrollmentStatus(t, st, tenant, id); got != "closed" {
			t.Errorf("enrollment %s of the closed class = %q; want \"closed\"", id, got)
		}
	}
	if got := enrollmentStatus(t, st, tenant, inB); got != "active" {
		t.Errorf("enrollment of ANOTHER class = %q; want \"active\" — the rule fanned out too far", got)
	}
	if got := enrollmentStatus(t, st, o3aTenantB, foreign); got != "active" {
		t.Errorf("tenant-B enrollment = %q; want \"active\" — a tenant-A rule wrote another tenant's record", got)
	}
	var run struct{ Status, Result string }
	if err := st.sdb.Get(&run, "SELECT status AS `status`, CAST(actions_result AS CHAR) AS `result` FROM sys_workflow_runs WHERE tenant_id = ? ORDER BY started_at DESC LIMIT 1", tenant); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.Status != "success" {
		t.Errorf("run status = %q (%s); want success", run.Status, run.Result)
	}
}

func TestWorkflowChildren_OverTheBudgetStopsAndSaysHowManyWereUpdated(t *testing.T) {
	st, tenant, admin := childrenWorld(t)
	ctx := context.Background()
	adminScope := RecordScope{IsAdmin: true}
	class, err := st.recordSvc.Create(ctx, adminScope, tenant, admin, "ch_classes", map[string]any{"status": "open"})
	if err != nil {
		t.Fatal(err)
	}
	store := dynrepo.New(st.sdb)
	rows := make([]map[string]any, workflow.MaxChildUpdatesPerRun+1)
	for i := range rows {
		rows[i] = map[string]any{"status": "active", "class": class}
	}
	ids, err := store.InsertRecords(ctx, "ch_enrollments", tenant, admin, rows, nil)
	if err != nil {
		t.Fatalf("bulk insert: %v", err)
	}
	if len(ids) != len(rows) {
		t.Fatalf("inserted %d, want %d", len(ids), len(rows))
	}
	if err := st.recordSvc.Update(ctx, adminScope, tenant, "ch_classes", class, map[string]any{"status": "closed"}, nil); err != nil {
		t.Fatalf("close class: %v", err)
	}
	st.drain(t)

	var closed, active int
	if err := st.sdb.Get(&closed, "SELECT COUNT(*) FROM obj_ch_enrollments WHERE tenant_id = ? AND JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.status')) = 'closed'", tenant); err != nil {
		t.Fatal(err)
	}
	if err := st.sdb.Get(&active, "SELECT COUNT(*) FROM obj_ch_enrollments WHERE tenant_id = ? AND JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.status')) = 'active'", tenant); err != nil {
		t.Fatal(err)
	}
	if closed != workflow.MaxChildUpdatesPerRun || active != 1 {
		t.Fatalf("closed=%d active=%d; want exactly %d closed and 1 left (the budget)", closed, active, workflow.MaxChildUpdatesPerRun)
	}
	var run struct{ Status, Result string }
	if err := st.sdb.Get(&run, "SELECT status AS `status`, CAST(actions_result AS CHAR) AS `result` FROM sys_workflow_runs WHERE tenant_id = ? ORDER BY started_at DESC LIMIT 1", tenant); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.Status != "failed" {
		t.Errorf("run status = %q; want failed — a partial fan-out must not read as success", run.Status)
	}
	if want := fmt.Sprintf("đã sửa %d bản ghi", workflow.MaxChildUpdatesPerRun); !strings.Contains(run.Result, want) {
		t.Errorf("actions_result = %s; want it to name the count (%q)", run.Result, want)
	}
}
