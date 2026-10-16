package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// specs/058 — field_update following a to-one relation ("hoá đơn Đã thanh toán ⇒ lịch hẹn đổi
// trạng thái"). These are the service halves: the runtime resolution of the target entity
// (WorkflowRecordMutator.RelatedEntity), the config-time guards (pure + apply path) and the dry run.

// relCatalog is a multi-entity EntityCatalog: invoices → appointments through `appointment`.
type relCatalog struct {
	entities map[string]*metadata.EntityDefinition // name → def
	fields   map[string][]metadata.FieldDefinition // entity id → fields
}

func (c *relCatalog) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	if def, ok := c.entities[name]; ok {
		return def, nil
	}
	return nil, metadata.ErrEntityNotFound
}

func (c *relCatalog) ListFields(_ context.Context, _, entityID string) ([]metadata.FieldDefinition, error) {
	fs, ok := c.fields[entityID]
	if !ok {
		return nil, fmt.Errorf("unknown entity id %q", entityID)
	}
	return fs, nil
}

func strOpt(s string) *string { return &s }

func relatedCatalog() *relCatalog {
	return &relCatalog{
		entities: map[string]*metadata.EntityDefinition{
			"invoices":     {ID: "ent-inv", TenantID: "t1", Name: "invoices"},
			"appointments": {ID: "ent-ap", TenantID: "t1", Name: "appointments"},
		},
		fields: map[string][]metadata.FieldDefinition{
			"ent-inv": {
				{Name: "status", FieldType: "picklist", Options: strOpt(`{"values":["unpaid","paid"]}`)},
				{Name: "appointment", FieldType: "relation", Options: strOpt(`{"relatedEntity":"appointments"}`)},
				{Name: "exclusive_ap", FieldType: "relation", Options: strOpt(`{"relatedEntity":"appointments","cardinality":"1-1"}`)},
				{Name: "legacy_many", FieldType: "relation", Options: strOpt(`{"relatedEntity":"appointments","cardinality":"N-N"}`)},
				{Name: "ghost_link", FieldType: "relation", Options: strOpt(`{"relatedEntity":"ghosts"}`)},
				{Name: "no_target", FieldType: "relation"},
				{Name: "regarding", FieldType: "polymorphic_relation", Options: strOpt(`{"targetEntities":["appointments"]}`)},
				{Name: "note", FieldType: "text"},
			},
			"ent-ap": {
				{Name: "status", FieldType: "picklist", Options: strOpt(`{"values":["booked","done","paid"]}`)},
				{Name: "total", FieldType: "formula", Options: strOpt(`{"expression":"1+1"}`)},
				{Name: "code", FieldType: "auto_number"},
				{Name: "memo", FieldType: "text"},
			},
		},
	}
}

func invoiceFieldTypes() map[string]string {
	out := map[string]string{}
	for _, f := range relatedCatalog().fields["ent-inv"] {
		out[f.Name] = f.FieldType
	}
	return out
}

func relWantCode(t *testing.T, err error, code string) *ValidationError {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil; want %s", code)
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v (%T); want *ValidationError %s", err, err, code)
	}
	if ve.Code != code {
		t.Fatalf("code = %s (%v); want %s", ve.Code, err, code)
	}
	return ve
}

// ---- runtime: the target entity comes from metadata ------------------------------------------

func TestRelatedEntity_ResolvesFromMetadata(t *testing.T) {
	mut := NewWorkflowRecordMutator(&fakeRelMutStore{}).WithCatalog(relatedCatalog())
	for via, want := range map[string]string{"appointment": "appointments", "exclusive_ap": "appointments"} {
		got, err := mut.RelatedEntity(context.Background(), "t1", "invoices", via)
		if err != nil || got != want {
			t.Errorf("RelatedEntity(%q) = %q, %v; want %q", via, got, err, want)
		}
	}
	for _, via := range []string{"status", "ghost", "legacy_many", "no_target", "regarding", "ghost_link"} {
		t.Run(via, func(t *testing.T) {
			_, err := mut.RelatedEntity(context.Background(), "t1", "invoices", via)
			relWantCode(t, err, "WORKFLOW_RELATED_VIA_INVALID")
		})
	}
	if _, err := NewWorkflowRecordMutator(&fakeRelMutStore{}).RelatedEntity(context.Background(), "t1", "invoices", "appointment"); err == nil {
		t.Errorf("RelatedEntity without a catalog = nil error; it cannot know the target and must refuse")
	}
}

// ---- config time, pure half (shared with the AI Builder's ValidateWorkflowConfig) --------------

func TestValidateWorkflowConfig_FieldUpdateVia(t *testing.T) {
	act := func(via, field string) []WorkflowActionInput {
		return []WorkflowActionInput{{Type: "field_update", Via: via, Field: field, Value: "paid"}}
	}
	t.Run("a relation via passes and the target field is NOT checked against the rule's entity", func(t *testing.T) {
		// "memo" does not exist on invoices — it is an appointments field.
		if err := ValidateWorkflowConfig("invoices", "after_update", nil, act("appointment", "memo"), invoiceFieldTypes()); err != nil {
			t.Fatalf("ValidateWorkflowConfig = %v; want nil", err)
		}
	})
	for _, via := range []string{"status", "note", "ghost", "regarding"} {
		t.Run("via "+via+" is not a relation of the rule's entity", func(t *testing.T) {
			ve := relWantCode(t, ValidateWorkflowConfig("invoices", "after_update", nil, act(via, "status"), invoiceFieldTypes()), "WORKFLOW_RELATED_VIA_INVALID")
			if ve.Field != via {
				t.Errorf("details.field = %q; want %q (the builder pins the error on the via picker)", ve.Field, via)
			}
		})
	}
	t.Run("via on another action type is refused, not stored and ignored", func(t *testing.T) {
		acts := []WorkflowActionInput{{Type: "in_app_notification", UserID: "$owner", Message: "x", Via: "appointment"}}
		relWantCode(t, ValidateWorkflowConfig("invoices", "after_update", nil, acts, invoiceFieldTypes()), "WORKFLOW_ACTION_INVALID")
	})
	t.Run("a save-time rule may not write another record", func(t *testing.T) {
		err := ValidateWorkflowConfig("invoices", "before_save", nil, act("appointment", "status"), invoiceFieldTypes())
		ve := relWantCode(t, err, "WORKFLOW_ACTION_NOT_ALLOWED_FOR_TRIGGER")
		if !strings.Contains(ve.Msg, "bản ghi liên quan") {
			t.Errorf("msg = %q; want it to say a related record is the reason", ve.Msg)
		}
	})
}

// ---- config time, apply path (needs the TARGET entity's fields) --------------------------------

func relatedInput(via, field string, value any) CreateWorkflowInput {
	return CreateWorkflowInput{
		Name: "paid-books", EntityName: "invoices", Trigger: "after_update",
		Rules:   []WorkflowRuleInput{{Field: "status", Op: "changedTo", Value: "paid"}},
		Actions: []WorkflowActionInput{{Type: "field_update", Via: via, Field: field, Value: value}},
		ActorID: "u1",
	}
}

func TestCreateWorkflow_FieldUpdateVia_TargetGuards(t *testing.T) {
	tests := []struct {
		name, via, field string
		value            any
		code, codeField  string
	}{
		{name: "unknown target field", via: "appointment", field: "ghost", value: "x", code: "RECORD_UNKNOWN_FIELD", codeField: "ghost"},
		{name: "a trigger-entity field is not a target field", via: "appointment", field: "note", value: "x", code: "RECORD_UNKNOWN_FIELD", codeField: "note"},
		{name: "formula target", via: "appointment", field: "total", value: 3, code: "RECORD_FORMULA_READONLY", codeField: "total"},
		{name: "auto_number target", via: "appointment", field: "code", value: "A-1", code: "RECORD_AUTONUMBER_READONLY", codeField: "code"},
		{name: "value outside the target picklist", via: "appointment", field: "status", value: "unpaid", code: "RECORD_INVALID_PICKLIST"},
		{name: "value of the wrong type", via: "appointment", field: "memo", value: []any{1}, code: "RECORD_INVALID_TYPE"},
		{name: "many-valued relation", via: "legacy_many", field: "status", value: "paid", code: "WORKFLOW_RELATED_VIA_INVALID", codeField: "legacy_many"},
		{name: "relation to an entity that is gone", via: "ghost_link", field: "status", value: "paid", code: "WORKFLOW_RELATED_VIA_INVALID", codeField: "ghost_link"},
		{name: "relation without a target", via: "no_target", field: "status", value: "paid", code: "WORKFLOW_RELATED_VIA_INVALID", codeField: "no_target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeWFRepo()
			svc := NewWorkflowService(repo).WithEntityChecker(relatedCatalog())
			_, err := svc.CreateWorkflow(context.Background(), "t1", relatedInput(tt.via, tt.field, tt.value))
			ve := relWantCode(t, err, tt.code)
			if tt.codeField != "" && ve.Field != tt.codeField {
				t.Errorf("details.field = %q; want %q", ve.Field, tt.codeField)
			}
			if len(repo.created) != 0 {
				t.Errorf("a rejected rule was persisted (%d)", len(repo.created))
			}
		})
	}
}

func TestCreateWorkflow_FieldUpdateVia_PersistsVia(t *testing.T) {
	repo := newFakeWFRepo()
	svc := NewWorkflowService(repo).WithEntityChecker(relatedCatalog())
	if _, err := svc.CreateWorkflow(context.Background(), "t1", relatedInput("appointment", "status", "paid")); err != nil {
		t.Fatalf("CreateWorkflow = %v; want nil", err)
	}
	if len(repo.created) != 1 {
		t.Fatalf("created = %d; want 1", len(repo.created))
	}
	var acts []workflow.Action
	if err := json.Unmarshal(repo.created[0].Actions, &acts); err != nil {
		t.Fatalf("stored actions: %v", err)
	}
	if len(acts) != 1 || acts[0].Via != "appointment" || acts[0].Field != "status" || acts[0].Value != "paid" {
		t.Fatalf("stored action = %+v; want via=appointment field=status value=paid", acts)
	}
}

func TestUpdateWorkflow_FieldUpdateVia_RunsTheSameTargetGuard(t *testing.T) {
	repo := newFakeWFRepo()
	svc := NewWorkflowService(repo).WithEntityChecker(relatedCatalog())
	rec, err := svc.CreateWorkflow(context.Background(), "t1", relatedInput("appointment", "status", "paid"))
	if err != nil {
		t.Fatalf("CreateWorkflow = %v", err)
	}
	in := relatedInput("appointment", "total", 1)
	_, err = svc.UpdateWorkflow(context.Background(), "t1", rec.ID, UpdateWorkflowInput{
		Name: in.Name, EntityName: in.EntityName, Trigger: in.Trigger, Rules: in.Rules, Actions: in.Actions, ActorID: "u1",
	})
	relWantCode(t, err, "RECORD_FORMULA_READONLY")
}

// ---- dry run -----------------------------------------------------------------------------------

func TestPreviewAction_FieldUpdateVia(t *testing.T) {
	a := workflow.Action{Type: "field_update", Via: "appointment", Field: "status", Value: "paid"}
	t.Run("linked: would apply, names the related record, no WF-01 miss on the rule's entity", func(t *testing.T) {
		evt := workflow.Event{TenantID: "t1", EntityName: "invoices", Data: map[string]any{"appointment": "ap-7", "status": "paid"}}
		detail, would := previewAction(context.Background(), "wf1", a, evt, true, invoiceFieldTypes(), nil, nil)
		if !would || detail["error"] != nil {
			t.Fatalf("detail = %v would=%v; want it to apply without error", detail, would)
		}
		if detail["via"] != "appointment" || detail["relatedId"] != "ap-7" || detail["after"] != "paid" {
			t.Errorf("detail = %v; want via=appointment relatedId=ap-7 after=paid", detail)
		}
		if _, has := detail["before"]; has {
			t.Errorf("detail = %v; the dry run does not read the related record, so it must not claim a before value", detail)
		}
	})
	t.Run("unlinked: would not apply, says why", func(t *testing.T) {
		evt := workflow.Event{TenantID: "t1", EntityName: "invoices", Data: map[string]any{"status": "paid"}}
		detail, would := previewAction(context.Background(), "wf1", a, evt, true, invoiceFieldTypes(), nil, nil)
		if would {
			t.Fatalf("detail = %v; an unlinked record has nothing to update", detail)
		}
		if msg, _ := detail["note"].(string); !strings.Contains(msg, "appointment") {
			t.Errorf("detail = %v; want a note naming the empty relation", detail)
		}
	})
}
