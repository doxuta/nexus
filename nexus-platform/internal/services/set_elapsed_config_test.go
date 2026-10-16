package services

import (
	"errors"
	"testing"
)

// TestValidateWorkflowConfig_SetElapsed pins the CONFIG-time guards of the set_elapsed action
// through ValidateWorkflowConfig — the SAME body CreateWorkflow/UpdateWorkflow run
// (buildWorkflowRule → buildActionsJSON).
//
// The guard that matters: the target must be a plain `number`. The action writes a float64 count
// of minutes; a text/datetime/currency/rating target would save cleanly and fail at fire time
// (currency wants an object, rating is 1-5), i.e. a rule that looks configured and records a FAILED
// run on every ticket closed — the validation-parity trap (B6).
func TestValidateWorkflowConfig_SetElapsed(t *testing.T) {
	fieldTypes := map[string]string{
		"resolution_minutes": "number",
		"subject":            "text",
		"closed_at":          "datetime",
		"cost":               "currency",
		"score":              "rating",
		"calc":               "formula",
		"ticket_no":          "auto_number",
	}
	elapsed := func(field string) []WorkflowActionInput {
		return []WorkflowActionInput{{Type: "set_elapsed", Field: field}}
	}
	tests := []struct {
		name     string
		actions  []WorkflowActionInput
		wantCode string // "" = expect no error
	}{
		{"valid: working minutes into a number field", elapsed("resolution_minutes"), ""},
		{"valid: target name is trimmed", elapsed("  resolution_minutes "), ""},
		{"empty target field", elapsed(" "), "WORKFLOW_ACTION_INVALID"},
		{"unknown target field", elapsed("nope"), "RECORD_UNKNOWN_FIELD"},
		{"computed target → readonly (parity field_update)", elapsed("calc"), "RECORD_FORMULA_READONLY"},
		{"auto_number target → readonly", elapsed("ticket_no"), "RECORD_AUTONUMBER_READONLY"},
		{"text target cannot hold a count", elapsed("subject"), "SET_ELAPSED_FIELD_TYPE_INVALID"},
		{"datetime target cannot hold a count", elapsed("closed_at"), "SET_ELAPSED_FIELD_TYPE_INVALID"},
		{"currency target wants an object, not a count", elapsed("cost"), "SET_ELAPSED_FIELD_TYPE_INVALID"},
		{"rating target is 1-5, not minutes", elapsed("score"), "SET_ELAPSED_FIELD_TYPE_INVALID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateWorkflowConfig("tk_tickets", "after_update", nil, tt.actions, fieldTypes)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("ValidateWorkflowConfig = %v; want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateWorkflowConfig = nil; want code %s", tt.wantCode)
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error %v is not a *ValidationError; want %s", err, tt.wantCode)
			}
			if ve.Code != tt.wantCode {
				t.Fatalf("code = %q; want %q (err=%v)", ve.Code, tt.wantCode, err)
			}
		})
	}
}
