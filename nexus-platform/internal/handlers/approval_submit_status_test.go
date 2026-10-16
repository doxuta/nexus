package handlers

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/services"
)

// missReader is an approval record reader whose read of the record always fails with err.
type missReader struct{ err error }

func (m missReader) Get(context.Context, services.RecordScope, string, string, string, []string, []string) (*dynrepo.Record, error) {
	return nil, m.err
}
func (m missReader) AuthorizeView(_ context.Context, _ string, sc services.RecordScope, _ string) (services.RecordScope, error) {
	return sc, nil
}

type noSubmit struct{}

func (noSubmit) Startable(context.Context, string, string, map[string]any) ([]bpm.StartableProcess, error) {
	return nil, nil
}
func (noSubmit) SubmitWith(context.Context, string, string, string, map[string]any, bpm.Actor, bpm.SubmitOptions) (*bpm.Instance, error) {
	return nil, errors.New("must not be reached")
}
func (noSubmit) Recall(context.Context, string, string, string, bpm.Actor) (*bpm.Instance, error) {
	return nil, nil
}
func (noSubmit) Delegate(context.Context, string, string, string, string, bpm.Actor) (*bpm.Instance, error) {
	return nil, nil
}

// TestApprovalSubmit_RefusalStatuses: POST /:entity/:id/approvals/submit answers a row the caller may
// not see with 404 like every record route (it used to fall through to a 500 APPROVAL_OP_FAILED —
// the sentinel was unmapped), and a row they see only through a manual share with 403
// RECORD_SHARED_READ_ONLY (DAILY-QUEUE #225).
func TestApprovalSubmit_RefusalStatuses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		{"record out of scope", services.ErrRecordNotFound, http.StatusNotFound, "RECORD_NOT_FOUND"},
		{"entity unknown", services.ErrEntityNotFound, http.StatusNotFound, "ENTITY_NOT_FOUND"},
		{"share-only viewer", services.ErrRecordSharedReadOnly, http.StatusForbidden, "RECORD_SHARED_READ_ONLY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := services.NewApprovalService(nil, nil, nil).WithRequestFlow(noSubmit{}, missReader{err: tc.err})
			rt := retryRouter(svc, false)
			NewApprovalHandler(svc, nil).RegisterSubmitRoute(rt.Group(""))
			w := sendJSON(rt, http.MethodPost, "/deals/rec-1/approvals/submit", ``)
			if w.Code != tc.wantCode || !jsonHas(w.Body.Bytes(), "code", tc.wantBody) {
				t.Fatalf("status %d %s; want %d %s", w.Code, w.Body.String(), tc.wantCode, tc.wantBody)
			}
		})
	}
}
