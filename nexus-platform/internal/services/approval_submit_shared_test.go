package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/dynrepo"
)

// shareOnlyReader is a record reader where the record is reachable ONLY through a manual share
// (specs/051): a scope without IncludeShares misses it, one with the flag reads it. visibleToAll=true
// models a record the caller reaches on their own.
type shareOnlyReader struct {
	visibleToAll bool
	getErr       error // a non-not-found failure for the first read
	gets         []RecordScope
}

func (r *shareOnlyReader) Get(_ context.Context, sc RecordScope, _, _, id string, _, _ []string) (*dynrepo.Record, error) {
	r.gets = append(r.gets, sc)
	if r.getErr != nil {
		return nil, r.getErr
	}
	if !r.visibleToAll && !sc.IncludeShares && !sc.IsAdmin {
		return nil, ErrRecordNotFound
	}
	return &dynrepo.Record{ID: id, Data: map[string]any{}}, nil
}

func (r *shareOnlyReader) AuthorizeView(_ context.Context, _ string, sc RecordScope, _ string) (RecordScope, error) {
	return sc, nil
}

type submitRecorder struct{ submitted bool }

func (s *submitRecorder) SubmitWith(context.Context, string, string, string, map[string]any, bpm.Actor, bpm.SubmitOptions) (*bpm.Instance, error) {
	s.submitted = true
	return &bpm.Instance{ID: "ai-1"}, nil
}
func (s *submitRecorder) Startable(context.Context, string, string, map[string]any) ([]bpm.StartableProcess, error) {
	return nil, nil
}
func (s *submitRecorder) Recall(context.Context, string, string, string, bpm.Actor) (*bpm.Instance, error) {
	return nil, nil
}
func (s *submitRecorder) Delegate(context.Context, string, string, string, string, bpm.Actor) (*bpm.Instance, error) {
	return nil, nil
}

// TestSubmitForApproval_SharedOnlyViewerGets403 pins the approval half of DAILY-QUEUE #225: the submit
// route is pinned to "edit", so a caller who sees the record only through a manual share used to get a
// bare 404; they already read it, so the answer is RECORD_SHARED_READ_ONLY (403) — and nothing is
// submitted. A stranger keeps the 404, and a caller who reaches the record on their own still submits.
func TestSubmitForApproval_SharedOnlyViewerGets403(t *testing.T) {
	actor := bpm.Actor{UserID: "u2"}
	edit := RecordScope{UserID: "u2", RoleID: "r", AccessLevel: "owner"} // what the edit-pinned route resolves

	run := func(rd *shareOnlyReader, sc RecordScope) (error, *submitRecorder) {
		req := &submitRecorder{}
		svc := NewApprovalService(&fakeApprovalProcessRepo{}, &fakeApprovalInstanceRepo{}, &fakeApprovalDecider{}).WithRequestFlow(req, rd)
		_, err := svc.SubmitForApproval(context.Background(), sc, "t1", "deals", "rec-1", actor, bpm.SubmitOptions{})
		return err, req
	}

	t.Run("share-only viewer: 403, nothing submitted", func(t *testing.T) {
		err, req := run(&shareOnlyReader{}, edit)
		if !errors.Is(err, ErrRecordSharedReadOnly) {
			t.Fatalf("err = %v; want ErrRecordSharedReadOnly", err)
		}
		if req.submitted {
			t.Fatal("a share-only viewer started an approval")
		}
	})

	t.Run("stranger (not even the flagged read reaches it): still 404", func(t *testing.T) {
		rd := &shareOnlyReader{getErr: ErrRecordNotFound}
		if err, _ := run(rd, edit); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("err = %v; want ErrRecordNotFound", err)
		}
	})

	t.Run("a caller who reaches the record on their own submits", func(t *testing.T) {
		err, req := run(&shareOnlyReader{visibleToAll: true}, edit)
		if err != nil || !req.submitted {
			t.Fatalf("err = %v submitted = %v; want a normal submit", err, req.submitted)
		}
	})

	t.Run("level all: the probe is skipped (a miss there means the record is gone)", func(t *testing.T) {
		rd := &shareOnlyReader{getErr: ErrRecordNotFound}
		sc := RecordScope{UserID: "u2", RoleID: "r", AccessLevel: "all"}
		if err, _ := run(rd, sc); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("err = %v; want ErrRecordNotFound", err)
		}
		if len(rd.gets) != 1 {
			t.Fatalf("reads = %d; want exactly 1 (no share probe at level all)", len(rd.gets))
		}
	})
}
