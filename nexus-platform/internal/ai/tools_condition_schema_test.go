package ai

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"nexus/platform/internal/workflow"
)

// conditionSchemaOf returns the condition-block schema a tool actually advertises (the wired
// ToolDefinitions entry, not the helper in isolation) under the given parameter key.
func conditionSchemaOf(t *testing.T, tool, param string) map[string]any {
	t.Helper()
	for _, d := range ToolDefinitions() {
		if d.Function.Name != tool {
			continue
		}
		props, _ := d.Function.Parameters["properties"].(map[string]any)
		s, _ := props[param].(map[string]any)
		if s == nil {
			t.Fatalf("%s has no %q parameter", tool, param)
		}
		return s
	}
	t.Fatalf("tool %q not in ToolDefinitions", tool)
	return nil
}

func schemaProp(t *testing.T, s map[string]any, path ...string) map[string]any {
	t.Helper()
	cur := s
	for _, p := range path {
		props, _ := cur["properties"].(map[string]any)
		if p == "items" {
			cur, _ = cur["items"].(map[string]any)
		} else {
			cur, _ = props[p].(map[string]any)
		}
		if cur == nil {
			t.Fatalf("schema path %v missing at %q", path, p)
		}
	}
	return cur
}

// descTokens splits a description into bare tokens so "changed" never matches inside "changedTo".
func descTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return strings.ContainsRune(" /;.,:()'\"«»", r)
	})
}

// TestConditionSchema_ParityWithValidator (queue #162): propose_workflow's condition and
// propose_approval's criteria share workflowConditionSchema, and the schema used to advertise
// match ['and'] and 9 state operators while both validators accept 'or' plus the edge operators
// changed/changedTo — so the model was never invited to write "A hoặc B" or "vừa đổi thành".
// Parity is checked through the TOOLS (the same validator bodies the apply path runs), both ways.
func TestConditionSchema_ParityWithValidator(t *testing.T) {
	cases := []struct {
		tool, param string
		call        func(match, op string, value any) string // tool args for a one-rule condition
	}{
		{"propose_workflow", "condition", func(match, op string, value any) string {
			return toolArgs(t, map[string]any{
				"name": "r", "entity": "treatments", "trigger": "after_update",
				"condition": condBlock(match, op, value),
				"actions":   []any{map[string]any{"type": "field_update", "field": "title", "value": "x"}},
			})
		}},
		{"propose_approval", "criteria", func(match, op string, value any) string {
			return toolArgs(t, map[string]any{
				"name": "p", "entity": "treatments", "trigger": "after_update",
				"criteria": condBlock(match, op, value),
				"steps":    []any{map[string]any{"approverType": "role", "approverRef": "manager"}},
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			s := conditionSchemaOf(t, tc.tool, tc.param)
			matchEnum, _ := schemaProp(t, s, "match")["enum"].([]string)
			opSchema := schemaProp(t, s, "rules", "items", "op")
			opEnum, _ := opSchema["enum"].([]string)
			valueDesc, _ := schemaProp(t, s, "rules", "items", "value")["description"].(string)
			accepts := func(match, op string, value any) bool {
				e := &toolExecutor{reader: twoTenantWorkflowReader(), tenantID: "tenant-A", docs: fullDocs(0)}
				ok, _ := parseToolResult(t, e.execute(context.Background(), tc.tool, tc.call(match, op, value)))
				return ok
			}

			// Operators: the enum is exactly the engine's operator set.
			if got, want := slices.Sorted(slices.Values(opEnum)), workflow.Operators(); !slices.Equal(got, want) {
				t.Errorf("op enum %v != engine operators %v", got, want)
			}
			// Match: every probe the validator accepts is in the enum, and every enum value is accepted.
			for _, m := range []string{"and", "or", "xor", "any", "all", "not"} {
				if in, acc := slices.Contains(matchEnum, m), accepts(m, "eq", "done"); in != acc {
					t.Errorf("match %q: in schema enum=%v, accepted by validator=%v", m, in, acc)
				}
			}
			// Value rule per operator: the description's BẮT BUỘC / KHÔNG buckets match the validator.
			req, forbid, _ := strings.Cut(valueDesc, "KHÔNG")
			for _, op := range workflow.Operators() {
				withValue, without := accepts("and", op, "done"), accepts("and", op, nil)
				if withValue == without {
					t.Errorf("op %q: validator accepts with-value=%v without=%v — want exactly one", op, withValue, without)
					continue
				}
				bucket := req
				if without {
					bucket = forbid
				}
				if !slices.Contains(descTokens(bucket), op) {
					t.Errorf("op %q (needs value=%v) not taught in the right half of value description %q", op, withValue, valueDesc)
				}
			}
			// The edge operators are taught as update-only.
			opDesc, _ := opSchema["description"].(string)
			for _, want := range []string{"changed", "changedTo", "after_update"} {
				if !slices.Contains(descTokens(opDesc), want) {
					t.Errorf("op description must mention %q: %q", want, opDesc)
				}
			}
		})
	}
}

// TestProposeApproval_EdgeOpOnCreate_Refused: the approval-only limit the description teaches is
// real — an edge operator on an after_create process is refused by the shared validator.
func TestProposeApproval_EdgeOpOnCreate_Refused(t *testing.T) {
	e := &toolExecutor{reader: twoTenantWorkflowReader(), tenantID: "tenant-A", docs: fullDocs(0)}
	mustReject(t, e, "propose_approval", toolArgs(t, map[string]any{
		"name": "p", "entity": "treatments", "trigger": "after_create",
		"criteria": condBlock("and", "changedTo", "done"),
		"steps":    []any{map[string]any{"approverType": "role", "approverRef": "manager"}},
	}), "APPROVAL_CRITERIA_EDGE_OP_UNSUPPORTED")
	desc, _ := conditionSchemaOf(t, "propose_approval", "criteria")["description"].(string)
	if !strings.Contains(desc, "after_update") {
		t.Errorf("criteria description must say edge operators need after_update: %q", desc)
	}
}

// TestProposeWorkflow_EdgeOpWithoutBeforeImage_Refused: the workflow twin of the approval refusal.
// The dispatcher evaluates a create or a delete with no before-image, so an edge rule there is
// always false — stored active and skipped forever. The shared validator refuses it, so the tool
// does too, on both proposable triggers that have no prior image.
func TestProposeWorkflow_EdgeOpWithoutBeforeImage_Refused(t *testing.T) {
	for _, trigger := range []string{"after_create", "after_delete"} {
		for _, op := range []string{"changed", "changedTo"} {
			t.Run(trigger+"/"+op, func(t *testing.T) {
				var value any
				if op == "changedTo" {
					value = "done"
				}
				e := &toolExecutor{reader: twoTenantWorkflowReader(), tenantID: "tenant-A", docs: fullDocs(0)}
				mustReject(t, e, "propose_workflow", toolArgs(t, map[string]any{
					"name": "r", "entity": "treatments", "trigger": trigger,
					"condition": condBlock("and", op, value),
					"actions":   []any{map[string]any{"type": "field_update", "field": "title", "value": "x"}},
				}), "WORKFLOW_CONDITION_NEVER_TRUE")
			})
		}
	}
	desc, _ := conditionSchemaOf(t, "propose_workflow", "condition")["description"].(string)
	if !strings.Contains(desc, "WORKFLOW_CONDITION_NEVER_TRUE") {
		t.Errorf("condition description must name the refusal: %q", desc)
	}
}

func condBlock(match, op string, value any) map[string]any {
	rule := map[string]any{"field": "status", "op": op}
	if value != nil {
		rule["value"] = value
	}
	return map[string]any{"match": match, "rules": []any{rule}}
}

func toolArgs(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
