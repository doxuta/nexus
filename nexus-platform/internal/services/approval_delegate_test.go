package services

// Delegate-target existence guard. Delegation parks the CURRENT step on a named person; a typo or a
// user id copied from another tenant would otherwise create an approval no one can ever act on — it
// sits pending forever (ghost-park). The service rejects the ghost BEFORE the transition writes, and
// keeps a transient DB failure distinct from a genuine "no such user".

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/bpm"
)

// recordingRequester satisfies approvalRequester and records whether Delegate was reached, so a
// test can prove the guard refuses BEFORE the transition (not after it already wrote history).
type recordingRequester struct{ delegated bool }

func (r *recordingRequester) SubmitWith(context.Context, string, string, string, map[string]any, bpm.Actor, bpm.SubmitOptions) (*bpm.Instance, error) {
	return nil, nil
}
func (r *recordingRequester) Startable(context.Context, string, string, map[string]any) ([]bpm.StartableProcess, error) {
	return nil, nil
}
func (r *recordingRequester) Recall(context.Context, string, string, string, bpm.Actor) (*bpm.Instance, error) {
	return nil, nil
}
func (r *recordingRequester) Delegate(_ context.Context, _, id, _, _ string, _ bpm.Actor) (*bpm.Instance, error) {
	r.delegated = true
	return &bpm.Instance{ID: id, Status: bpm.StatusPending}, nil
}

func TestDelegateApproval_TargetMustExist(t *testing.T) {
	actor := bpm.Actor{UserID: "u1"}

	t.Run("unknown user → 422, transition never runs", func(t *testing.T) {
		req := &recordingRequester{}
		svc := NewApprovalService(&fakeApprovalProcessRepo{}, &fakeApprovalInstanceRepo{}, &fakeApprovalDecider{}).
			WithRequestFlow(req, &fakeApprovalRecordReader{}).
			WithUserChecker(func(context.Context, string, string) (bool, error) { return false, nil })

		_, err := svc.DelegateApproval(context.Background(), "t1", "ai-1", "ghost", "", actor)
		if codeOf(err) != "APPROVAL_DELEGATE_UNKNOWN_USER" {
			t.Fatalf("err = %v, want APPROVAL_DELEGATE_UNKNOWN_USER", err)
		}
		if req.delegated {
			t.Error("the transition ran despite an unknown target — the step ghost-parked")
		}
	})

	t.Run("real user → delegates", func(t *testing.T) {
		req := &recordingRequester{}
		svc := NewApprovalService(&fakeApprovalProcessRepo{}, &fakeApprovalInstanceRepo{}, &fakeApprovalDecider{}).
			WithRequestFlow(req, &fakeApprovalRecordReader{}).
			WithUserChecker(func(context.Context, string, string) (bool, error) { return true, nil })

		if _, err := svc.DelegateApproval(context.Background(), "t1", "ai-1", "u2", "", actor); err != nil {
			t.Fatalf("delegate to a real user: %v", err)
		}
		if !req.delegated {
			t.Error("a valid target was not delegated")
		}
	})

	t.Run("checker DB failure surfaces, not swallowed as unknown-user", func(t *testing.T) {
		req := &recordingRequester{}
		boom := errors.New("db down")
		svc := NewApprovalService(&fakeApprovalProcessRepo{}, &fakeApprovalInstanceRepo{}, &fakeApprovalDecider{}).
			WithRequestFlow(req, &fakeApprovalRecordReader{}).
			WithUserChecker(func(context.Context, string, string) (bool, error) { return false, boom })

		_, err := svc.DelegateApproval(context.Background(), "t1", "ai-1", "u2", "", actor)
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the underlying DB error wrapped (a transient fault must not read as a bad target)", err)
		}
		if req.delegated {
			t.Error("delegated despite the check erroring")
		}
	})

	t.Run("no checker wired → delegates (backward compatible)", func(t *testing.T) {
		req := &recordingRequester{}
		svc := NewApprovalService(&fakeApprovalProcessRepo{}, &fakeApprovalInstanceRepo{}, &fakeApprovalDecider{}).
			WithRequestFlow(req, &fakeApprovalRecordReader{})

		if _, err := svc.DelegateApproval(context.Background(), "t1", "ai-1", "u2", "", actor); err != nil {
			t.Fatalf("unwired checker should not block delegation: %v", err)
		}
		if !req.delegated {
			t.Error("delegation blocked with no checker wired")
		}
	})
}
