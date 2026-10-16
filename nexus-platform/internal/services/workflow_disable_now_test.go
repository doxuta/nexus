package services

// workflow_disable_now_test.go — specs/063-draft-publish T051a (FR-017): "Tắt ngay cho khách".

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/workflow"
)

// liveFake is the raw live store: fakeWFRepo + SetInactive (what *workflow.SQLRepository adds).
type liveFake struct{ *fakeWFRepo }

func (l liveFake) SetInactive(_ context.Context, tenantID, id, _ string) error {
	r, ok := l.rows[id]
	if !ok || r.TenantID != tenantID {
		return workflow.ErrWorkflowNotFound
	}
	r.IsActive = false
	return nil
}

func disableNowSvc(t *testing.T) (*WorkflowService, *dwEnv) {
	t.Helper()
	e := newDWEnv(true)
	return NewWorkflowService(e.repo).WithEntityChecker(w6Catalog()).WithLiveRepo(liveFake{e.live}), e
}

func TestDisableNow_SwitchesLiveRuleOffAndTouchesNoDraft(t *testing.T) {
	svc, e := disableNowSvc(t)
	ctx := builderCtx()
	id := e.seedLive(t, "running rule")
	// The builder has a pending edit of the same rule.
	if err := e.repo.Update(ctx, dwTenant, id, dwUpdateInput("builder edit", true)); err != nil {
		t.Fatal(err)
	}
	savesBefore := e.drafts.saves
	draftBefore, _ := e.drafts.GetByKey(ctx, dwTenant, DocTypeWorkflow, id)

	rec, err := svc.DisableNow(ctx, dwTenant, id, "builder")
	if err != nil {
		t.Fatal(err)
	}
	if rec.IsActive || rec.DraftState != "" || rec.Name != "running rule" {
		t.Fatalf("DisableNow returned %+v, want the LIVE rule, off, content untouched", rec)
	}
	if live, _ := e.live.Get(context.Background(), dwTenant, id); live.IsActive {
		t.Fatal("the live rule is still active")
	}
	// The draft is untouched: same row, no new save.
	draftAfter, _ := e.drafts.GetByKey(ctx, dwTenant, DocTypeWorkflow, id)
	if e.drafts.saves != savesBefore || draftAfter == nil || draftAfter.UpdatedAt != draftBefore.UpdatedAt || string(draftAfter.DraftJSON) != string(draftBefore.DraftJSON) {
		t.Fatal("DisableNow wrote or changed a draft")
	}
	// Idempotent.
	if _, err := svc.DisableNow(ctx, dwTenant, id, "builder"); err != nil {
		t.Fatalf("second DisableNow: %v", err)
	}
}

func TestDisableNow_Refusals(t *testing.T) {
	svc, e := disableNowSvc(t)
	ctx := builderCtx()
	// A rule that exists only as a draft is not live: nothing to switch off.
	draftOnly, _ := e.repo.Create(ctx, dwCreateInput("draft only"))
	_, err := svc.DisableNow(ctx, dwTenant, draftOnly, "builder")
	wantCode(t, err, "WORKFLOW_NOT_FOUND")
	if d, _ := e.drafts.GetByKey(ctx, dwTenant, DocTypeWorkflow, draftOnly); d == nil {
		t.Fatal("DisableNow removed a draft-only rule's draft")
	}
	_, err = svc.DisableNow(ctx, dwTenant, "ghost", "builder")
	wantCode(t, err, "WORKFLOW_NOT_FOUND")
	_, err = svc.DisableNow(ctx, dwTenant, "  ", "builder")
	wantCode(t, err, "WORKFLOW_ID_REQUIRED")
	// Another tenant's rule is the same 404.
	id := e.seedLive(t, "mine")
	_, err = svc.DisableNow(ctx, "other-tenant", id, "builder")
	wantCode(t, err, "WORKFLOW_NOT_FOUND")
	if live, _ := e.live.Get(context.Background(), dwTenant, id); !live.IsActive {
		t.Fatal("another tenant switched the rule off")
	}
}

// Not wired ⇒ loud failure, never a fall back to the draft-aware repo (which would write a draft).
func TestDisableNow_UnwiredLiveRepoFailsLoudly(t *testing.T) {
	e := newDWEnv(true)
	svc := NewWorkflowService(e.repo)
	id := e.seedLive(t, "r")
	_, err := svc.DisableNow(builderCtx(), dwTenant, id, "b")
	if err == nil {
		t.Fatal("DisableNow without a live repo succeeded")
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		t.Fatalf("a wiring error must not be a ValidationError: %v", err)
	}
	if e.drafts.saves != 0 {
		t.Fatal("DisableNow fell back to the draft layer")
	}
}
