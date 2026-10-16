//go:build integration

// EPIC 4 M1 — Workflow/Automation engine, exercised END-TO-END through the RecordService over a
// freshly-migrated throwaway MySQL DB (reuses o3aSetup, which runs ALL migrations incl. 000016
// sys_workflows + 000017 sys_notifications via the REAL golang-migrate CLI — not AutoMigrate).
//
// What this proves that the package unit tests (internal/workflow/workflow_test.go, fakes only)
// cannot: the WHOLE stack wired exactly as cmd/server/main.go does —
//
//	RecordService.{Create,Update,Delete}  (after-commit Emit)
//	  → workflow.AsyncDispatcher           (async worker pool, fail-open)
//	    → workflow.CachedLoader            (TTL cache, tenant-scoped key)
//	      → workflow.SQLRepository.ListActive   (real sys_workflows query)
//	    → services.WorkflowRecordMutator   (real dynrepo.UpdateRecord, system scope)
//	    → workflow.SQLRepository.InsertNotification (real sys_notifications row)
//
// — fires correctly against a real database, with the workflow rules themselves registered through
// the production WorkflowService admin path (validation + cache-invalidate after commit).
//
// Covered (task acceptance):
//   - after_update + condition MATCHES → field_update runs (the record is patched).
//   - condition does NOT match → the action is SKIPPED (record untouched).
//   - action ERROR → fail-open: the committed mutation is unaffected; no panic; engine drains.
//   - in_app_notification writes the correct sys_notifications row (tenant/user/message/links).
//   - tenant isolation: tenant B's record mutation NEVER fires tenant A's rule.
//   - after_create fires; after_delete fires in_app_notification (field_update is a no-op).
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run Workflow
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

// wfStack is the fully-wired workflow + record composition a single integration test drives.
type wfStack struct {
	sdb         *sqlx.DB
	entitySvc   *EntityService
	recordSvc   *RecordService
	workflowSvc *WorkflowService
	dispatcher  *workflow.AsyncDispatcher
	cache       *workflow.CachedLoader
}

// wfWire stands up the production composition (mirroring cmd/server/main.go) over a freshly
// migrated throwaway DB. It uses a SHORT TTL on the cache and a single-worker dispatcher so a
// test can register a rule, drive a mutation, then deterministically drain.
//
// IMPORTANT: a rule registered via workflowSvc.CreateWorkflow invalidates the cache after commit,
// so the engine reloads the fresh rule on the next Emit — no stale-cache race in the test.
func wfWire(t *testing.T) *wfStack {
	t.Helper()
	gdb, sdb := o3aSetup(t)

	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	mc := NewMetaCache(metaRepo, DefaultMetaTTL)

	entitySvc := NewEntityService(dynRepo, metaRepo).WithMetaCache(mc)

	// Real workflow stack (mirrors main.go: the mutator carries the WF-01 catalog guard).
	wfRepo := workflow.NewSQLRepository(sdb)
	wfCache := workflow.NewCachedLoader(wfRepo, 500*time.Millisecond)
	wfMutator := NewWorkflowRecordMutator(dynRepo).WithCatalog(mc)
	dispatcher := workflow.NewAsyncDispatcher(
		workflow.DispatcherConfig{Buffer: 64, Workers: 1},
		wfCache, wfMutator, wfRepo, nil,
	).WithRunRecorder(wfRepo)
	wfMutator.WithEmitter(dispatcher.Emit) // #330: a related write fires the target's rules, as in main.go
	workflowSvc := NewWorkflowService(wfRepo).WithEntityChecker(mc).WithCache(wfCache)

	recordSvc := NewRecordService(mc, dynRepo).WithEngine(dispatcher)

	st := &wfStack{
		sdb:         sdb,
		entitySvc:   entitySvc,
		recordSvc:   recordSvc,
		workflowSvc: workflowSvc,
		dispatcher:  dispatcher,
		cache:       wfCache,
	}
	return st
}

// createEntity registers a dynamic entity (and its obj_ table) so the test can drive records.
func (s *wfStack) createEntity(t *testing.T, ctx context.Context, tenant, name string, fields []FieldInput) {
	t.Helper()
	if _, _, err := s.entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name: name, Label: name, Fields: fields,
	}); err != nil {
		t.Fatalf("CreateEntity %s: %v", name, err)
	}
}

// drain shuts the dispatcher down (graceful drain), blocking until every emitted event has been
// processed. After drain the dispatcher is closed — a test emits all its events first, then drains
// once, then asserts the resulting DB state.
func (s *wfStack) drain(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.dispatcher.Shutdown(ctx); err != nil {
		t.Fatalf("dispatcher drain: %v", err)
	}
}

// wfAdmin is the admin record scope (full visibility, bypasses RLS) the tests mutate under.
func wfAdmin(userID string) RecordScope {
	return RecordScope{UserID: userID, AccessLevel: "all", IsAdmin: true}
}

// notifCount returns how many sys_notifications rows exist for (tenant, user).
func (s *wfStack) notifCount(t *testing.T, tenant, user string) int {
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
// after_update + condition MATCHES → field_update runs.
// ---------------------------------------------------------------------------

func TestWorkflow_AfterUpdate_ConditionMatch_RunsFieldUpdate(t *testing.T) {
	st := wfWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-000000000001"

	st.createEntity(t, ctx, tenant, "deals", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
		{Name: "stage", Label: "Stage", FieldType: "text"},
	})

	// Rule: on after_update of deals, IF status == "won" THEN set stage = "closed".
	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "won-closes",
		EntityName: "deals",
		Trigger:    "after_update",
		Rules:      []WorkflowRuleInput{{Field: "status", Op: "eq", Value: "won"}},
		Actions:    []WorkflowActionInput{{Type: "field_update", Field: "stage", Value: "closed"}},
		ActorID:    user,
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	id, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "deals", map[string]any{"status": "open", "stage": "new"})
	if err != nil {
		t.Fatalf("Create deal: %v", err)
	}

	// Update status → "won": the after_update workflow must fire and set stage = "closed".
	if err := st.recordSvc.Update(ctx, wfAdmin(user), tenant, "deals", id, map[string]any{"status": "won"}, nil); err != nil {
		t.Fatalf("Update status=won: %v", err)
	}

	st.drain(t) // wait for the async engine to finish.

	rec, err := st.recordSvc.Get(ctx, wfAdmin(user), tenant, "deals", id, nil, nil)
	if err != nil {
		t.Fatalf("Get after fire: %v", err)
	}
	if got, _ := rec.Data["stage"].(string); got != "closed" {
		t.Fatalf("field_update did NOT run: stage=%q; want \"closed\" (after_update + status==won)", got)
	}
}

// ---------------------------------------------------------------------------
// condition does NOT match → action SKIPPED.
// ---------------------------------------------------------------------------

func TestWorkflow_AfterUpdate_ConditionNoMatch_Skips(t *testing.T) {
	st := wfWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-000000000002"

	st.createEntity(t, ctx, tenant, "deals2", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
		{Name: "stage", Label: "Stage", FieldType: "text"},
	})

	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "won-closes",
		EntityName: "deals2",
		Trigger:    "after_update",
		Rules:      []WorkflowRuleInput{{Field: "status", Op: "eq", Value: "won"}},
		Actions:    []WorkflowActionInput{{Type: "field_update", Field: "stage", Value: "closed"}},
		ActorID:    user,
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	id, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "deals2", map[string]any{"status": "open", "stage": "new"})
	if err != nil {
		t.Fatalf("Create deal: %v", err)
	}

	// Update status → "lost" (NOT "won"): the condition fails, the field_update must be skipped.
	if err := st.recordSvc.Update(ctx, wfAdmin(user), tenant, "deals2", id, map[string]any{"status": "lost"}, nil); err != nil {
		t.Fatalf("Update status=lost: %v", err)
	}

	st.drain(t)

	rec, err := st.recordSvc.Get(ctx, wfAdmin(user), tenant, "deals2", id, nil, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got, _ := rec.Data["stage"].(string); got != "new" {
		t.Fatalf("non-matching condition fired anyway: stage=%q; want \"new\" (unchanged)", got)
	}
}

// ---------------------------------------------------------------------------
// action ERROR → fail-open: the committed mutation is unaffected; engine drains; no panic.
// ---------------------------------------------------------------------------

func TestWorkflow_ActionError_FailOpen_MutationUnaffected(t *testing.T) {
	st := wfWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-000000000003"

	st.createEntity(t, ctx, tenant, "deals3", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
	})

	// Rule's field_update targets a field that does NOT exist on the entity ("ghost"). WF-01:
	// CreateWorkflow now REJECTS such a rule at config time (RECORD_UNKNOWN_FIELD), so this
	// pre-existing-bad-rule scenario is seeded straight through the repo (bypassing validation,
	// like a row written before the guard existed). The mutator's execution-time guard rejects
	// it, so the action errors; the engine must swallow it (fail-open) — the original update
	// (status="won") stays committed.
	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "broken-action",
		EntityName: "deals3",
		Trigger:    "after_update",
		Actions:    []WorkflowActionInput{{Type: "field_update", Field: "ghost", Value: "x"}},
		ActorID:    user,
	}); err == nil {
		t.Fatalf("WF-01: CreateWorkflow with an unknown field_update target must be rejected at config time")
	}
	wfRepo := workflow.NewSQLRepository(st.sdb)
	if _, err := wfRepo.Create(ctx, workflow.CreateInput{
		TenantID:   tenant,
		Name:       "broken-action",
		EntityName: "deals3",
		Trigger:    workflow.TriggerAfterUpdate,
		Actions:    []byte(`[{"type":"field_update","field":"ghost","value":"x"}]`),
		IsActive:   true,
		CreatedBy:  user,
	}); err != nil {
		t.Fatalf("seed pre-existing bad rule via repo: %v", err)
	}

	id, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "deals3", map[string]any{"status": "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The Update itself must SUCCEED (the workflow runs async, after commit). A failing action
	// must NOT surface as an error from Update.
	if err := st.recordSvc.Update(ctx, wfAdmin(user), tenant, "deals3", id, map[string]any{"status": "won"}, nil); err != nil {
		t.Fatalf("Update must succeed despite a broken downstream workflow action: %v", err)
	}

	st.drain(t) // must not hang / panic even though the action errored.

	rec, err := st.recordSvc.Get(ctx, wfAdmin(user), tenant, "deals3", id, nil, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got, _ := rec.Data["status"].(string); got != "won" {
		t.Fatalf("committed mutation was affected by a failing workflow action: status=%q; want \"won\"", got)
	}
}

// ---------------------------------------------------------------------------
// in_app_notification writes the correct sys_notifications row.
// ---------------------------------------------------------------------------

func TestWorkflow_InAppNotification_WritesRow(t *testing.T) {
	st := wfWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const actor = "11111111-0000-0000-0000-000000000004"
	const recipient = "99999999-0000-0000-0000-000000000009"

	st.createEntity(t, ctx, tenant, "tickets", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
	})

	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "notify-on-resolve",
		EntityName: "tickets",
		Trigger:    "after_update",
		Rules:      []WorkflowRuleInput{{Field: "status", Op: "eq", Value: "resolved"}},
		Actions: []WorkflowActionInput{{
			Type: "in_app_notification", UserID: recipient, Title: "Resolved", Message: "Ticket resolved",
		}},
		ActorID: actor,
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	id, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, actor, "tickets", map[string]any{"status": "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := st.recordSvc.Update(ctx, wfAdmin(actor), tenant, "tickets", id, map[string]any{"status": "resolved"}, nil); err != nil {
		t.Fatalf("Update status=resolved: %v", err)
	}

	st.drain(t)

	// Exactly one notification row for the recipient, linked back to the ticket.
	if got := st.notifCount(t, tenant, recipient); got != 1 {
		t.Fatalf("notification rows for recipient = %d; want 1", got)
	}
	var row struct {
		Message    string  `db:"message"`
		Title      *string `db:"title"`
		EntityName *string `db:"entity_name"`
		RecordID   *string `db:"record_id"`
		WorkflowID *string `db:"workflow_id"`
		IsRead     bool    `db:"is_read"`
	}
	if err := st.sdb.Get(&row,
		"SELECT message, title, entity_name, record_id, workflow_id, is_read FROM sys_notifications WHERE tenant_id = ? AND user_id = ?",
		tenant, recipient,
	); err != nil {
		t.Fatalf("read notification row: %v", err)
	}
	if row.Message != "Ticket resolved" {
		t.Errorf("notification message=%q; want \"Ticket resolved\"", row.Message)
	}
	if row.Title == nil || *row.Title != "Resolved" {
		t.Errorf("notification title=%v; want \"Resolved\"", row.Title)
	}
	if row.EntityName == nil || *row.EntityName != "tickets" {
		t.Errorf("notification entity_name=%v; want \"tickets\"", row.EntityName)
	}
	if row.RecordID == nil || *row.RecordID != id {
		t.Errorf("notification record_id=%v; want %q", row.RecordID, id)
	}
	if row.WorkflowID == nil || *row.WorkflowID == "" {
		t.Errorf("notification workflow_id not linked: %v", row.WorkflowID)
	}
	if row.IsRead {
		t.Errorf("new notification should be unread (is_read=0)")
	}
}

// ---------------------------------------------------------------------------
// "$owner" sentinel resolves to the record's REAL creator end-to-end (M7 MED-1 fix, review
// 2026-07-08) — proves the plumbing RecordService.{Create,Update} → workflow.Event.OwnerUserID
// → runInAppNotification actually wires up, not just the isolated actions.go unit test.
// ---------------------------------------------------------------------------

// TestWorkflow_InAppNotification_OwnerSentinel_ResolvesToCreator drives a REAL after_create rule
// whose action targets the "$owner" sentinel. Before the MED-1 fix the literal string "$owner"
// was written as sys_notifications.user_id (a notification to a nonexistent user, silently
// dead); this proves the resolved row lands on the ACTUAL creator instead.
func TestWorkflow_InAppNotification_OwnerSentinel_ResolvesToCreator(t *testing.T) {
	st := wfWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const creator = "11111111-0000-0000-0000-00000000000c" // the record's owner (created_by)

	st.createEntity(t, ctx, tenant, "treatments", []FieldInput{
		{Name: "title", Label: "Title", FieldType: "text"},
	})

	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "notify-owner-on-create",
		EntityName: "treatments",
		Trigger:    "after_create",
		Actions: []WorkflowActionInput{{
			Type: "in_app_notification", UserID: workflow.OwnerSentinel, Message: "Hồ sơ mới của bạn",
		}},
		ActorID: creator,
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	id, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, creator, "treatments", map[string]any{"title": "Khám tổng quát"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	st.drain(t)

	// The resolved recipient is the CREATOR — never the literal sentinel string.
	if got := st.notifCount(t, tenant, creator); got != 1 {
		t.Fatalf("notification rows for creator %q = %d; want 1 (owner sentinel must resolve to created_by)", creator, got)
	}
	if got := st.notifCount(t, tenant, workflow.OwnerSentinel); got != 0 {
		t.Fatalf("the literal sentinel %q must NEVER be persisted as user_id, found %d rows", workflow.OwnerSentinel, got)
	}
	var recordID string
	if err := st.sdb.Get(&recordID,
		"SELECT record_id FROM sys_notifications WHERE tenant_id = ? AND user_id = ?", tenant, creator,
	); err != nil {
		t.Fatalf("read notification row: %v", err)
	}
	if recordID != id {
		t.Errorf("notification record_id=%q; want the created record %q", recordID, id)
	}
}

// TestWorkflow_InAppNotification_OwnerSentinel_UpdateResolvesToOriginalCreator proves the
// after_update path resolves "$owner" to the record's ORIGINAL creator, not the user performing
// this update (evt.ActorUserID is the mutator, not the owner — the MED-1 fix must never conflate
// the two).
func TestWorkflow_InAppNotification_OwnerSentinel_UpdateResolvesToOriginalCreator(t *testing.T) {
	st := wfWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const creator = "11111111-0000-0000-0000-00000000000d" // original owner
	const editor = "22222222-0000-0000-0000-00000000000e"  // performs the update (NOT the owner)

	st.createEntity(t, ctx, tenant, "treatments2", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
	})

	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "notify-owner-on-update",
		EntityName: "treatments2",
		Trigger:    "after_update",
		Rules:      []WorkflowRuleInput{{Field: "status", Op: "eq", Value: "done"}},
		Actions: []WorkflowActionInput{{
			Type: "in_app_notification", UserID: workflow.OwnerSentinel, Message: "Hồ sơ của bạn đã xong",
		}},
		ActorID: creator,
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	id, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, creator, "treatments2", map[string]any{"status": "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A DIFFERENT user (editor) performs the update that fires the rule.
	if err := st.recordSvc.Update(ctx, wfAdmin(editor), tenant, "treatments2", id, map[string]any{"status": "done"}, nil); err != nil {
		t.Fatalf("Update status=done: %v", err)
	}
	st.drain(t)

	if got := st.notifCount(t, tenant, creator); got != 1 {
		t.Fatalf("notification rows for original creator %q = %d; want 1", creator, got)
	}
	if got := st.notifCount(t, tenant, editor); got != 0 {
		t.Fatalf("the update's ACTOR must never receive the owner notification, found %d rows for %q", got, editor)
	}
}

// TestWorkflow_InAppNotification_OwnerSentinel_DeleteResolvesToOriginalCreator proves the
// after_delete path — where field_update is a no-op but in_app_notification still fires (a
// plausible real rule: "notify the owner their record was deleted") — resolves "$owner" to the
// record's original creator, not the user performing the delete. Structurally identical to the
// Update proof above (both read the before-image's created_by), covered directly rather than by
// analogy since after_delete is the one modified emit site otherwise only exercised by field_update
// tests.
func TestWorkflow_InAppNotification_OwnerSentinel_DeleteResolvesToOriginalCreator(t *testing.T) {
	st := wfWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const creator = "11111111-0000-0000-0000-00000000000f" // original owner
	const deleter = "22222222-0000-0000-0000-000000000010" // performs the delete (NOT the owner)

	st.createEntity(t, ctx, tenant, "treatments3", []FieldInput{
		{Name: "title", Label: "Title", FieldType: "text"},
	})

	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "notify-owner-on-delete",
		EntityName: "treatments3",
		Trigger:    "after_delete",
		Actions: []WorkflowActionInput{{
			Type: "in_app_notification", UserID: workflow.OwnerSentinel, Message: "Hồ sơ của bạn đã bị xoá",
		}},
		ActorID: creator,
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	id, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, creator, "treatments3", map[string]any{"title": "Sổ khám cũ"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A DIFFERENT user (deleter) performs the delete that fires the rule.
	if err := st.recordSvc.Delete(ctx, wfAdmin(deleter), tenant, "treatments3", id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	st.drain(t)

	if got := st.notifCount(t, tenant, creator); got != 1 {
		t.Fatalf("notification rows for original creator %q = %d; want 1", creator, got)
	}
	if got := st.notifCount(t, tenant, deleter); got != 0 {
		t.Fatalf("the delete's ACTOR must never receive the owner notification, found %d rows for %q", got, deleter)
	}
}

// ---------------------------------------------------------------------------
// tenant isolation: tenant B's mutation must NEVER fire tenant A's rule.
// ---------------------------------------------------------------------------

func TestWorkflow_TenantIsolation(t *testing.T) {
	st := wfWire(t)
	ctx := context.Background()
	const userA = "aaaa1111-0000-0000-0000-000000000001"
	const userB = "bbbb1111-0000-0000-0000-000000000001"

	// Same entity name in both tenants (shared physical obj_ table, per-tenant metadata).
	st.createEntity(t, ctx, o3aTenantA, "leads", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
		{Name: "stage", Label: "Stage", FieldType: "text"},
	})
	st.createEntity(t, ctx, o3aTenantB, "leads", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
		{Name: "stage", Label: "Stage", FieldType: "text"},
	})

	// Rule lives ONLY in tenant A.
	if _, err := st.workflowSvc.CreateWorkflow(ctx, o3aTenantA, CreateWorkflowInput{
		Name:       "a-only",
		EntityName: "leads",
		Trigger:    "after_update",
		Rules:      []WorkflowRuleInput{{Field: "status", Op: "eq", Value: "won"}},
		Actions:    []WorkflowActionInput{{Type: "field_update", Field: "stage", Value: "A_FIRED"}},
		ActorID:    userA,
	}); err != nil {
		t.Fatalf("CreateWorkflow (tenant A): %v", err)
	}

	// Tenant B creates + updates its OWN lead to status=won. Tenant A's rule must NOT fire on it.
	idB, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, o3aTenantB, userB, "leads", map[string]any{"status": "open", "stage": "new"})
	if err != nil {
		t.Fatalf("Create B lead: %v", err)
	}
	if err := st.recordSvc.Update(ctx, wfAdmin(userB), o3aTenantB, "leads", idB, map[string]any{"status": "won"}, nil); err != nil {
		t.Fatalf("Update B lead: %v", err)
	}

	st.drain(t)

	recB, err := st.recordSvc.Get(ctx, wfAdmin(userB), o3aTenantB, "leads", idB, nil, nil)
	if err != nil {
		t.Fatalf("Get B lead: %v", err)
	}
	if got, _ := recB.Data["stage"].(string); got != "new" {
		t.Fatalf("TENANT LEAK: tenant A's rule fired on tenant B's record (stage=%q; want \"new\" untouched)", got)
	}
}

// ---------------------------------------------------------------------------
// after_create fires; after_delete fires in_app_notification (field_update no-op on delete).
// ---------------------------------------------------------------------------

func TestWorkflow_AfterCreate_And_AfterDelete(t *testing.T) {
	st := wfWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-000000000005"
	const recipient = "88888888-0000-0000-0000-000000000008"

	st.createEntity(t, ctx, tenant, "orders", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
		{Name: "flag", Label: "Flag", FieldType: "text"},
	})

	// after_create rule: unconditionally set flag = "seen".
	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "stamp-on-create",
		EntityName: "orders",
		Trigger:    "after_create",
		Actions:    []WorkflowActionInput{{Type: "field_update", Field: "flag", Value: "seen"}},
		ActorID:    user,
	}); err != nil {
		t.Fatalf("CreateWorkflow after_create: %v", err)
	}
	// after_delete rule: notify the recipient. field_update would be a no-op on a deleted record,
	// so we use in_app_notification (which still fires on delete).
	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "notify-on-delete",
		EntityName: "orders",
		Trigger:    "after_delete",
		Actions:    []WorkflowActionInput{{Type: "in_app_notification", UserID: recipient, Message: "Order deleted"}},
		ActorID:    user,
	}); err != nil {
		t.Fatalf("CreateWorkflow after_delete: %v", err)
	}

	id, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "orders", map[string]any{"status": "open"})
	if err != nil {
		t.Fatalf("Create order: %v", err)
	}

	// The after_create field_update runs ASYNC. Delete is synchronous and would race it (deleting
	// the row before the stamp lands). Wait for the create-time stamp to commit FIRST, so the two
	// phases are ordered deterministically, THEN delete. (Single dispatcher: it can't be drained
	// mid-test — Shutdown is one-shot — so we poll the committed effect instead.)
	rawFlag := func() *string {
		var f *string
		if err := st.sdb.Get(&f,
			"SELECT JSON_UNQUOTE(JSON_EXTRACT(data, '$.flag')) FROM obj_orders WHERE tenant_id = ? AND id = ?",
			tenant, id,
		); err != nil {
			t.Fatalf("read raw obj_orders.flag: %v", err)
		}
		return f
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if f := rawFlag(); f != nil && *f == "seen" {
			break // after_create field_update committed.
		}
		if time.Now().After(deadline) {
			t.Fatalf("after_create field_update did not stamp flag within deadline: got %v; want \"seen\"", rawFlag())
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := st.recordSvc.Delete(ctx, wfAdmin(user), tenant, "orders", id); err != nil {
		t.Fatalf("Delete order: %v", err)
	}

	st.drain(t) // wait for the after_delete notification to be written.

	// The after_delete notification must have been written (field_update is a no-op on delete).
	if got := st.notifCount(t, tenant, recipient); got != 1 {
		t.Fatalf("after_delete notification rows = %d; want 1", got)
	}
}
