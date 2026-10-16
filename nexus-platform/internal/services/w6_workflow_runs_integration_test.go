//go:build integration

// W6 — workflow run history + WF-01 defense-in-depth, exercised END-TO-END over a freshly
// migrated throwaway MySQL DB (o3aSetup runs ALL migrations incl. 000032 sys_workflow_runs via
// the real golang-migrate CLI). The composition mirrors cmd/server/main.go exactly:
//
//	RecordService.{Create,Update}            (after-commit Emit — sync path, ZERO added latency)
//	  → workflow.AsyncDispatcher             (async worker; W6 WithRunRecorder)
//	    → workflow.SQLRepository.InsertRun   (real sys_workflow_runs INSERT, fail-open)
//	    → services.WorkflowRecordMutator     (WF-01 WithCatalog execution guard)
//
// Proves:
//   - every (event × rule) evaluation lands a run row: success AND skipped(condition-false)
//     AND failed — with tenant/workflow/trigger/record ids and per-action actions_result;
//   - ListWorkflowRuns pages newest-first, tenant-pinned;
//   - WF-01 defense in depth: a PRE-EXISTING bad rule (seeded through the repo, bypassing the
//     config-time guard) targeting an auto_number field is BLOCKED at execution — the assigned
//     number survives byte-identical and the run is recorded as failed;
//   - the dry-run endpoint mutates NOTHING against a real database.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run TestW6Runs
package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// w6Stack is the run-recording composition one W6 integration test drives.
type w6Stack struct {
	sdb         *sqlx.DB
	entitySvc   *EntityService
	recordSvc   *RecordService
	workflowSvc *WorkflowService
	wfRepo      *workflow.SQLRepository
	dispatcher  *workflow.AsyncDispatcher
}

// w6Wire mirrors main.go's W6 wiring: run recorder on the dispatcher, WF-01 catalog on the
// mutator, run reader + record reader on the admin service.
func w6Wire(t *testing.T) *w6Stack {
	t.Helper()
	gdb, sdb := o3aSetup(t)

	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	mc := NewMetaCache(metaRepo, DefaultMetaTTL)

	entitySvc := NewEntityService(dynRepo, metaRepo).WithMetaCache(mc)

	wfRepo := workflow.NewSQLRepository(sdb)
	wfCache := workflow.NewCachedLoader(wfRepo, 500*time.Millisecond)
	wfMutator := NewWorkflowRecordMutator(dynRepo).WithCatalog(mc)
	dispatcher := workflow.NewAsyncDispatcher(
		workflow.DispatcherConfig{Buffer: 64, Workers: 1},
		wfCache, wfMutator, wfRepo, nil,
	).WithRunRecorder(wfRepo)
	workflowSvc := NewWorkflowService(wfRepo).WithEntityChecker(mc).WithCache(wfCache).
		WithRunReader(wfRepo).WithRecordReader(dynRepo)

	recordSvc := NewRecordService(mc, dynRepo).WithEngine(dispatcher).WithSequenceClaimer(dynRepo)

	return &w6Stack{
		sdb: sdb, entitySvc: entitySvc, recordSvc: recordSvc,
		workflowSvc: workflowSvc, wfRepo: wfRepo, dispatcher: dispatcher,
	}
}

func (s *w6Stack) createEntity(t *testing.T, ctx context.Context, tenant, name string, fields []FieldInput) {
	t.Helper()
	if _, _, err := s.entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name: name, Label: name, Fields: fields,
	}); err != nil {
		t.Fatalf("CreateEntity %s: %v", name, err)
	}
}

func (s *w6Stack) drain(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.dispatcher.Shutdown(ctx); err != nil {
		t.Fatalf("dispatcher drain: %v", err)
	}
}

// runRow is the raw sys_workflow_runs projection the assertions read.
type runRow struct {
	WorkflowID    string  `db:"workflow_id"`
	TriggerType   string  `db:"trigger_type"`
	RecordID      *string `db:"record_id"`
	Status        string  `db:"status"`
	Error         *string `db:"error"`
	ActionsResult *string `db:"actions_result"`
}

func (s *w6Stack) runRows(t *testing.T, tenant string) []runRow {
	t.Helper()
	var rows []runRow
	if err := s.sdb.Select(&rows,
		`SELECT workflow_id, trigger_type, record_id, status, error, actions_result
		 FROM sys_workflow_runs WHERE tenant_id = ? ORDER BY started_at ASC, id ASC`, tenant); err != nil {
		t.Fatalf("read sys_workflow_runs: %v", err)
	}
	return rows
}

func TestW6Runs_SuccessAndSkippedRecorded(t *testing.T) {
	st := w6Wire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "66666666-0000-0000-0000-000000000001"

	st.createEntity(t, ctx, tenant, "w6deals", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
		{Name: "stage", Label: "Stage", FieldType: "text"},
	})
	created, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "won-closes",
		EntityName: "w6deals",
		Trigger:    "after_update",
		Rules:      []WorkflowRuleInput{{Field: "status", Op: "eq", Value: "won"}},
		Actions:    []WorkflowActionInput{{Type: "field_update", Field: "stage", Value: "closed"}},
		ActorID:    user,
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	// AI-APPLY-IDEMPOTENT (3.1): a second create with the SAME name must 409, never mint a
	// double-firing twin.
	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name: "won-closes", EntityName: "w6deals", Trigger: "after_update",
		Actions: []WorkflowActionInput{{Type: "field_update", Field: "stage", Value: "x"}},
		ActorID: user,
	}); err == nil {
		t.Fatal("duplicate workflow name must be rejected")
	} else {
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "WORKFLOW_ALREADY_EXISTS" {
			t.Fatalf("want WORKFLOW_ALREADY_EXISTS, got %v", err)
		}
	}

	id, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "w6deals", map[string]any{"status": "open", "stage": "new"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Update 1: condition FALSE → skipped run. Update 2: condition TRUE → success run.
	if err := st.recordSvc.Update(ctx, wfAdmin(user), tenant, "w6deals", id, map[string]any{"status": "lost"}, nil); err != nil {
		t.Fatalf("Update lost: %v", err)
	}
	if err := st.recordSvc.Update(ctx, wfAdmin(user), tenant, "w6deals", id, map[string]any{"status": "won"}, nil); err != nil {
		t.Fatalf("Update won: %v", err)
	}
	st.drain(t)

	rows := st.runRows(t, tenant)
	// Exactly two runs: the automation's own field_update writes through the RecordMutator →
	// dynrepo.UpdateRecord (repo layer) and NEVER re-Emits — otherwise every matched rule would
	// be an infinite loop. So no third evaluation exists (reviewer-verified; tightened from ≥2).
	if len(rows) != 2 {
		t.Fatalf("expected exactly 2 run rows (skipped + success, no re-Emit), got %d", len(rows))
	}
	first, second := rows[0], rows[1]
	if first.Status != "skipped" || first.WorkflowID != created.ID || first.RecordID == nil || *first.RecordID != id {
		t.Errorf("row[0] = %+v; want skipped run for record %s", first, id)
	}
	if first.TriggerType != "after_update" {
		t.Errorf("row[0].trigger = %s; want after_update", first.TriggerType)
	}
	if second.Status != "success" || second.RecordID == nil || *second.RecordID != id {
		t.Errorf("row[1] = %+v; want success run for record %s", second, id)
	}
	if second.ActionsResult == nil {
		t.Fatalf("success run must carry actions_result")
	}
	var results []workflow.ActionResult
	if err := json.Unmarshal([]byte(*second.ActionsResult), &results); err != nil {
		t.Fatalf("actions_result JSON: %v", err)
	}
	if len(results) != 1 || !results[0].OK || results[0].Field != "stage" {
		t.Errorf("actions_result = %+v; want one ok field_update on stage", results)
	}

	// Runs API: newest-first, tenant-pinned, paged.
	list, total, err := st.workflowSvc.ListWorkflowRuns(ctx, tenant, created.ID, 50, 0)
	if err != nil {
		t.Fatalf("ListWorkflowRuns: %v", err)
	}
	if total != len(rows) || len(list) != len(rows) {
		t.Errorf("ListWorkflowRuns total=%d len=%d; want %d", total, len(list), len(rows))
	}
	for i := 1; i < len(list); i++ {
		if list[i].StartedAt.After(list[i-1].StartedAt) {
			t.Errorf("runs not newest-first at index %d", i)
		}
	}
	// Cross-tenant: another tenant must see NOTHING (and not even the rule).
	if _, _, err := st.workflowSvc.ListWorkflowRuns(ctx, o3aTenantB, created.ID, 10, 0); err == nil {
		t.Errorf("cross-tenant ListWorkflowRuns must be not-found")
	}
}

func TestW6Runs_PreexistingBadRule_AutoNumberSurvives(t *testing.T) {
	st := w6Wire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "66666666-0000-0000-0000-000000000002"

	st.createEntity(t, ctx, tenant, "w6orders", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
		{Name: "order_no", Label: "Order No", FieldType: "auto_number",
			Options: map[string]any{"prefix": "ORD-", "padding": float64(4)}},
	})

	// WF-01 config-time: the service must REJECT a rule targeting the auto_number field.
	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "corruptor",
		EntityName: "w6orders",
		Trigger:    "after_update",
		Actions:    []WorkflowActionInput{{Type: "field_update", Field: "order_no", Value: "HACK-1"}},
		ActorID:    user,
	}); err == nil {
		t.Fatalf("WF-01: CreateWorkflow targeting auto_number must be rejected")
	} else {
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "RECORD_AUTONUMBER_READONLY" {
			t.Fatalf("reject code = %v; want RECORD_AUTONUMBER_READONLY", err)
		}
	}

	// Seed the PRE-EXISTING bad rule straight through the repo (a row written before the guard).
	badID, err := st.wfRepo.Create(ctx, workflow.CreateInput{
		TenantID:   tenant,
		Name:       "corruptor",
		EntityName: "w6orders",
		Trigger:    workflow.TriggerAfterUpdate,
		Actions:    []byte(`[{"type":"field_update","field":"order_no","value":"HACK-1"}]`),
		IsActive:   true,
		CreatedBy:  user,
	})
	if err != nil {
		t.Fatalf("seed bad rule: %v", err)
	}

	id, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "w6orders", map[string]any{"status": "open"})
	if err != nil {
		t.Fatalf("Create order: %v", err)
	}
	rec, err := st.recordSvc.Get(ctx, wfAdmin(user), tenant, "w6orders", id, nil, nil)
	if err != nil {
		t.Fatalf("Get order: %v", err)
	}
	assigned, _ := rec.Data["order_no"].(string)
	if assigned == "" || !strings.HasPrefix(assigned, "ORD-") {
		t.Fatalf("auto_number not assigned at create: %q", assigned)
	}

	// Fire the bad rule (after_update) — the mutator's execution guard must BLOCK the write.
	if err := st.recordSvc.Update(ctx, wfAdmin(user), tenant, "w6orders", id, map[string]any{"status": "paid"}, nil); err != nil {
		t.Fatalf("Update: %v", err)
	}
	st.drain(t)

	after, err := st.recordSvc.Get(ctx, wfAdmin(user), tenant, "w6orders", id, nil, nil)
	if err != nil {
		t.Fatalf("Get after fire: %v", err)
	}
	if got, _ := after.Data["order_no"].(string); got != assigned {
		t.Fatalf("AUTO_NUMBER CORRUPTED by pre-existing workflow rule: %q → %q (WF-01 execution guard missing)", assigned, got)
	}

	// The blocked attempt is visible in the run history as a FAILED run with the readonly code.
	rows := st.runRows(t, tenant)
	var failed *runRow
	for i := range rows {
		if rows[i].WorkflowID == badID && rows[i].Status == "failed" {
			failed = &rows[i]
			break
		}
	}
	if failed == nil {
		t.Fatalf("expected a failed run row for the blocked rule; rows = %+v", rows)
	}
	if failed.Error == nil || !strings.Contains(*failed.Error, "RECORD_AUTONUMBER_READONLY") {
		t.Errorf("failed run error = %v; want RECORD_AUTONUMBER_READONLY surfaced", failed.Error)
	}
}

func TestW6Runs_DryRun_NoSideEffects(t *testing.T) {
	st := w6Wire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "66666666-0000-0000-0000-000000000003"

	st.createEntity(t, ctx, tenant, "w6quotes", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
		{Name: "stage", Label: "Stage", FieldType: "text"},
	})
	created, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "won-closes",
		EntityName: "w6quotes",
		Trigger:    "after_update",
		Rules:      []WorkflowRuleInput{{Field: "status", Op: "eq", Value: "won"}},
		Actions:    []WorkflowActionInput{{Type: "field_update", Field: "stage", Value: "closed"}},
		ActorID:    user,
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	id, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "w6quotes", map[string]any{"status": "won", "stage": "new"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	res, err := st.workflowSvc.TestWorkflow(ctx, tenant, created.ID, id)
	if err != nil {
		t.Fatalf("TestWorkflow: %v", err)
	}
	if !res.Matched {
		t.Errorf("dry-run Matched = false; want true")
	}
	if len(res.Trace) != 3 { // trigger + 1 condition + 1 action
		t.Errorf("trace = %+v; want 3 steps", res.Trace)
	}

	// ZERO side effects against the REAL database: the record is untouched and NO run row exists.
	rec, err := st.recordSvc.Get(ctx, wfAdmin(user), tenant, "w6quotes", id, nil, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got, _ := rec.Data["stage"].(string); got != "new" {
		t.Errorf("dry-run mutated the record: stage = %q; want \"new\"", got)
	}
	var n int
	if err := st.sdb.Get(&n, `SELECT COUNT(*) FROM sys_workflow_runs WHERE tenant_id = ? AND workflow_id = ?`, tenant, created.ID); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	// Only the after_create Emit could have landed rows for OTHER rules; this rule is
	// after_update and was never fired — the dry-run itself must record nothing.
	if n != 0 {
		t.Errorf("dry-run recorded %d run rows; want 0 (it is a preview, not a run)", n)
	}
	st.drain(t)
}

// TestW6Runs_FieldUpdateValueTypeMismatch_FailsRunAndLeavesRecord is the WF-VALUE-TYPECHECK
// end-to-end proof: a rule whose field_update sets a STRING value into a NUMBER field passes the
// config-time guard (the target is a plain, client-writable field — only the VALUE is wrong) but
// must FAIL at execution: the mutator type-checks the value against the field spec (the SAME
// validator the record PUT write-path uses), the run is recorded as failed with RECORD_INVALID_TYPE
// surfaced, and the record's amount survives byte-identical (no junk merged into obj_*.data).
func TestW6Runs_FieldUpdateValueTypeMismatch_FailsRunAndLeavesRecord(t *testing.T) {
	st := w6Wire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-0000000000f1"

	st.createEntity(t, ctx, tenant, "w6amounts", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
		{Name: "amount", Label: "Amount", FieldType: "number"},
	})

	// Rule: on after_update, IF status == "won" THEN set amount = "abc" (a STRING into a number
	// field). Since queue #282 the save refuses that value (checkFieldUpdateValues), so the rule is
	// saved valid and its stored actions rewritten below — mirroring a rule that was saved BEFORE the
	// save-time check existed (the case the run-time guard still has to hold for).
	created, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "bad-value",
		EntityName: "w6amounts",
		Trigger:    "after_update",
		Rules:      []WorkflowRuleInput{{Field: "status", Op: "eq", Value: "won"}},
		Actions:    []WorkflowActionInput{{Type: "field_update", Field: "amount", Value: float64(1)}},
		ActorID:    user,
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	if _, err := st.sdb.Exec(`UPDATE sys_workflows SET actions = ? WHERE tenant_id = ? AND id = ?`,
		`[{"type":"field_update","field":"amount","value":"abc"}]`, tenant, created.ID); err != nil {
		t.Fatalf("rewrite stored actions: %v", err)
	}

	// Seed a record with a valid numeric amount, then fire the rule via an update to status.
	id, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "w6amounts",
		map[string]any{"status": "open", "amount": float64(100)})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}
	if err := st.recordSvc.Update(ctx, wfAdmin(user), tenant, "w6amounts", id,
		map[string]any{"status": "won"}, nil); err != nil {
		t.Fatalf("update status: %v", err)
	}
	st.drain(t)

	// The bad value must NEVER have merged into the record — amount survives byte-identical.
	rec, err := st.recordSvc.Get(ctx, wfAdmin(user), tenant, "w6amounts", id, nil, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got, _ := rec.Data["amount"].(float64); got != 100 {
		t.Fatalf("amount CORRUPTED by bad-typed workflow value: got %#v; want 100 (value type-check missing)", rec.Data["amount"])
	}

	// The run is recorded as FAILED with the validation code surfaced.
	rows := st.runRows(t, tenant)
	var failed *runRow
	for i := range rows {
		if rows[i].WorkflowID == created.ID && rows[i].Status == "failed" {
			failed = &rows[i]
			break
		}
	}
	if failed == nil {
		t.Fatalf("expected a failed run for the bad-value rule; rows = %+v", rows)
	}
	if failed.Error == nil || !strings.Contains(*failed.Error, "RECORD_INVALID_TYPE") {
		t.Errorf("failed run error = %v; want RECORD_INVALID_TYPE surfaced", failed.Error)
	}
}
