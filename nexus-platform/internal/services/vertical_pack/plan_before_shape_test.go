package vertical_pack

// Queue #315 — a pack whose before_* rule has a malformed shape must be refused by the plan gate,
// BEFORE the entity or any sample row is written. Without the gate the shape error only surfaced at
// the workflows pass (the last data pass), after the entities and sample rows were already committed
// and auto-committing DDL could not be undone. The fake workflow writer does not validate, so a
// failure here can only come from the gate.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/services"
)

func beforeShapePack(rule PackWorkflow) *Pack {
	return &Pack{
		Entities: []PackEntity{{Name: "wf_deals", Label: "Deals", Fields: []PackField{
			{Name: "name", Label: "Name", FieldType: "text"},
			{Name: "stage", Label: "Stage", FieldType: "text"},
			{Name: "close_date", Label: "Close date", FieldType: "date"},
		}}},
		Samples:   []PackSample{{Entity: "wf_deals", Records: []PackRecord{{Data: map[string]any{"name": "Sample"}}}}},
		Workflows: []PackWorkflow{rule},
	}
}

func veto(field, msg string) PackWorkflowAction {
	return PackWorkflowAction{Type: "veto", Field: field, Message: msg}
}

func TestBuildPlan_BeforeRuleShape_RefusedBeforeAnyWrite(t *testing.T) {
	tests := []struct {
		name     string
		rule     PackWorkflow
		wantCode string
	}{
		{"veto mixed with a value-changing action", PackWorkflow{Name: "mixed", EntityName: "wf_deals", Trigger: "before_save",
			Actions: []PackWorkflowAction{veto("close_date", "Cần ngày chốt"), {Type: "field_update", Field: "stage", Value: "x"}}},
			"WORKFLOW_BEFORE_ACTIONS_MIXED"},
		{"saveOn is neither create nor update", PackWorkflow{Name: "bad saveOn", EntityName: "wf_deals", Trigger: "before_save",
			Condition: &PackWorkflowCondition{SaveOn: "weekly"}, Actions: []PackWorkflowAction{veto("close_date", "Cần ngày chốt")}},
			"WORKFLOW_SAVE_ON_INVALID"},
		{"changedTo on a rule that also runs on create", PackWorkflow{Name: "never true", EntityName: "wf_deals", Trigger: "before_save",
			Condition: &PackWorkflowCondition{SaveOn: "create", Rules: []PackWorkflowRule{{Field: "stage", Op: "changedTo", Value: "Won"}}},
			Actions:   []PackWorkflowAction{veto("close_date", "Cần ngày chốt")}},
			"WORKFLOW_CONDITION_NEVER_TRUE"},
		{"an outside-effect action on a before rule", PackWorkflow{Name: "mail", EntityName: "wf_deals", Trigger: "before_save",
			Actions: []PackWorkflowAction{{Type: "email_alert", To: "a@b.vn", Subject: "s", Body: "b"}}},
			""}, // any ValidationError: the exact code is the service's business
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wf := &fakeWorkflowWriter{}
			svc, meta := newWorkflowTestService(wf)
			pack := beforeShapePack(tt.rule)

			for label, call := range map[string]func() error{
				"Install": func() error { _, err := svc.Install(context.Background(), "tenant-1", "user-1", pack); return err },
				"DryRun":  func() error { _, err := svc.DryRunInstall(context.Background(), "tenant-1", pack); return err },
			} {
				var ve *services.ValidationError
				err := call()
				if !errors.As(err, &ve) {
					t.Fatalf("%s: err = %v, want a ValidationError from the plan gate", label, err)
				}
				if tt.wantCode != "" && ve.Code != tt.wantCode {
					t.Errorf("%s: code = %q, want %q", label, ve.Code, tt.wantCode)
				}
				if ve.Msg == "" || !strings.Contains(ve.Msg, tt.rule.Name) {
					t.Errorf("%s: message %q does not name the rule %q", label, ve.Msg, tt.rule.Name)
				}
			}
			if got := meta.defs["tenant-1"]; len(got) != 0 {
				t.Errorf("entities written before the gate refused the pack: %v", got)
			}
			if len(wf.created) != 0 {
				t.Errorf("CreateWorkflow reached: %v", wf.created)
			}
		})
	}
}

// A well-formed before rule installs exactly as before the gate existed.
func TestBuildPlan_BeforeRuleShape_ValidRuleInstalls(t *testing.T) {
	wf := &fakeWorkflowWriter{}
	svc, _ := newWorkflowTestService(wf)
	pack := beforeShapePack(PackWorkflow{Name: "close date", EntityName: "wf_deals", Trigger: "before_save",
		Condition: &PackWorkflowCondition{SaveOn: "update", Rules: []PackWorkflowRule{{Field: "stage", Op: "changedTo", Value: "Won"}}},
		Actions:   []PackWorkflowAction{veto("close_date", "Cần ngày chốt")}})
	pack.Samples = nil
	res, err := svc.Install(context.Background(), "tenant-1", "user-1", pack)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(res.WorkflowsCreated) != 1 || len(wf.created) != 1 {
		t.Fatalf("WorkflowsCreated = %v, want the one valid rule", res.WorkflowsCreated)
	}
}
