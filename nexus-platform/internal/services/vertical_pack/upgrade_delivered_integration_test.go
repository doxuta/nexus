//go:build integration

// DAILY-QUEUE #435 / specs/069 L7 against real MySQL and the real draft layer: a project that was
// delivered (first publish done) takes a pack upgrade as its builder in the project's own session.
// Rules go to drafts (the live rule does not move, the baseline does not move), a queue is applied
// at once only with the tick, a draft already in place is never overwritten, a cross-workspace
// attempt is refused with an actionable 409, Install stays blocked, and after Publish the plan
// reads the items as already matching the pack.
//
//	TEST_DATABASE_URL=... bash scripts/run-integration.sh ./internal/services/vertical_pack/ -run UpgradeDelivered
package vertical_pack

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/services"
)

func TestUpgradeDelivered_DraftsRulesQueueNeedsTickOpenWorkspaceRefused(t *testing.T) {
	gdb, sdb := vpSetup(t)
	reg := metaversion.NewRegistry(services.NewWorkflowDocAdapter())
	drafts := metaversion.NewDraftRepo(sdb, reg)
	vers := metaversion.NewVersionRepo(sdb)
	gate := services.NewDraftGate(vers)
	svc, wfSvc := vpBuildWFWith(gdb, sdb, gate, drafts)
	store := newUpgStore()
	svc.WithItemStore(store).WithDeliveryGate(gate).WithDraftReader(drafts)
	pub := services.NewPublishService(sdb, drafts, vers, reg, gate, nil, nil)

	builder := uuid.NewString()
	bctx := metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: builder, TenantID: vpTenantA, HomeTenantID: vpTenantA, IsBuilder: true, ReadDraft: true})
	otherCtx := metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: builder, TenantID: vpTenantB, HomeTenantID: vpTenantB, IsBuilder: true, ReadDraft: true})

	if _, err := svc.Install(context.Background(), vpTenantA, vpSeedUserID, wfPack()); err != nil {
		t.Fatalf("install v1.0.0: %v", err)
	}
	if _, err := pub.Baseline(bctx, vpTenantA, "giao"); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	// v1.1.0: the routing rule changes its value, a NEW rule arrives, a queue arrives.
	next := wfPack()
	next.Manifest.Version = "1.1.0"
	next.Workflows[0].Actions = []PackWorkflowAction{{Type: "field_update", Field: "priority", Value: "critical"}}
	active := true
	next.Workflows = append(next.Workflows, PackWorkflow{Name: "Escalate urgent", EntityName: "wf_tickets", Trigger: "after_create", IsActive: &active,
		Actions: []PackWorkflowAction{{Type: "field_update", Field: "priority", Value: "urgent"}}})
	next.WorkQueues = []PackWorkQueue{{Name: "today", Label: "Hôm nay", EntityName: "wf_tickets"}}

	liveRules := func() string {
		rs, err := wfSvc.ListWorkflows(metaversion.WithLiveRead(bctx), vpTenantA)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(rs)
		return string(b)
	}
	before := liveRules()

	plan, err := svc.PlanUpgrade(bctx, vpTenantA, "wf-ticketing", next, "upload")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	modes := map[string]string{}
	for _, it := range plan.Items {
		modes[it.ItemType+"/"+it.ItemKey] = it.ApplyMode + "/" + it.Status
	}
	if !plan.Delivered || modes[ItemWorkflow+"/Auto-escalate new tickets"] != ApplyModeDraft+"/"+StatusUpdateSafe ||
		modes[ItemWorkflow+"/Escalate urgent"] != ApplyModeDraft+"/"+StatusAdded ||
		modes[ItemWorkQueue+"/today"] != ApplyModeUntraced+"/"+StatusAdded {
		t.Fatalf("plan delivered=%v modes=%v", plan.Delivered, modes)
	}

	// T107: the queue reaches the customer at once -> no tick, no write, no lock left behind.
	if _, err := svc.ApplyUpgrade(bctx, vpTenantA, builder, "wf-ticketing", next, "upload", plan.PlanHash, nil); vcode(err) != "PACK_UPGRADE_IMMEDIATE_UNCONFIRMED" {
		t.Fatalf("without the tick: %v; want PACK_UPGRADE_IMMEDIATE_UNCONFIRMED", err)
	}
	if qs, _ := svc.workQueueWr.ListAdmin(bctx, vpTenantA); len(qs) != 0 || liveRules() != before || store.running != nil {
		t.Fatalf("a refused apply wrote: queues=%d running=%v", len(qs), store.running)
	}

	// T106: a hand draft on the routing rule stays and the item is Giu-only.
	var routing string
	rules, _ := wfSvc.ListWorkflows(metaversion.WithLiveRead(bctx), vpTenantA)
	for _, r := range rules {
		if r.Name == "Auto-escalate new tickets" {
			routing = r.ID
		}
	}
	off := false
	if _, err := wfSvc.UpdateWorkflow(bctx, vpTenantA, routing, services.UpdateWorkflowInput{Name: "Auto-escalate new tickets", EntityName: "wf_tickets", Trigger: "after_create",
		IsActive: &off, Actions: []services.WorkflowActionInput{{Type: "field_update", Field: "priority", Value: "high"}}}); err != nil {
		t.Fatal(err)
	}
	plan2, err := svc.PlanUpgrade(bctx, vpTenantA, "wf-ticketing", next, "upload")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range plan2.Items {
		if it.ItemKey == "Auto-escalate new tickets" && (it.Status != StatusDraftPending || len(it.Actions) != 1) {
			t.Fatalf("routing rule with a hand draft = %s %v; want draft_pending, keep only", it.Status, it.Actions)
		}
	}

	// With the tick: the new rule becomes a draft, the queue lands at once, the hand draft is untouched.
	run, err := svc.ApplyUpgrade(WithConfirmImmediate(bctx), vpTenantA, builder, "wf-ticketing", next, "upload", plan2.PlanHash, nil)
	if err != nil {
		t.Fatalf("apply with tick: %v", err)
	}
	if run.Mode != "draft" || run.Counts[OutcomeDrafted] != 1 || run.Counts[OutcomeApplied] != 1 {
		t.Fatalf("run mode=%s status=%s counts=%v results=%+v; want 1 drafted (rule) + 1 applied (queue)", run.Mode, run.Status, run.Counts, run.Results)
	}
	if liveRules() != before {
		t.Fatal("the LIVE rules moved: a delivered customer saw a change before Publish")
	}
	if _, ok := store.rows[vpTenantA+"|wf-ticketing|workflow|Escalate urgent"]; ok {
		t.Fatal("baseline row for an item that only entered a draft")
	}
	if r := store.rows[vpTenantA+"|wf-ticketing|work_queue|today"]; r.PackVersion != "1.1.0" {
		t.Fatalf("queue baseline = %+v; an applied-at-once item gets its baseline", r)
	}
	n, err := drafts.Count(context.Background(), vpTenantA)
	if err != nil || n != 2 {
		t.Fatalf("drafts = %d, %v; want 2 (the hand draft + the new rule)", n, err)
	}

	// Plan again: the new rule is draft_pending now (a replay would otherwise draft a duplicate).
	plan3, _ := svc.PlanUpgrade(bctx, vpTenantA, "wf-ticketing", next, "upload")
	for _, it := range plan3.Items {
		if it.ItemType == ItemWorkflow && it.Status != StatusDraftPending && it.Status != StatusInSync && it.Status != StatusUnchanged {
			t.Errorf("rule %q = %s after drafting; want draft_pending", it.ItemKey, it.Status)
		}
	}

	// T108: ?targetTenant= style (the builder's token tenant is another tenant) -> 409, nothing written.
	cnt := func() int {
		var c int
		_ = sdb.Get(&c, "SELECT COUNT(*) FROM sys_meta_drafts WHERE tenant_id = ?", vpTenantA)
		return c
	}
	c0 := cnt()
	if _, err := svc.ApplyUpgrade(otherCtx, vpTenantA, builder, "wf-ticketing", next, "upload", plan3.PlanHash, nil); vcode(err) != "PACK_UPGRADE_OPEN_WORKSPACE" {
		t.Fatalf("cross-workspace apply: %v; want PACK_UPGRADE_OPEN_WORKSPACE", err)
	}
	if cnt() != c0 {
		t.Fatal("cross-workspace apply wrote drafts")
	}
	// ... and a non-builder (customer admin) never reaches the draft path; Install stays blocked.
	custCtx := metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: builder, TenantID: vpTenantA, HomeTenantID: vpTenantA})
	if _, err := svc.ApplyUpgrade(custCtx, vpTenantA, builder, "wf-ticketing", next, "upload", plan3.PlanHash, nil); vcode(err) != "PROJECT_DELIVERED_INSTALL_BLOCKED" {
		t.Fatalf("customer apply: %v", err)
	}
	if _, err := svc.Install(bctx, vpTenantA, vpSeedUserID, next); vcode(err) != "PROJECT_DELIVERED_INSTALL_BLOCKED" {
		t.Fatalf("install into delivered: %v", err)
	}

	// T109: discarding the new rule's draft sends it back to "added" (safe to apply again).
	changes, _, err := pub.Changes(bctx, vpTenantA)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if c.Label == "Escalate urgent" {
			if err := pub.Discard(bctx, vpTenantA, c.ID); err != nil {
				t.Fatalf("discard: %v", err)
			}
		}
	}
	plan4, _ := svc.PlanUpgrade(bctx, vpTenantA, "wf-ticketing", next, "upload")
	for _, it := range plan4.Items {
		if it.ItemKey == "Escalate urgent" && it.Status != StatusAdded {
			t.Fatalf("after discarding its draft the rule = %s; want added", it.Status)
		}
	}
}
