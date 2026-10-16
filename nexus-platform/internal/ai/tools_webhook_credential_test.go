package ai

import (
	"context"
	"strings"
	"testing"
)

// TestProposeWorkflow_StillRejectsWebhookWithCredential (specs/050 S2b, T040): a webhook that
// names a vault key is still an outbound call the model must not invent — the policy guard rejects
// the whole action type, whatever shape it takes, and nothing is buffered.
func TestProposeWorkflow_StillRejectsWebhookWithCredential(t *testing.T) {
	e := &toolExecutor{reader: twoTenantWorkflowReader(), tenantID: "tenant-A"}
	args := `{"name":"r","entity":"treatments","trigger":"after_create",
		"actions":[{"type":"outbound_webhook","credentialId":"cred-1","path":"/orders"}]}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_workflow", args))
	if ok {
		t.Fatalf("a vault webhook must NOT be proposable: %+v", m)
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "outbound_webhook không tự đề xuất") {
		t.Fatalf("reject must name the policy, got %q", errStr)
	}
	if len(e.draft) != 0 {
		t.Fatalf("rejected proposal must not buffer, got %+v", e.draft)
	}
}
