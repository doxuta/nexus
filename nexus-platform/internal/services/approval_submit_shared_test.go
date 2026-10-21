package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/bpm"
)

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

// TestSubmitForApproval_WriteMissAnswers pins "Gửi duyệt" to the ONE write-miss decision (#43, with
// DAILY-QUEUE #225 and #18): the route is pinned to "edit", so a send whose edit scope misses the
// record answers what a PUT would — RECORD_SHARED_READ_ONLY for a share-only viewer, FORBIDDEN for a
// record their view scope reaches, the bare 404 for one they cannot see (another team's, a deleted
// one) — and nothing is submitted. The reader is a REAL RecordService, so this is the shared decision
// itself, not a copy of it.
func TestSubmitForApproval_WriteMissAnswers(t *testing.T) {
	edit := RecordScope{UserID: "u-rep", RoleID: "r1", AccessLevel: "owner"} // what the edit-pinned route resolves
	cases := []struct {
		name   string
		store  RecordStore
		levels map[string]string
		want   error // nil = submitted
	}{
		{"own record → submitted", &editScopeStore{mine: true}, map[string]string{"view": "all", "edit": "owner"}, nil},
		{"view=all edit=own, a colleague's record → 403 FORBIDDEN", &viewNotEditStore{}, map[string]string{"view": "all", "edit": "owner"}, ErrRecordEditForbidden},
		{"view=team edit=own, a teammate's record → 403 FORBIDDEN", &viewNotEditStore{inMyTeam: true}, map[string]string{"view": "team", "edit": "owner"}, ErrRecordEditForbidden},
		{"view=team edit=own, another team's record → 404", &viewNotEditStore{}, map[string]string{"view": "team", "edit": "owner"}, ErrRecordNotFound},
		{"view=all, the record is gone → 404", &viewNotEditStore{gone: true}, map[string]string{"view": "all", "edit": "owner"}, ErrRecordNotFound},
		{"share-only viewer → 403 RECORD_SHARED_READ_ONLY", &shareOnlyStore{shared: true}, map[string]string{"view": "owner", "edit": "owner"}, ErrRecordSharedReadOnly},
		{"stranger → 404", &shareOnlyStore{}, map[string]string{"view": "owner", "edit": "owner"}, ErrRecordNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records := shareTestService(tc.store).WithACLAuthorizer(&perActionAuthz{levels: tc.levels})
			req := &submitRecorder{}
			svc := NewApprovalService(&fakeApprovalProcessRepo{}, &fakeApprovalInstanceRepo{}, &fakeApprovalDecider{}).WithRequestFlow(req, records)
			_, err := svc.SubmitForApproval(context.Background(), edit, "tenant-1", "widgets", "rec-1", bpm.Actor{UserID: "u-rep"}, bpm.SubmitOptions{})
			if tc.want == nil {
				if err != nil || !req.submitted {
					t.Fatalf("err = %v submitted = %v; want a normal submit", err, req.submitted)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v; want %v", err, tc.want)
			}
			if req.submitted {
				t.Fatal("submitted despite the refusal")
			}
		})
	}
}
