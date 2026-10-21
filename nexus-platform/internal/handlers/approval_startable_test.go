package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/services"
)

// okReader lets every record read through.
type okReader struct{}

func (okReader) Get(context.Context, services.RecordScope, string, string, string, []string, []string) (*dynrepo.Record, error) {
	return &dynrepo.Record{Data: map[string]any{"total": 1}}, nil
}
func (okReader) ExplainWriteMiss(_ context.Context, _ services.RecordScope, _, _, _ string, err error) error {
	return err
}
func (okReader) AuthorizeView(_ context.Context, _ string, sc services.RecordScope, _ string) (services.RecordScope, error) {
	return sc, nil
}

// sendRecorder records what the submit route handed the engine and answers with a scripted result.
type sendRecorder struct {
	noSubmit
	opt bpm.SubmitOptions
	err error
}

func (s *sendRecorder) SubmitWith(_ context.Context, _, _, _ string, _ map[string]any, _ bpm.Actor, opt bpm.SubmitOptions) (*bpm.Instance, error) {
	s.opt = opt
	if s.err != nil {
		return nil, s.err
	}
	return &bpm.Instance{ID: "ai-1", Status: bpm.StatusPending}, nil
}
func (s *sendRecorder) Startable(context.Context, string, string, map[string]any) ([]bpm.StartableProcess, error) {
	return []bpm.StartableProcess{{ProcessID: "p-b", Name: "Duyệt B"}}, nil
}

func postSubmit(rec *sendRecorder, body, key string) *httptest.ResponseRecorder {
	svc := services.NewApprovalService(nil, nil, nil).WithRequestFlow(rec, okReader{})
	rt := retryRouter(svc, false)
	NewApprovalHandler(svc, nil).RegisterSubmitRoute(rt.Group(""))
	req := httptest.NewRequest(http.MethodPost, "/deals/rec-1/approvals/submit", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	return w
}

func TestApprovalSubmit_PassesProcessIdAndIdempotencyKey(t *testing.T) {
	rec := &sendRecorder{}
	w := postSubmit(rec, `{"processId":" p-b "}`, "k-1")
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	if rec.opt.ProcessID != "p-b" || rec.opt.IdempotencyKey != "k-1" {
		t.Fatalf("engine got %+v, want processId p-b and key k-1", rec.opt)
	}
}

func TestApprovalSubmit_SendErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		key    string
		status int
		code   string
	}{
		{"process not applicable", bpm.ErrProcessNotApplicable, "", http.StatusUnprocessableEntity, "APPROVAL_PROCESS_NOT_APPLICABLE"},
		{"key reused", bpm.ErrIdempotencyKeyReused, "k", http.StatusUnprocessableEntity, "APPROVAL_IDEMPOTENCY_KEY_REUSED"},
		{"key too long", nil, strings.Repeat("k", 129), http.StatusBadRequest, "IDEMPOTENCY_KEY_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := postSubmit(&sendRecorder{err: tc.err}, `{}`, tc.key)
			if w.Code != tc.status || !jsonHas(w.Body.Bytes(), "code", tc.code) {
				t.Fatalf("status %d %s; want %d %s", w.Code, w.Body.String(), tc.status, tc.code)
			}
		})
	}
}

func TestApprovalStartable_ListsProcesses(t *testing.T) {
	svc := services.NewApprovalService(nil, nil, nil).WithRequestFlow(&sendRecorder{}, okReader{})
	rt := retryRouter(svc, false)
	NewApprovalHandler(svc, nil).RegisterSubmitRoute(rt.Group(""))
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/deals/rec-1/approvals/startable", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"processId":"p-b"`) || !strings.Contains(w.Body.String(), `"name":"Duyệt B"`) {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
}
