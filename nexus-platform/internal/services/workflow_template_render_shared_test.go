package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/workflow"
)

// DAILY-QUEUE #335: the same save-time refusal covers the other two actions that render
// placeholders — in_app_notification (title/message) and create_record (string values).
func TestNotifyAndCreateRecordTemplateRefs_ConfigTimeValidation(t *testing.T) {
	notify := func(title, msg string) WorkflowActionInput {
		return WorkflowActionInput{Type: "in_app_notification", UserID: "u1", Title: title, Message: msg}
	}
	create := func(v string) WorkflowActionInput {
		return WorkflowActionInput{Type: "create_record", CreateEntity: "bac_si", Values: map[string]any{"ten": v}}
	}
	tests := []struct {
		name      string
		action    WorkflowActionInput
		wantField string // "" = accepted
	}{
		{name: "notify title through a relation", action: notify("Hẹn của {{benh_nhan.ho_ten}}", "m")},
		{name: "notify bad relation", action: notify("t", "{{ghi_chu.x}}"), wantField: "ghi_chu.x"},
		{name: "notify field the linked entity lacks", action: notify("{{benh_nhan.so_dt}}", "m"), wantField: "benh_nhan.so_dt"},
		{name: "notify unrenderable currency", action: notify("t", "Nợ {{benh_nhan.cong_no}}"), wantField: "benh_nhan.cong_no"},
		{name: "create_record value through a relation", action: create("{{benh_nhan.ho_ten}}")},
		{name: "create_record bad relation", action: create("{{khong_co.ten}}"), wantField: "khong_co.ten"},
		{name: "create_record two hops", action: create("{{benh_nhan.bac_si.ten}}"), wantField: "benh_nhan.bac_si.ten"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeWFRepo()
			svc := NewWorkflowService(repo).WithEntityChecker(tplFixture())
			_, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
				Name: "r", EntityName: "lich_hen", Trigger: "after_create", ActorID: "u1",
				Actions: []WorkflowActionInput{tt.action},
			})
			if tt.wantField == "" {
				if err != nil || len(repo.created) != 1 {
					t.Fatalf("CreateWorkflow = %v (created %d); want the rule saved", err, len(repo.created))
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != "WORKFLOW_TEMPLATE_REF_INVALID" {
				t.Fatalf("err = %v; want WORKFLOW_TEMPLATE_REF_INVALID", err)
			}
			if got, _ := ve.Details["field"].(string); got != tt.wantField {
				t.Errorf("details.field = %q; want %q", got, tt.wantField)
			}
		})
	}
}

// "Chạy thử": notification title and create_record values show the linked record's value.
func TestPreviewNotifyAndCreateRecord_ShowResolvedValues(t *testing.T) {
	recs := &tplRecords{rows: map[string]map[string]any{"benh_nhan/p-1": {"ho_ten": "Lan"}}}
	res := NewWorkflowTemplateResolver(tplFixture(), recs)
	evt := workflow.Event{TenantID: "t1", EntityName: "lich_hen", Data: map[string]any{"benh_nhan": "p-1"}}

	n := workflow.Action{Type: workflow.ActionInAppNotification, UserID: "u1", Title: "Hẹn của {{benh_nhan.ho_ten}}", Message: "m"}
	detail, _ := previewAction(context.Background(), "wf-1", n, evt, true, nil, nil, res)
	if detail["title"] != "Hẹn của Lan" {
		t.Errorf("notification title = %v; want the resolved name", detail["title"])
	}

	c := workflow.Action{Type: workflow.ActionCreateRecord, CreateEntity: "nhac", Values: map[string]any{"ten": "Gọi {{benh_nhan.ho_ten}}"}}
	detail, _ = previewAction(context.Background(), "wf-1", c, evt, true, nil, nil, res)
	if v, _ := detail["values"].(map[string]any); v["ten"] != "Gọi Lan" {
		t.Errorf("create_record values = %v; want the resolved name", detail["values"])
	}

	recs.err = errors.New("db down")
	detail, _ = previewAction(context.Background(), "wf-1", n, evt, true, nil, nil, res)
	if detail["wouldSend"] != false || detail["error"] == nil {
		t.Errorf("read failure must preview as not sendable with a reason, got %v", detail)
	}
}
