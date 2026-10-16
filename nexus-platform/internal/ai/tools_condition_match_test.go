package ai

import (
	"context"
	"testing"
)

// TestProposeConditions_MatchBufferedCanonical: both validators read match case-insensitively, but
// the draft card compares it strictly (match === "or"). A draft buffered as "OR" read "và" on the
// card while Apply created an OR rule — the reviewer approved AND and got OR. The buffered match is
// the canonical lowercase token the apply path persists.
func TestProposeConditions_MatchBufferedCanonical(t *testing.T) {
	for _, m := range []struct{ in, want string }{{"OR", "or"}, {" Or ", "or"}, {"AND", "and"}, {"", ""}} {
		t.Run("workflow/"+m.in, func(t *testing.T) {
			e := &toolExecutor{reader: twoTenantWorkflowReader(), tenantID: "tenant-A", docs: fullDocs(0)}
			ok, out := parseToolResult(t, e.execute(context.Background(), "propose_workflow", toolArgs(t, map[string]any{
				"name": "r", "entity": "treatments", "trigger": "after_update",
				"condition": condBlock(m.in, "eq", "done"),
				"actions":   []any{map[string]any{"type": "field_update", "field": "title", "value": "x"}},
			})))
			if !ok {
				t.Fatalf("propose_workflow refused: %v", out)
			}
			if got := e.draft[0].Workflow.Condition.Match; got != m.want {
				t.Errorf("buffered match = %q, want %q", got, m.want)
			}
		})
		t.Run("approval/"+m.in, func(t *testing.T) {
			e := &toolExecutor{reader: twoTenantWorkflowReader(), tenantID: "tenant-A", docs: fullDocs(0)}
			ok, out := parseToolResult(t, e.execute(context.Background(), "propose_approval", toolArgs(t, map[string]any{
				"name": "p", "entity": "treatments", "trigger": "after_update",
				"criteria": condBlock(m.in, "eq", "done"),
				"steps":    []any{map[string]any{"approverType": "role", "approverRef": "manager"}},
			})))
			if !ok {
				t.Fatalf("propose_approval refused: %v", out)
			}
			if got := e.draft[0].Approval.Criteria.Match; got != m.want {
				t.Errorf("buffered match = %q, want %q", got, m.want)
			}
		})
	}
}
