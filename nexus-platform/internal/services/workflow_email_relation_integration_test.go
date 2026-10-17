//go:build integration

// specs/059 (DAILY-QUEUE #271) end-to-end over a freshly migrated throwaway MySQL DB: an appointment
// rule mails the PATIENT the appointment points at. Wired as cmd/server/main.go does — RecordService
// emits, the AsyncDispatcher runs the rule, WorkflowTemplateResolver reads the linked record through
// the metadata cache + dynrepo, the run lands in sys_workflow_runs.
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestWorkflowEmailRelation
package services

import (
	"context"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

const (
	emlTenant  = "e1e1e1e1-0000-4000-8000-000000000056"
	emlTenantB = "e2e2e2e2-0000-4000-8000-000000000056"
	emlUser    = "e3e3e3e3-0000-4000-8000-000000000056"
)

func TestWorkflowEmailRelation_MailsTheLinkedPatient(t *testing.T) {
	gdb, sdb := o3aSetup(t)
	ctx := context.Background()
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	mc := NewMetaCache(metaRepo, DefaultMetaTTL)
	entitySvc := NewEntityService(dynRepo, metaRepo).WithMetaCache(mc)

	wfRepo := workflow.NewSQLRepository(sdb)
	wfCache := workflow.NewCachedLoader(wfRepo, 100*time.Millisecond)
	email := &capturingEmailSender{}
	resolver := NewWorkflowTemplateResolver(mc, dynRepo)
	engine := workflow.NewAsyncDispatcher(workflow.DispatcherConfig{Buffer: 16, Workers: 1},
		wfCache, NewWorkflowRecordMutator(dynRepo), wfRepo, nil).
		WithEmailSender(email).
		WithTemplateResolver(resolver).
		WithRunRecorder(wfRepo)
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	workflowSvc := NewWorkflowService(wfRepo).WithEntityChecker(mc).WithCache(wfCache).
		WithRecordReader(dynRepo).WithTemplateResolver(resolver)
	recordSvc := NewRecordService(mc, dynRepo).WithEngine(engine)
	admin := RecordScope{UserID: emlUser, AccessLevel: "all", IsAdmin: true}

	for _, tenant := range []string{emlTenant, emlTenantB} {
		if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{Name: "benh_nhan", Label: "Bệnh nhân", Fields: []FieldInput{
			{Name: "ho_ten", Label: "Họ tên", FieldType: "text", IsRequired: true},
			{Name: "email", Label: "Email", FieldType: "email"},
		}}); err != nil {
			t.Fatalf("CreateEntity benh_nhan: %v", err)
		}
		if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{Name: "lich_hen", Label: "Lịch hẹn", Fields: []FieldInput{
			{Name: "tieu_de", Label: "Tiêu đề", FieldType: "text", IsRequired: true},
			{Name: "benh_nhan", Label: "Bệnh nhân", FieldType: "relation", Options: map[string]any{"relatedEntity": "benh_nhan"}},
			{Name: "email_bn", Label: "Email bệnh nhân", FieldType: "lookup", Options: map[string]any{"via": "benh_nhan", "sourceField": "email"}},
		}}); err != nil {
			t.Fatalf("CreateEntity lich_hen: %v", err)
		}
	}
	rule, err := workflowSvc.CreateWorkflow(ctx, emlTenant, CreateWorkflowInput{
		Name: "Nhắc hẹn", EntityName: "lich_hen", Trigger: "after_create", ActorID: emlUser,
		Actions: []WorkflowActionInput{{
			Type: "email_alert", To: "{{benh_nhan.email}}",
			Subject: "Nhắc hẹn {{benh_nhan.ho_ten}}", Body: "Lịch {{tieu_de}} — {{email_bn}}",
		}},
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	lan, err := recordSvc.Create(ctx, admin, emlTenant, emlUser, "benh_nhan", map[string]any{"ho_ten": "Nguyễn Thị Lan", "email": "lan@nhakhoa.vn"})
	if err != nil {
		t.Fatalf("create patient: %v", err)
	}
	appt, err := recordSvc.Create(ctx, admin, emlTenant, emlUser, "lich_hen", map[string]any{"tieu_de": "Cạo vôi", "benh_nhan": lan})
	if err != nil {
		t.Fatalf("create appointment: %v", err)
	}
	waitUntil(t, "one mail to the linked patient", func() bool { return email.count() == 1 })
	email.mu.Lock()
	got := email.calls[0]
	email.mu.Unlock()
	if got.to != "lan@nhakhoa.vn" || got.subject != "Nhắc hẹn Nguyễn Thị Lan" || got.body != "Lịch Cạo vôi — lan@nhakhoa.vn" {
		t.Fatalf("sent %+v; want the linked patient's address, name and lookup value", got)
	}

	// The patient has no email: FAILED run, reason names the template, nothing sent.
	hung, err := recordSvc.Create(ctx, admin, emlTenant, emlUser, "benh_nhan", map[string]any{"ho_ten": "Trần Hùng"})
	if err != nil {
		t.Fatalf("create patient without email: %v", err)
	}
	apptNoMail, err := recordSvc.Create(ctx, admin, emlTenant, emlUser, "lich_hen", map[string]any{"tieu_de": "Nhổ răng", "benh_nhan": hung})
	if err != nil {
		t.Fatalf("create appointment: %v", err)
	}
	var run workflow.RunRecord
	// startRun writes the row as running before the actions run: judge it once it is final (#25).
	waitUntil(t, "the run of the appointment without a patient email to reach a final status", func() bool {
		return sdb.GetContext(ctx, &run, `SELECT id, tenant_id, workflow_id, trigger_type, record_id, status, error, actions_result, started_at, finished_at
			FROM sys_workflow_runs WHERE tenant_id = ? AND record_id = ?`, emlTenant, apptNoMail) == nil && run.Status != string(workflow.RunRunning)
	})
	if run.Status != string(workflow.RunFailed) || run.Error == nil || !strings.Contains(*run.Error, "{{benh_nhan.email}}") || !strings.Contains(*run.Error, "không có địa chỉ") {
		t.Fatalf("run = status %q error %v; want failed, naming {{benh_nhan.email}}", run.Status, run.Error)
	}
	if email.count() != 1 {
		t.Fatalf("mails sent = %d; want still 1 (nothing to an empty recipient)", email.count())
	}

	// "Chạy thử" previews the SAME address the send used.
	res, err := workflowSvc.TestWorkflow(ctx, emlTenant, rule.ID, appt)
	if err != nil {
		t.Fatalf("TestWorkflow: %v", err)
	}
	last := res.Trace[len(res.Trace)-1].Detail
	if last["to"] != "lan@nhakhoa.vn" || last["wouldSend"] != true {
		t.Fatalf("dry-run email detail = %v; want to=lan@nhakhoa.vn, wouldSend", last)
	}

	// Tenant isolation on the real read: an appointment whose FK names ANOTHER tenant's patient
	// (forced at the storage level — the write path refuses it) resolves nothing.
	other, err := recordSvc.Create(ctx, admin, emlTenantB, emlUser, "benh_nhan", map[string]any{"ho_ten": "Người tenant B", "email": "b@tenant-b.vn"})
	if err != nil {
		t.Fatalf("create tenant-B patient: %v", err)
	}
	vals, err := resolver.ResolveTemplateValues(ctx, emlTenant, "lich_hen", map[string]any{"benh_nhan": other}, []string{"benh_nhan.email", "email_bn"})
	if err != nil {
		t.Fatalf("ResolveTemplateValues: %v", err)
	}
	if len(vals) != 0 {
		t.Fatalf("resolved %v across tenants; want nothing", vals)
	}
}

// waitUntil polls cond for up to 20 s (the dispatcher and the queue are asynchronous; CI runners
// are several times slower than a laptop).
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
