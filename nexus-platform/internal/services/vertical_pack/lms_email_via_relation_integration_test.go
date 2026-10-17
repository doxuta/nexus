//go:build integration

// DAILY-QUEUE #341 (specs/059 §6): the email-via-relation journey on the SHIPPED LMS pack ("Sao Mai").
// Installs lmsPack, adds a rule on lms_enrollments that mails {{student.email}}, then creates
// enrolments through RecordService wired as main.go does (AsyncDispatcher + WorkflowTemplateResolver).
// A student with an email gets exactly one mail; a student without one turns the run Lỗi naming the template.
package vertical_pack

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

type lmsMailSpy struct {
	mu    sync.Mutex
	calls [][3]string
}

func (m *lmsMailSpy) SendEmail(_ context.Context, to, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, [3]string{to, subject, body})
	return nil
}

func (m *lmsMailSpy) snapshot() [][3]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][3]string(nil), m.calls...)
}

func TestVerticalPack_LMSEmailViaStudentRelation(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, _ := vpBuildWF(gdb, sdb)
	ctx := context.Background()
	if err := gdb.Exec(
		"INSERT INTO users (id, tenant_id, email, password_hash, status) VALUES (?, ?, ?, ?, 'active')",
		vpSeedUserID, vpTenantA, "installer@lms-mail.example.com", "x",
	).Error; err != nil {
		t.Fatalf("seed install user: %v", err)
	}
	pack, ok := BuiltinPack("lms")
	if !ok {
		t.Fatal("builtin lms pack not found")
	}
	if _, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack); err != nil {
		t.Fatalf("install lms: %v", err)
	}

	metaRepo := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sdb)
	mc := services.NewMetaCache(metaRepo, services.DefaultMetaTTL)
	wfRepo := workflow.NewSQLRepository(sdb)
	wfCache := workflow.NewCachedLoader(wfRepo, 100*time.Millisecond)
	spy := &lmsMailSpy{}
	resolver := services.NewWorkflowTemplateResolver(mc, dyn)
	engine := workflow.NewAsyncDispatcher(workflow.DispatcherConfig{Buffer: 16, Workers: 1},
		wfCache, services.NewWorkflowRecordMutator(dyn), wfRepo, nil).
		WithEmailSender(spy).WithTemplateResolver(resolver).WithRunRecorder(wfRepo)
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	wfSvc := services.NewWorkflowService(wfRepo).WithEntityChecker(mc).WithCache(wfCache).
		WithRecordReader(dyn).WithTemplateResolver(resolver)
	recSvc := services.NewRecordService(mc, dyn).WithEngine(engine)
	admin := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}

	rule, err := wfSvc.CreateWorkflow(ctx, vpTenantA, services.CreateWorkflowInput{
		Name: "Xác nhận ghi danh", EntityName: "lms_enrollments", Trigger: "after_create", ActorID: vpSeedUserID,
		Actions: []services.WorkflowActionInput{{
			Type: "email_alert", To: "{{student.email}}",
			Subject: "Ghi danh của {{student.name}}", Body: "Khoá {{course.title}}",
		}},
	})
	if err != nil {
		t.Fatalf("CreateWorkflow on the pack's enrolment: %v", err)
	}

	course, err := recSvc.Create(ctx, admin, vpTenantA, vpSeedUserID, "lms_courses", map[string]any{"title": "IELTS 6.5"})
	if err != nil {
		t.Fatalf("create course: %v", err)
	}
	withMail, err := recSvc.Create(ctx, admin, vpTenantA, vpSeedUserID, "lms_students", map[string]any{"name": "Mai Lan", "email": "lan@saomai.vn"})
	if err != nil {
		t.Fatalf("create student: %v", err)
	}
	if _, err := recSvc.Create(ctx, admin, vpTenantA, vpSeedUserID, "lms_enrollments", map[string]any{"course": course, "student": withMail}); err != nil {
		t.Fatalf("create enrolment: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for len(spy.snapshot()) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for one mail to the enrolled student")
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := spy.snapshot()
	if len(got) != 1 || got[0][0] != "lan@saomai.vn" || got[0][1] != "Ghi danh của Mai Lan" || got[0][2] != "Khoá IELTS 6.5" {
		t.Fatalf("mails = %v; want exactly one to lan@saomai.vn with the student name and course title", got)
	}

	noMail, err := recSvc.Create(ctx, admin, vpTenantA, vpSeedUserID, "lms_students", map[string]any{"name": "Hùng"})
	if err != nil {
		t.Fatalf("create student without email: %v", err)
	}
	enrol, err := recSvc.Create(ctx, admin, vpTenantA, vpSeedUserID, "lms_enrollments", map[string]any{"course": course, "student": noMail})
	if err != nil {
		t.Fatalf("create enrolment 2: %v", err)
	}
	var run workflow.RunRecord
	deadline = time.Now().Add(20 * time.Second)
	for {
		// startRun writes the row as running before the actions run: judge it once it is final (#25).
		err := sdb.GetContext(ctx, &run, `SELECT id, tenant_id, workflow_id, trigger_type, record_id, status, error, actions_result, started_at, finished_at
			FROM sys_workflow_runs WHERE tenant_id = ? AND record_id = ? AND workflow_id = ?`, vpTenantA, enrol, rule.ID)
		if err == nil && run.Status != string(workflow.RunRunning) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run of the enrolment without a student email did not reach a final status in 20s (last read: status %q, error %v)", run.Status, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if run.Status != string(workflow.RunFailed) || run.Error == nil || !strings.Contains(*run.Error, "{{student.email}}") {
		t.Fatalf("run = %q / %v; want failed naming {{student.email}}", run.Status, derefStr(run.Error))
	}
	if n := len(spy.snapshot()); n != 1 {
		t.Fatalf("mails = %d; want still 1", n)
	}
}

func derefStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
