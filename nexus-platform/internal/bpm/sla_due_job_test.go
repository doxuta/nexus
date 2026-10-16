package bpm

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"nexus/platform/internal/jobs"
)

func dueJob(tenant, instance string, seq int64) jobs.Job {
	p, _ := json.Marshal(slaDuePayload{InstanceID: instance, Seq: seq})
	return jobs.Job{ID: "j1", TenantID: tenant, Type: SLADueJobType, Payload: p}
}

// The scheduler is NEVER Started here: the job alone must escalate and notify — the whole point of
// S3c (a default install used to depend on the ticker being on).
func TestSLADueJob_FiresWithSchedulerDisabled(t *testing.T) {
	store := newFakeStore()
	past := time.Now().UTC().Add(-time.Minute)
	id, _ := store.CreateInstance(context.Background(), CreateInstanceInput{
		TenantID: "t1", ProcessID: "p1", EntityName: "deals", RecordID: "r1",
		CurrentApproverType: ApproverUser, CurrentApproverID: "u-mgr", SLADueAt: &past,
	})
	s := NewSLAScheduler(SLASchedulerConfig{Interval: time.Minute}, store, store, nil, nil)
	h := s.NewSLADueJobHandler(store)

	if err := h(context.Background(), dueJob("t1", id, 1)); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !store.instances[id].Escalated || len(store.notes) != 1 || store.notes[0].UserID != "u-mgr" {
		t.Fatalf("job did not escalate+notify: escalated=%v notes=%+v", store.instances[id].Escalated, store.notes)
	}
	// Re-delivery (at-least-once queue) must not double-fire.
	if err := h(context.Background(), dueJob("t1", id, 1)); err != nil || len(store.notes) != 1 {
		t.Fatalf("redelivery: err=%v notes=%d; want nil, 1", err, len(store.notes))
	}
}

func TestSLADueJob_StaleSeqOrDueNoop(t *testing.T) {
	past := time.Now().UTC().Add(-time.Minute)
	future := time.Now().UTC().Add(time.Hour)
	cases := []struct {
		name    string
		mutate  func(*Instance)
		wantErr bool
	}{
		{"seq moved on (decided/delegated since)", func(i *Instance) { i.Seq = 2 }, false},
		{"not pending", func(i *Instance) { i.Status = StatusApproved }, false},
		{"no deadline any more", func(i *Instance) { i.SLADueAt = nil }, false},
		{"deadline pushed out, seq equal => retry later", func(i *Instance) { i.SLADueAt = &future }, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newFakeStore()
			id, _ := store.CreateInstance(context.Background(), CreateInstanceInput{
				TenantID: "t1", EntityName: "deals", RecordID: "r1",
				CurrentApproverType: ApproverUser, CurrentApproverID: "u", SLADueAt: &past,
			})
			c.mutate(store.instances[id])
			s := NewSLAScheduler(SLASchedulerConfig{}, store, store, nil, nil)
			err := s.NewSLADueJobHandler(store)(context.Background(), dueJob("t1", id, 1))
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v; wantErr %v", err, c.wantErr)
			}
			if len(store.notes) != 0 || len(store.breachClaim) != 0 {
				t.Fatalf("stale job claimed/notified: claims=%v notes=%+v", store.breachClaim, store.notes)
			}
		})
	}
}

// The job row's tenant is the only tenant the handler trusts: another tenant's id with a valid
// instance id in the payload finds nothing; a job without a tenant is refused for good.
func TestSLADueJob_TenantPinned(t *testing.T) {
	store := newFakeStore()
	past := time.Now().UTC().Add(-time.Minute)
	id, _ := store.CreateInstance(context.Background(), CreateInstanceInput{
		TenantID: "t1", EntityName: "deals", RecordID: "r1",
		CurrentApproverType: ApproverUser, CurrentApproverID: "u", SLADueAt: &past,
	})
	h := NewSLAScheduler(SLASchedulerConfig{}, store, store, nil, nil).NewSLADueJobHandler(store)

	if err := h(context.Background(), dueJob("t2", id, 1)); err != nil {
		t.Fatalf("other tenant: %v", err)
	}
	if store.instances[id].Escalated || len(store.notes) != 0 {
		t.Fatal("job of tenant t2 escalated tenant t1's instance")
	}
	err := h(context.Background(), dueJob("", id, 1))
	if err == nil || !jobs.IsPermanent(err) {
		t.Fatalf("tenant-less job: err = %v; want a Permanent error", err)
	}
}

func TestBreach_NotifiesAllSeatHoldersOncePerSeat(t *testing.T) {
	store := newFakeStore()
	past := time.Now().UTC().Add(-time.Minute)
	id, _ := store.CreateInstance(context.Background(), CreateInstanceInput{
		TenantID: "t1", EntityName: "deals", RecordID: "r1",
		CurrentApproverType: ApproverRole, CurrentApproverID: "mgr-role", SLADueAt: &past,
	})
	store.seats[id] = []Seat{{NodeID: "n1", AssigneeType: ApproverRole, AssigneeID: "mgr-role"}}
	store.roleUsers = map[string][]string{"mgr-role": {"duyet1", "duyet2", "duyet3"}}
	s := NewSLAScheduler(SLASchedulerConfig{}, store, store, nil, nil)

	s.runBreaches(context.Background(), time.Now().UTC())
	s.runBreaches(context.Background(), time.Now().UTC()) // second sweep: nothing new
	got := map[string]int{}
	for _, n := range store.notes {
		got[n.UserID]++
	}
	if len(got) != 3 || got["duyet1"] != 1 || got["duyet2"] != 1 || got["duyet3"] != 1 {
		t.Fatalf("notices per user = %v; want each of duyet1..3 exactly once", got)
	}

	// The cap: a role of 80 people pings breachNotifyCap of them.
	store2 := newFakeStore()
	id2, _ := store2.CreateInstance(context.Background(), CreateInstanceInput{
		TenantID: "t1", EntityName: "deals", RecordID: "r2",
		CurrentApproverType: ApproverRole, CurrentApproverID: "big", SLADueAt: &past,
	})
	store2.seats[id2] = []Seat{{AssigneeType: ApproverRole, AssigneeID: "big"}}
	store2.roleUsers = map[string][]string{}
	for i := 0; i < 80; i++ {
		store2.roleUsers["big"] = append(store2.roleUsers["big"], fmt.Sprintf("u%02d", i))
	}
	NewSLAScheduler(SLASchedulerConfig{}, store2, store2, nil, nil).runBreaches(context.Background(), time.Now().UTC())
	if len(store2.notes) != breachNotifyCap {
		t.Fatalf("notices = %d; want cap %d", len(store2.notes), breachNotifyCap)
	}
}
