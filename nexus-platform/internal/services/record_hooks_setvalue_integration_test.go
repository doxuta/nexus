//go:build integration

// specs/048 S4 (T078) — before_save field_update ("tự điền") on real MySQL, through the real
// WorkflowService + CachedLoader + dynrepo + RunSink (hkSetup). Proves the rule's value leaves in the
// SAME UPDATE as the user's change (one version bump, one audit row carrying both, the formula
// recomputed from it); a create fills an empty field and keeps a chosen one; a rule never crosses
// tenants; and the run history says success on the record, with no value in it.
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true" \
//	  go test -tags=integration ./internal/services/ -run 'TestRecordUpdate_BeforeSaveSetValue_RecomputesFormulaSingleAudit' -race -count=1 -v
package services

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/workflow"
)

func (e hkEnv) createSetValueOpps(t *testing.T, tenant string) {
	t.Helper()
	if _, _, err := e.ent.CreateEntity(context.Background(), tenant, CreateEntityInput{
		Name: "hk_sv_opps", Label: "Opps",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
			{Name: "stage", Label: "Stage", FieldType: "picklist", Options: map[string]any{"values": []any{"Proposal", "Closed Won"}}},
			{Name: "source", Label: "Source", FieldType: "picklist", Options: map[string]any{"values": []any{"Web", "Event"}}},
			{Name: "amount", Label: "Amount", FieldType: "number"},
			{Name: "probability", Label: "Probability", FieldType: "number"},
			{Name: "weighted", Label: "Weighted", FieldType: "formula", Options: map[string]any{"formula": "amount * probability / 100"}},
		},
	}); err != nil {
		t.Fatalf("create hk_sv_opps in %s: %v", tenant, err)
	}
}

func (e hkEnv) fillRule(t *testing.T, tenant, name, saveOn string, rules []WorkflowRuleInput, a WorkflowActionInput) string {
	t.Helper()
	on := true
	wf, err := e.wf.CreateWorkflow(context.Background(), tenant, CreateWorkflowInput{
		Name: name, EntityName: "hk_sv_opps", Trigger: string(workflow.TriggerBeforeSave), SaveOn: saveOn,
		Rules: rules, Actions: []WorkflowActionInput{a}, IsActive: &on,
	})
	if err != nil {
		t.Fatalf("create fill rule %q: %v", name, err)
	}
	return wf.ID
}

func (e hkEnv) storedIn(t *testing.T, tenant, entity, id string) *dynrepo.Record {
	t.Helper()
	rec, err := e.dyn.GetRecord(context.Background(), entity, tenant, id, dynrepo.RowScope{IsAdmin: true})
	if err != nil {
		t.Fatalf("read back %s/%s: %v", entity, id, err)
	}
	return rec
}

func (e hkEnv) updateAuditDiffs() []*audit.Diff {
	var out []*audit.Diff
	for _, ev := range e.audit.events {
		if ev.Action == audit.ActionUpdate {
			out = append(out, ev.Diff)
		}
	}
	return out
}

func TestRecordUpdate_BeforeSaveSetValue_RecomputesFormulaSingleAudit(t *testing.T) {
	env := hkSetup(t)
	ctx := context.Background()
	env.createSetValueOpps(t, engTenantA)
	env.createSetValueOpps(t, engTenantB)
	rep := RecordScope{UserID: hkUser, AccessLevel: "all", RoleID: "role-rep"}

	wonRule := env.fillRule(t, engTenantA, "Thắng thì xác suất 100", "",
		[]WorkflowRuleInput{{Field: "stage", Op: workflow.OpChangedTo, Value: "Closed Won"}},
		WorkflowActionInput{Type: workflow.ActionFieldUpdate, Field: "probability", Value: 100.0})
	webRule := env.fillRule(t, engTenantA, "Không nguồn là Website", workflow.SaveOnCreate, nil,
		WorkflowActionInput{Type: workflow.ActionFieldUpdate, Field: "source", Value: "Web", OnlyIfEmpty: true})

	// Create: an empty source is filled, a chosen one is kept.
	id, err := env.rec.Create(ctx, rep, engTenantA, hkUser, "hk_sv_opps",
		map[string]any{"name": "SECRET-DEAL", "stage": "Proposal", "amount": 1000.0, "probability": 40.0})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	evID, err := env.rec.Create(ctx, rep, engTenantA, hkUser, "hk_sv_opps",
		map[string]any{"name": "Event deal", "stage": "Proposal", "source": "Event"})
	if err != nil {
		t.Fatalf("create with source: %v", err)
	}
	if got := env.storedIn(t, engTenantA, "hk_sv_opps", id).Data["source"]; got != "Web" {
		t.Fatalf("source = %v, want Web filled by the rule", got)
	}
	if got := env.storedIn(t, engTenantA, "hk_sv_opps", evID).Data["source"]; got != "Event" {
		t.Fatalf("source = %v, want Event kept", got)
	}

	// Update: the rule's value, the formula and the user's change leave in ONE write.
	before := env.storedIn(t, engTenantA, "hk_sv_opps", id)
	audits := len(env.updateAuditDiffs())
	if err := env.rec.Update(ctx, rep, engTenantA, "hk_sv_opps", id, map[string]any{"stage": "Closed Won"}, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	after := env.storedIn(t, engTenantA, "hk_sv_opps", id)
	if after.Data["stage"] != "Closed Won" || after.Data["probability"] != 100.0 || after.Data["weighted"] != 1000.0 {
		t.Fatalf("stored = %v, want stage Closed Won, probability 100, weighted 1000", after.Data)
	}
	if after.Version != before.Version+1 {
		t.Fatalf("version %d → %d, want exactly one UPDATE", before.Version, after.Version)
	}
	diffs := env.updateAuditDiffs()
	if len(diffs) != audits+1 {
		t.Fatalf("update audit rows = %d, want exactly one more", len(diffs)-audits)
	}
	if last := diffs[len(diffs)-1]; last == nil || last.After["stage"] != "Closed Won" || last.After["probability"] != 100.0 ||
		last.Before["probability"] != 40.0 {
		t.Fatalf("audit diff = %+v, want stage + probability (40 → 100) in the same row", last)
	}

	// Tenant B has no rule: the same change keeps its probability.
	b1, err := env.rec.Create(ctx, rep, engTenantB, hkUser, "hk_sv_opps", map[string]any{"name": "B1", "stage": "Proposal", "probability": 40.0})
	if err != nil {
		t.Fatalf("create in tenant B: %v", err)
	}
	if err := env.rec.Update(ctx, rep, engTenantB, "hk_sv_opps", b1, map[string]any{"stage": "Closed Won"}, nil); err != nil {
		t.Fatalf("update in tenant B: %v", err)
	}
	if got := env.storedIn(t, engTenantB, "hk_sv_opps", b1).Data; got["probability"] != 40.0 || got["source"] != nil {
		t.Fatalf("tenant B record = %v — tenant A's rules ran on it", got)
	}

	// Run history: one success run per rule that changed a value, on the record, no value in it.
	for _, tc := range []struct {
		rule, field string
	}{{wonRule, "probability"}, {webRule, "source"}} {
		runs := env.drainRuns(t, engTenantA, tc.rule)
		want := []any{map[string]any{"type": "field_update", "field": tc.field, "ok": true}}
		if len(runs) != 1 || runs[0].Status != "success" || runs[0].RecordID == nil || *runs[0].RecordID != id ||
			runs[0].TriggerType != "before_save" || !jsonEqual(actionsOf(t, runs[0]), want) {
			t.Fatalf("%s runs = %+v, want one success run on %s", tc.field, runs, id)
		}
		if raw := fmt.Sprint(actionsOf(t, runs[0])); strings.Contains(raw, "SECRET") || strings.Contains(raw, "Web") || strings.Contains(raw, "100") {
			t.Fatalf("run carries a value: %s", raw)
		}
	}
}
