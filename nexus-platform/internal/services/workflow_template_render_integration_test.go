//go:build integration

// DAILY-QUEUE #335 end-to-end over a freshly migrated throwaway MySQL DB: an appointment rule's
// create_record value and in_app_notification title carry {{benh_nhan.ho_ten}}, and the engine
// writes the LINKED patient's name — through the same resolver seam as email_alert. The trigger is
// a NON-admin user (the engine reads the linked record as a system principal, so who fired the
// rule must not change the result).
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestWorkflowTemplateRender
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
	tprTenant = "f1f1f1f1-0000-4000-8000-000000000335"
	tprStaff  = "f3f3f3f3-0000-4000-8000-000000000335"
)

func TestWorkflowTemplateRender_CreateRecordAndNotificationUseTheLinkedRecord(t *testing.T) {
	gdb, sdb := o3aSetup(t)
	ctx := context.Background()
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	mc := NewMetaCache(metaRepo, DefaultMetaTTL)
	entitySvc := NewEntityService(dynRepo, metaRepo).WithMetaCache(mc)

	wfRepo := workflow.NewSQLRepository(sdb)
	wfCache := workflow.NewCachedLoader(wfRepo, 100*time.Millisecond)
	resolver := NewWorkflowTemplateResolver(mc, dynRepo)
	engine := workflow.NewAsyncDispatcher(workflow.DispatcherConfig{Buffer: 16, Workers: 1},
		wfCache, NewWorkflowRecordMutator(dynRepo), wfRepo, nil).
		WithTemplateResolver(resolver).
		WithRunRecorder(wfRepo)
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	workflowSvc := NewWorkflowService(wfRepo).WithEntityChecker(mc).WithCache(wfCache).
		WithRecordReader(dynRepo).WithTemplateResolver(resolver)
	recordSvc := NewRecordService(mc, dynRepo).WithEngine(engine)
	engine.WithRecordCreator(NewWorkflowRecordCreator(recordSvc), 3)
	admin := RecordScope{UserID: tprStaff, AccessLevel: "all", IsAdmin: true}
	staff := RecordScope{UserID: tprStaff, AccessLevel: "all"} // the non-admin who fires the rule

	for _, e := range []CreateEntityInput{
		{Name: "benh_nhan", Label: "Bệnh nhân", Fields: []FieldInput{{Name: "ho_ten", Label: "Họ tên", FieldType: "text", IsRequired: true}}},
		{Name: "nhac_viec", Label: "Nhắc việc", Fields: []FieldInput{{Name: "tieu_de", Label: "Tiêu đề", FieldType: "text"}}},
		{Name: "lich_hen", Label: "Lịch hẹn", Fields: []FieldInput{
			{Name: "tieu_de", Label: "Tiêu đề", FieldType: "text", IsRequired: true},
			{Name: "benh_nhan", Label: "Bệnh nhân", FieldType: "relation", Options: map[string]any{"relatedEntity": "benh_nhan"}},
		}},
	} {
		if _, _, err := entitySvc.CreateEntity(ctx, tprTenant, e); err != nil {
			t.Fatalf("CreateEntity %s: %v", e.Name, err)
		}
	}

	// A placeholder that can never resolve is refused when the rule is SAVED.
	_, err := workflowSvc.CreateWorkflow(ctx, tprTenant, CreateWorkflowInput{
		Name: "xấu", EntityName: "lich_hen", Trigger: "after_create", ActorID: tprStaff,
		Actions: []WorkflowActionInput{{Type: "in_app_notification", UserID: "$owner", Title: "{{khong_co.ten}}", Message: "m"}},
	})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "WORKFLOW_TEMPLATE_REF_INVALID" {
		t.Fatalf("bad notification ref: err = %v; want WORKFLOW_TEMPLATE_REF_INVALID", err)
	}

	rule, err := workflowSvc.CreateWorkflow(ctx, tprTenant, CreateWorkflowInput{
		Name: "Nhắc hẹn", EntityName: "lich_hen", Trigger: "after_create", ActorID: tprStaff,
		Actions: []WorkflowActionInput{
			{Type: "in_app_notification", UserID: "$owner", Title: "Hẹn của {{benh_nhan.ho_ten}}", Message: "{{tieu_de}}"},
			{Type: "create_record", CreateEntity: "nhac_viec", Values: map[string]any{"tieu_de": "Gọi lại {{benh_nhan.ho_ten}}"}},
		},
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	lan, err := recordSvc.Create(ctx, admin, tprTenant, tprStaff, "benh_nhan", map[string]any{"ho_ten": "Nguyễn Thị Lan"})
	if err != nil {
		t.Fatalf("create patient: %v", err)
	}
	appt, err := recordSvc.Create(ctx, staff, tprTenant, tprStaff, "lich_hen", map[string]any{"tieu_de": "Cạo vôi", "benh_nhan": lan})
	if err != nil {
		t.Fatalf("create appointment: %v", err)
	}

	var title, message string
	waitUntil(t, "the notification with the patient's name", func() bool {
		return sdb.QueryRowxContext(ctx, `SELECT title, message FROM sys_notifications WHERE tenant_id = ? AND record_id = ?`, tprTenant, appt).Scan(&title, &message) == nil
	})
	if title != "Hẹn của Nguyễn Thị Lan" || message != "Cạo vôi" {
		t.Fatalf("notification = %q / %q; want the linked patient's name", title, message)
	}
	var created string
	waitUntil(t, "the follow-up record with the patient's name", func() bool {
		res, err := recordSvc.List(ctx, admin, tprTenant, "nhac_viec", ListQuery{Limit: 10})
		if err != nil || len(res.Records) != 1 {
			return false
		}
		created, _ = res.Records[0].Data["tieu_de"].(string)
		return true
	})
	if created != "Gọi lại Nguyễn Thị Lan" {
		t.Fatalf("created tieu_de = %q; want the linked patient's name", created)
	}

	// "Chạy thử" shows the resolved values.
	res, err := workflowSvc.TestWorkflow(ctx, tprTenant, rule.ID, appt)
	if err != nil {
		t.Fatalf("TestWorkflow: %v", err)
	}
	if got := res.Trace[len(res.Trace)-2].Detail["title"]; got != "Hẹn của Nguyễn Thị Lan" {
		t.Fatalf("dry-run notification title = %v", got)
	}
	if v, _ := res.Trace[len(res.Trace)-1].Detail["values"].(map[string]any); v["tieu_de"] != "Gọi lại Nguyễn Thị Lan" {
		t.Fatalf("dry-run create_record values = %v", res.Trace[len(res.Trace)-1].Detail["values"])
	}
}
