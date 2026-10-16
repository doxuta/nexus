//go:build integration

// DAILY-QUEUE #336 end-to-end over a freshly migrated throwaway MySQL DB: an appointment rule mails
// the USER the record's `user` field points at ({{nguoi_phu_trach.email}}), read from the system
// `users` table under the event's tenant. A user of another tenant is never read.
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestWorkflowTemplateUserField
package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

const (
	tufTenantA = "a1a1a1a1-0000-4000-8000-000000000336"
	tufTenantB = "b2b2b2b2-0000-4000-8000-000000000336"
	tufOwnerA  = "a3a3a3a3-0000-4000-8000-000000000336"
	tufOwnerB  = "b3b3b3b3-0000-4000-8000-000000000336"
	tufStaff   = "a4a4a4a4-0000-4000-8000-000000000336"
)

func TestWorkflowTemplateUserField_MailsTheResponsibleUser(t *testing.T) {
	gdb, sdb := o3aSetup(t)
	ctx := context.Background()
	for _, tn := range []struct{ id, slug string }{{tufTenantA, "tufa"}, {tufTenantB, "tufb"}} {
		if _, err := sdb.ExecContext(ctx, `INSERT INTO tenants (id, slug, name) VALUES (?, ?, ?)`, tn.id, tn.slug, tn.slug); err != nil {
			t.Fatalf("insert tenant: %v", err)
		}
	}
	for _, u := range []struct{ id, tenant, email, name string }{
		{tufOwnerA, tufTenantA, "tan@nhakhoa.vn", "Lê Tân"},
		{tufOwnerB, tufTenantB, "b@tenant-b.vn", "Người tenant B"},
	} {
		if _, err := sdb.ExecContext(ctx, `INSERT INTO users (id, tenant_id, email, password_hash, full_name, status) VALUES (?, ?, ?, 'x', ?, 'active')`,
			u.id, u.tenant, u.email, u.name); err != nil {
			t.Fatalf("insert user: %v", err)
		}
	}
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
		WithEmailSender(email).WithTemplateResolver(resolver).WithRunRecorder(wfRepo)
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	workflowSvc := NewWorkflowService(wfRepo).WithEntityChecker(mc).WithCache(wfCache).
		WithRecordReader(dynRepo).WithTemplateResolver(resolver)
	recordSvc := NewRecordService(mc, dynRepo).WithEngine(engine)
	staff := RecordScope{UserID: tufStaff, AccessLevel: "all"} // non-admin trigger

	for _, tenant := range []string{tufTenantA, tufTenantB} {
		if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{Name: "lich_hen", Label: "Lịch hẹn", Fields: []FieldInput{
			{Name: "tieu_de", Label: "Tiêu đề", FieldType: "text", IsRequired: true},
			{Name: "nguoi_phu_trach", Label: "Người phụ trách", FieldType: "user"},
		}}); err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
	}

	// Config time: a column users do not expose is refused when the rule is saved.
	_, err := workflowSvc.CreateWorkflow(ctx, tufTenantA, CreateWorkflowInput{
		Name: "xấu", EntityName: "lich_hen", Trigger: "after_create", ActorID: tufStaff,
		Actions: []WorkflowActionInput{{Type: "email_alert", To: "{{nguoi_phu_trach.password_hash}}", Subject: "s", Body: "b"}},
	})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "WORKFLOW_TEMPLATE_REF_INVALID" {
		t.Fatalf("bad user field: err = %v; want WORKFLOW_TEMPLATE_REF_INVALID", err)
	}

	rule, err := workflowSvc.CreateWorkflow(ctx, tufTenantA, CreateWorkflowInput{
		Name: "Báo người phụ trách", EntityName: "lich_hen", Trigger: "after_create", ActorID: tufStaff,
		Actions: []WorkflowActionInput{{Type: "email_alert", To: "{{nguoi_phu_trach.email}}",
			Subject: "Lịch mới cho {{nguoi_phu_trach.name}}", Body: "{{tieu_de}}"}},
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	appt, err := recordSvc.Create(ctx, staff, tufTenantA, tufStaff, "lich_hen", map[string]any{"tieu_de": "Cạo vôi", "nguoi_phu_trach": tufOwnerA})
	if err != nil {
		t.Fatalf("create appointment: %v", err)
	}
	waitUntil(t, "one mail to the responsible user", func() bool { return email.count() == 1 })
	email.mu.Lock()
	got := email.calls[0]
	email.mu.Unlock()
	if got.to != "tan@nhakhoa.vn" || got.subject != "Lịch mới cho Lê Tân" {
		t.Fatalf("sent %+v; want the responsible user's address and name", got)
	}

	res, err := workflowSvc.TestWorkflow(ctx, tufTenantA, rule.ID, appt)
	if err != nil {
		t.Fatalf("TestWorkflow: %v", err)
	}
	if last := res.Trace[len(res.Trace)-1].Detail; last["to"] != "tan@nhakhoa.vn" || last["wouldSend"] != true {
		t.Fatalf("dry-run detail = %v", last)
	}

	// Tenant isolation on the real read: tenant B's user id in tenant A's record resolves nothing.
	vals, err := resolver.ResolveTemplateValues(ctx, tufTenantA, "lich_hen", map[string]any{"nguoi_phu_trach": tufOwnerB}, []string{"nguoi_phu_trach.email", "nguoi_phu_trach.name"})
	if err != nil {
		t.Fatalf("ResolveTemplateValues: %v", err)
	}
	if len(vals) != 0 {
		t.Fatalf("resolved %v across tenants; want nothing", vals)
	}
}
