package services

// Config-time coverage for workflow condition JOIN MODE (AND/OR). The engine-side Evaluate OR
// semantics are proven in internal/workflow/workflow_test.go; here we prove the SAVE path accepts
// "or", rejects anything else, defaults to "and", and persists the join mode so a saved rule
// actually fires with OR semantics (buildConditionJSON is the one place the mode is validated +
// written).

import (
	"encoding/json"
	"testing"

	"nexus/platform/internal/workflow"
)

func TestBuildConditionJSON_JoinMode(t *testing.T) {
	rules := []WorkflowRuleInput{
		{Field: "status", Op: "eq", Value: "won"},
		{Field: "amount", Op: "gt", Value: float64(1000)},
	}

	decode := func(t *testing.T, blob []byte) workflow.Condition {
		t.Helper()
		var c workflow.Condition
		if err := json.Unmarshal(blob, &c); err != nil {
			t.Fatalf("unmarshal condition: %v", err)
		}
		return c
	}

	t.Run("or is accepted and persisted", func(t *testing.T) {
		blob, err := buildConditionJSON("or", rules, nil, "")
		if err != nil {
			t.Fatalf("buildConditionJSON(or): %v", err)
		}
		if got := decode(t, blob); got.Match != "or" {
			t.Fatalf("persisted match = %q, want or", got.Match)
		}
	})

	t.Run("empty and and default to and", func(t *testing.T) {
		for _, m := range []string{"", "and", "AND"} {
			blob, err := buildConditionJSON(m, rules, nil, "")
			if err != nil {
				t.Fatalf("buildConditionJSON(%q): %v", m, err)
			}
			if got := decode(t, blob); got.Match != "and" {
				t.Fatalf("match %q → persisted %q, want and", m, got.Match)
			}
		}
	})

	t.Run("unsupported join mode rejected", func(t *testing.T) {
		if _, err := buildConditionJSON("xor", rules, nil, ""); codeOf(err) != "WORKFLOW_CONDITION_INVALID" {
			t.Fatalf("code = %q, want WORKFLOW_CONDITION_INVALID", codeOf(err))
		}
	})

	t.Run("garbage match rejected even on a timer-only condition (0 rules)", func(t *testing.T) {
		timer := &workflow.TimerConfig{
			DateField: "expiry_date",
			Offset:    workflow.TimerOffset{Amount: 1, Unit: "days"},
			Direction: "before",
		}
		if _, err := buildConditionJSON("xor", nil, timer, ""); codeOf(err) != "WORKFLOW_CONDITION_INVALID" {
			t.Fatalf("code = %q, want WORKFLOW_CONDITION_INVALID (typo must not slip in via a timer-only condition)", codeOf(err))
		}
	})

	t.Run("persisted OR condition fires with OR semantics", func(t *testing.T) {
		blob, err := buildConditionJSON("or", []WorkflowRuleInput{
			{Field: "status", Op: "eq", Value: "lost"},         // false for the data below
			{Field: "amount", Op: "gte", Value: float64(1000)}, // true
		}, nil, "")
		if err != nil {
			t.Fatalf("buildConditionJSON: %v", err)
		}
		cond := decode(t, blob)
		if !workflow.Evaluate(&cond, map[string]any{"status": "won", "amount": float64(1500)}) {
			t.Fatalf("saved OR condition should fire when one rule matches")
		}
	})
}

// TestValidateWorkflowConfig_OrAccepted proves the exported config validator (the AI-Builder /
// admin save gate) accepts an OR condition end-to-end.
func TestValidateWorkflowConfig_OrAccepted(t *testing.T) {
	cond := &WorkflowConditionInput{Match: "or", Rules: []WorkflowRuleInput{
		{Field: "stage", Op: "eq", Value: "new"},
		{Field: "stage", Op: "eq", Value: "reopened"},
	}}
	actions := []WorkflowActionInput{{Type: "field_update", Field: "flag", Value: "1"}}
	fieldTypes := map[string]string{"stage": "picklist", "flag": "text"}
	if err := ValidateWorkflowConfig("tickets", "after_update", cond, actions, fieldTypes); err != nil {
		t.Fatalf("ValidateWorkflowConfig(or) = %v, want nil", err)
	}
}
