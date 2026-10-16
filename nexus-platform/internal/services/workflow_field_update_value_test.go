package services

import (
	"context"
	"encoding/json"
	"testing"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// Queue #282: a field_update constant on the record that triggered the rule was only checked when
// the rule RAN (WorkflowRecordMutator.validateFieldUpdate → ValidatePartialRecord), so a rule that
// wrote "100" into a number field, or a status outside the picklist, saved green and failed on every
// firing. The save now runs the same validator (checkFieldUpdateValues) — parity with before_save and
// with a field_update through a relation (checkRelatedFieldTargets).

func orderCatalog() *relCatalog {
	return &relCatalog{
		entities: map[string]*metadata.EntityDefinition{
			"orders": {ID: "ent-ord", TenantID: "t1", Name: "orders"},
		},
		fields: map[string][]metadata.FieldDefinition{
			"ent-ord": {
				{Name: "amount", FieldType: "number"},
				{Name: "status", FieldType: "picklist", Options: strOpt(`{"values":["new","paid"]}`)},
				{Name: "code", FieldType: "text", IsRequired: true},
				{Name: "memo", FieldType: "text"},
				{Name: "price", FieldType: "currency"},
				{Name: "buyer", FieldType: "user"},
			},
		},
	}
}

func orderRule(trigger string, value any, field string) CreateWorkflowInput {
	in := CreateWorkflowInput{
		Name: "stamp", EntityName: "orders", Trigger: trigger, ActorID: "u1",
		Actions: []WorkflowActionInput{{Type: "field_update", Field: field, Value: value}},
	}
	if trigger == "scheduled" {
		in.IntervalSeconds = 3600
	}
	return in
}

func TestCreateWorkflow_FieldUpdateValue_CheckedAtSave(t *testing.T) {
	tests := []struct {
		name, trigger, field string
		value                any
		code                 string // "" = saves
	}{
		{name: "a number typed as text", trigger: "after_update", field: "amount", value: "100", code: "RECORD_INVALID_TYPE"},
		{name: "a status outside the picklist", trigger: "after_create", field: "status", value: "shipped", code: "RECORD_INVALID_PICKLIST"},
		{name: "blanking a required field", trigger: "after_update", field: "code", value: nil, code: "RECORD_REQUIRED_MISSING"},
		{name: "a scheduled rule is checked too", trigger: "scheduled", field: "amount", value: true, code: "RECORD_INVALID_TYPE"},
		{name: "a number", trigger: "after_update", field: "amount", value: float64(100)},
		{name: "a picklist option", trigger: "after_create", field: "status", value: "paid"},
		{name: "an explicit null clears an optional field", trigger: "after_update", field: "memo", value: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeWFRepo()
			svc := NewWorkflowService(repo).WithEntityChecker(orderCatalog())
			_, err := svc.CreateWorkflow(context.Background(), "t1", orderRule(tt.trigger, tt.value, tt.field))
			if tt.code == "" {
				if err != nil {
					t.Fatalf("CreateWorkflow = %v; want the rule saved", err)
				}
				return
			}
			ve := relWantCode(t, err, tt.code)
			if ve.Field != tt.field {
				t.Errorf("details.field = %q; want %q", ve.Field, tt.field)
			}
			if len(repo.created) != 0 {
				t.Errorf("a rule that fails on every run was persisted (%d)", len(repo.created))
			}
		})
	}
}

func TestUpdateWorkflow_FieldUpdateValue_CheckedAtSave(t *testing.T) {
	repo := newFakeWFRepo()
	svc := NewWorkflowService(repo).WithEntityChecker(orderCatalog())
	rec, err := svc.CreateWorkflow(context.Background(), "t1", orderRule("after_update", float64(100), "amount"))
	if err != nil {
		t.Fatalf("CreateWorkflow = %v", err)
	}
	stored := string(repo.rows[rec.ID].Actions)
	bad := orderRule("after_update", "100", "amount")
	_, err = svc.UpdateWorkflow(context.Background(), "t1", rec.ID, UpdateWorkflowInput{
		Name: bad.Name, EntityName: bad.EntityName, Trigger: bad.Trigger, Actions: bad.Actions, ActorID: "u1",
	})
	relWantCode(t, err, "RECORD_INVALID_TYPE")
	if got := string(repo.rows[rec.ID].Actions); got != stored {
		t.Errorf("stored actions = %s; want them untouched (%s)", got, stored)
	}
	var acts []workflow.Action
	if err := json.Unmarshal([]byte(stored), &acts); err != nil || len(acts) != 1 || acts[0].Value != float64(100) {
		t.Fatalf("stored = %s (%v); want value 100 as a number", stored, err)
	}
}
