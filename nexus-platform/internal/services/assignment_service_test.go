package services

import (
	"errors"
	"testing"
)

// TestNextInRing exercises the pure round-robin advance helper: the member AFTER last_user_id in
// the ordered ring, wrapping at the end, and restarting from the first when the cursor is fresh
// (empty last) or when last is no longer in the ring (member left the team). This is the logic the
// FOR UPDATE tx computes under the lock; keeping it pure lets the round-robin sequence be asserted
// without a database (spec §9 test #1 + #3).
func TestNextInRing(t *testing.T) {
	members := []string{"u1", "u2", "u3"}
	tests := []struct {
		name    string
		members []string
		last    string
		want    string
	}{
		{"fresh cursor (empty last) → first", members, "", "u1"},
		{"after first → second", members, "u1", "u2"},
		{"after second → third", members, "u2", "u3"},
		{"after last → wrap to first", members, "u3", "u1"},
		{"last no longer in team → restart first", members, "gone", "u1"},
		{"single member wraps to itself", []string{"solo"}, "solo", "solo"},
		{"single member fresh → itself", []string{"solo"}, "", "solo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextInRing(tt.members, tt.last); got != tt.want {
				t.Fatalf("nextInRing(%v, %q) = %q; want %q", tt.members, tt.last, got, tt.want)
			}
		})
	}
}

// TestNextInRing_FullRotationSequence proves the exact spec §9 test #1 sequence: a 3-member team
// fired 4 times threads the cursor u1 → u2 → u3 → u1 (advance + wrap), by feeding each pick back
// as the next call's last (what the persisted cursor does across fires).
func TestNextInRing_FullRotationSequence(t *testing.T) {
	members := []string{"u1", "u2", "u3"}
	want := []string{"u1", "u2", "u3", "u1"}
	last := ""
	for i, w := range want {
		got := nextInRing(members, last)
		if got != w {
			t.Fatalf("fire %d: nextInRing(last=%q) = %q; want %q", i+1, last, got, w)
		}
		last = got
	}
}

// TestValidateWorkflowConfig_Assign covers the config-time (parity) validation of the assign
// action through ValidateWorkflowConfig — the SAME body CreateWorkflow/UpdateWorkflow run
// (buildWorkflowRule → buildActionsJSON). Team EXISTENCE is DB-dependent (checkAssignTeams, apply
// path only) and NOT asserted here; the non-empty team, strategy allowlist, and field-target guard
// ARE. The field-guard order (unknown → readonly → non-text) is the point of the computed vs
// number cases.
func TestValidateWorkflowConfig_Assign(t *testing.T) {
	fieldTypes := map[string]string{
		"owner":  "text",
		"rep":    "user", // the CORRECT assignment target since the `user` field type landed
		"amount": "number",
		"calc":   "formula",
		"code":   "auto_number",
	}
	assign := func(strategy, team, field string) []WorkflowActionInput {
		return []WorkflowActionInput{{Type: "assign", Strategy: strategy, Team: team, Field: field}}
	}
	tests := []struct {
		name     string
		actions  []WorkflowActionInput
		wantCode string // "" = expect no error
	}{
		{"valid round_robin into text field", assign("round_robin", "team-1", "owner"), ""},
		{"valid load_based into text field (ASSIGN-2)", assign("load_based", "team-1", "owner"), ""},
		{"empty strategy defaults to round_robin (back-compat)", assign("", "team-1", "owner"), ""},
		{"unknown strategy", assign("random", "team-1", "owner"), "ASSIGN_STRATEGY_INVALID"},
		{"empty team", assign("round_robin", "", "owner"), "ASSIGN_TEAM_INVALID"},
		{"empty target field", assign("round_robin", "team-1", ""), "ASSIGN_FIELD_REQUIRED"},
		{"unknown target field", assign("round_robin", "team-1", "nope"), "RECORD_UNKNOWN_FIELD"},
		{"computed target → readonly (parity field_update)", assign("round_robin", "team-1", "calc"), "RECORD_FORMULA_READONLY"},
		{"auto_number target → readonly", assign("round_robin", "team-1", "code"), "RECORD_AUTONUMBER_READONLY"},
		// A `user` target is the one this action was always trying to express; it was rejected until
		// 2026-07-29 because the guard predated the type, which is why the packs used free text and
		// every screen showed a raw UUID. `text` MUST stay valid (row above) — tenants already
		// installed route into a text column and would break at fire time.
		{"valid round_robin into user field", assign("round_robin", "team-1", "rep"), ""},
		{"valid load_based into user field", assign("load_based", "team-1", "rep"), ""},
		{"neither user nor text → type invalid", assign("round_robin", "team-1", "amount"), "ASSIGN_FIELD_TYPE_INVALID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateWorkflowConfig("tickets", "after_create", nil, tt.actions, fieldTypes)
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
