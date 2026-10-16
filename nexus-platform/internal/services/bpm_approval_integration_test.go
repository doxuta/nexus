//go:build integration

// EPIC 5 M1 — BPM / Approval / SLA engine, exercised END-TO-END through the production composition
// over a freshly-migrated throwaway MySQL DB (reuses o3aSetup, which runs ALL migrations incl.
// 000021 sys_approval_processes + sys_approval_instances via the REAL golang-migrate CLI — not
// AutoMigrate).
//
// What this proves that the bpm package unit tests (internal/bpm/engine_test.go, fakes only) cannot:
// the WHOLE stack wired exactly as cmd/server/main.go does —
//
//	RecordService.{Create,Update}        (after-commit Emit → bpm.AsyncEngine)
//	  → bpm.AsyncEngine.arm               (load active processes, match criteria, idempotent arm)
//	    → bpm.SQLRepository               (real sys_approval_processes / sys_approval_instances)
//	  → ApprovalService.Decide → bpm.Decider (the SYNCHRONOUS approve/reject path, optimistic guard)
//	  → bpm.SLAScheduler.runBreaches      (the breach scan + at-most-once CAS escalation)
//
// — fires correctly against a real database, with the approval processes themselves registered
// through the production ApprovalService admin path (validation + entity-existence check).
//
// Covered (task acceptance):
//   - sequential 2-level chain: arm on after_create → approve step 1 (advances to step 2, new
//     approver/SLA) → approve step 2 → terminal 'approved'; history records both decisions.
//   - reject: a rejection at step 1 is terminal 'rejected'; step 2 is never opened.
//   - SLA breach → notify + escalate: an instance past its step SLA is escalated AT MOST ONCE and a
//     sys_notifications row is written to the current approver.
//   - tenant isolation: tenant B's record mutation NEVER arms tenant A's process.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run BPM
package services

import (
	"context"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// bpmStack is the fully-wired BPM approval + record composition a single integration test drives.
type bpmStack struct {
	sdb       *sqlx.DB
	repo      *bpm.SQLRepository
	entitySvc *EntityService
	recordSvc *RecordService
	approval  *ApprovalService
	engine    *bpm.AsyncEngine
	sla       *bpm.SLAScheduler
	requester *bpm.Requester // the manual arming path (POST /:entity/:id/approvals/submit)
	runner    *bpm.Runner    // the walk through gateway / auto / end nodes (specs/043 S7)
}

// bpmWire stands up the production composition (mirroring cmd/server/main.go) over a freshly
// migrated throwaway DB: the async arming engine, the synchronous decider behind ApprovalService,
// and the SLA scheduler — all over ONE *bpm.SQLRepository, with a single-worker engine so a test
// can drive a mutation then deterministically drain. No TTL cache sits in front of the process
// loader (the engine reads sys_approval_processes live), so a freshly-created process is armed on
// the very next Emit with no stale-cache race.
func bpmWire(t *testing.T) *bpmStack {
	t.Helper()
	gdb, sdb := o3aSetup(t)
	// The SLA sweep walks the tenant registry (specs/043 S3b), so the two fixture tenants must be
	// registered like any real one.
	for _, tn := range []struct{ id, slug string }{{o3aTenantA, "bpm-a"}, {o3aTenantB, "bpm-b"}} {
		if _, err := sdb.Exec(`INSERT IGNORE INTO tenants (id, slug, name, status) VALUES (?, ?, ?, 'active')`, tn.id, tn.slug, tn.slug); err != nil {
			t.Fatalf("register fixture tenant %s: %v", tn.slug, err)
		}
	}

	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	mc := NewMetaCache(metaRepo, DefaultMetaTTL)

	entitySvc := NewEntityService(dynRepo, metaRepo).WithMetaCache(mc)

	repo := bpm.NewSQLRepository(sdb, nil)
	// Single worker so drain (Shutdown) is a hard barrier: after it returns, every armed event is
	// fully processed and its instance row is committed.
	engine := bpm.NewAsyncEngine(bpm.EngineConfig{Buffer: 64, Workers: 1}, repo, repo, repo, nil)
	recordSvc := NewRecordService(mc, dynRepo).WithApprovalEngine(engine)
	// The outcome write-back is wired exactly as main.go does, so a test can assert WHEN the
	// record changes (only after the last pinned step) — not just that the instance ended.
	decider := bpm.NewDecider(repo, repo, repo, nil, nil).WithRecordWriter(NewApprovalOutcomeWriter(recordSvc))
	approval := NewApprovalService(repo, repo, decider).WithEntityChecker(mc)
	// SLA scheduler is built but NOT Start()ed — tests drive a deterministic single pass via
	// RunBreachesForTest(now) instead of waiting on the ticker.
	sla := bpm.NewSLAScheduler(bpm.SLASchedulerConfig{Interval: time.Minute}, repo, repo, nil, nil)

	// The walk through machine nodes, wired exactly as main.go does (specs/043 S7).
	outcomeWriter := NewApprovalOutcomeWriter(recordSvc)
	runner := bpm.NewRunner(repo, NewApprovalRecordReader(recordSvc), outcomeWriter, nil, nil).
		WithRecordWriter(outcomeWriter).WithInbox(repo)
	engine.WithRunner(runner)
	decider.WithRunner(runner)
	approval.WithRetrier(runner)

	return &bpmStack{
		sdb:       sdb,
		repo:      repo,
		entitySvc: entitySvc,
		recordSvc: recordSvc,
		approval:  approval,
		engine:    engine,
		sla:       sla,
		requester: bpm.NewRequester(repo, repo, repo, repo, nil).WithRunner(runner),
		runner:    runner,
	}
}

// createEntity registers a dynamic entity (and its obj_ table) so the test can drive records.
func (s *bpmStack) createEntity(t *testing.T, ctx context.Context, tenant, name string, fields []FieldInput) {
	t.Helper()
	if _, _, err := s.entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name: name, Label: name, Fields: fields,
	}); err != nil {
		t.Fatalf("CreateEntity %s: %v", name, err)
	}
}

// drain shuts the async arming engine down (graceful drain), blocking until every emitted event has
// been processed. A test emits all its arming events (via record mutations), drains once, then
// asserts. After drain the engine is closed — so each test stands up its own stack.
func (s *bpmStack) drain(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.engine.Shutdown(ctx); err != nil {
		t.Fatalf("engine drain: %v", err)
	}
}

// bpmAdmin is the admin record scope (full visibility, bypasses RLS) the tests mutate records under.
func bpmAdmin(userID string) RecordScope {
	return RecordScope{UserID: userID, AccessLevel: "all", IsAdmin: true}
}

// onlyPendingInstance returns the single pending instance for (tenant, entity, record). It fails the
// test if zero or more than one exists — the arming path must create exactly one.
func (s *bpmStack) onlyPendingInstance(t *testing.T, tenant, entity, recordID string) *bpm.Instance {
	t.Helper()
	var id string
	if err := s.sdb.Get(&id,
		"SELECT id FROM sys_approval_instances WHERE tenant_id = ? AND entity_name = ? AND record_id = ? AND status = 'pending'",
		tenant, entity, recordID,
	); err != nil {
		t.Fatalf("locate pending instance for %s/%s: %v", entity, recordID, err)
	}
	inst, err := s.repo.GetInstance(context.Background(), tenant, id)
	if err != nil {
		t.Fatalf("GetInstance %s: %v", id, err)
	}
	return inst
}

// instanceCount returns how many sys_approval_instances rows exist for (tenant, entity, record).
func (s *bpmStack) instanceCount(t *testing.T, tenant, entity, recordID string) int {
	t.Helper()
	var n int
	if err := s.sdb.Get(&n,
		"SELECT COUNT(*) FROM sys_approval_instances WHERE tenant_id = ? AND entity_name = ? AND record_id = ?",
		tenant, entity, recordID,
	); err != nil {
		t.Fatalf("count instances: %v", err)
	}
	return n
}

// notifCount returns how many sys_notifications rows exist for (tenant, user).
func (s *bpmStack) notifCount(t *testing.T, tenant, user string) int {
	t.Helper()
	var n int
	if err := s.sdb.Get(&n,
		"SELECT COUNT(*) FROM sys_notifications WHERE tenant_id = ? AND user_id = ?", tenant, user,
	); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return n
}

// ---------------------------------------------------------------------------
// sequential 2-level chain: arm on create → approve step 1 → approve step 2 → 'approved'.
// ---------------------------------------------------------------------------

func TestBPM_SequentialTwoLevel_Approve(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const submitter = "11111111-0000-0000-0000-000000000001"
	const approver1 = "22222222-0000-0000-0000-000000000002"
	const approver2 = "33333333-0000-0000-0000-000000000003"

	st.createEntity(t, ctx, tenant, "expenses", []FieldInput{
		{Name: "amount", Label: "Amount", FieldType: "number"},
		{Name: "title", Label: "Title", FieldType: "text"},
	})

	// Process: on after_create of expenses where amount >= 1000, require sequential approval by
	// user approver1 (step 1) then user approver2 (step 2). Step 1 has a long SLA (no breach here).
	if _, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name:       "big-expense-approval",
		EntityName: "expenses",
		Trigger:    "after_create",
		Match:      "and",
		Rules:      []ApprovalCriteriaRuleInput{{Field: "amount", Op: "gte", Value: 1000.0}},
		Steps: []ApprovalStepInput{
			{Name: "Manager", ApproverType: "user", ApproverID: approver1, SLASeconds: 3600},
			{Name: "Director", ApproverType: "user", ApproverID: approver2},
		},
		IsActive: boolPtr(true),
		ActorID:  submitter,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}

	// Create a matching record → after_create arms the approval instance on step 1.
	id, err := st.recordSvc.Create(ctx, bpmAdmin(submitter), tenant, submitter, "expenses", map[string]any{"amount": 1500.0, "title": "Laptop"})
	if err != nil {
		t.Fatalf("Create expense: %v", err)
	}
	st.drain(t) // wait for the async arm to finish.

	inst := st.onlyPendingInstance(t, tenant, "expenses", id)
	if inst.CurrentStep != 0 || inst.Status != bpm.StatusPending {
		t.Fatalf("armed instance not at step 0/pending: step=%d status=%s", inst.CurrentStep, inst.Status)
	}
	if inst.CurrentApproverID != approver1 {
		t.Fatalf("step-0 approver=%q; want %q", inst.CurrentApproverID, approver1)
	}
	if inst.SLADueAt == nil {
		t.Fatalf("step-0 SLA due-at not set despite slaSeconds=3600")
	}

	// approver2 must NOT be able to decide step 1 (not the current approver).
	if _, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: approver2}); err == nil {
		t.Fatalf("approver2 approved step 1 — authorization leak (should be APPROVAL_NOT_AUTHORIZED)")
	}

	// approver1 approves step 1 → advances to step 2, new approver = approver2.
	after1, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "ok by manager", bpm.Actor{UserID: approver1})
	if err != nil {
		t.Fatalf("approver1 approve step 1: %v", err)
	}
	if after1.Status != bpm.StatusPending || after1.CurrentStep != 1 {
		t.Fatalf("after step-1 approve: status=%s step=%d; want pending/1", after1.Status, after1.CurrentStep)
	}
	if after1.CurrentApproverID != approver2 {
		t.Fatalf("after step-1 approve: approver=%q; want %q", after1.CurrentApproverID, approver2)
	}

	// approver2 approves step 2 (last) → terminal 'approved'.
	after2, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "ok by director", bpm.Actor{UserID: approver2})
	if err != nil {
		t.Fatalf("approver2 approve step 2: %v", err)
	}
	if after2.Status != bpm.StatusApproved {
		t.Fatalf("after final approve: status=%s; want approved", after2.Status)
	}

	// History must record BOTH approvals in order.
	final, err := st.repo.GetInstance(ctx, tenant, inst.ID)
	if err != nil {
		t.Fatalf("GetInstance final: %v", err)
	}
	if len(final.History) != 2 {
		t.Fatalf("history entries=%d; want 2 (two approvals)", len(final.History))
	}
	if final.History[0].Action != bpm.HistoryApproved || final.History[0].Actor != approver1 || final.History[0].Step != 0 {
		t.Errorf("history[0]=%+v; want step0 approved by approver1", final.History[0])
	}
	if final.History[1].Action != bpm.HistoryApproved || final.History[1].Actor != approver2 || final.History[1].Step != 1 {
		t.Errorf("history[1]=%+v; want step1 approved by approver2", final.History[1])
	}
}

// ---------------------------------------------------------------------------
// reject at step 1 is terminal; step 2 is never opened.
// ---------------------------------------------------------------------------

func TestBPM_RejectIsTerminal(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const submitter = "11111111-0000-0000-0000-000000000010"
	const approver1 = "22222222-0000-0000-0000-000000000020"
	const approver2 = "33333333-0000-0000-0000-000000000030"

	st.createEntity(t, ctx, tenant, "leaves", []FieldInput{
		{Name: "days", Label: "Days", FieldType: "number"},
	})

	if _, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name:       "leave-approval",
		EntityName: "leaves",
		Trigger:    "after_create",
		Steps: []ApprovalStepInput{
			{Name: "Lead", ApproverType: "user", ApproverID: approver1},
			{Name: "HR", ApproverType: "user", ApproverID: approver2},
		},
		IsActive: boolPtr(true),
		ActorID:  submitter,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}

	id, err := st.recordSvc.Create(ctx, bpmAdmin(submitter), tenant, submitter, "leaves", map[string]any{"days": 5.0})
	if err != nil {
		t.Fatalf("Create leave: %v", err)
	}
	st.drain(t)

	inst := st.onlyPendingInstance(t, tenant, "leaves", id)

	// approver1 rejects step 1 → terminal 'rejected'.
	after, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionReject, "not enough notice", bpm.Actor{UserID: approver1})
	if err != nil {
		t.Fatalf("reject step 1: %v", err)
	}
	if after.Status != bpm.StatusRejected {
		t.Fatalf("after reject: status=%s; want rejected", after.Status)
	}

	// A further decision on a terminal instance must be refused (APPROVAL_NOT_PENDING) — step 2
	// was never opened, and approver2 cannot resurrect it.
	if _, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: approver2}); err == nil {
		t.Fatalf("approve after reject succeeded — terminal instance was mutated")
	}

	final, err := st.repo.GetInstance(ctx, tenant, inst.ID)
	if err != nil {
		t.Fatalf("GetInstance final: %v", err)
	}
	if final.Status != bpm.StatusRejected {
		t.Fatalf("final status=%s; want rejected", final.Status)
	}
	if len(final.History) != 1 || final.History[0].Action != bpm.HistoryRejected {
		t.Fatalf("history=%+v; want exactly one 'rejected' entry", final.History)
	}
}

// ---------------------------------------------------------------------------
// SLA breach → escalate + notify, AT MOST ONCE per step.
// ---------------------------------------------------------------------------

func TestBPM_SLABreach_EscalatesAndNotifies(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const submitter = "11111111-0000-0000-0000-000000000100"
	const approver = "22222222-0000-0000-0000-000000000200"

	st.createEntity(t, ctx, tenant, "tickets", []FieldInput{
		{Name: "priority", Label: "Priority", FieldType: "text"},
	})

	// Step has a 1-second SLA so it is trivially breachable; the scan is driven at now+1h.
	if _, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name:       "ticket-approval",
		EntityName: "tickets",
		Trigger:    "after_create",
		Steps: []ApprovalStepInput{
			{Name: "OnCall", ApproverType: "user", ApproverID: approver, SLASeconds: 1},
		},
		IsActive: boolPtr(true),
		ActorID:  submitter,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}

	id, err := st.recordSvc.Create(ctx, bpmAdmin(submitter), tenant, submitter, "tickets", map[string]any{"priority": "high"})
	if err != nil {
		t.Fatalf("Create ticket: %v", err)
	}
	st.drain(t)

	inst := st.onlyPendingInstance(t, tenant, "tickets", id)
	if inst.Escalated {
		t.Fatalf("instance escalated before any SLA pass")
	}
	if inst.SLADueAt == nil {
		t.Fatalf("step SLA due-at not set despite slaSeconds=1")
	}

	// Notifications written by the ARM path (approval-needed) are baseline; capture the count so we
	// assert the SLA pass adds exactly one breach notice.
	before := st.notifCount(t, tenant, approver)

	// Drive a deterministic breach scan well past the 1s deadline.
	st.sla.RunBreachesForTest(ctx, time.Now().UTC().Add(time.Hour))

	after, err := st.repo.GetInstance(ctx, tenant, inst.ID)
	if err != nil {
		t.Fatalf("GetInstance after breach: %v", err)
	}
	if !after.Escalated {
		t.Fatalf("instance NOT escalated after SLA breach scan")
	}
	if after.Status != bpm.StatusPending {
		t.Fatalf("escalation must NOT terminate the instance: status=%s; want pending", after.Status)
	}
	gotEscalated := false
	for _, h := range after.History {
		if h.Action == bpm.HistoryEscalated && h.Step == 0 {
			gotEscalated = true
		}
	}
	if !gotEscalated {
		t.Fatalf("no 'escalated' history entry for step 0: %+v", after.History)
	}
	if got := st.notifCount(t, tenant, approver); got != before+1 {
		t.Fatalf("breach notifications: got %d; want %d (exactly one breach notice)", got, before+1)
	}

	// A SECOND scan must NOT double-escalate (at-most-once per step): escalated stays 1, history and
	// notification count are unchanged.
	st.sla.RunBreachesForTest(ctx, time.Now().UTC().Add(2*time.Hour))
	after2, err := st.repo.GetInstance(ctx, tenant, inst.ID)
	if err != nil {
		t.Fatalf("GetInstance after second pass: %v", err)
	}
	if len(after2.History) != len(after.History) {
		t.Fatalf("second SLA pass changed history (double-escalation): %d → %d", len(after.History), len(after2.History))
	}
	if got := st.notifCount(t, tenant, approver); got != before+1 {
		t.Fatalf("second SLA pass sent another notice (double-fire): got %d; want %d", got, before+1)
	}
}

// ---------------------------------------------------------------------------
// tenant isolation: tenant B's record mutation must NEVER arm tenant A's process.
// ---------------------------------------------------------------------------

func TestBPM_TenantIsolation(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const userA = "aaaa1111-0000-0000-0000-000000000001"
	const userB = "bbbb1111-0000-0000-0000-000000000001"
	const approverA = "aaaa2222-0000-0000-0000-000000000002"

	// Same entity name in both tenants (shared physical obj_ table, per-tenant metadata).
	st.createEntity(t, ctx, o3aTenantA, "pos", []FieldInput{{Name: "total", Label: "Total", FieldType: "number"}})
	st.createEntity(t, ctx, o3aTenantB, "pos", []FieldInput{{Name: "total", Label: "Total", FieldType: "number"}})

	// Approval process lives ONLY in tenant A.
	if _, err := st.approval.CreateProcess(ctx, o3aTenantA, CreateApprovalProcessInput{
		Name:       "a-only",
		EntityName: "pos",
		Trigger:    "after_create",
		Steps:      []ApprovalStepInput{{Name: "A", ApproverType: "user", ApproverID: approverA}},
		IsActive:   boolPtr(true),
		ActorID:    userA,
	}); err != nil {
		t.Fatalf("CreateProcess (tenant A): %v", err)
	}

	// Tenant B creates its OWN record. Tenant A's process must NOT arm an instance on it.
	idB, err := st.recordSvc.Create(ctx, bpmAdmin(userB), o3aTenantB, userB, "pos", map[string]any{"total": 999.0})
	if err != nil {
		t.Fatalf("Create B pos: %v", err)
	}
	// Tenant A also creates a matching record → its process SHOULD arm (positive control).
	idA, err := st.recordSvc.Create(ctx, bpmAdmin(userA), o3aTenantA, userA, "pos", map[string]any{"total": 999.0})
	if err != nil {
		t.Fatalf("Create A pos: %v", err)
	}
	st.drain(t)

	if got := st.instanceCount(t, o3aTenantB, "pos", idB); got != 0 {
		t.Fatalf("TENANT LEAK: tenant A's process armed %d instance(s) on tenant B's record; want 0", got)
	}
	if got := st.instanceCount(t, o3aTenantA, "pos", idA); got != 1 {
		t.Fatalf("tenant A's own process did not arm its own record: instances=%d; want 1", got)
	}
}

// ---------------------------------------------------------------------------
// DAILY-QUEUE #8 (approval-pins-definition): an instance finishes the process it STARTED under.
// Editing the live process mid-flight must not re-wire a pending instance — before this, a
// 3-step process shortened to 2 ended the record 'approved' at step 2, outcome written, silent.
// ---------------------------------------------------------------------------

// recordField reads one field off a record under the system scope.
func (s *bpmStack) recordField(t *testing.T, tenant, entity, id, field string) any {
	t.Helper()
	rec, err := s.recordSvc.Get(context.Background(), bpmAdmin("11111111-0000-0000-0000-000000000801"), tenant, entity, id, nil, nil)
	if err != nil {
		t.Fatalf("Get %s/%s: %v", entity, id, err)
	}
	return rec.Data[field]
}

func TestBPM_ProcessEditMidFlight_PinnedDefinitionSurvives(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const submitter = "11111111-0000-0000-0000-000000000801"
	const approver1 = "22222222-0000-0000-0000-000000000802"
	const approver2 = "33333333-0000-0000-0000-000000000803"
	const approver3 = "44444444-0000-0000-0000-000000000804"

	st.createEntity(t, ctx, tenant, "quotes8", []FieldInput{
		{Name: "amount", Label: "Amount", FieldType: "number"},
		{Name: "status", Label: "Status", FieldType: "text"},
	})
	proc, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name:       "quote-3-level",
		EntityName: "quotes8",
		Trigger:    "after_create",
		Steps: []ApprovalStepInput{
			{Name: "Trưởng phòng", ApproverType: "user", ApproverID: approver1},
			{Name: "Giám đốc", ApproverType: "user", ApproverID: approver2},
			{Name: "CFO", ApproverType: "user", ApproverID: approver3},
		},
		Outcome:  &ApprovalOutcomeInput{StatusField: "status", ApprovedValue: "approved"},
		IsActive: boolPtr(true),
		ActorID:  submitter,
	})
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}

	id, err := st.recordSvc.Create(ctx, bpmAdmin(submitter), tenant, submitter, "quotes8", map[string]any{"amount": 5000.0, "status": "draft"})
	if err != nil {
		t.Fatalf("Create quote: %v", err)
	}
	st.drain(t)

	// The engine's arming path must have pinned all three steps + the outcome onto the row.
	inst := st.onlyPendingInstance(t, tenant, "quotes8", id)
	if inst.Definition == nil || len(inst.Definition.Steps) != 3 || inst.Definition.Outcome == nil {
		t.Fatalf("armed instance carries no pinned definition (process_snapshot): %+v", inst.Definition)
	}

	// Step 1 approved → step 2.
	after1, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: approver1})
	if err != nil {
		t.Fatalf("approve step 1: %v", err)
	}
	if after1.Status != bpm.StatusPending || after1.CurrentStep != 1 {
		t.Fatalf("after step 1: %s@%d; want pending@1", after1.Status, after1.CurrentStep)
	}

	// The tenant-admin shortens the LIVE process to two steps while the record waits at step 2.
	if _, err := st.approval.UpdateProcess(ctx, tenant, proc.ID, UpdateApprovalProcessInput{
		Name:       "quote-2-level",
		EntityName: "quotes8",
		Trigger:    "after_create",
		Steps: []ApprovalStepInput{
			{Name: "Trưởng phòng", ApproverType: "user", ApproverID: approver1},
			{Name: "Giám đốc", ApproverType: "user", ApproverID: approver2},
		},
		Outcome:  &ApprovalOutcomeInput{StatusField: "status", ApprovedValue: "approved"},
		IsActive: boolPtr(true),
		ActorID:  submitter,
	}); err != nil {
		t.Fatalf("UpdateProcess: %v", err)
	}

	// Step 2 approved → the instance must STILL wait on step 3 (the CFO); nothing written yet.
	after2, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: approver2})
	if err != nil {
		t.Fatalf("approve step 2: %v", err)
	}
	if after2.Status != bpm.StatusPending || after2.CurrentStep != 2 || after2.CurrentApproverID != approver3 {
		t.Fatalf("live process shortened mid-flight: got %s@%d approver=%q; want pending@2/%s — a step was skipped",
			after2.Status, after2.CurrentStep, after2.CurrentApproverID, approver3)
	}
	if got := st.recordField(t, tenant, "quotes8", id, "status"); got != "draft" {
		t.Fatalf("outcome written before the last pinned step: status=%v; want draft", got)
	}
	// The approver the live process no longer names must still be the one who can decide.
	if _, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: approver2}); err == nil {
		t.Fatalf("approver2 decided step 3 — the pinned step's approver is the CFO")
	}

	// Step 3 approved → terminal, outcome written by the pinned definition.
	after3, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "final", bpm.Actor{UserID: approver3})
	if err != nil {
		t.Fatalf("approve step 3: %v", err)
	}
	if after3.Status != bpm.StatusApproved {
		t.Fatalf("after the last pinned step: %s; want approved", after3.Status)
	}
	if got := st.recordField(t, tenant, "quotes8", id, "status"); got != "approved" {
		t.Fatalf("outcome not applied after the last pinned step: status=%v; want approved", got)
	}
	final, err := st.repo.GetInstance(ctx, tenant, inst.ID)
	if err != nil {
		t.Fatalf("GetInstance final: %v", err)
	}
	if len(final.History) != 3 {
		t.Fatalf("history entries=%d; want 3 (one per pinned step)", len(final.History))
	}
}

// TestBPM_PreSnapshotInstance_ReadsLiveProcess: rows armed BEFORE migration 000065 have a NULL
// process_snapshot. They must keep deciding through the live process, exactly as before.
func TestBPM_PreSnapshotInstance_ReadsLiveProcess(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const submitter = "11111111-0000-0000-0000-000000000811"
	const approver1 = "22222222-0000-0000-0000-000000000812"
	const approver2 = "33333333-0000-0000-0000-000000000813"

	st.createEntity(t, ctx, tenant, "quotes8b", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
	})
	if _, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name:       "legacy-2-level",
		EntityName: "quotes8b",
		Trigger:    "after_create",
		Steps: []ApprovalStepInput{
			{Name: "Trưởng phòng", ApproverType: "user", ApproverID: approver1},
			{Name: "Giám đốc", ApproverType: "user", ApproverID: approver2},
		},
		Outcome:  &ApprovalOutcomeInput{StatusField: "status", ApprovedValue: "approved"},
		IsActive: boolPtr(true),
		ActorID:  submitter,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	id, err := st.recordSvc.Create(ctx, bpmAdmin(submitter), tenant, submitter, "quotes8b", map[string]any{"status": "draft"})
	if err != nil {
		t.Fatalf("Create quote: %v", err)
	}
	st.drain(t)
	armed := st.onlyPendingInstance(t, tenant, "quotes8b", id)

	// Simulate a row from before the column existed.
	if _, err := st.sdb.Exec(`UPDATE sys_approval_instances SET process_snapshot = NULL WHERE tenant_id = ? AND id = ?`, tenant, armed.ID); err != nil {
		t.Fatalf("null the snapshot: %v", err)
	}
	inst, err := st.repo.GetInstance(ctx, tenant, armed.ID)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if inst.Definition != nil {
		t.Fatalf("a NULL process_snapshot decoded as a pinned definition: %+v", inst.Definition)
	}

	after1, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: approver1})
	if err != nil {
		t.Fatalf("approve step 1 (live fallback): %v", err)
	}
	if after1.Status != bpm.StatusPending || after1.CurrentStep != 1 || after1.CurrentApproverID != approver2 {
		t.Fatalf("live fallback after step 1: %s@%d approver=%q; want pending@1/%s", after1.Status, after1.CurrentStep, after1.CurrentApproverID, approver2)
	}
	after2, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: approver2})
	if err != nil {
		t.Fatalf("approve step 2 (live fallback): %v", err)
	}
	if after2.Status != bpm.StatusApproved {
		t.Fatalf("live fallback final: %s; want approved", after2.Status)
	}
	if got := st.recordField(t, tenant, "quotes8b", id, "status"); got != "approved" {
		t.Fatalf("live outcome not applied for a pre-snapshot instance: status=%v", got)
	}
}

// TestBPM_Submit_PinsDefinition: the manual path (Requester.Submit — "Gửi duyệt") is the second
// way an instance is born and must persist the same pin through the same column.
func TestBPM_Submit_PinsDefinition(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const submitter = "11111111-0000-0000-0000-000000000821"
	const approver1 = "22222222-0000-0000-0000-000000000822"
	const approver2 = "33333333-0000-0000-0000-000000000823"

	st.createEntity(t, ctx, tenant, "quotes8c", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
	})
	if _, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name:       "manual-2-level",
		EntityName: "quotes8c",
		Trigger:    "manual",
		Steps: []ApprovalStepInput{
			{Name: "Trưởng phòng", ApproverType: "user", ApproverID: approver1},
			{Name: "Giám đốc", ApproverType: "user", ApproverID: approver2, SLASeconds: 3600},
		},
		Outcome:  &ApprovalOutcomeInput{StatusField: "status", ApprovedValue: "approved", RejectedValue: "rejected"},
		IsActive: boolPtr(true),
		ActorID:  submitter,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	id, err := st.recordSvc.Create(ctx, bpmAdmin(submitter), tenant, submitter, "quotes8c", map[string]any{"status": "draft"})
	if err != nil {
		t.Fatalf("Create quote: %v", err)
	}
	st.drain(t) // nothing arms on create (manual trigger) — drain only proves that
	if got := st.instanceCount(t, tenant, "quotes8c", id); got != 0 {
		t.Fatalf("a manual process armed on create: instances=%d", got)
	}

	sub, err := st.requester.Submit(ctx, tenant, "quotes8c", id, map[string]any{"status": "draft"}, bpm.Actor{UserID: submitter})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	inst, err := st.repo.GetInstance(ctx, tenant, sub.ID)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	def := inst.Definition
	if def == nil || len(def.Steps) != 2 || def.Steps[1].SLASeconds != 3600 || def.Steps[1].ApproverID != approver2 {
		t.Fatalf("submitted instance did not persist the pinned steps: %+v", def)
	}
	if def.Outcome == nil || def.Outcome.StatusField != "status" || def.Outcome.RejectedValue != "rejected" {
		t.Fatalf("submitted instance did not persist the pinned outcome: %+v", def.Outcome)
	}
}

// TestBPM_CreateInstance_RefusesUnpinned: NULL process_snapshot means ONE thing — "armed before
// migration 000065". A caller that forgets the Definition must fail loudly at the write boundary,
// not silently re-open the step-skip bug #8 closed by writing a NULL the engine reads as legacy.
func TestBPM_CreateInstance_RefusesUnpinned(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	base := bpm.CreateInstanceInput{
		TenantID: o3aTenantA, ProcessID: "00000000-0000-0000-0000-0000000000aa", EntityName: "quotes8c",
		RecordID: "00000000-0000-0000-0000-0000000000bb", CurrentApproverType: bpm.ApproverUser,
		CurrentApproverID: "00000000-0000-0000-0000-0000000000cc",
	}
	for name, def := range map[string]*bpm.Definition{"nil": nil, "no steps": {}} {
		in := base
		in.Definition = def
		if _, err := st.repo.CreateInstance(ctx, in); err == nil {
			t.Errorf("%s definition: CreateInstance succeeded; want a refusal", name)
		}
	}
	var n int
	if err := st.sdb.Get(&n, `SELECT COUNT(*) FROM sys_approval_instances WHERE tenant_id = ? AND entity_name = 'quotes8c'`, o3aTenantA); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d unpinned row(s) written; want 0", n)
	}
}

// TestBPM_AfterUpdateChangedTo_ArmsOnEdgeOnly (specs/043 S2b, US7 #1): the whole road — an
// after_update process "status vừa đổi thành 'Chờ duyệt'" saved through the admin validator, record
// edits through RecordService.Update (the only place the before-image exists), the async engine
// judging the edge. Editing another field starts nothing; the save that moves the status starts
// exactly one approval; and once that approval is over, a later edit of the still-"Chờ duyệt"
// record starts nothing again (what eq would have done).
func TestBPM_AfterUpdateChangedTo_ArmsOnEdgeOnly(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const rep = "11111111-0000-0000-0000-00000000002b"
	const approver = "22222222-0000-0000-0000-00000000002b"

	st.createEntity(t, ctx, tenant, "quotes2b", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
		{Name: "note", Label: "Note", FieldType: "text"},
	})
	if _, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: "gui-duyet-khi-cho-duyet", EntityName: "quotes2b", Trigger: "after_update",
		Rules:    []ApprovalCriteriaRuleInput{{Field: "status", Op: "changedTo", Value: "Chờ duyệt"}},
		Steps:    []ApprovalStepInput{{ApproverType: "user", ApproverID: approver}},
		IsActive: boolPtr(true), ActorID: rep,
	}); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}

	id, err := st.recordSvc.Create(ctx, bpmAdmin(rep), tenant, rep, "quotes2b", map[string]any{"status": "Nháp", "note": "a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	update := func(patch map[string]any) {
		t.Helper()
		if err := st.recordSvc.Update(ctx, bpmAdmin(rep), tenant, "quotes2b", id, patch, nil); err != nil {
			t.Fatalf("Update %v: %v", patch, err)
		}
	}
	update(map[string]any{"note": "b"})           // status untouched
	update(map[string]any{"status": "Chờ duyệt"}) // the edge

	// The engine is async: wait for the one arm rather than draining (the stack is reused below).
	var n int
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		n = st.instanceCount(t, tenant, "quotes2b", id)
		if n > 0 || time.Now().After(deadline) {
			break
		}
	}
	if n != 1 {
		t.Fatalf("after the note edit + the status change: %d instance(s); want exactly 1", n)
	}
	inst := st.onlyPendingInstance(t, tenant, "quotes2b", id)
	if _, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: approver}); err != nil {
		t.Fatalf("approve: %v", err)
	}

	update(map[string]any{"note": "c"}) // still "Chờ duyệt", nothing pending — eq would re-arm here
	st.drain(t)
	if got := st.instanceCount(t, tenant, "quotes2b", id); got != 1 {
		t.Fatalf("a later edit of a record already in 'Chờ duyệt' re-armed: %d instances; want 1", got)
	}
}
