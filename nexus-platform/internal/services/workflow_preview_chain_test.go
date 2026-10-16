package services

// #442 (spec 048 Nợ 18): with WithEntityRules the dry-run applies the entity's OTHER active before
// rules in save order — older ones before the rule under test, newer ones after — and judges the
// vetoes on the final image. Without the flag the rule is judged alone, as before.

import (
	"context"
	"testing"

	"nexus/platform/internal/workflow"
)

func TestDryRun_BeforeSave_WithEntityRules(t *testing.T) {
	closeWon := []WorkflowRuleInput{{Field: "stage", Op: workflow.OpChangedTo, Value: "Closed Won"}, {Field: "close_date", Op: workflow.OpIsNull}}
	veto := WorkflowActionInput{Type: workflow.ActionVeto, Field: "close_date", Message: previewVetoMsg}
	fill := WorkflowActionInput{Type: workflow.ActionFieldUpdate, Field: "close_date", Value: "2026-12-31"}
	changes := map[string]any{"stage": "Closed Won"}
	tests := []struct {
		name      string
		fillFirst bool // true: the filler is OLDER than the veto
		flag      bool
		wantBlock bool
	}{
		{name: "no flag: judged alone, blocks", fillFirst: true, flag: false, wantBlock: true},
		{name: "flag: older filler satisfies the veto", fillFirst: true, flag: true, wantBlock: false},
		{name: "flag: newer filler also satisfies the final image", fillFirst: false, flag: true, wantBlock: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, _, _ := previewSvc(t, previewStored())
			on := true
			seedFill := func() {
				if _, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
					Name: "filler", EntityName: "deals", Trigger: "before_save", SaveOn: "update",
					Rules: closeWon[:1], Actions: []WorkflowActionInput{fill}, IsActive: &on, ActorID: "u1",
				}); err != nil {
					t.Fatalf("seed filler: %v", err)
				}
			}
			var vetoID string
			if tt.fillFirst {
				seedFill()
				vetoID = previewRule(t, svc, "before_save", "update", closeWon, veto)
			} else {
				vetoID = previewRule(t, svc, "before_save", "update", closeWon, veto)
				seedFill()
			}
			got, err := svc.DryRun(context.Background(), "t1", vetoID, WorkflowTestInput{RecordID: "r1", Changes: changes, WithEntityRules: tt.flag})
			if err != nil {
				t.Fatalf("DryRun: %v", err)
			}
			if b := lastAction(t, got)["wouldBlock"]; b != tt.wantBlock {
				t.Errorf("wouldBlock = %v, want %v (trace %+v)", b, tt.wantBlock, got.Trace)
			}
		})
	}
}
