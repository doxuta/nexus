package services

// The dry run's answer for create_record.
//
// create_record is a first-class engine action with config-time validation and a full editor in
// the builder, but previewAction's default arm answered "action type không được hỗ trợ", which the
// FE renders as a red BLOCKED row. The highest-blast-radius rule Nexus has — one record creating
// another — was the one rule you could not rehearse, and the Test button that exists to build
// confidence before going live said it would not work.

import (
	"context"
	"testing"

	"nexus/platform/internal/workflow"
)

func TestPreviewAction_CreateRecord(t *testing.T) {
	act := workflow.Action{
		Type:         workflow.ActionCreateRecord,
		CreateEntity: "tasks",
		Values:       map[string]any{"title": "Gọi lại khách", "owner": "$owner"},
	}
	evt := workflow.Event{Data: map[string]any{"status": "New"}}

	t.Run("condition matched → the plan is reported, not refused", func(t *testing.T) {
		detail, would := previewAction(context.Background(), "wf-1", act, evt, true, map[string]string{"status": "picklist"}, nil, nil)
		if !would {
			t.Fatal("wouldRun = false — the dry run still refuses a shipped action")
		}
		if _, isErr := detail["error"]; isErr {
			t.Fatalf("detail carries an error: %v", detail)
		}
		if detail["entityName"] != "tasks" {
			t.Errorf("entityName = %v, want the TARGET entity", detail["entityName"])
		}
		if detail["wouldCreate"] != true {
			t.Errorf("wouldCreate = %v, want true", detail["wouldCreate"])
		}
		vals, _ := detail["values"].(map[string]any)
		if vals["title"] != "Gọi lại khách" {
			t.Errorf("values = %v, want the configured field map", detail["values"])
		}
	})

	t.Run("condition not matched → would not create, still no error", func(t *testing.T) {
		detail, would := previewAction(context.Background(), "wf-1", act, evt, false, nil, nil, nil)
		if would || detail["wouldCreate"] != false {
			t.Errorf("would=%v detail=%v, want a clean 'would not run'", would, detail)
		}
		if _, isErr := detail["error"]; isErr {
			t.Errorf("an unmatched condition must not read as a broken action: %v", detail)
		}
	})

	t.Run("the TRIGGER entity's field list must not invent errors about the TARGET", func(t *testing.T) {
		// fieldTypes describes the entity that FIRED the rule. create_record writes a different
		// entity, so validating its values against this map would report fields as missing that
		// were never supposed to be here. The real check runs at config time.
		detail, would := previewAction(context.Background(), "wf-1", act, evt, true, map[string]string{"status": "picklist"}, nil, nil)
		if !would {
			t.Fatalf("trigger-entity metadata blocked a create on another entity: %v", detail)
		}
	})

	t.Run("a genuinely unknown action still says so", func(t *testing.T) {
		detail, would := previewAction(context.Background(), "wf-1", workflow.Action{Type: "teleport_record"}, evt, true, nil, nil, nil)
		if would || detail["error"] == nil {
			t.Errorf("unknown type = %v (would=%v), want the honest refusal", detail, would)
		}
	})
}
