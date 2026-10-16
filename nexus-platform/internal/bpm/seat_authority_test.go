package bpm

// specs/043 S3b (`bpm-seat-authority`): the decision engine's authority and its guard.
// The fakes here are the package's shared ones (engine_test.go) — their DecideTaskTx / ClaimBreach
// mirror the real contract; the real transaction is proven on MySQL in
// internal/services/bpm_seat_authority_integration_test.go.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// twoStepFixture arms a pinned two-step instance (u1 then u2) on a fake store.
func twoStepFixture(t *testing.T) (*fakeStore, *Decider, string) {
	t.Helper()
	store := newFakeStore()
	def := &Definition{Steps: NormalizeSteps([]Step{
		{Name: "Trưởng phòng", ApproverType: ApproverUser, ApproverID: "u1"},
		{Name: "Kế toán", ApproverType: ApproverUser, ApproverID: "u2", SLASeconds: 3600, SLAClock: SLAClockBusiness},
	})}
	id, err := store.CreateInstance(context.Background(), CreateInstanceInput{
		TenantID: "t1", ProcessID: "p1", EntityName: "deals", RecordID: "r1",
		CurrentApproverType: ApproverUser, CurrentApproverID: "u1", Definition: def,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	return store, NewDecider(store, store, nil, nil, nil), id
}

// TestAdvance_SeqCAS_StaleDecisionLoses (T067, ABA): the decision carries the seq it READ. A write
// that lands between the read and the decision leaves current_step and escalated exactly as they
// were — so the old CAS could not see it — but moves seq, and the decision loses.
func TestAdvance_SeqCAS_StaleDecisionLoses(t *testing.T) {
	store, d, id := twoStepFixture(t)
	fired := false
	store.onGet = func(f *fakeStore, _ *Instance) {
		if fired {
			return
		}
		fired = true
		// A delegation u1 → u3 slips in after u1's read: same step, same escalated flag, new seq.
		in := f.instances[id]
		in.CurrentApproverID = "u3"
		in.Seq++
	}
	_, err := d.Decide(context.Background(), "t1", id, DecisionApprove, "", Actor{UserID: "u1"})
	if !errors.Is(err, ErrConcurrent) {
		t.Fatalf("stale decision: err = %v; want ErrConcurrent", err)
	}
	if in := store.instances[id]; in.CurrentStep != 0 || in.Status != StatusPending || in.CurrentApproverID != "u3" {
		t.Fatalf("the lost decision changed the instance: %+v", in)
	}
}

// TestDecide_AuthorityIsTheSeat: who may decide is who holds an open seat of the step (the
// inbox's AwaitsActor), not who the projection names — a NULL projection is no longer a step
// nobody can decide. An admin still decides, as before.
func TestDecide_AuthorityIsTheSeat(t *testing.T) {
	store, d, id := twoStepFixture(t)
	store.instances[id].CurrentApproverType, store.instances[id].CurrentApproverID = "", "" // D-5: not exactly one user/role seat
	store.seats[id] = []Seat{{NodeID: "s1", AssigneeType: ApproverUser, AssigneeID: "u7"}}

	if _, err := d.Decide(context.Background(), "t1", id, DecisionApprove, "", Actor{UserID: "u1"}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a user with no seat: err = %v; want ErrNotAuthorized", err)
	}
	if store.instances[id].CurrentStep != 0 {
		t.Fatalf("a refused decision moved the instance")
	}
	got, err := d.Decide(context.Background(), "t1", id, DecisionApprove, "", Actor{UserID: "u7"})
	if err != nil || got.CurrentStep != 1 {
		t.Fatalf("the seat holder: step=%v err=%v; want to advance to step 1", got, err)
	}
	if _, err := d.Decide(context.Background(), "t1", id, DecisionReject, "no", Actor{UserID: "boss", IsAdmin: true}); err != nil {
		t.Fatalf("an admin must still decide a single-seat step: %v", err)
	}
}

// TestDecide_ChecksSeatBeforeDoingWork: an actor who holds no seat is refused before the engine
// reads a calendar or computes the next step — a refusal must not be a way to probe calendar errors.
func TestDecide_ChecksSeatBeforeDoingWork(t *testing.T) {
	store, d, id := twoStepFixture(t)
	d.deadlines = failingCalc{}
	_, err := d.Decide(context.Background(), "t1", id, DecisionApprove, "", Actor{UserID: "stranger"})
	if !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("err = %v; want ErrNotAuthorized, not the calendar's failure", err)
	}
	if store.advanceCnt != 0 {
		t.Fatalf("a refused decision wrote")
	}
}

type failingCalc struct{}

func (failingCalc) DueAt(context.Context, string, string, time.Time, time.Duration) (time.Time, SLABasis, error) {
	return time.Time{}, "", errors.New("calendar store down")
}

// TestSLAScheduler_IteratesTenantsPinned (T074): the sweep asks the store for its tenants and then
// for each tenant's breached instances — never one query across all of them — and claims every
// instance with the seq it was read at, under the tenant it was read in.
func TestSLAScheduler_IteratesTenantsPinned(t *testing.T) {
	store := newFakeStore()
	past := time.Now().UTC().Add(-time.Hour)
	for _, tenant := range []string{"t-b", "t-a"} {
		if _, err := store.CreateInstance(context.Background(), CreateInstanceInput{
			TenantID: tenant, ProcessID: "p", EntityName: "deals", RecordID: "r-" + tenant,
			CurrentApproverType: ApproverUser, CurrentApproverID: "u", SLADueAt: &past,
			Definition: &Definition{Steps: NormalizeSteps([]Step{{ApproverType: ApproverUser, ApproverID: "u"}})},
		}); err != nil {
			t.Fatalf("CreateInstance: %v", err)
		}
	}
	NewSLAScheduler(SLASchedulerConfig{Interval: time.Minute}, store, store, nil, nil).runBreaches(context.Background(), time.Now().UTC())

	if len(store.scanned) != 2 || store.scanned[0] != "t-a" || store.scanned[1] != "t-b" {
		t.Fatalf("per-tenant scans = %v; want [t-a t-b], one pinned scan each", store.scanned)
	}
	for _, id := range []string{"inst-r-t-a", "inst-r-t-b"} {
		if !store.instances[id].Escalated {
			t.Errorf("%s was not escalated", id)
		}
		if store.claimedSeq[id] != 1 {
			t.Errorf("%s claimed with seq %d; want the seq it was read at (1)", id, store.claimedSeq[id])
		}
	}
}

// TestDecideAt_LateSecondClickDoesNotSkipAStep: the sequential half of the double-click. Tab 1
// approves step 0 and commits; tab 2, still showing step 0, clicks Approve AFTER. The server's own
// read now says step 1 — and the actor (an admin, who may decide any step) would pass authority —
// so only the step the client SAW can tell the click is stale. Without it the file jumps two steps
// on one human intention.
func TestDecideAt_LateSecondClickDoesNotSkipAStep(t *testing.T) {
	ctx := context.Background()
	admin := Actor{UserID: "boss", IsAdmin: true}
	saw := 0 // both tabs rendered step 0
	cases := []struct {
		name     string
		expected *int
		wantErr  error
		wantStep int
	}{
		{"client saw the step that is now current → decides", &saw, nil, 1},
		{"no step sent (older client) → decides on what it reads", nil, nil, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, d, id := twoStepFixture(t)
			got, err := d.DecideAt(ctx, "t1", id, DecisionApprove, "", admin, tc.expected)
			if !errors.Is(err, tc.wantErr) || got == nil || got.CurrentStep != tc.wantStep {
				t.Fatalf("got=%+v err=%v; want step %d err %v", got, err, tc.wantStep, tc.wantErr)
			}
		})
	}

	t.Run("tab 2 clicks after tab 1 committed → ErrConcurrent, step 1 untouched", func(t *testing.T) {
		store, d, id := twoStepFixture(t)
		if _, err := d.DecideAt(ctx, "t1", id, DecisionApprove, "", admin, &saw); err != nil { // tab 1
			t.Fatalf("tab 1: %v", err)
		}
		_, err := d.DecideAt(ctx, "t1", id, DecisionApprove, "", admin, &saw) // tab 2, still on step 0
		if !errors.Is(err, ErrConcurrent) {
			t.Fatalf("tab 2: err = %v; want ErrConcurrent", err)
		}
		if in := store.instances[id]; in.CurrentStep != 1 || in.Status != StatusPending {
			t.Fatalf("the late click moved the instance: step=%d status=%s", in.CurrentStep, in.Status)
		}
	})
}
