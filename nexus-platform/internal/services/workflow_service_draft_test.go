package services

// workflow_service_draft_test.go — specs/063-draft-publish T025 (FR-006, FR-015): the SAME
// WorkflowService body runs over the draft-aware repo, so a draft save meets the same refusals as a
// live save, the silent-key-drop guard compares against the overlay, and the dry-run reads a draft.

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/workflow"
)

func dwService(t *testing.T) (*WorkflowService, *dwEnv, *fakeRecordReader) {
	t.Helper()
	e := newDWEnv(true)
	reader := &fakeRecordReader{rec: &dynrepo.Record{ID: "r1", TenantID: "t1", Data: map[string]any{"status": "won", "stage": "new"}}}
	svc := NewWorkflowService(e.repo).WithEntityChecker(w6Catalog()).WithRecordReader(reader)
	return svc, e, reader
}

// A draft save refuses exactly what a live save refuses: same code, nothing written.
func TestWorkflowService_DraftMode_SameRefusals(t *testing.T) {
	svc, e, _ := dwService(t)
	ctx := builderCtx()

	_, err := svc.CreateWorkflow(ctx, "t1", w6CreateInput("total")) // WF-01: formula target
	wantCode(t, err, "RECORD_FORMULA_READONLY")
	in := w6CreateInput("stage")
	in.EntityName = "no_such_entity"
	_, err = svc.CreateWorkflow(ctx, "t1", in)
	if err == nil {
		t.Fatal("a rule on an unknown entity was accepted in draft mode")
	}
	if e.drafts.saves != 0 {
		t.Fatalf("a refused save wrote %d draft(s)", e.drafts.saves)
	}

	// Duplicate name is judged on the overlay (live + drafts).
	if _, err := svc.CreateWorkflow(ctx, "t1", w6CreateInput("stage")); err != nil {
		t.Fatal(err)
	}
	_, err = svc.CreateWorkflow(ctx, "t1", w6CreateInput("stage"))
	wantCode(t, err, "WORKFLOW_ALREADY_EXISTS")
	if e.drafts.saves != 1 {
		t.Fatalf("drafts written = %d, want 1", e.drafts.saves)
	}
}

// The cap on active before rules is judged on live + drafts (what the customer will run).
func TestWorkflowService_DraftMode_BeforeRuleCapOnOverlay(t *testing.T) {
	svc, e, _ := dwService(t)
	ctx := builderCtx()
	// 50 live active veto rules on deals.
	for i := 0; i < maxActiveBeforeRules; i++ {
		if _, err := e.live.Create(context.Background(), workflow.CreateInput{TenantID: "t1", Name: "live" + string(rune('A'+i%26)) + string(rune('a'+i/26)), EntityName: "deals",
			Trigger: workflow.TriggerBeforeSave, Actions: []byte(`[{"type":"veto"}]`), IsActive: true}); err != nil {
			t.Fatal(err)
		}
	}
	in := CreateWorkflowInput{Name: "one too many", EntityName: "deals", Trigger: string(workflow.TriggerBeforeSave), SaveOn: workflow.SaveOnUpdate,
		Actions: []WorkflowActionInput{{Type: workflow.ActionVeto, Field: "stage", Message: "no"}}}
	_, err := svc.CreateWorkflow(ctx, "t1", in)
	wantCode(t, err, "WORKFLOW_BEFORE_RULE_LIMIT")
}

// refuseSilentWebhookKeyDrop compares with the rule the builder SEES (the overlay), so a bare
// {type,url} save over a keyed webhook is refused in draft mode too.
func TestWorkflowService_DraftMode_WebhookKeyDropGuardUsesOverlay(t *testing.T) {
	svc, e, _ := dwService(t)
	ctx := builderCtx()
	keyed := []byte(`[{"type":"outbound_webhook","url":"https://partner.example/hook","secret":"S3CRET"}]`)
	id, err := e.live.Create(context.Background(), workflow.CreateInput{TenantID: "t1", Name: "hook", EntityName: "deals",
		Trigger: workflow.TriggerAfterUpdate, Actions: keyed, IsActive: true})
	if err != nil {
		t.Fatal(err)
	}
	bare := UpdateWorkflowInput{Name: "hook", EntityName: "deals", Trigger: "after_update", IsActive: boolPtr(true),
		Actions: []WorkflowActionInput{{Type: "outbound_webhook", URL: "https://partner.example/hook"}}}
	_, err = svc.UpdateWorkflow(ctx, "t1", id, bare)
	wantCode(t, err, "WEBHOOK_LEGACY_SECRET_PENDING")
	if e.drafts.saves != 0 {
		t.Fatal("the refused save still wrote a draft")
	}
}

// FR-015: a dry-run reads the draft — including a rule that exists ONLY as a draft — and touches
// nothing but the record read.
func TestWorkflowService_DraftMode_DryRunOnDraftOnlyRule(t *testing.T) {
	svc, e, reader := dwService(t)
	ctx := builderCtx()
	created, err := svc.CreateWorkflow(ctx, "t1", CreateWorkflowInput{Name: "draft only", EntityName: "deals", Trigger: "after_update",
		Rules:   []WorkflowRuleInput{{Field: "status", Op: "eq", Value: "won"}},
		Actions: []WorkflowActionInput{{Type: "field_update", Field: "stage", Value: "closed"}}})
	if err != nil {
		t.Fatal(err)
	}
	if created.DraftState != "new" {
		t.Fatalf("created.DraftState = %q, want new", created.DraftState)
	}
	if _, err := e.live.Get(context.Background(), "t1", created.ID); !errors.Is(err, workflow.ErrWorkflowNotFound) {
		t.Fatal("the draft-only rule exists live")
	}
	res, err := svc.TestWorkflow(ctx, "t1", created.ID, "r1")
	if err != nil {
		t.Fatalf("TestWorkflow on a draft-only rule: %v", err)
	}
	if !res.Matched || res.WorkflowID != created.ID {
		t.Fatalf("dry-run = %+v, want matched on the draft's condition", res)
	}
	if reader.reads != 1 {
		t.Fatalf("record reads = %d, want exactly 1", reader.reads)
	}
	// Edit the draft's condition so it no longer matches: the dry-run follows the NEW draft.
	if _, err := svc.UpdateWorkflow(ctx, "t1", created.ID, UpdateWorkflowInput{Name: "draft only", EntityName: "deals", Trigger: "after_update",
		Rules:   []WorkflowRuleInput{{Field: "status", Op: "eq", Value: "lost"}},
		Actions: []WorkflowActionInput{{Type: "field_update", Field: "stage", Value: "closed"}}}); err != nil {
		t.Fatal(err)
	}
	res, _ = svc.TestWorkflow(ctx, "t1", created.ID, "r1")
	if res.Matched {
		t.Fatal("the dry-run still matched the OLD draft condition")
	}
	// A legacy admin (non-builder) cannot even see the draft-only rule.
	if _, err := svc.GetWorkflow(legacyAdminCtx(), "t1", created.ID); err == nil {
		t.Fatal("a non-builder resolved a draft-only rule")
	}
}
