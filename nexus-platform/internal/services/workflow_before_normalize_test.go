package services

// specs/048 S5 (T092) — the normalize action of a before_save rule is checked when the rule is saved:
// one to four distinct known operations, on a free-text field (text, email) of the entity. A rule
// stored past these would do nothing — or something else than it says — at every save.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"nexus/platform/internal/workflow"
)

func normalizeInput(field string, ops ...string) WorkflowActionInput {
	return WorkflowActionInput{Type: workflow.ActionNormalize, Field: field, Ops: ops}
}

func TestBuildWorkflowRule_Normalize(t *testing.T) {
	tests := []struct {
		name      string
		trigger   string
		saveOn    string
		actions   []WorkflowActionInput
		wantCode  string
		wantField string
	}{
		{name: "trim and collapse a text field", trigger: "before_save",
			actions: []WorkflowActionInput{normalizeInput("note", "trim", "collapse_spaces")}},
		{name: "trim and lower an email field", trigger: "before_save",
			actions: []WorkflowActionInput{normalizeInput("email", "trim", "lower")}},
		{name: "all four operations", trigger: "before_save",
			actions: []WorkflowActionInput{normalizeInput("note", "upper", "trim", "collapse_spaces", "lower")}},
		{name: "on updates only", trigger: "before_save", saveOn: "update",
			actions: []WorkflowActionInput{normalizeInput("note", "trim")}},
		{name: "next to a fill in the same rule", trigger: "before_save",
			actions: []WorkflowActionInput{{Type: workflow.ActionFieldUpdate, Field: "source", Value: "Web", OnlyIfEmpty: true},
				normalizeInput("email", "lower")}},

		{name: "no operation", trigger: "before_save",
			actions: []WorkflowActionInput{normalizeInput("note")}, wantCode: "NORMALIZE_OPS_INVALID", wantField: "note"},
		{name: "empty operation list", trigger: "before_save",
			actions: []WorkflowActionInput{{Type: workflow.ActionNormalize, Field: "note", Ops: []string{}}}, wantCode: "NORMALIZE_OPS_INVALID", wantField: "note"},
		{name: "more than four operations", trigger: "before_save",
			actions:  []WorkflowActionInput{normalizeInput("note", "trim", "lower", "upper", "collapse_spaces", "trim")},
			wantCode: "NORMALIZE_OPS_INVALID", wantField: "note"},
		{name: "an operation twice", trigger: "before_save",
			actions: []WorkflowActionInput{normalizeInput("note", "trim", "trim")}, wantCode: "NORMALIZE_OPS_INVALID", wantField: "note"},
		{name: "an unknown operation", trigger: "before_save",
			actions: []WorkflowActionInput{normalizeInput("note", "titlecase")}, wantCode: "NORMALIZE_OPS_INVALID", wantField: "note"},
		{name: "a blank operation", trigger: "before_save",
			actions: []WorkflowActionInput{normalizeInput("note", "trim", " ")}, wantCode: "NORMALIZE_OPS_INVALID", wantField: "note"},

		// Rich text is HTML: casing it rewrites entities (&Agrave; is À, &agrave; is à) and markup.
		{name: "rich text", trigger: "before_save",
			actions: []WorkflowActionInput{normalizeInput("summary", "trim")}, wantCode: "NORMALIZE_FIELD_TYPE_INVALID", wantField: "summary"},
		{name: "a number", trigger: "before_save",
			actions: []WorkflowActionInput{normalizeInput("probability", "trim")}, wantCode: "NORMALIZE_FIELD_TYPE_INVALID", wantField: "probability"},
		{name: "a picklist (its values are codes)", trigger: "before_save",
			actions: []WorkflowActionInput{normalizeInput("stage", "lower")}, wantCode: "NORMALIZE_FIELD_TYPE_INVALID", wantField: "stage"},
		{name: "a relation", trigger: "before_save",
			actions: []WorkflowActionInput{normalizeInput("account", "trim")}, wantCode: "NORMALIZE_FIELD_TYPE_INVALID", wantField: "account"},
		{name: "an auto_number", trigger: "before_save",
			actions: []WorkflowActionInput{normalizeInput("code", "lower")}, wantCode: "NORMALIZE_FIELD_TYPE_INVALID", wantField: "code"},
		{name: "a field that does not exist", trigger: "before_save",
			actions: []WorkflowActionInput{normalizeInput("nickname", "trim")}, wantCode: "RECORD_UNKNOWN_FIELD", wantField: "nickname"},
		{name: "no field", trigger: "before_save",
			actions: []WorkflowActionInput{normalizeInput(" ", "trim")}, wantCode: "WORKFLOW_ACTION_INVALID"},

		{name: "on an after rule", trigger: "after_update",
			actions: []WorkflowActionInput{normalizeInput("note", "trim")}, wantCode: "WORKFLOW_ACTION_NOT_ALLOWED_FOR_TRIGGER"},
		{name: "on a before_delete rule", trigger: "before_delete",
			actions: []WorkflowActionInput{normalizeInput("note", "trim")}, wantCode: "WORKFLOW_ACTION_NOT_ALLOWED_FOR_TRIGGER"},
		{name: "next to a veto", trigger: "before_save",
			actions:  []WorkflowActionInput{vetoAction("", "Không lưu"), normalizeInput("note", "trim")},
			wantCode: "WORKFLOW_BEFORE_ACTIONS_MIXED"},

		// Keys that mean nothing to the action they ride on are refused, not dropped.
		{name: "onlyIfEmpty on normalize", trigger: "before_save",
			actions:  []WorkflowActionInput{{Type: workflow.ActionNormalize, Field: "note", Ops: []string{"trim"}, OnlyIfEmpty: true}},
			wantCode: "WORKFLOW_ACTION_INVALID"},
		{name: "valueSource on normalize", trigger: "before_save",
			actions:  []WorkflowActionInput{{Type: workflow.ActionNormalize, Field: "note", Ops: []string{"trim"}, ValueSource: "constant"}},
			wantCode: "WORKFLOW_ACTION_INVALID"},
		{name: "via on normalize", trigger: "before_save",
			actions:  []WorkflowActionInput{{Type: workflow.ActionNormalize, Field: "note", Ops: []string{"trim"}, Via: "account"}},
			wantCode: "WORKFLOW_ACTION_INVALID"},
		{name: "ops on a fill", trigger: "before_save",
			actions:  []WorkflowActionInput{{Type: workflow.ActionFieldUpdate, Field: "note", Value: "x", Ops: []string{"trim"}}},
			wantCode: "WORKFLOW_ACTION_INVALID"},
		{name: "ops on an after rule's field_update", trigger: "after_update",
			actions:  []WorkflowActionInput{{Type: workflow.ActionFieldUpdate, Field: "note", Value: "x", Ops: []string{"lower"}}},
			wantCode: "WORKFLOW_ACTION_INVALID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewWorkflowService(newFakeWFRepo()).WithEntityChecker(setValueCatalog())
			_, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
				Name: "r", EntityName: "deals", Trigger: tt.trigger, SaveOn: tt.saveOn, Actions: tt.actions, IsActive: boolp(false),
			})
			ve := wantBeforeCode(t, err, tt.wantCode)
			if tt.wantField != "" && (ve.Field != tt.wantField || ve.Details["field"] != tt.wantField) {
				t.Fatalf("field = %q details=%v, want %q", ve.Field, ve.Details, tt.wantField)
			}
		})
	}
}

// The persisted form: the field and each operation trimmed, the order kept, nothing else.
func TestBuildWorkflowRule_NormalizePersistedShape(t *testing.T) {
	svc := NewWorkflowService(newFakeWFRepo()).WithEntityChecker(setValueCatalog())
	rec, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
		Name: "r", EntityName: "deals", Trigger: "before_save", IsActive: boolp(false),
		Actions: []WorkflowActionInput{normalizeInput(" email ", " trim", "lower ")},
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	var acts []map[string]any
	if err := json.Unmarshal(rec.Actions, &acts); err != nil {
		t.Fatalf("actions %s: %v", rec.Actions, err)
	}
	if want := `[map[field:email ops:[trim lower] type:normalize]]`; fmt.Sprint(acts) != want {
		t.Fatalf("actions = %v\nwant      %s", acts, want)
	}
}
