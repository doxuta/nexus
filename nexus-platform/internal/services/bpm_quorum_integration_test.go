//go:build integration

// specs/043 S6b — quorum % on real MySQL (tasks T112, T115): 60% of 3 needs 2; a third seat closes
// as superseded; two rejections make the quorum unreachable and reject at once; two last votes
// pressed together advance the step exactly once (-race); the detail shows "2/3 — cần 2".
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run 'TestQuorum_'
package services

import (
	"context"
	"sync"
	"testing"

	"nexus/platform/internal/bpm"
)

func quorum60() *ApprovalCompletionInput {
	return &ApprovalCompletionInput{Mode: "quorum", Percent: 60}
}

func TestQuorum_TwoOfThreeCarriesAndClosesTheThirdSeat_MySQL(t *testing.T) {
	f := armCouncilsWith(t, "quorcarry", quorum60(), 1, users(seatAp1, seatAp2, seatAp3)...)
	ctx := context.Background()
	id := f.insts[0]

	got, err := f.decide(t, id, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp1})
	if err != nil || got.CurrentStep != 0 {
		t.Fatalf("first approval: %v step %d; want still on the council", err, got.CurrentStep)
	}
	d, err := f.st.approval.GetApprovalDetail(ctx, RecordScope{UserID: seatAp2}, f.tenant, id, bpm.Actor{UserID: seatAp2})
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if d.Completion == nil || d.Completion.Mode != "quorum" || d.Completion.Percent != 60 || d.Completion.Total != 3 ||
		d.Completion.Approved != 1 || d.Completion.Required != 2 {
		t.Fatalf("completion = %+v; want quorum 60%%, 1 of 3 approved, 2 required", d.Completion)
	}
	got, err = f.decide(t, id, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp2})
	if err != nil {
		t.Fatalf("second approval: %v", err)
	}
	if got.CurrentStep != 1 || got.CurrentApproverID != seatAp4 {
		t.Fatalf("after 2 of 3: step %d approver %q; want the director's step", got.CurrentStep, got.CurrentApproverID)
	}
	if open, approved, rejected, superseded := seatsByNode(t, f, id, "s1"); open != 0 || approved != 2 || rejected != 0 || superseded != 1 {
		t.Fatalf("seats: open=%d approved=%d rejected=%d superseded=%d; want 0/2/0/1", open, approved, rejected, superseded)
	}
	_, err = f.decide(t, id, bpm.DecisionApprove, "", bpm.Actor{UserID: seatAp3})
	wantApprovalCode(t, err, "APPROVAL_NOT_AUTHORIZED") // the third seat is closed
	if rows, _ := f.st.repo.ListPendingForUser(ctx, f.tenant, seatAp3, nil, nil); len(rows) != 0 {
		t.Fatalf("the third approver still has a finished approval in the inbox")
	}
}

func TestQuorum_UnreachableRejectsAtOnce_MySQL(t *testing.T) {
	f := armCouncilsWith(t, "quorrej", quorum60(), 1, users(seatAp1, seatAp2, seatAp3)...)
	id := f.insts[0]
	got, err := f.decide(t, id, bpm.DecisionReject, "giá cao", bpm.Actor{UserID: seatAp1})
	if err != nil || got.Status != bpm.StatusPending {
		t.Fatalf("first rejection: %v %s; want pending (2 approvals still reachable)", err, got.Status)
	}
	got, err = f.decide(t, id, bpm.DecisionReject, "giá cao", bpm.Actor{UserID: seatAp2})
	if err != nil {
		t.Fatalf("second rejection: %v", err)
	}
	if got.Status != bpm.StatusRejected {
		t.Fatalf("status = %s; want rejected once 2 approvals cannot be reached", got.Status)
	}
	if open, _, rejected, superseded := seatsByNode(t, f, id, "s1"); open != 0 || rejected != 2 || superseded != 1 {
		t.Fatalf("seats: open=%d rejected=%d superseded=%d; want 0/2/1", open, rejected, superseded)
	}
	if v := f.st.recordField(t, f.tenant, f.entity, f.records[0], "status"); v != "rejected" {
		t.Fatalf("record status = %v; want the rejected value", v)
	}
}

// SC-003 for the quorum rule: the two votes that reach the quorum pressed together lose no vote and
// advance the step exactly once.
func TestQuorum_ConcurrentVotesAdvanceOnce_MySQL(t *testing.T) {
	const rounds = 12
	f := armCouncilsWith(t, "quorrace", quorum60(), rounds, users(seatAp1, seatAp2, seatAp3)...)
	for i := 0; i < rounds; i++ {
		id := f.insts[i]
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for k, voter := range []string{seatAp1, seatAp2} {
			wg.Add(1)
			go func(k int, voter string) {
				defer wg.Done()
				_, errs[k] = f.decide(t, id, bpm.DecisionApprove, "", bpm.Actor{UserID: voter})
			}(k, voter)
		}
		wg.Wait()
		for k, err := range errs {
			if err != nil {
				t.Fatalf("round %d voter %d: %v — a vote must never be lost to a conflict", i, k, err)
			}
		}
		inst := f.st.onlyPendingInstance(t, f.tenant, f.entity, f.records[i])
		if inst.CurrentStep != 1 {
			t.Fatalf("round %d: step %d; want the instance on the director's step exactly once", i, inst.CurrentStep)
		}
		if open, approved, _, superseded := seatsByNode(t, f, id, "s1"); open != 0 || approved != 2 || superseded != 1 {
			t.Fatalf("round %d seats: open=%d approved=%d superseded=%d; want 0/2/1", i, open, approved, superseded)
		}
	}
}
