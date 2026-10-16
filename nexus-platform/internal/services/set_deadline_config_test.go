package services

import (
	"errors"
	"testing"
)

// TestValidateWorkflowConfig_SetDeadline pins the CONFIG-time guards of the set_deadline action
// through ValidateWorkflowConfig — the SAME body CreateWorkflow/UpdateWorkflow run
// (buildWorkflowRule → buildActionsJSON).
//
// The two guards that matter most here are the ones that would otherwise produce a rule that looks
// saved and does nothing useful:
//   - a NON-datetime target (a `date` field holds "YYYY-MM-DD" and a `text` field holds anything):
//     the action writes an RFC3339 instant, which a date field's validator rejects at fire time —
//     so without this guard the rule saves cleanly and every firing is a FAILED run nobody reads.
//   - the unit vocabulary: it must stay EQUAL to the date_offset trigger's (workflow.ValidTimerUnit),
//     business_hours included since DAILY-QUEUE #10 threaded the tenant calendar into the action.
//     A unit this validator accepts but the runner cannot compute saves cleanly and fails on every
//     firing; a unit it rejects that the timer accepts means a deadline and the reminder about it
//     speak different languages.
func TestValidateWorkflowConfig_SetDeadline(t *testing.T) {
	fieldTypes := map[string]string{
		"due_date":   "datetime",
		"start_day":  "date",
		"subject":    "text",
		"calc":       "formula",
		"ticket_no":  "auto_number",
		"resolution": "text",
	}
	deadline := func(field string, amount int, unit string) []WorkflowActionInput {
		a := WorkflowActionInput{Type: "set_deadline", Field: field}
		if unit != "" || amount != 0 {
			a.Offset = &WorkflowTimerOffsetInput{Amount: amount, Unit: unit}
		}
		return []WorkflowActionInput{a}
	}
	tests := []struct {
		name     string
		actions  []WorkflowActionInput
		wantCode string // "" = expect no error
	}{
		{"valid +4 hours into a datetime field", deadline("due_date", 4, "hours"), ""},
		{"valid +30 minutes", deadline("due_date", 30, "minutes"), ""},
		{"valid +3 days", deadline("due_date", 3, "days"), ""},
		{"empty target field", deadline("", 4, "hours"), "WORKFLOW_ACTION_INVALID"},
		{"missing offset", []WorkflowActionInput{{Type: "set_deadline", Field: "due_date"}}, "WORKFLOW_ACTION_INVALID"},
		{"zero amount", deadline("due_date", 0, "hours"), "SET_DEADLINE_OFFSET_INVALID"},
		{"negative amount", deadline("due_date", -1, "hours"), "SET_DEADLINE_OFFSET_INVALID"},
		{"unknown unit", deadline("due_date", 4, "fortnights"), "SET_DEADLINE_UNIT_INVALID"},
		{"valid +4 business hours (#10: calendar wired)", deadline("due_date", 4, "business_hours"), ""},
		{"unknown target field", deadline("nope", 4, "hours"), "RECORD_UNKNOWN_FIELD"},
		{"computed target → readonly (parity field_update)", deadline("calc", 4, "hours"), "RECORD_FORMULA_READONLY"},
		{"auto_number target → readonly", deadline("ticket_no", 4, "hours"), "RECORD_AUTONUMBER_READONLY"},
		{"date target cannot hold an instant", deadline("start_day", 4, "hours"), "SET_DEADLINE_FIELD_TYPE_INVALID"},
		{"text target cannot hold an instant", deadline("subject", 4, "hours"), "SET_DEADLINE_FIELD_TYPE_INVALID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateWorkflowConfig("tk_tickets", "after_create", nil, tt.actions, fieldTypes)
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
