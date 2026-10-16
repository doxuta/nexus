package services

import (
	"context"
	"testing"

	"nexus/platform/internal/workflow"
)

// Queue #333: a field_update value may be "$record.<field>" or a "{{field}}" template (resolved at fire
// time, like create_record). The save-time check can no longer validate it as a literal, so it checks
// what CAN be known: the source exists on the rule's entity and its type fits the target.

func TestCreateWorkflow_FieldUpdateDynamicValue_CheckedAtSave(t *testing.T) {
	tests := []struct {
		name, field string
		value       any
		code        string // "" = saves
	}{
		{name: "typed copy of a same-type field", field: "amount", value: "$record.amount"},
		{name: "template into a text field", field: "memo", value: "Đơn {{memo}} / {{status}}"},
		{name: "unknown source field", field: "amount", value: "$record.ghost", code: "RECORD_UNKNOWN_FIELD"},
		{name: "source of another type", field: "amount", value: "$record.memo", code: "RECORD_INVALID_TYPE"},
		{name: "template into a number", field: "amount", value: "{{memo}}", code: "RECORD_INVALID_TYPE"},
		{name: "template reading an unknown field", field: "memo", value: "{{ghost}}", code: "RECORD_UNKNOWN_FIELD"},
		{name: "template reading a currency object renders empty forever", field: "memo", value: "{{price}}", code: "RECORD_INVALID_TYPE"},
		{name: "template reading a user id is refused", field: "memo", value: "x {{buyer}}", code: "RECORD_INVALID_TYPE"},
		{name: "$owner into a user field", field: "buyer", value: "$owner"},
		{name: "$owner into a number fails every run", field: "amount", value: "$owner", code: "RECORD_INVALID_TYPE"},
		{name: "$record into a number fails every run", field: "amount", value: "$record", code: "RECORD_INVALID_TYPE"},
		{name: "template reading through a link is not resolvable", field: "memo", value: "{{customer.name}}", code: "WORKFLOW_TEMPLATE_REF_INVALID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeWFRepo()
			svc := NewWorkflowService(repo).WithEntityChecker(orderCatalog())
			_, err := svc.CreateWorkflow(context.Background(), "t1", orderRule("after_update", tt.value, tt.field))
			if tt.code == "" {
				if err != nil {
					t.Fatalf("CreateWorkflow = %v; want the rule saved", err)
				}
				return
			}
			relWantCode(t, err, tt.code)
			if len(repo.created) != 0 {
				t.Errorf("a rule that fails on every run was persisted (%d)", len(repo.created))
			}
		})
	}
}

func TestCreateWorkflow_FieldUpdateDynamicValue_ViaChecksTargetEntity(t *testing.T) {
	tests := []struct {
		name, field string
		value       any
		code        string
	}{
		{name: "invoice text copied to appointment text", field: "memo", value: "$record.note"},
		{name: "invoice text into appointment picklist", field: "status", value: "$record.note", code: "RECORD_INVALID_TYPE"},
		{name: "computed target stays refused", field: "total", value: "$record.note", code: "RECORD_FORMULA_READONLY"},
		{name: "source field of the TARGET entity does not count", field: "memo", value: "$record.memo", code: "RECORD_UNKNOWN_FIELD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewWorkflowService(newFakeWFRepo()).WithEntityChecker(relatedCatalog())
			_, err := svc.CreateWorkflow(context.Background(), "t1", relatedInput("appointment", tt.field, tt.value))
			if tt.code == "" {
				if err != nil {
					t.Fatalf("CreateWorkflow = %v; want saved", err)
				}
				return
			}
			relWantCode(t, err, tt.code)
		})
	}
}

// The dry run shows the value the live run would write, not the placeholder text.
func TestPreviewAction_FieldUpdateDynamicValue_ShowsResolvedValue(t *testing.T) {
	evt := workflow.Event{TenantID: "t1", EntityName: "invoices", RecordID: "inv-1",
		Data: map[string]any{"appointment": "ap-7", "note": "ghi chú A"}}
	for name, a := range map[string]workflow.Action{
		"same record": {Type: "field_update", Field: "note", Value: "{{note}} (đã xử lý)"},
		"via":         {Type: "field_update", Via: "appointment", Field: "memo", Value: "$record.note"},
	} {
		t.Run(name, func(t *testing.T) {
			detail, would := previewAction(context.Background(), "wf1", a, evt, true, invoiceFieldTypes(), nil, nil)
			if !would {
				t.Fatalf("detail = %v; want it to apply", detail)
			}
			want := "ghi chú A"
			if name == "same record" {
				want = "ghi chú A (đã xử lý)"
			}
			if detail["after"] != want {
				t.Errorf("after = %v; want the resolved %q", detail["after"], want)
			}
		})
	}
	t.Run("empty source: would not apply, says why", func(t *testing.T) {
		a := workflow.Action{Type: "field_update", Field: "note", Value: "$record.ghost"}
		detail, would := previewAction(context.Background(), "wf1", a, evt, true, invoiceFieldTypes(), nil, nil)
		if would || detail["note"] == nil {
			t.Fatalf("detail = %v would=%v; want a skip with a note", detail, would)
		}
	})
}
