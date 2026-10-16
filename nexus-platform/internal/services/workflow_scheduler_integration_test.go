//go:build integration

// EPIC 4 M2 — Workflow SCHEDULER + email_alert + outbound_webhook, exercised END-TO-END over a
// freshly-migrated throwaway MySQL DB (reuses o3aSetup, which runs ALL migrations incl. 000019
// sys_workflows scheduler columns via the REAL golang-migrate CLI).
//
// What this proves that the package unit tests (fakes only) cannot — the WHOLE M2 stack wired as
// cmd/server/main.go does, against a real database:
//
//	workflow.Scheduler.runDue
//	  → workflow.SQLRepository.ListDueScheduled / ClaimScheduledRun   (real sys_workflows CAS)
//	    → services.WorkflowRecordLister → dynrepo.ListRecords          (real obj_* scan, admin scope)
//	      → Evaluate(condition) per record
//	        → field_update  → dynrepo.UpdateRecord                     (real patch)
//	        → email_alert   → EmailSender                              (captured)
//	        → outbound_webhook → HTTPWebhookSender                     (real POST to httptest)
//
// Covered (task acceptance):
//   - scheduled rule fires exactly ONCE per matching record (field_update patches the record).
//   - email_alert action calls the email sender with the {{field}}-rendered subject/body.
//   - outbound_webhook action POSTs the record JSON (+ HMAC) to the endpoint, with retry.
//   - claim CAS: a second scheduler pass in the same window does NOT double-fire.
//   - fail-open: a broken action (bad webhook URL down) never breaks the pass; other actions run.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run WorkflowScheduler
package services

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// capturingEmailSender records every SendEmail call for assertions.
type capturingEmailSender struct {
	mu    sync.Mutex
	calls []struct{ to, subject, body string }
}

func (c *capturingEmailSender) SendEmail(_ context.Context, to, subject, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, struct{ to, subject, body string }{to, subject, body})
	return nil
}

func (c *capturingEmailSender) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// schedStack is the fully-wired scheduler composition a single integration test drives.
type schedStack struct {
	sdb         *sqlx.DB
	entitySvc   *EntityService
	recordSvc   *RecordService
	workflowSvc *WorkflowService
	scheduler   *workflow.Scheduler
	repo        *workflow.SQLRepository
	email       *capturingEmailSender
}

// schedWire stands up the production scheduler composition over a freshly migrated throwaway DB.
// engineDeps (email + webhook senders) are injected through a real (but unstarted) AsyncDispatcher
// so the scheduler shares the same action adapters main.go wires. extra wires more deps onto the
// engine BEFORE the scheduler snapshots them (the durable webhook queue, as main.go does).
func schedWire(t *testing.T, webhookSender workflow.WebhookSender, extra ...func(*workflow.AsyncDispatcher, *sqlx.DB)) *schedStack {
	t.Helper()
	gdb, sdb := o3aSetup(t)

	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	mc := NewMetaCache(metaRepo, DefaultMetaTTL)
	entitySvc := NewEntityService(dynRepo, metaRepo).WithMetaCache(mc)

	wfRepo := workflow.NewSQLRepository(sdb)
	wfCache := workflow.NewCachedLoader(wfRepo, 500*time.Millisecond)
	wfMutator := NewWorkflowRecordMutator(dynRepo)
	email := &capturingEmailSender{}

	// An AsyncDispatcher carries the action deps the scheduler reuses (not started here — the
	// scheduler drives runDue directly so the test is deterministic).
	engine := workflow.NewAsyncDispatcher(
		workflow.DispatcherConfig{Buffer: 16, Workers: 1},
		wfCache, wfMutator, wfRepo, nil,
	).WithEmailSender(email).WithWebhookSender(webhookSender)
	for _, wire := range extra {
		wire(engine, sdb)
	}

	lister := NewWorkflowRecordLister(dynRepo)
	scheduler := workflow.NewScheduler(
		workflow.SchedulerConfig{Interval: time.Minute}, wfRepo, lister, engine, nil)

	workflowSvc := NewWorkflowService(wfRepo).WithEntityChecker(mc).WithCache(wfCache)
	recordSvc := NewRecordService(mc, dynRepo).WithEngine(engine)

	return &schedStack{
		sdb: sdb, entitySvc: entitySvc, recordSvc: recordSvc,
		workflowSvc: workflowSvc, scheduler: scheduler, repo: wfRepo, email: email,
	}
}

func (s *schedStack) createEntity(t *testing.T, ctx context.Context, tenant, name string, fields []FieldInput) {
	t.Helper()
	if _, _, err := s.entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name: name, Label: name, Fields: fields,
	}); err != nil {
		t.Fatalf("CreateEntity %s: %v", name, err)
	}
}

// rawField reads a single JSON field of a record straight from the obj_ table (bypassing the
// service) so the test asserts the committed effect of a scheduled field_update.
func (s *schedStack) rawField(t *testing.T, table, tenant, id, field string) *string {
	t.Helper()
	var v *string
	if err := s.sdb.Get(&v,
		"SELECT JSON_UNQUOTE(JSON_EXTRACT(data, CONCAT('$.', ?))) FROM `"+table+"` WHERE tenant_id = ? AND id = ?",
		field, tenant, id,
	); err != nil {
		t.Fatalf("read raw %s.%s: %v", table, field, err)
	}
	return v
}

// ---------------------------------------------------------------------------
// scheduled rule fires once + email_alert + outbound_webhook.
// ---------------------------------------------------------------------------

func TestWorkflowScheduler_FiresFieldUpdateEmailWebhookOnce(t *testing.T) {
	// httptest endpoint captures webhook POSTs (counts + signature header presence).
	var webhookHits int32
	var sawSig int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&webhookHits, 1)
		if r.Header.Get(workflow.SignatureHeader) != "" {
			atomic.AddInt32(&sawSig, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := workflow.NewHTTPWebhookSender(2*time.Second, 3, 10*time.Millisecond)
	st := schedWire(t, sender)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-000000000010"

	st.createEntity(t, ctx, tenant, "invoices", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
		{Name: "stage", Label: "Stage", FieldType: "text"},
		{Name: "owner_email", Label: "Owner Email", FieldType: "text"},
	})

	// Scheduled rule: IF status == "overdue" THEN field_update stage="reminded" + email + webhook.
	active := true
	rule, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:            "overdue-reminder",
		EntityName:      "invoices",
		Trigger:         "scheduled",
		IntervalSeconds: 0, // fire whenever due
		IsActive:        &active,
		Rules:           []WorkflowRuleInput{{Field: "status", Op: "eq", Value: "overdue"}},
		Actions: []WorkflowActionInput{
			{Type: "field_update", Field: "stage", Value: "reminded"},
			{Type: "email_alert", To: "{{owner_email}}", Subject: "Invoice {{status}}", Body: "Please pay"},
			{Type: "outbound_webhook", URL: srv.URL},
		},
		ActorID: user,
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	// The signing key as a rule saved before specs/050 S3a carries it — no write path accepts one now.
	seedLegacyWebhookActions(t, st.sdb, tenant, rule.ID, []workflow.Action{
		{Type: workflow.ActionFieldUpdate, Field: "stage", Value: "reminded"},
		{Type: workflow.ActionEmailAlert, To: "{{owner_email}}", Subject: "Invoice {{status}}", Body: "Please pay"},
		{Type: workflow.ActionWebhook, URL: srv.URL, Secret: "k"},
	})

	// One matching record, one non-matching.
	idMatch, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "invoices",
		map[string]any{"status": "overdue", "stage": "new", "owner_email": "ar@acme.test"})
	if err != nil {
		t.Fatalf("Create matching invoice: %v", err)
	}
	idNoMatch, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "invoices",
		map[string]any{"status": "paid", "stage": "new", "owner_email": "ar@acme.test"})
	if err != nil {
		t.Fatalf("Create non-matching invoice: %v", err)
	}

	// Run ONE scheduler pass.
	st.scheduler.RunDueForTest(ctx, time.Now())

	// field_update ran on the match, NOT on the non-match.
	if got := st.rawField(t, "obj_invoices", tenant, idMatch, "stage"); got == nil || *got != "reminded" {
		t.Fatalf("matching record stage = %v; want \"reminded\"", got)
	}
	if got := st.rawField(t, "obj_invoices", tenant, idNoMatch, "stage"); got == nil || *got != "new" {
		t.Fatalf("non-matching record stage = %v; want \"new\" (untouched)", got)
	}
	// email_alert fired once with rendered subject.
	if st.email.count() != 1 {
		t.Fatalf("email sends = %d; want 1", st.email.count())
	}
	if st.email.calls[0].to != "ar@acme.test" || st.email.calls[0].subject != "Invoice overdue" {
		t.Errorf("email = %+v; want rendered to/subject", st.email.calls[0])
	}
	// webhook POSTed once with a signature.
	if got := atomic.LoadInt32(&webhookHits); got != 1 {
		t.Fatalf("webhook hits = %d; want 1", got)
	}
	if got := atomic.LoadInt32(&sawSig); got != 1 {
		t.Errorf("webhook signature header missing (HMAC), sawSig=%d", got)
	}

	// SECOND pass in the same window: claim CAS must NOT double-fire (no extra email/webhook).
	st.scheduler.RunDueForTest(ctx, time.Now())
	if st.email.count() != 1 {
		t.Errorf("second pass double-fired email: sends = %d; want still 1", st.email.count())
	}
	if got := atomic.LoadInt32(&webhookHits); got != 1 {
		t.Errorf("second pass double-fired webhook: hits = %d; want still 1", got)
	}
}

// ---------------------------------------------------------------------------
// "$owner" sentinel resolves from the REAL obj_* created_by column end-to-end through the
// scheduler's real dynrepo-backed lister (M7 MED-1 fix, review 2026-07-08) — the one hop the
// workflow-package scheduler unit test (fakeRecordLister) cannot prove: that
// services.WorkflowRecordLister actually SELECTs created_by from the real table and carries it
// through to ScheduledRecord.CreatedBy.
// ---------------------------------------------------------------------------

func TestWorkflowScheduler_OwnerSentinel_ResolvesFromRealCreatedBy(t *testing.T) {
	st := schedWire(t, nil)
	ctx := context.Background()
	const tenant = o3aTenantA
	const creator = "11111111-0000-0000-0000-000000000012"

	st.createEntity(t, ctx, tenant, "reminders", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
	})

	active := true
	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "owner-reminder",
		EntityName: "reminders",
		Trigger:    "scheduled",
		IsActive:   &active,
		Actions: []WorkflowActionInput{
			{Type: "in_app_notification", UserID: workflow.OwnerSentinel, Message: "Reminder due"},
		},
		ActorID: creator,
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	id, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, creator, "reminders", map[string]any{"status": "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	st.scheduler.RunDueForTest(ctx, time.Now())

	var n int
	if err := st.sdb.Get(&n,
		"SELECT COUNT(*) FROM sys_notifications WHERE tenant_id = ? AND user_id = ? AND record_id = ?",
		tenant, creator, id,
	); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	if n != 1 {
		t.Fatalf("notification rows for creator %q = %d; want 1 (scheduled $owner must resolve via the real obj_* created_by column)", creator, n)
	}
	var deadRows int
	if err := st.sdb.Get(&deadRows,
		"SELECT COUNT(*) FROM sys_notifications WHERE tenant_id = ? AND user_id = ?",
		tenant, workflow.OwnerSentinel,
	); err != nil {
		t.Fatalf("count sentinel rows: %v", err)
	}
	if deadRows != 0 {
		t.Fatalf("the literal sentinel %q must NEVER be persisted as user_id, found %d rows", workflow.OwnerSentinel, deadRows)
	}
}

// ---------------------------------------------------------------------------
// fail-open: a broken action (webhook to a closed endpoint) never breaks the pass.
// ---------------------------------------------------------------------------

func TestWorkflowScheduler_FailOpenOnWebhookDown(t *testing.T) {
	// Point the webhook at a server we immediately close → every POST fails (connection refused).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	downURL := srv.URL
	srv.Close()

	sender := workflow.NewHTTPWebhookSender(500*time.Millisecond, 2, 5*time.Millisecond)
	st := schedWire(t, sender)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-000000000011"

	st.createEntity(t, ctx, tenant, "orders2", []FieldInput{
		{Name: "status", Label: "Status", FieldType: "text"},
		{Name: "stage", Label: "Stage", FieldType: "text"},
	})

	active := true
	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "broken-webhook",
		EntityName: "orders2",
		Trigger:    "scheduled",
		IsActive:   &active,
		Actions: []WorkflowActionInput{
			// webhook is down (fails), but field_update must still run (fail-open per action).
			{Type: "outbound_webhook", URL: downURL},
			{Type: "field_update", Field: "stage", Value: "done"},
		},
		ActorID: user,
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	id, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "orders2", map[string]any{"status": "x", "stage": "new"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Must not panic / hang even though the webhook errors on every retry.
	st.scheduler.RunDueForTest(ctx, time.Now())

	// The field_update (the action AFTER the failing webhook) still committed.
	if got := st.rawField(t, "obj_orders2", tenant, id, "stage"); got == nil || *got != "done" {
		t.Fatalf("field_update did not run after a failing webhook: stage = %v; want \"done\"", got)
	}
}
