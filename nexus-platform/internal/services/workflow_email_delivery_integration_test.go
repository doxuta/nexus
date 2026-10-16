//go:build integration

// specs/059 FR-006/007 end-to-end over a freshly migrated throwaway MySQL DB: a queued email that the
// queue gives up on turns ITS run failed in sys_workflow_runs — the builder's history — instead of
// the "success" the enqueue recorded. Wired as main.go: RecordService → AsyncDispatcher →
// EnqueuingWorkflowEmailSender (real sys_jobs) → jobs.Manager → NewEmailAlertJobHandler(transport,
// workflow.SQLRepository).
//
//	TEST_DATABASE_URL=… go test -tags=integration ./internal/services/ -run 'TestWorkflowEmailDelivery|TestMarkRunActionFailed'
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/emaillog"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/notify"
	"nexus/platform/internal/workflow"
)

const emlDeliveryTenant = "e4e4e4e4-0000-4000-8000-000000000056"

// deliveryStack wires one tenant with benh_nhan + lich_hen and an after_create mail rule to the
// linked patient, over a real queue whose workers deliver through transport.
func deliveryStack(t *testing.T, transport workflowEmailTransport, maxAttempts int) (*sqlx.DB, *RecordService, *jobs.Manager) {
	t.Helper()
	env := deliveryStackFull(t, transport, maxAttempts)
	return env.sdb, env.recordSvc, env.mgr
}

// deliveryEnv is deliveryStack plus the email-resend service wired as main.go wires it (specs/066 L2):
// the LIVE rule store via WithLiveRepo, the engine's own sender, the real email-log repository.
type deliveryEnv struct {
	sdb       *sqlx.DB
	recordSvc *RecordService
	mgr       *jobs.Manager
	workflows *WorkflowService
	resend    *EmailLogService
}

func deliveryStackFull(t *testing.T, transport workflowEmailTransport, maxAttempts int) deliveryEnv {
	t.Helper()
	gdb, sdb := o3aSetup(t)
	ctx := context.Background()
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	mc := NewMetaCache(metaRepo, DefaultMetaTTL)
	entitySvc := NewEntityService(dynRepo, metaRepo).WithMetaCache(mc)
	wfRepo := workflow.NewSQLRepository(sdb)
	wfCache := workflow.NewCachedLoader(wfRepo, 100*time.Millisecond)

	store := jobs.NewSQLStore(sdb, maxAttempts)
	if err := store.ScrubPayloadOnFinish(EmailAlertJobType); err != nil {
		t.Fatalf("scrub: %v", err)
	}
	reg := jobs.NewRegistry()
	if err := reg.Register(EmailAlertJobType, NewEmailAlertJobHandler(transport, wfRepo, emaillog.NewRepository(sdb), nil)); err != nil {
		t.Fatalf("register: %v", err)
	}
	mgr := jobs.NewManager(jobs.ManagerConfig{Workers: 1, PollInterval: 100 * time.Millisecond, Lease: time.Minute}, store, reg, nil)

	emailSender := NewEnqueuingWorkflowEmailSender(store).WithEmailLog(emaillog.NewRepository(sdb), nil)
	engine := workflow.NewAsyncDispatcher(workflow.DispatcherConfig{Buffer: 16, Workers: 1},
		wfCache, NewWorkflowRecordMutator(dynRepo), wfRepo, nil).
		WithEmailSender(emailSender).
		WithTemplateResolver(NewWorkflowTemplateResolver(mc, dynRepo)).
		WithRunRecorder(wfRepo)
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	workflowSvc := NewWorkflowService(wfRepo).WithEntityChecker(mc).WithCache(wfCache).
		WithLiveRepo(wfRepo).WithRecordReader(dynRepo).WithTemplateResolver(NewWorkflowTemplateResolver(mc, dynRepo))
	resend := NewEmailLogService(emaillog.NewRepository(sdb)).WithMailConfigured(true).WithQueueEnabled(true).
		WithRenderer(workflowSvc).WithSender(emailSender)
	recordSvc := NewRecordService(mc, dynRepo).WithEngine(engine)

	if _, _, err := entitySvc.CreateEntity(ctx, emlDeliveryTenant, CreateEntityInput{Name: "benh_nhan", Label: "Bệnh nhân", Fields: []FieldInput{
		{Name: "ho_ten", Label: "Họ tên", FieldType: "text", IsRequired: true},
		{Name: "email", Label: "Email", FieldType: "email"},
	}}); err != nil {
		t.Fatalf("CreateEntity benh_nhan: %v", err)
	}
	if _, _, err := entitySvc.CreateEntity(ctx, emlDeliveryTenant, CreateEntityInput{Name: "lich_hen", Label: "Lịch hẹn", Fields: []FieldInput{
		{Name: "tieu_de", Label: "Tiêu đề", FieldType: "text", IsRequired: true},
		{Name: "benh_nhan", Label: "Bệnh nhân", FieldType: "relation", Options: map[string]any{"relatedEntity": "benh_nhan"}},
	}}); err != nil {
		t.Fatalf("CreateEntity lich_hen: %v", err)
	}
	if _, err := workflowSvc.CreateWorkflow(ctx, emlDeliveryTenant, CreateWorkflowInput{
		Name: "Nhắc hẹn", EntityName: "lich_hen", Trigger: "after_create", ActorID: emlUser,
		Actions: []WorkflowActionInput{
			{Type: "in_app_notification", UserID: emlUser, Message: "slot 0 — not the email"},
			{Type: "email_alert", To: "{{benh_nhan.email}}", Subject: "Nhắc hẹn", Body: "b"},
		},
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	return deliveryEnv{sdb: sdb, recordSvc: recordSvc, mgr: mgr, workflows: workflowSvc, resend: resend}
}

func createLinkedAppointment(t *testing.T, recordSvc *RecordService, email string) string {
	t.Helper()
	ctx := context.Background()
	admin := RecordScope{UserID: emlUser, AccessLevel: "all", IsAdmin: true}
	patient, err := recordSvc.Create(ctx, admin, emlDeliveryTenant, emlUser, "benh_nhan", map[string]any{"ho_ten": "Lan", "email": email})
	if err != nil {
		t.Fatalf("create patient: %v", err)
	}
	appt, err := recordSvc.Create(ctx, admin, emlDeliveryTenant, emlUser, "lich_hen", map[string]any{"tieu_de": "Cạo vôi", "benh_nhan": patient})
	if err != nil {
		t.Fatalf("create appointment: %v", err)
	}
	return appt
}

type deliveryRow struct {
	RunStatus  string  `db:"run_status"`
	RunError   *string `db:"run_error"`
	Actions    string  `db:"actions"`
	JobTenant  *string `db:"job_tenant"`
	JobStatus  string  `db:"job_status"`
	JobPayload *string `db:"job_payload"`
}

// deliveryOutcome waits until the appointment's email job finished, then reads it with its run.
func deliveryOutcome(t *testing.T, sdb *sqlx.DB, appt string) deliveryRow {
	t.Helper()
	var row deliveryRow
	waitUntil(t, "the email job to finish", func() bool {
		err := sdb.Get(&row, `
			SELECT r.status AS run_status, r.error AS run_error, CAST(r.actions_result AS CHAR) AS actions,
			       j.tenant_id AS job_tenant, j.status AS job_status, CAST(j.payload AS CHAR) AS job_payload
			FROM sys_workflow_runs r
			JOIN sys_jobs j ON j.job_type = ? AND j.tenant_id = r.tenant_id
			WHERE r.tenant_id = ? AND r.record_id = ? AND j.status IN ('done','failed')`,
			EmailAlertJobType, emlDeliveryTenant, appt)
		return err == nil
	})
	return row
}

func TestWorkflowEmailDelivery_GivingUpTurnsTheRunFailed(t *testing.T) {
	const personal = "lan.canhan@gmail.com"
	// The server echoes the address it refuses — the mask must hold on the run too (#85/#96).
	echo := fmt.Errorf("notify.SMTPSender.Send: rcpt to: 550 5.1.1 <%s>: Recipient address rejected", personal)
	sdb, recordSvc, mgr := deliveryStack(t, &fakeMailTransport{err: echo}, 1)
	mgr.Start()
	defer stopManager(t, mgr)

	appt := createLinkedAppointment(t, recordSvc, personal)
	row := deliveryOutcome(t, sdb, appt)

	if row.JobTenant == nil || *row.JobTenant != emlDeliveryTenant || row.JobStatus != "failed" || row.JobPayload != nil {
		t.Fatalf("job = tenant %v status %q payload %v; want tenant-pinned, failed, payload scrubbed", row.JobTenant, row.JobStatus, row.JobPayload)
	}
	if row.RunStatus != "failed" || row.RunError == nil {
		t.Fatalf("run = %q / %v; want failed with the delivery reason", row.RunStatus, row.RunError)
	}
	var slots []workflow.ActionResult
	if err := json.Unmarshal([]byte(row.Actions), &slots); err != nil || len(slots) != 2 {
		t.Fatalf("actions_result %s: %v", row.Actions, err)
	}
	if !slots[0].OK || slots[1].OK || !strings.Contains(slots[1].Error, "Recipient address rejected") {
		t.Fatalf("slots = %+v; want slot 0 untouched, slot 1 (the email) failed with the SMTP reason", slots)
	}
	for _, text := range []string{*row.RunError, row.Actions} {
		if strings.Contains(strings.ToLower(text), personal) {
			t.Fatalf("the run carries the patient's address: %s", text)
		}
	}
}

func TestWorkflowEmailDelivery_NoMailConfiguredFailsAtOnce(t *testing.T) {
	// Default attempt budget (5): ErrMailNotConfigured must not burn it over 7 minutes.
	sdb, recordSvc, mgr := deliveryStack(t, notify.NewNoopSender(), 5)
	mgr.Start()
	defer stopManager(t, mgr)

	row := deliveryOutcome(t, sdb, createLinkedAppointment(t, recordSvc, "lan@nhakhoa.vn"))
	if row.JobStatus != "failed" || row.RunStatus != "failed" || row.RunError == nil || !strings.Contains(*row.RunError, "không thể thử lại") {
		t.Fatalf("job %q run %q error %v; want both failed on the first attempt, saying it cannot be retried", row.JobStatus, row.RunStatus, row.RunError)
	}
}

func TestWorkflowEmailDelivery_SentLeavesTheRunSuccessful(t *testing.T) {
	tr := &fakeMailTransport{}
	sdb, recordSvc, mgr := deliveryStack(t, tr, 1)
	mgr.Start()
	defer stopManager(t, mgr)

	row := deliveryOutcome(t, sdb, createLinkedAppointment(t, recordSvc, "lan@nhakhoa.vn"))
	if row.JobStatus != "done" || row.RunStatus != "success" || row.RunError != nil {
		t.Fatalf("job %q run %q error %v; want done + success", row.JobStatus, row.RunStatus, row.RunError)
	}
	if sent := tr.mails(); len(sent) != 1 || sent[0].To != "lan@nhakhoa.vn" {
		t.Fatalf("sent %+v; want exactly one mail to the linked patient", sent)
	}
}

// The write-back SQL on its own: slot marked on an event run, summary kept on a scheduled run,
// first error kept, another tenant's / a missing id answers not-found, a re-mark answers found.
func TestMarkRunActionFailed_Integration(t *testing.T) {
	_, sdb := o3aSetup(t)
	ctx := context.Background()
	repo := workflow.NewSQLRepository(sdb)
	now := time.Now().UTC()
	insert := func(id, tenant, status, errText, actions string) {
		t.Helper()
		if err := repo.InsertRun(ctx, workflow.RunInput{ID: id, TenantID: tenant, WorkflowID: "wf-1", TriggerType: "after_create",
			Status: workflow.RunStatus(status), Error: errText, ActionsResult: []byte(actions), StartedAt: now, FinishedAt: now}); err != nil {
			t.Fatalf("InsertRun %s: %v", id, err)
		}
	}
	const tA, tB = "a0a0a0a0-0000-4000-8000-000000000056", "b0b0b0b0-0000-4000-8000-000000000056"
	insert("10000000-0000-4000-8000-000000000001", tA, "success", "", `[{"type":"field_update","ok":true},{"type":"email_alert","ok":true}]`)
	insert("10000000-0000-4000-8000-000000000002", tA, "failed", "first error", `{"scanned":3,"matched":2,"failed":0}`)

	found, err := repo.MarkRunActionFailed(ctx, tA, "10000000-0000-4000-8000-000000000001", 1, "j", "gửi lỗi [redacted]")
	if err != nil || !found {
		t.Fatalf("mark event run: %v, found=%v", err, found)
	}
	var st, actions string
	var errText *string
	read := func(id string) {
		t.Helper()
		if err := sdb.QueryRowxContext(ctx, `SELECT status, error, CAST(actions_result AS CHAR) FROM sys_workflow_runs WHERE id = ?`, id).Scan(&st, &errText, &actions); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
	}
	read("10000000-0000-4000-8000-000000000001")
	var slots []map[string]any
	_ = json.Unmarshal([]byte(actions), &slots)
	if st != "failed" || errText == nil || *errText != "gửi lỗi [redacted]" || slots[0]["ok"] != true || slots[1]["ok"] != false || slots[1]["error"] != "gửi lỗi [redacted]" {
		t.Fatalf("event run = %s / %v / %s", st, errText, actions)
	}

	if found, err := repo.MarkRunActionFailed(ctx, tA, "10000000-0000-4000-8000-000000000002", 0, "j", "later"); err != nil || !found {
		t.Fatalf("mark scheduled run: %v, found=%v", err, found)
	}
	read("10000000-0000-4000-8000-000000000002")
	if st != "failed" || *errText != "first error" || !strings.Contains(actions, `"scanned": 3`) {
		t.Fatalf("scheduled run = %s / %v / %s; want the first error and the summary kept", st, *errText, actions)
	}

	for _, c := range []struct{ tenant, id string }{{tB, "10000000-0000-4000-8000-000000000001"}, {tA, "10000000-0000-4000-8000-00000000dead"}} {
		if found, err := repo.MarkRunActionFailed(ctx, c.tenant, c.id, 1, "j", "x"); err != nil || found {
			t.Errorf("tenant %s run %s: found=%v err=%v; want not found", c.tenant, c.id, found, err)
		}
	}
	if found, err := repo.MarkRunActionFailed(ctx, tA, "10000000-0000-4000-8000-000000000001", 1, "j", "gửi lỗi [redacted]"); err != nil || !found {
		t.Errorf("re-mark of an already-failed run: found=%v err=%v; want found", found, errors.Unwrap(err))
	}
}

// #339: the email's slot says where the mail is — "queued" while the queue still retries, "sent" once
// delivered ("failed" is the existing give-up path). The run itself stays success until the queue gives up.
func emailSlotDelivery(t *testing.T, sdb *sqlx.DB, appt string) (runStatus, delivery string) {
	t.Helper()
	var actions string
	if err := sdb.QueryRowx(`SELECT status, CAST(actions_result AS CHAR) FROM sys_workflow_runs WHERE tenant_id = ? AND record_id = ?`,
		emlDeliveryTenant, appt).Scan(&runStatus, &actions); err != nil {
		return "", ""
	}
	var slots []map[string]any
	if json.Unmarshal([]byte(actions), &slots) != nil || len(slots) != 2 {
		return runStatus, ""
	}
	delivery, _ = slots[1]["delivery"].(string)
	return runStatus, delivery
}

func TestWorkflowEmailDelivery_SlotIsQueuedWhileTheQueueRetries(t *testing.T) {
	// A transient SMTP failure with the default budget: after attempt 1 the job waits for its backoff.
	temp := fmt.Errorf("notify.SMTPSender.Send: 451 4.3.0 try again later")
	sdb, recordSvc, mgr := deliveryStack(t, &fakeMailTransport{err: temp}, 5)
	mgr.Start()
	defer stopManager(t, mgr)

	appt := createLinkedAppointment(t, recordSvc, "lan@nhakhoa.vn")
	waitUntil(t, "the first delivery attempt", func() bool {
		var n int
		return sdb.Get(&n, `SELECT COUNT(*) FROM sys_jobs WHERE job_type = ? AND tenant_id = ? AND attempts >= 1 AND status = 'queued'`,
			EmailAlertJobType, emlDeliveryTenant) == nil && n == 1
	})
	waitUntil(t, "the run row to carry the queued slot", func() bool {
		_, d := emailSlotDelivery(t, sdb, appt)
		return d != ""
	})
	if st, d := emailSlotDelivery(t, sdb, appt); st != "success" || d != "queued" {
		t.Fatalf("run %q slot delivery %q; want success + queued while the queue retries", st, d)
	}
}

func TestWorkflowEmailDelivery_SlotTurnsSentWhenDelivered(t *testing.T) {
	sdb, recordSvc, mgr := deliveryStack(t, &fakeMailTransport{}, 1)
	mgr.Start()
	defer stopManager(t, mgr)

	appt := createLinkedAppointment(t, recordSvc, "lan@nhakhoa.vn")
	row := deliveryOutcome(t, sdb, appt)
	if row.JobStatus != "done" {
		t.Fatalf("job %q; want done", row.JobStatus)
	}
	waitUntil(t, "the slot to turn sent", func() bool { _, d := emailSlotDelivery(t, sdb, appt); return d == "sent" })
	if st, d := emailSlotDelivery(t, sdb, appt); st != "success" || d != "sent" {
		t.Fatalf("run %q slot delivery %q; want success + sent", st, d)
	}
}

// #340: a scheduled firing writes ONE summary run for all its records; an email the queue gives up
// on must show in that summary's failed count — once per delivery, however often the give-up lands.
func TestMarkRunActionFailed_ScheduledSummaryCountsFailed(t *testing.T) {
	_, sdb := o3aSetup(t)
	ctx := context.Background()
	repo := workflow.NewSQLRepository(sdb)
	now := time.Now().UTC()
	const tenant, runID = "a0a0a0a0-0000-4000-8000-000000000340", "10000000-0000-4000-8000-000000000340"
	if err := repo.InsertRun(ctx, workflow.RunInput{ID: runID, TenantID: tenant, WorkflowID: "wf-s", TriggerType: "scheduled",
		Status: workflow.RunSuccess, ActionsResult: []byte(`{"scanned":5,"matched":3,"failed":0}`), StartedAt: now, FinishedAt: now}); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	read := func() (string, map[string]any) {
		t.Helper()
		var st, actions string
		if err := sdb.QueryRowxContext(ctx, `SELECT status, CAST(actions_result AS CHAR) FROM sys_workflow_runs WHERE id = ?`, runID).Scan(&st, &actions); err != nil {
			t.Fatalf("read: %v", err)
		}
		var sum map[string]any
		if err := json.Unmarshal([]byte(actions), &sum); err != nil {
			t.Fatalf("summary %s: %v", actions, err)
		}
		return st, sum
	}
	mark := func(job string) {
		t.Helper()
		if found, err := repo.MarkRunActionFailed(ctx, tenant, runID, 0, job, "bỏ cuộc"); err != nil || !found {
			t.Fatalf("mark %s: found=%v err=%v", job, found, err)
		}
	}
	mark("job-1")
	if st, sum := read(); st != "failed" || sum["failed"] != float64(1) || sum["matched"] != float64(3) {
		t.Fatalf("after 1 give-up: %s %v; want failed with failed=1, matched kept", st, sum)
	}
	mark("job-1") // the same delivery reporting again
	if _, sum := read(); sum["failed"] != float64(1) {
		t.Fatalf("same delivery twice: failed=%v; want still 1", sum["failed"])
	}
	mark("job-2") // another record's email
	if _, sum := read(); sum["failed"] != float64(2) {
		t.Fatalf("second delivery: failed=%v; want 2", sum["failed"])
	}
}
