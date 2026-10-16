package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/workflow"
)

// Queue #81 / specs/043 S14a — the config-time half of set_deadline's `mode` / `stampField` /
// `anchor`. Every option the runner reads is refused here when it is mistyped (B6 validation
// parity): an unknown mode that saved cleanly would fail every firing, and a tighten_only rule with
// no stamp would have no way to tell the machine's deadline from a hand-typed one.
func TestValidateSetDeadline_TightenOnlyRequiresStampField(t *testing.T) {
	fieldTypes := map[string]string{
		"due_date":      "datetime",
		"due_date_auto": "datetime",
		"start_day":     "date",
		"calc":          "formula",
	}
	act := func(mode, stamp, anchor string) []WorkflowActionInput {
		return []WorkflowActionInput{{Type: "set_deadline", Field: "due_date",
			Offset: &WorkflowTimerOffsetInput{Amount: 4, Unit: "business_hours"},
			Mode:   mode, StampField: stamp, Anchor: anchor}}
	}
	for _, tt := range []struct {
		name     string
		actions  []WorkflowActionInput
		wantCode string
	}{
		{"tighten_only + stamp + created_at", act("tighten_only", "due_date_auto", "created_at"), ""},
		{"overwrite + stamp (the creation tiers)", act("overwrite", "due_date_auto", ""), ""},
		{"defaults spelled out", act("overwrite", "", "now"), ""},
		{"tighten_only without a stamp", act("tighten_only", "", "created_at"), "SET_DEADLINE_STAMP_FIELD_INVALID"},
		{"stamp is the deadline field itself", act("tighten_only", "due_date", "created_at"), "SET_DEADLINE_STAMP_FIELD_INVALID"},
		{"stamp field unknown", act("tighten_only", "nope", "created_at"), "SET_DEADLINE_STAMP_FIELD_INVALID"},
		{"stamp field is a date, cannot hold the instant", act("overwrite", "start_day", ""), "SET_DEADLINE_STAMP_FIELD_INVALID"},
		{"stamp field is computed", act("overwrite", "calc", ""), "SET_DEADLINE_STAMP_FIELD_INVALID"},
		{"unknown mode (the rejected extend_only draft)", act("extend_only", "due_date_auto", ""), "WORKFLOW_ACTION_INVALID"},
		{"unknown anchor", act("", "", "updated_at"), "WORKFLOW_ACTION_INVALID"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateWorkflowConfig("tk_tickets", "after_update", nil, tt.actions, fieldTypes)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("ValidateWorkflowConfig = %v; want nil", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != tt.wantCode {
				t.Fatalf("err = %v; want code %s", err, tt.wantCode)
			}
		})
	}
}

// The defaults are stored as ABSENT, so a rule saved before these options existed and one saved
// after with the defaults are byte-identical — the pack diff never reports a phantom change, and
// the stored shape the runner reads is the one the engine documents.
func TestBuildActionsJSON_SetDeadlineDefaultsStoredAbsentOptionsKept(t *testing.T) {
	fieldTypes := map[string]string{"due_date": "datetime", "due_date_auto": "datetime"}
	b, err := buildActionsJSON([]WorkflowActionInput{
		{Type: "set_deadline", Field: "due_date", Offset: &WorkflowTimerOffsetInput{Amount: 4, Unit: "hours"}, Mode: "overwrite", Anchor: "now"},
		{Type: "set_deadline", Field: "due_date", Offset: &WorkflowTimerOffsetInput{Amount: 4, Unit: "hours"},
			Mode: " tighten_only ", StampField: " due_date_auto ", Anchor: " created_at "},
	}, fieldTypes)
	if err != nil {
		t.Fatalf("buildActionsJSON: %v", err)
	}
	var got []workflow.Action
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	if strings.Contains(string(b), `"mode":"overwrite"`) || strings.Contains(string(b), `"anchor":"now"`) {
		t.Fatalf("defaults were persisted: %s", b)
	}
	if got[1].Mode != workflow.DeadlineModeTightenOnly || got[1].StampField != "due_date_auto" || got[1].Anchor != workflow.DeadlineAnchorCreatedAt {
		t.Fatalf("tighten_only options not kept (trimmed): %+v", got[1])
	}
}

// The dry run of a tighten_only rule must give the SAME verdict the run will — it goes through the
// one planner both share (workflow.PlanDeadline). Lesson #10 of the set_deadline slices: an option
// opened in the validator and the runner but not in the rehearsal makes "Chạy thử" promise a write
// the rule will not make (or hide one it will).
func TestPreviewAction_SetDeadlineTightenOnly(t *testing.T) {
	created := time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC)
	promised := created.Add(40 * time.Hour).Format("2006-01-02 15:04:05")
	act := workflow.Action{Type: workflow.ActionSetDeadline, Field: "due_date",
		Offset: &workflow.TimerOffset{Amount: 4, Unit: workflow.TimerUnitHours},
		Mode:   workflow.DeadlineModeTightenOnly, StampField: "due_date_auto", Anchor: workflow.DeadlineAnchorCreatedAt}
	fieldTypes := map[string]string{"due_date": "datetime", "due_date_auto": "datetime", "subject": "text"}
	evt := func(data map[string]any) workflow.Event {
		return workflow.Event{TenantID: "t1", EntityName: "tk_tickets", RecordID: "r1",
			Trigger: workflow.TriggerAfterUpdate, Data: data, CreatedAt: created}
	}

	t.Run("machine deadline, earlier tier → would apply, from creation", func(t *testing.T) {
		detail, would := previewAction(context.Background(), "wf-1", act,
			evt(map[string]any{"due_date": promised, "due_date_auto": promised}), true, fieldTypes, nil, nil)
		if !would || detail["wouldApply"] != true {
			t.Fatalf("wouldApply = %v; want true (detail=%+v)", would, detail)
		}
		if want := created.Add(4 * time.Hour).Format(time.RFC3339); detail["after"] != want {
			t.Fatalf("after = %v; want %s (created_at + 4h, not now + 4h)", detail["after"], want)
		}
		if detail["mode"] != workflow.DeadlineModeTightenOnly || detail["stampField"] != "due_date_auto" || detail["anchor"] != workflow.DeadlineAnchorCreatedAt {
			t.Fatalf("detail does not name the policy: %+v", detail)
		}
	})

	t.Run("hand-typed deadline → would NOT apply, says why, not an error", func(t *testing.T) {
		detail, would := previewAction(context.Background(), "wf-1", act,
			evt(map[string]any{"due_date": "2026-12-24 02:00:00", "due_date_auto": promised}), true, fieldTypes, nil, nil)
		if would || detail["wouldApply"] != false {
			t.Fatalf("wouldApply = %v; want false (detail=%+v)", would, detail)
		}
		if _, isErr := detail["error"]; isErr {
			t.Fatalf("keeping a hand-typed deadline is policy, not an error: %+v", detail)
		}
		if keep, _ := detail["keep"].(string); !strings.Contains(keep, "gõ tay") {
			t.Fatalf("detail[keep] = %q; want the reason (hand-typed)", keep)
		}
	})

	t.Run("stamp field deleted after the rule was saved → error row", func(t *testing.T) {
		detail, would := previewAction(context.Background(), "wf-1", act,
			evt(map[string]any{"due_date": promised, "due_date_auto": promised}), true,
			map[string]string{"due_date": "datetime"}, nil, nil)
		if would {
			t.Fatalf("wouldApply = true with the stamp field gone (detail=%+v)", detail)
		}
		if e, _ := detail["error"].(string); !strings.Contains(e, "SET_DEADLINE_STAMP_FIELD_INVALID") {
			t.Fatalf("detail[error] = %q; want SET_DEADLINE_STAMP_FIELD_INVALID", e)
		}
	})
}
