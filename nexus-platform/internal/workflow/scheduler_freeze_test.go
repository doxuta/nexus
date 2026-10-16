package workflow

// specs/068-tenant-backup-restore T037 — a due scheduled rule of a write-frozen tenant is neither
// claimed (the CAS would push next_run_at forward and the firing would be lost for the whole
// interval) nor fired; it stays due and fires on the first pass after the freeze lapses.

import (
	"context"
	"testing"
	"time"
)

func TestScheduler_SkipsFrozenTenantWithoutClaiming(t *testing.T) {
	mut := &fakeMutator{}
	mk := func(id, tenant string) ScheduledWorkflow {
		return ScheduledWorkflow{Workflow: Workflow{
			ID: id, TenantID: tenant, EntityName: "deals", Trigger: TriggerScheduled, IsActive: true,
			Actions: []Action{{Type: ActionFieldUpdate, Field: "flag", Value: "X"}},
		}}
	}
	loader := newFakeScheduledLoader(mk("wf-frozen", "t-frozen"), mk("wf-free", "t-free"))
	lister := &fakeRecordLister{records: map[string][]ScheduledRecord{"deals": {{ID: "r1", Data: map[string]any{}}}}}
	frozen := true
	s := newScheduler(loader, lister, actionDeps{mut: mut}).
		WithTenantGate(func(_ context.Context, tenantID string) bool { return frozen && tenantID == "t-frozen" })

	s.runDue(context.Background(), time.Now())
	if n := loader.claimed["t-frozen|wf-frozen"]; n != 0 {
		t.Fatalf("a frozen tenant's rule was claimed %d time(s); the claim would consume its interval", n)
	}
	if n := loader.claimed["t-free|wf-free"]; n != 1 {
		t.Fatalf("the unfrozen rule must fire once, claims=%d", n)
	}

	frozen = false
	s.runDue(context.Background(), time.Now())
	if n := loader.claimed["t-frozen|wf-frozen"]; n != 1 {
		t.Fatalf("after the freeze lapses the rule is claimed and fires, claims=%d", n)
	}
}
