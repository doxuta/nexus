package vertical_pack

// specs/050 S3a (T059): a pack is never a way to carry a key — export and install strip a webhook's
// inline secret/headers. The warnings that say so used to tell the operator to "nhập lại
// secret/header" on the rule, which since S3a is refused (WEBHOOK_INLINE_SECRET_REMOVED): the one
// way forward they name must be the vault — create the key in Quản trị › Kho khoá kết nối and pick
// it in the builder.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"nexus/platform/internal/workflow"
)

func assertPointsAtVault(t *testing.T, where string, warnings []string, rule string) {
	t.Helper()
	var hits []string
	for _, w := range warnings {
		if strings.Contains(w, rule) && strings.Contains(w, "outbound_webhook") {
			hits = append(hits, w)
		}
	}
	if len(hits) == 0 {
		t.Fatalf("%s: warnings = %q; want the stripped key of %q said aloud", where, warnings, rule)
	}
	for _, w := range hits {
		low := strings.ToLower(w)
		if strings.Contains(low, "nhập lại") {
			t.Errorf("%s: %q still sends the operator to re-enter the key on the rule — refused since S3a", where, w)
		}
		if !strings.Contains(w, "Kho khoá kết nối") {
			t.Errorf("%s: %q does not name the vault (Quản trị › Kho khoá kết nối)", where, w)
		}
	}
}

func TestPackInlineKeyWarnings_PointAtVault(t *testing.T) {
	const rule = "CRM → ERP"

	t.Run("install", func(t *testing.T) {
		wf := &fakeWorkflowWriter{}
		svc, _ := newWorkflowTestService(wf)
		pack := workflowTestPack()
		pack.Workflows = append(pack.Workflows, PackWorkflow{
			Name: rule, EntityName: "wf_tickets", Trigger: "after_create",
			Actions: []PackWorkflowAction{{Type: "outbound_webhook", URL: "https://partner.example.com/hooks/crm",
				Secret: "hmac-prod-8f3a", Headers: map[string]string{"Authorization": "Bearer sk_live_xxx"}}},
		})
		res, err := svc.Install(context.Background(), "tenant-1", "user-1", pack)
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		assertPointsAtVault(t, "install", res.Warnings, rule)
		for _, in := range wf.created {
			for _, a := range in.Actions {
				if a.Secret != "" || len(a.Headers) > 0 {
					t.Fatalf("install handed an inline key to CreateWorkflow: %+v", a)
				}
			}
		}
	})

	t.Run("export", func(t *testing.T) {
		acts, _ := json.Marshal([]workflow.Action{{Type: workflow.ActionWebhook, URL: "https://erp.test/in",
			Secret: "hmac-prod-8f3a", Headers: map[string]string{"Authorization": "Bearer sk_live_xxx"}}})
		wf := &fakeWorkflowWriter{existing: []workflow.WorkflowRecord{
			{ID: "wf-1", Name: rule, EntityName: "deals", Trigger: "after_create", Actions: acts, IsActive: true},
		}}
		svc, _ := newWorkflowTestService(wf)
		pack := &Pack{}
		var warnings []string
		warn := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
		if err := svc.exportWorkflows(context.Background(), "t1", map[string]bool{"deals": true}, nil, pack, warn, func(string) {}); err != nil {
			t.Fatalf("exportWorkflows: %v", err)
		}
		assertPointsAtVault(t, "export", warnings, rule)
	})
}
