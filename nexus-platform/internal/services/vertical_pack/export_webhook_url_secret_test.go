package vertical_pack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"nexus/platform/internal/workflow"
)

// specs/050 S3a review (queue #367): stripWorkflowCredentials removes a webhook's secret and headers
// but the url survives — and a url can itself be the credential (user:pass@, ?token=). A pack is the
// file Nexus hands to another customer, and install copies it into that tenant's rule and provenance
// rows, so a token in the url crossed tenants. Export leaves such a rule out, aloud, like the other
// things a pack cannot carry; a url with nothing to hide ships as before.
func TestExportWorkflows_WebhookUrlCredentialLeftOutAloud(t *testing.T) {
	rule := func(id, name, url string) workflow.WorkflowRecord {
		acts, _ := json.Marshal([]workflow.Action{{Type: workflow.ActionWebhook, URL: url}})
		return workflow.WorkflowRecord{ID: id, Name: name, EntityName: "deals", Trigger: "after_create", Actions: acts, IsActive: true}
	}
	wf := &fakeWorkflowWriter{existing: []workflow.WorkflowRecord{
		rule("wf-1", "gọi ERP", "https://erp.test/in"),
		rule("wf-2", "gọi kho có token", "https://kho.test/in?token=SECRET-QUERY-TOKEN"),
		rule("wf-3", "gọi kế toán có tài khoản", "https://sk_live_SECRET-USERINFO:@ketoan.test/in"),
	}}
	svc, _ := newWorkflowTestService(wf)
	pack := &Pack{}
	var warnings []string
	warn := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	if err := svc.exportWorkflows(context.Background(), "t1", map[string]bool{"deals": true}, nil, pack, warn, func(string) {}); err != nil {
		t.Fatalf("exportWorkflows: %v", err)
	}
	if len(pack.Workflows) != 1 || pack.Workflows[0].Name != "gọi ERP" {
		t.Fatalf("exported %+v; want only the rule whose url carries no credential", pack.Workflows)
	}
	said := strings.Join(warnings, "\n")
	for _, name := range []string{"gọi kho có token", "gọi kế toán có tài khoản"} {
		if !strings.Contains(said, name) {
			t.Fatalf("warnings = %q; want %q named as left out", said, name)
		}
	}
	if raw, _ := json.Marshal(pack); strings.Contains(string(raw)+said, "SECRET-") {
		t.Fatalf("a url credential reached the pack or its warnings: %s %s", raw, said)
	}
}
