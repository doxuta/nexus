//go:build integration

package vertical_pack

// live_read_integration_test.go — specs/063-draft-publish T032 + T032a (FR-009, FR-044, SC-014).
// A delivered workspace takes no pack install, and pack export / "so với lúc cài" read the LIVE
// configuration even for the builder who has drafts on the very rules the pack installed.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/services"
)

func TestDeliveredWorkspace_InstallBlocked_ExportAndDiffReadLive_Integration(t *testing.T) {
	gdb, sdb := vpSetup(t)
	reg := metaversion.NewRegistry(services.NewWorkflowDocAdapter())
	drafts := metaversion.NewDraftRepo(sdb, reg)
	vers := metaversion.NewVersionRepo(sdb)
	gate := services.NewDraftGate(vers)
	svc, wfSvc := vpBuildWFWith(gdb, sdb, gate, drafts)
	svc.WithItemStore(newFakeItemStore()).WithDeliveryGate(gate)
	pub := services.NewPublishService(sdb, drafts, vers, reg, gate, nil, nil)

	builder := uuid.NewString()
	bctx := metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: builder, TenantID: vpTenantA, HomeTenantID: vpTenantA, IsBuilder: true, ReadDraft: true})

	// Before delivery the install works as ever (the gate only looks at delivered tenants).
	if _, err := svc.Install(bctx, vpTenantA, vpSeedUserID, wfPack()); err != nil {
		t.Fatalf("install into a tenant that is not delivered: %v", err)
	}
	// Tenant B is installed too and stays undelivered: the control for "no cross-tenant effect".
	if _, err := svc.Install(context.Background(), vpTenantB, vpSeedUserID, wfPack()); err != nil {
		t.Fatalf("install into tenant B: %v", err)
	}

	if _, err := pub.Baseline(bctx, vpTenantA, "giao"); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	// Builder drafts: switch the routing rule off and add a rule that is not in the pack.
	rules, err := wfSvc.ListWorkflows(bctx, vpTenantA)
	if err != nil || len(rules) != 2 {
		t.Fatalf("rules = %d, %v", len(rules), err)
	}
	var routing = rules[0]
	for _, r := range rules {
		if r.Name == "Auto-escalate new tickets" {
			routing = r
		}
	}
	off := false
	if _, err := wfSvc.UpdateWorkflow(bctx, vpTenantA, routing.ID, services.UpdateWorkflowInput{
		Name: routing.Name + " (builder draft)", EntityName: "wf_tickets", Trigger: "after_create", IsActive: &off,
		Actions: []services.WorkflowActionInput{{Type: "field_update", Field: "priority", Value: "critical"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := wfSvc.CreateWorkflow(bctx, vpTenantA, services.CreateWorkflowInput{
		Name: "Draft only rule", EntityName: "wf_tickets", Trigger: "after_create",
		Actions: []services.WorkflowActionInput{{Type: "field_update", Field: "priority", Value: "low"}}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := wfSvc.ListWorkflows(bctx, vpTenantA); len(got) != 3 {
		t.Fatalf("the builder's overlay shows %d rules, want 3 (2 live + 1 new draft)", len(got))
	}

	// FR-044 / SC-014: export from the builder's /admin context returns the LIVE rules, byte for byte as without drafts.
	pack, _, err := svc.Export(bctx, vpTenantA, []string{"wf_tickets"}, false, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	names := map[string]bool{}
	for _, w := range pack.Workflows {
		names[w.Name] = true
		if w.IsActive != nil && !*w.IsActive && w.Name == "Auto-escalate new tickets" {
			t.Errorf("export carried the draft's isActive=false for %q", w.Name)
		}
	}
	if !names["Auto-escalate new tickets"] || names["Draft only rule"] || names["Auto-escalate new tickets (builder draft)"] || len(pack.Workflows) != 2 {
		t.Fatalf("export workflows = %v — must be the 2 LIVE pack rules, no draft content", names)
	}

	// "So với lúc cài": the routing rule is still unchanged (the live rule never moved).
	report, err := svc.DiffInstalled(bctx, vpTenantA, "wf-ticketing")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	for _, it := range report.Items {
		if it.ItemType == ItemWorkflow && it.Status != "unchanged" {
			t.Errorf("diff reports %s/%s %q because of a DRAFT: %v", it.ItemType, it.ItemKey, it.Status, it.ChangedPaths)
		}
		if strings.Contains(it.ItemKey, "Draft only") {
			t.Errorf("diff mentions the draft-only rule: %+v", it)
		}
	}

	// FR-009: installing into the delivered workspace is refused with a code, and writes nothing.
	count := func(table string, tenant string) int {
		var n int
		if err := sdb.Get(&n, "SELECT COUNT(*) FROM "+table+" WHERE tenant_id = ?", tenant); err != nil {
			t.Fatal(err)
		}
		return n
	}
	wfBefore, entBefore := count("sys_workflows", vpTenantA), count("sys_entities", vpTenantA)
	for name, ctx := range map[string]context.Context{"builder": bctx, "no caller": context.Background()} {
		_, err := svc.Install(ctx, vpTenantA, vpSeedUserID, wfPack())
		var ve *services.ValidationError
		if !errors.As(err, &ve) || ve.Code != "PROJECT_DELIVERED_INSTALL_BLOCKED" {
			t.Fatalf("%s: install into a delivered tenant = %v, want PROJECT_DELIVERED_INSTALL_BLOCKED", name, err)
		}
	}
	if count("sys_workflows", vpTenantA) != wfBefore || count("sys_entities", vpTenantA) != entBefore {
		t.Fatal("a blocked install wrote rows")
	}
	// A dry-run changes nothing, so it stays available; and tenant B (not delivered) still installs.
	if _, err := svc.DryRunInstall(bctx, vpTenantA, wfPack()); err != nil {
		t.Fatalf("dry-run on a delivered tenant: %v", err)
	}
	if _, err := svc.Install(context.Background(), vpTenantB, vpSeedUserID, wfPack()); err != nil {
		t.Fatalf("re-install into the undelivered tenant B: %v", err)
	}
}
