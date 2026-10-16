//go:build integration

package services

// draft_rule_never_fires_integration_test.go — specs/063-draft-publish T029 (SC-005), the scheduler
// half: a rule that exists only as a draft is invisible to the background scheduler's two DB steps
// (the due scan and the claim) and an edit that switches a LIVE scheduled rule off stays a draft until
// Publish. The before-rule half (real record service + real CachedLoader) is
// TestPublish_DraftThenPublish_Integration.
//
// ponytail: this drives the repository calls a beat makes (ListDueScheduled → ClaimScheduledRun), not
// a ticking Scheduler goroutine — the engine reads ONLY the raw repo, so what those two return is what
// a beat fires. A full tick test needs the AsyncDispatcher harness; add it if a second reader of
// sys_workflows ever appears.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/workflow"
)

func scheduledRule(name string) CreateWorkflowInput {
	on := true
	return CreateWorkflowInput{Name: name, EntityName: "hk_deals", Trigger: string(workflow.TriggerScheduled), IntervalSeconds: 60,
		Actions: []WorkflowActionInput{{Type: workflow.ActionInAppNotification, UserID: "$owner", Message: "nhac"}}, IsActive: &on}
}

func TestDraftScheduledRuleNeverFiresUntilPublished_Integration(t *testing.T) {
	e := dpSetup(t)
	tenant := e.newTenant(t)
	builder := uuid.NewString()
	bctx := builderCtxOf(tenant, builder)
	raw := workflow.NewSQLRepository(e.sdb)
	beat := func() []string { // what one scheduler beat would fire for THIS tenant
		due, err := raw.ListDueScheduled(context.Background(), time.Now().Add(24*time.Hour))
		if err != nil {
			t.Fatalf("ListDueScheduled: %v", err)
		}
		var ids []string
		for _, sw := range due {
			if sw.TenantID == tenant {
				ids = append(ids, sw.ID)
			}
		}
		return ids
	}

	live, err := e.wf.CreateWorkflow(bctx, tenant, scheduledRule("live scheduled"))
	if err != nil {
		t.Fatalf("create live scheduled rule: %v", err)
	}
	e.deliver(t, tenant, builder)
	if ids := beat(); len(ids) != 1 || ids[0] != live.ID {
		t.Fatalf("baseline beat = %v, want only the live rule %s", ids, live.ID)
	}

	// A draft-only scheduled rule + a draft that switches the live one off.
	draftOnly, err := e.wf.CreateWorkflow(bctx, tenant, scheduledRule("draft scheduled"))
	if err != nil || draftOnly.DraftState != "new" {
		t.Fatalf("create draft-only rule = %+v, %v", draftOnly, err)
	}
	if _, err := e.wf.UpdateWorkflow(bctx, tenant, live.ID, asUpdate(scheduledRule("live scheduled"), "live scheduled", false)); err != nil {
		t.Fatalf("draft edit: %v", err)
	}
	for i := 0; i < 3; i++ { // three beats: the draft rule is never due, the live rule is still due (its OFF is a draft)
		ids := beat()
		if len(ids) != 1 || ids[0] != live.ID {
			t.Fatalf("beat %d = %v, want only the live rule %s", i+1, ids, live.ID)
		}
	}
	if ok, err := raw.ClaimScheduledRun(context.Background(), tenant, draftOnly.ID, time.Now()); err != nil || ok {
		t.Fatalf("a beat claimed the draft-only rule: ok=%v err=%v", ok, err)
	}

	// Publish: the draft rule appears, the live one stops.
	if _, err := e.publishWithCurrentHash(bctx, tenant, false); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if ids := beat(); len(ids) != 1 || ids[0] != draftOnly.ID {
		t.Fatalf("post-publish beat = %v, want only %s", ids, draftOnly.ID)
	}
}
