package bpm

// specs/068-tenant-backup-restore T037 — the SLA breach sweep leaves a write-frozen tenant alone: no
// claim (so the instance is not marked escalated and nothing is written to it), and the next sweep
// after the freeze lapses escalates it as usual. Other tenants are unaffected.

import (
	"context"
	"testing"
	"time"
)

func TestSLAScheduler_SkipsFrozenTenant(t *testing.T) {
	store := newFakeStore()
	past := time.Now().UTC().Add(-time.Hour)
	mk := func(tenant, rec string) string {
		id, _ := store.CreateInstance(context.Background(), CreateInstanceInput{
			TenantID: tenant, ProcessID: "p1", EntityName: "deals", RecordID: rec,
			CurrentApproverType: ApproverUser, CurrentApproverID: "u-mgr", SLADueAt: &past,
		})
		return id
	}
	frozenID, freeID := mk("t-frozen", "r1"), mk("t-free", "r2")
	frozen := true
	s := NewSLAScheduler(SLASchedulerConfig{Interval: time.Minute}, store, store, &fakeRecorder{}, nil).
		WithTenantGate(func(_ context.Context, tenantID string) bool { return frozen && tenantID == "t-frozen" })

	s.runBreaches(context.Background(), time.Now().UTC())
	if store.instances[frozenID].Escalated {
		t.Fatal("a frozen tenant's instance must not be claimed/escalated")
	}
	if !store.instances[freeID].Escalated {
		t.Fatal("an unfrozen tenant must still escalate")
	}
	for _, scanned := range store.scanned {
		if scanned == "t-frozen" {
			t.Fatal("a frozen tenant must not even be scanned")
		}
	}

	frozen = false
	s.runBreaches(context.Background(), time.Now().UTC())
	if !store.instances[frozenID].Escalated {
		t.Fatal("after the freeze lapses the breach is escalated by the next sweep")
	}
}
