package bpm

import (
	"context"
	"errors"
	"testing"
)

// US2 AS5: after a rejection returned the approval to s1, an old "Duyệt" click of duyet2 (whose
// screen showed s2) is a 409, not an answer about who they are — they held a seat on the step they
// saw. A stranger sending the same stale step still hears the plain refusal (no step probing).
func TestReject_ReturnLoop_OldClickOfTheRejectorIsAConflict(t *testing.T) {
	store, d, _, id := reworkFixture(t, `{"to":"s1"}`)
	ctx := context.Background()
	if _, err := d.Decide(ctx, "t1", id, DecisionReject, "sai", Actor{UserID: "duyet2"}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	// What the seat table says after the return: duyet2's seat on s2 is done, duyet1 holds s1.
	store.visits[id] = []VisitSeat{{NodeID: "s2", AssigneeType: ApproverUser, AssigneeID: "duyet2", Status: "done", Outcome: SeatRejected, DecidedBy: "duyet2"}}
	saw := 1
	if _, err := d.DecideAt(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "duyet2"}, &saw); !errors.Is(err, ErrConcurrent) {
		t.Fatalf("old click of the rejector: err = %v; want ErrConcurrent", err)
	}
	if _, err := d.DecideAt(ctx, "t1", id, DecisionApprove, "", Actor{UserID: "stranger"}, &saw); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("stranger with a stale step: err = %v; want ErrNotAuthorized", err)
	}
	if in := store.instances[id]; in.CurrentStep != 0 || in.Status != StatusPending {
		t.Fatalf("a refused click moved the instance: step=%d status=%s", in.CurrentStep, in.Status)
	}
}
