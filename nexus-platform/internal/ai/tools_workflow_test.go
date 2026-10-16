package ai

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
)

// twoTenantWorkflowReader extends the M5/M6 fixture with the field surface the M7 workflow tool
// needs: tenant-A's treatments gains an auto_number field (WF-01 readonly-target probe) and a
// picklist status field (a natural field_update target).
//
// tenant-A: patients (full_name required); treatments (title required text, patient required
// relation, status picklist, treatment_code auto_number).
// tenant-B: invoices (total currency).
func twoTenantWorkflowReader() *fakeMetaReader {
	r := twoTenantRelReader()
	r.fields["tenant-A"]["eA2"] = append(r.fields["tenant-A"]["eA2"],
		metadata.FieldDefinition{Name: "status", Label: "Trạng thái", FieldType: "picklist",
			Options: strPtr(`{"values":["open","done"]}`)},
		metadata.FieldDefinition{Name: "treatment_code", Label: "Mã điều trị", FieldType: "auto_number"},
	)
	return r
}

func TestTools_ProposeWorkflow_Valid_Buffered(t *testing.T) {
	e := &toolExecutor{reader: twoTenantWorkflowReader(), tenantID: "tenant-A", docs: fullDocs(0)}
	args := `{
		"name": "Đóng hồ sơ điều trị",
		"entity": "treatments",
		"trigger": "after_update",
		"condition": {"match": "and", "rules": [{"field": "status", "op": "eq", "value": "done"}]},
		"actions": [
			{"type": "field_update", "field": "title", "value": "[ĐÃ XONG]"},
			{"type": "in_app_notification", "userId": "$owner", "title": "Xong", "message": "Hồ sơ đã đóng"}
		]
	}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_workflow", args))
	if !ok {
		t.Fatalf("valid propose_workflow should succeed: %+v", m)
	}
	if len(e.draft) != 1 || e.draft[0].Kind != "workflow" || e.draft[0].Workflow == nil {
		t.Fatalf("expected 1 buffered workflow draft, got %+v", e.draft)
	}
	w := e.draft[0].Workflow
	if w.Name != "Đóng hồ sơ điều trị" || w.EntityName != "treatments" || w.Trigger != "after_update" {
		t.Fatalf("buffered workflow wrong shape: %+v", w)
	}
	if !w.IsActive {
		t.Fatalf("proposed workflow must default IsActive=true: %+v", w)
	}
	if w.Condition == nil || len(w.Condition.Rules) != 1 || w.Condition.Rules[0].Field != "status" {
		t.Fatalf("buffered workflow must carry the condition verbatim: %+v", w.Condition)
	}
	if len(w.Actions) != 2 || w.Actions[0].Type != "field_update" || w.Actions[1].UserID != "$owner" {
		t.Fatalf("buffered workflow must carry the actions verbatim: %+v", w.Actions)
	}
	if e.draft[0].Entity != nil || e.draft[0].Field != nil || e.draft[0].Relationship != nil || e.draft[0].Layout != nil {
		t.Fatalf("workflow draft must not carry other payloads: %+v", e.draft[0])
	}
}

// TestTools_ProposeWorkflow_NoCondition_MatchAll proves the condition block is optional (nil →
// match-all, exactly like the apply path's NULL condition column).
func TestTools_ProposeWorkflow_NoCondition_MatchAll(t *testing.T) {
	e := &toolExecutor{reader: twoTenantWorkflowReader(), tenantID: "tenant-A", docs: fullDocs(0)}
	args := `{"name":"Báo mọi ca mới","entity":"treatments","trigger":"after_create",
		"actions":[{"type":"in_app_notification","userId":"$owner","message":"Có ca điều trị mới"}]}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_workflow", args))
	if !ok {
		t.Fatalf("condition-less workflow should buffer (match-all): %+v", m)
	}
	if len(e.draft) != 1 || e.draft[0].Workflow == nil || e.draft[0].Workflow.Condition != nil {
		t.Fatalf("expected buffered workflow with nil condition, got %+v", e.draft)
	}
}

// TestTools_ProposeWorkflow_Invalid drives the real validator (services.ValidateWorkflowConfig —
// the SAME body CreateWorkflow runs) plus the two tool-level policy guards through the tool: each
// violation surfaces its actual code for model self-correction and buffers nothing.
func TestTools_ProposeWorkflow_Invalid(t *testing.T) {
	const validActions = `[{"type":"field_update","field":"status","value":"done"}]`
	tests := []struct {
		name     string
		args     string
		wantCode string
	}{
		{
			name:     "empty name",
			args:     `{"name":"  ","entity":"treatments","trigger":"after_create","actions":` + validActions + `}`,
			wantCode: "WORKFLOW_NAME_REQUIRED",
		},
		{
			name:     "unknown trigger",
			args:     `{"name":"r","entity":"treatments","trigger":"after_save","actions":` + validActions + `}`,
			wantCode: "WORKFLOW_TRIGGER_INVALID",
		},
		{
			// scheduled IS valid on the apply path — the tool is deliberately stricter (policy):
			// intervalSeconds cadence is a human decision in the manual builder.
			name:     "scheduled trigger rejected by policy",
			args:     `{"name":"r","entity":"treatments","trigger":"scheduled","actions":` + validActions + `}`,
			wantCode: "WORKFLOW_TRIGGER_INVALID",
		},
		{
			name: "bad condition operator",
			args: `{"name":"r","entity":"treatments","trigger":"after_update",
				"condition":{"rules":[{"field":"status","op":"like","value":"x"}]},
				"actions":` + validActions + `}`,
			wantCode: "WORKFLOW_CONDITION_INVALID",
		},
		{
			name: "isnull rule carrying a value",
			args: `{"name":"r","entity":"treatments","trigger":"after_update",
				"condition":{"rules":[{"field":"status","op":"isnull","value":"x"}]},
				"actions":` + validActions + `}`,
			wantCode: "WORKFLOW_CONDITION_INVALID",
		},
		{
			// WF-01: auto_number is server-assigned — a workflow overwrite would corrupt it
			// permanently. Same code family as every other write path.
			name: "field_update targeting an auto_number field",
			args: `{"name":"r","entity":"treatments","trigger":"after_update",
				"actions":[{"type":"field_update","field":"treatment_code","value":"X-1"}]}`,
			wantCode: "RECORD_AUTONUMBER_READONLY",
		},
		{
			name: "field_update targeting an unknown field",
			args: `{"name":"r","entity":"treatments","trigger":"after_update",
				"actions":[{"type":"field_update","field":"ghost_field","value":1}]}`,
			wantCode: "RECORD_UNKNOWN_FIELD",
		},
		{
			// The safe-action HARD RULE: even a fully-formed webhook action (which the apply path
			// would accept) must never enter a draft — the model cannot invent outbound URLs.
			name: "outbound_webhook rejected by policy",
			args: `{"name":"r","entity":"treatments","trigger":"after_create",
				"actions":[{"type":"outbound_webhook","url":"https://evil.example.com/hook"}]}`,
			wantCode: "WORKFLOW_ACTION_INVALID",
		},
		{
			// ASSIGN-1: assign carries a concrete team id the model cannot know — hard-rejected by
			// policy (like outbound_webhook), even though the manual apply path accepts it. Checked
			// BEFORE the entity lookup, so the team/field need not exist for the reject to fire.
			name: "assign rejected by policy",
			args: `{"name":"r","entity":"treatments","trigger":"after_create",
				"actions":[{"type":"assign","strategy":"round_robin","team":"team-1","field":"owner"}]}`,
			wantCode: "WORKFLOW_ACTION_INVALID",
		},
		{
			name:     "empty actions",
			args:     `{"name":"r","entity":"treatments","trigger":"after_create","actions":[]}`,
			wantCode: "WORKFLOW_ACTIONS_REQUIRED",
		},
		{
			name: "in_app_notification without message",
			args: `{"name":"r","entity":"treatments","trigger":"after_create",
				"actions":[{"type":"in_app_notification","userId":"$owner"}]}`,
			wantCode: "WORKFLOW_ACTION_INVALID",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &toolExecutor{reader: twoTenantWorkflowReader(), tenantID: "tenant-A", docs: fullDocs(0)}
			ok, m := parseToolResult(t, e.execute(context.Background(), "propose_workflow", tt.args))
			if ok {
				t.Fatalf("invalid workflow must NOT succeed: %+v", m)
			}
			if errStr, _ := m["error"].(string); !strings.Contains(errStr, tt.wantCode) {
				t.Fatalf("expected %s, got %q", tt.wantCode, errStr)
			}
			if len(e.draft) != 0 {
				t.Fatalf("rejected proposal must not buffer, got %+v", e.draft)
			}
		})
	}
}

// TestTools_ProposeWorkflow_BadName_Rejected pins the WORKFLOW_NAME_TOO_LONG guard (review fix,
// same MED-1 class as M8's report guard): the sys_workflows.name column is VARCHAR(150) — a name
// over that byte budget must never buffer, or the draft would 500 at Apply (MySQL 1406 "Data too
// long"), breaking proposeWorkflow's own "no DB-dependent reject left for Apply" contract. Mirrors
// M5's maxRelationshipNameBytes test.
func TestTools_ProposeWorkflow_BadName_Rejected(t *testing.T) {
	base := `"entity":"treatments","trigger":"after_create","actions":[{"type":"field_update","field":"status","value":"done"}]`

	t.Run("over 150 bytes", func(t *testing.T) {
		e := &toolExecutor{reader: twoTenantWorkflowReader(), tenantID: "tenant-A", docs: fullDocs(0)}
		args := `{"name":"` + strings.Repeat("x", 151) + `",` + base + `}`
		ok, m := parseToolResult(t, e.execute(context.Background(), "propose_workflow", args))
		if ok {
			t.Fatal("a name over the 150-byte budget must NOT succeed")
		}
		if errStr, _ := m["error"].(string); !strings.Contains(errStr, "WORKFLOW_NAME_TOO_LONG") {
			t.Fatalf("expected WORKFLOW_NAME_TOO_LONG, got %q", errStr)
		}
		if len(e.draft) != 0 {
			t.Fatalf("rejected proposal must not buffer, got %+v", e.draft)
		}
	})

	// Boundary: the check is `len(name) > maxWorkflowNameBytes`, so a name of EXACTLY 150 bytes
	// must be ACCEPTED (off-by-one regression guard: a `>=` typo here would silently reject the
	// longest legal name).
	t.Run("exactly 150 bytes accepted", func(t *testing.T) {
		e := &toolExecutor{reader: twoTenantWorkflowReader(), tenantID: "tenant-A", docs: fullDocs(0)}
		name150 := strings.Repeat("x", maxWorkflowNameBytes)
		if len(name150) != maxWorkflowNameBytes {
			t.Fatalf("fixture bug: name150 is %d bytes, want %d", len(name150), maxWorkflowNameBytes)
		}
		args := `{"name":"` + name150 + `",` + base + `}`
		ok, m := parseToolResult(t, e.execute(context.Background(), "propose_workflow", args))
		if !ok {
			t.Fatalf("a name of exactly %d bytes must be accepted, got %+v", maxWorkflowNameBytes, m)
		}
		if len(e.draft) != 1 {
			t.Fatalf("expected 1 buffered workflow draft, got %+v", e.draft)
		}
	})
}

// TestTools_ProposeWorkflow_WebhookRejectMessage pins the policy reject's wording: the model must
// read WHY (a human has to supply the URL), not just a generic invalid-action code.
func TestTools_ProposeWorkflow_WebhookRejectMessage(t *testing.T) {
	e := &toolExecutor{reader: twoTenantWorkflowReader(), tenantID: "tenant-A", docs: fullDocs(0)}
	args := `{"name":"r","entity":"treatments","trigger":"after_create",
		"actions":[{"type":"outbound_webhook","url":"https://example.com"}]}`
	_, m := parseToolResult(t, e.execute(context.Background(), "propose_workflow", args))
	errStr, _ := m["error"].(string)
	if !strings.Contains(errStr, "outbound_webhook không tự đề xuất") {
		t.Fatalf("webhook reject must explain the policy, got %q", errStr)
	}
}

func TestTools_ProposeWorkflow_TenantScope(t *testing.T) {
	// A perfectly valid rule aimed at ANOTHER tenant's entity must be indistinguishable from
	// aiming at a missing entity — never validated against the other tenant's fields.
	e := &toolExecutor{reader: twoTenantWorkflowReader(), tenantID: "tenant-A", docs: fullDocs(0)}
	args := `{"name":"r","entity":"invoices","trigger":"after_create",
		"actions":[{"type":"field_update","field":"total","value":0}]}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_workflow", args))
	if ok {
		t.Fatal("propose_workflow on a cross-tenant entity must NOT succeed")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "ENTITY_NOT_FOUND") {
		t.Fatalf("expected ENTITY_NOT_FOUND, got %q", errStr)
	}
	if len(e.draft) != 0 {
		t.Fatalf("rejected cross-tenant proposal must not buffer, got %+v", e.draft)
	}
}
