package ai

// specs/048 S1a (T012, FR-011): the AI builder never proposes a before_save / before_delete rule —
// a rule that can block every save of an entity is a human decision made in the manual builder.
// The refusal must say so by name, so the model (and the admin reading the chat) is pointed at the
// builder instead of retrying with another spelling.

import (
	"context"
	"strings"
	"testing"
)

func TestProposeWorkflow_RejectsBeforeTriggers(t *testing.T) {
	for _, trig := range []string{"before_save", "before_delete"} {
		t.Run(trig, func(t *testing.T) {
			e := &toolExecutor{reader: twoTenantWorkflowReader(), tenantID: "tenant-A"}
			args := `{"name":"Chặn","entity":"treatments","trigger":"` + trig + `",
				"actions":[{"type":"veto","message":"Không được lưu"}]}`
			ok, m := parseToolResult(t, e.execute(context.Background(), "propose_workflow", args))
			if ok {
				t.Fatalf("%s must not be proposable: %+v", trig, m)
			}
			errStr, _ := m["error"].(string)
			if !strings.Contains(errStr, "WORKFLOW_TRIGGER_INVALID") || !strings.Contains(errStr, "before_save/before_delete") ||
				!strings.Contains(errStr, "builder thủ công") {
				t.Fatalf("refusal must name the before triggers and point at the manual builder, got %q", errStr)
			}
			if len(e.draft) != 0 {
				t.Fatalf("rejected proposal must not buffer, got %+v", e.draft)
			}
		})
	}
}
