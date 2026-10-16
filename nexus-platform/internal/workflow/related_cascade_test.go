package workflow

import (
	"context"
	"strings"
	"testing"
)

// specs/058 §2.4 (#330): a related field_update emits after_update for the target record with
// Depth+1; the cascade stops at maxDepth and the write still lands.

// cascadeMutator is a fakeMutator that also implements RelatedWriter and records the depth it was
// handed.
type cascadeMutator struct {
	*fakeMutator
	relCalls []int
}

func (m *cascadeMutator) UpdateRelatedField(ctx context.Context, entity, tenantID, recordID, actor, field string, value any, depth int) error {
	m.relCalls = append(m.relCalls, depth)
	return m.fakeMutator.UpdateField(ctx, entity, tenantID, recordID, actor, field, value)
}

func cascadeWF() Workflow {
	return Workflow{ID: "wf1", Actions: []Action{{Type: ActionFieldUpdate, Via: "appointment", Field: "status", Value: "done"}}}
}

func TestRunActions_RelatedFieldUpdate_CascadesWithDepthPlusOne(t *testing.T) {
	m := &cascadeMutator{fakeMutator: relatedMutator()}
	evt := Event{TenantID: "t1", EntityName: "invoices", RecordID: "inv-1", Trigger: TriggerAfterUpdate, Depth: 1,
		Data: map[string]any{"appointment": "ap-7"}}
	results, err := runActions(context.Background(), cascadeWF(), evt, actionDeps{mut: m, maxDepth: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.relCalls) != 1 || m.relCalls[0] != 2 {
		t.Fatalf("related depths = %v, want [2]", m.relCalls)
	}
	if results[0].Warning != "" {
		t.Errorf("warning = %q, want none below the cap", results[0].Warning)
	}
}

func TestRunActions_RelatedFieldUpdate_StopsCascadeAtMaxDepth(t *testing.T) {
	m := &cascadeMutator{fakeMutator: relatedMutator()}
	evt := Event{TenantID: "t1", EntityName: "invoices", RecordID: "inv-1", Trigger: TriggerAfterUpdate, Depth: 5,
		Data: map[string]any{"appointment": "ap-7"}}
	results, err := runActions(context.Background(), cascadeWF(), evt, actionDeps{mut: m, maxDepth: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.relCalls) != 0 {
		t.Errorf("cascading write called at the cap: %v", m.relCalls)
	}
	if len(m.calls) != 1 {
		t.Fatalf("plain write calls = %d, want 1 (the value still lands, only the event is withheld)", len(m.calls))
	}
	if !strings.Contains(results[0].Warning, "độ sâu") {
		t.Errorf("warning = %q, want a note naming the depth cap", results[0].Warning)
	}
}
