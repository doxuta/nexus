//go:build integration

// FORMULA-RECOMPUTE (follow-up #83) — a workflow field_update / assign that changes a formula's
// INPUT must recompute the formula in the SAME write, so the stored formula value is never left
// stale (and any rollup summing it reads a correct value). Exercised END-TO-END over a freshly
// migrated throwaway MySQL DB, wired exactly like cmd/server/main.go:
//
//	RecordService.Update            (client edit → after_update Emit)
//	  → workflow.AsyncDispatcher    (async worker; run recorder for the no-loop assertion)
//	    → services.WorkflowRecordMutator.UpdateField   (BYPASSES RecordService)
//	        → recomputeFormulas → applyComputedFields   (folds formula into the SAME store write)
//	        → dynrepo.UpdateRecord (system scope, emits NOTHING — no cascade)
//
// Proves:
//   - the staleness bug is fixed: after a field_update edits a formula input, the stored formula
//     value == the NEW input product (before the fix it stayed the create-time value);
//   - rollup-over-formula composes under automation: a field_update on a child line's quantity
//     makes the parent quote's `total` rollup (sum of child line_total formulas) reflect the new
//     sum on the next read (formula recompute + rollup-at-read compose);
//   - NO new cascade: a formula-recomputing field_update does NOT emit an after_update event, so
//     the after_update rule that drove it fires exactly ONCE (the create_record recursion guard
//     depends on field_update emitting nothing — this is the regression guard for that property).
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run Formula
package services

import (
	"context"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// frStack is the fully-wired workflow + record + rollup composition a recompute test drives. It
// combines the workflow engine (dispatcher + WF-01 catalog mutator + run recorder, as w6Wire) with
// the read-time rollup wiring (WithRollupMeta / WithRollupLinked, as engWire) so ONE stack can both
// fire a field_update rule AND read a parent rollup over a child formula.
type frStack struct {
	sdb         *sqlx.DB
	entitySvc   *EntityService
	fieldSvc    *FieldService
	recordSvc   *RecordService
	workflowSvc *WorkflowService
	dispatcher  *workflow.AsyncDispatcher
	// machineAudit captures what the mutator hands the audit sink (queue #86) — wired exactly like
	// main.go, so the before-image the diff is built from is a REAL MySQL read.
	machineAudit *captureAuditor
	meta         *metadata.Repository
	dyn          *dynrepo.Repository
}

func frWire(t *testing.T) *frStack {
	t.Helper()
	gdb, sdb := o3aSetup(t)

	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	mc := NewMetaCache(metaRepo, DefaultMetaTTL)

	entitySvc := NewEntityService(dynRepo, metaRepo).WithMetaCache(mc)
	fieldSvc := NewFieldService(dynRepo, metaRepo).WithMetaCache(mc)

	wfRepo := workflow.NewSQLRepository(sdb)
	wfCache := workflow.NewCachedLoader(wfRepo, 500*time.Millisecond)
	machineAudit := &captureAuditor{}
	wfMutator := NewWorkflowRecordMutator(dynRepo).WithCatalog(mc).WithAuditor(machineAudit)
	dispatcher := workflow.NewAsyncDispatcher(
		workflow.DispatcherConfig{Buffer: 64, Workers: 1},
		wfCache, wfMutator, wfRepo, nil,
	).WithRunRecorder(wfRepo)
	workflowSvc := NewWorkflowService(wfRepo).WithEntityChecker(mc).WithCache(wfCache)

	recordSvc := NewRecordService(mc, dynRepo).
		WithEngine(dispatcher).
		WithRollupMeta(metaRepo).
		WithRollupLinked(dynRepo)

	return &frStack{
		sdb: sdb, entitySvc: entitySvc, fieldSvc: fieldSvc,
		recordSvc: recordSvc, workflowSvc: workflowSvc, dispatcher: dispatcher,
		machineAudit: machineAudit, meta: metaRepo, dyn: dynRepo,
	}
}

func (s *frStack) createEntity(t *testing.T, ctx context.Context, tenant, name string, fields []FieldInput) {
	t.Helper()
	if _, _, err := s.entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name: name, Label: name, Fields: fields,
	}); err != nil {
		t.Fatalf("CreateEntity %s: %v", name, err)
	}
}

func (s *frStack) drain(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.dispatcher.Shutdown(ctx); err != nil {
		t.Fatalf("dispatcher drain: %v", err)
	}
}

// runCountFor returns how many sys_workflow_runs rows exist for a single rule (the no-loop canary).
func (s *frStack) runCountFor(t *testing.T, tenant, workflowID string) int {
	t.Helper()
	var n int
	if err := s.sdb.Get(&n,
		"SELECT COUNT(*) FROM sys_workflow_runs WHERE tenant_id = ? AND workflow_id = ?", tenant, workflowID,
	); err != nil {
		t.Fatalf("count sys_workflow_runs: %v", err)
	}
	return n
}

// ---------------------------------------------------------------------------
// THE key test: a workflow field_update edits a formula INPUT → the stored formula is recomputed
// fresh (before the fix it stayed the stale create-time value).
// ---------------------------------------------------------------------------

func TestFormulaRecompute_FieldUpdate_RefreshesStaleFormula(t *testing.T) {
	st := frWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "cccc1111-0000-0000-0000-000000000001"

	// line_total = quantity * unit_price (formula, number result — the default resultType).
	st.createEntity(t, ctx, tenant, "inv_lines", []FieldInput{
		{Name: "sku", Label: "SKU", FieldType: "text"},
		{Name: "quantity", Label: "Quantity", FieldType: "number"},
		{Name: "unit_price", Label: "Unit Price", FieldType: "number"},
		{Name: "line_total", Label: "Line Total", FieldType: "formula",
			Options: map[string]any{"formula": "quantity * unit_price"}},
	})

	// Rule: on after_update, IF sku == "recompute" THEN field_update quantity = 10.
	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "bump-quantity",
		EntityName: "inv_lines",
		Trigger:    "after_update",
		Rules:      []WorkflowRuleInput{{Field: "sku", Op: "eq", Value: "recompute"}},
		Actions:    []WorkflowActionInput{{Type: "field_update", Field: "quantity", Value: float64(10)}},
		ActorID:    user,
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	// Create: quantity=2, unit_price=5 → line_total = 10 at create time. Number values must be
	// float64 (the JSON/number shape the record validator accepts) when calling Create directly.
	id, err := st.recordSvc.Create(ctx, wfAdmin(user), tenant, user, "inv_lines",
		map[string]any{"sku": "init", "quantity": 2.0, "unit_price": 5.0})
	if err != nil {
		t.Fatalf("Create line: %v", err)
	}

	// Client edit sku="recompute" (does NOT touch quantity). This fires the rule, whose field_update
	// sets quantity=10 → the stored line_total must be recomputed to 10*5 = 50 (was stale 10 pre-fix).
	if err := st.recordSvc.Update(ctx, wfAdmin(user), tenant, "inv_lines", id,
		map[string]any{"sku": "recompute"}, nil); err != nil {
		t.Fatalf("Update sku=recompute: %v", err)
	}

	st.drain(t)

	rec, err := st.recordSvc.Get(ctx, wfAdmin(user), tenant, "inv_lines", id, nil, nil)
	if err != nil {
		t.Fatalf("Get after fire: %v", err)
	}
	// Sanity: the field_update landed.
	if got, _ := rec.Data["quantity"].(float64); got != 10 {
		t.Fatalf("field_update did not set quantity: got %v; want 10", rec.Data["quantity"])
	}
	// The crux: line_total is FRESH (10*5=50), not the stale create-time 10.
	if got, _ := rec.Data["line_total"].(float64); got != 50 {
		t.Fatalf("STALE FORMULA: line_total=%v after field_update set quantity=10; want 50 (=10*5). "+
			"Pre-fix this stored the stale create-time value 10.", rec.Data["line_total"])
	}
	// Queue #86: the machine write left ONE audit row by the engine, and the SAME before-image read
	// that fed the recompute fed the diff — the input AND the formula it moved, each old → new.
	evt := soleEvent(t, st.machineAudit)
	if evt.ActorUserID != workflow.SystemActorID || evt.RecordID != id || evt.Diff == nil {
		t.Fatalf("machine write audit = %+v; want an update on %s by the system actor", evt, id)
	}
	for field, want := range map[string][2]float64{"quantity": {2, 10}, "line_total": {10, 50}} {
		b, _ := evt.Diff.Before[field].(float64)
		a, _ := evt.Diff.After[field].(float64)
		if b != want[0] || a != want[1] {
			t.Errorf("audit diff %s = %v → %v; want %v → %v", field, evt.Diff.Before[field], evt.Diff.After[field], want[0], want[1])
		}
	}
}

// ---------------------------------------------------------------------------
// Rollup-over-formula under automation (the quote case): a field_update on a child line's quantity
// makes the parent quote's `total` rollup (sum of child line_total formulas) reflect the new sum.
// ---------------------------------------------------------------------------

func TestFormulaRecompute_RollupOverFormula_UnderAutomation(t *testing.T) {
	st := frWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "cccc1111-0000-0000-0000-000000000002"

	// Parent must exist before a child relation field can target it (FIELD_RELATION_TARGET_NOT_FOUND).
	st.createEntity(t, ctx, tenant, "quotes", []FieldInput{
		{Name: "name", Label: "Name", FieldType: "text"},
	})
	st.createEntity(t, ctx, tenant, "quote_lines", []FieldInput{
		{Name: "quote", Label: "Quote", FieldType: "relation", Options: map[string]any{"relatedEntity": "quotes"}},
		{Name: "tag", Label: "Tag", FieldType: "text"},
		{Name: "quantity", Label: "Quantity", FieldType: "number"},
		{Name: "unit_price", Label: "Unit Price", FieldType: "number"},
		{Name: "line_total", Label: "Line Total", FieldType: "formula",
			Options: map[string]any{"formula": "quantity * unit_price"}},
	})
	// quotes.total = SUM(child quote_lines.line_total) — a rollup over a formula field (number
	// resultType), an explicitly supported aggField (rollup_validate.go).
	if _, err := st.fieldSvc.AddField(ctx, tenant, AddFieldInput{
		EntityName: "quotes",
		Field: FieldInput{
			Name: "total", Label: "Total", FieldType: "rollup",
			Options: map[string]any{"childEntity": "quote_lines", "viaRelation": "quote", "metric": "sum", "aggField": "line_total"},
		},
	}); err != nil {
		t.Fatalf("AddField quotes.total rollup: %v", err)
	}

	// Rule: on after_update of a line, IF tag == "bump" THEN field_update quantity = 3.
	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "bump-line",
		EntityName: "quote_lines",
		Trigger:    "after_update",
		Rules:      []WorkflowRuleInput{{Field: "tag", Op: "eq", Value: "bump"}},
		Actions:    []WorkflowActionInput{{Type: "field_update", Field: "quantity", Value: float64(3)}},
		ActorID:    user,
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	qid, err := st.recordSvc.Create(ctx, wfAdmin(user), tenant, user, "quotes", map[string]any{"name": "Q-1"})
	if err != nil {
		t.Fatalf("Create quote: %v", err)
	}
	// Line: quantity=1, unit_price=100 → line_total=100 at create → quote total=100.
	lid, err := st.recordSvc.Create(ctx, wfAdmin(user), tenant, user, "quote_lines",
		map[string]any{"quote": qid, "tag": "init", "quantity": 1.0, "unit_price": 100.0})
	if err != nil {
		t.Fatalf("Create line: %v", err)
	}

	if got := st.quoteTotal(t, ctx, user, tenant, qid); got != "100.0000000000" {
		t.Fatalf("baseline quote total=%q; want \"100.0000000000\" (1*100)", got)
	}

	// Fire the rule: field_update sets the line's quantity=3 → line_total must recompute to 300 →
	// the parent quote's total rollup must read 300 on its next read (rollup is compute-at-READ, so
	// no parent write is needed — only the child's persisted line_total had to be refreshed).
	if err := st.recordSvc.Update(ctx, wfAdmin(user), tenant, "quote_lines", lid,
		map[string]any{"tag": "bump"}, nil); err != nil {
		t.Fatalf("Update line tag=bump: %v", err)
	}
	st.drain(t)

	if got := st.quoteTotal(t, ctx, user, tenant, qid); got != "300.0000000000" {
		t.Fatalf("STALE ROLLUP: quote total=%q after field_update set line quantity=3; want \"300.0000000000\" (=3*100). "+
			"Pre-fix the child line_total stayed 100 so the parent sum stayed 100.", got)
	}
}

// quoteTotal reads the quote's `total` rollup (SUM over the child line_total formula) via an
// explicit ?rollup= request and returns it as the exact DECIMAL(38,10) string the aggregate emits
// (no float drift — mirrors eng_computed's rollup assertions).
func (s *frStack) quoteTotal(t *testing.T, ctx context.Context, user, tenant, qid string) string {
	t.Helper()
	rec, err := s.recordSvc.Get(ctx, wfAdmin(user), tenant, "quotes", qid, nil, []string{"total"})
	if err != nil {
		t.Fatalf("Get quote with rollup: %v", err)
	}
	got, ok := rec.Data["total"].(string)
	if !ok {
		t.Fatalf("quote total rollup not a DECIMAL string: %v (%T)", rec.Data["total"], rec.Data["total"])
	}
	return got
}

// ---------------------------------------------------------------------------
// No cascade: a formula-recomputing field_update emits NO after_update event, so the after_update
// rule that drove it fires exactly ONCE. If a regression routed field_update through a path that
// re-emits, this rule (whose condition stays true on the recomputed after-image) would re-fire
// (run count > 1) or loop forever (drain timeout) — the create_record recursion guard relies on
// field_update emitting nothing.
// ---------------------------------------------------------------------------

func TestFormulaRecompute_NoCascade_FieldUpdateDoesNotReEmit(t *testing.T) {
	st := frWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "cccc1111-0000-0000-0000-000000000003"

	st.createEntity(t, ctx, tenant, "nc_lines", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
		{Name: "quantity", Label: "Quantity", FieldType: "number"},
		{Name: "unit_price", Label: "Unit Price", FieldType: "number"},
		{Name: "line_total", Label: "Line Total", FieldType: "formula",
			Options: map[string]any{"formula": "quantity * unit_price"}},
	})

	// Rule: on after_update, IF status == "won" THEN field_update quantity = 9. Its field_update
	// recomputes line_total; if that recompute re-emitted an after_update, status=="won" would STILL
	// be true on the after-image → the rule would fire again (and again — an infinite loop).
	created, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "won-bumps-quantity",
		EntityName: "nc_lines",
		Trigger:    "after_update",
		Rules:      []WorkflowRuleInput{{Field: "status", Op: "eq", Value: "won"}},
		Actions:    []WorkflowActionInput{{Type: "field_update", Field: "quantity", Value: float64(9)}},
		ActorID:    user,
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	id, err := st.recordSvc.Create(ctx, wfAdmin(user), tenant, user, "nc_lines",
		map[string]any{"status": "open", "quantity": 1.0, "unit_price": 2.0})
	if err != nil {
		t.Fatalf("Create line: %v", err)
	}

	// One client edit sets status="won" → the rule fires ONCE and its field_update recomputes
	// line_total. drain() blocks until every emitted event is processed (a cascade would either
	// bump the run count or hang the drain).
	if err := st.recordSvc.Update(ctx, wfAdmin(user), tenant, "nc_lines", id,
		map[string]any{"status": "won"}, nil); err != nil {
		t.Fatalf("Update status=won: %v", err)
	}
	st.drain(t)

	// The recompute happened (proves the field_update actually went through recomputeFormulas)...
	rec, err := st.recordSvc.Get(ctx, wfAdmin(user), tenant, "nc_lines", id, nil, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got, _ := rec.Data["line_total"].(float64); got != 18 {
		t.Fatalf("line_total=%v; want 18 (=9*2) — the recompute path was not exercised", rec.Data["line_total"])
	}
	// ...and the rule fired EXACTLY ONCE (no re-emit from the recomputing field_update).
	if got := st.runCountFor(t, tenant, created.ID); got != 1 {
		t.Fatalf("CASCADE DETECTED: after_update rule ran %d times; want 1. A formula-recomputing "+
			"field_update must NOT re-emit an after_update event (create_record recursion guard).", got)
	}
}
