package handlers

// specs/043 S7: POST /admin/approval-instances/:id/retry · /cancel and the stuck filter of
// GET /admin/approval-processes/:id/instances — wired to the service, errors mapped to the codes the
// admin screen branches on.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/services"
)

type scriptedRetrier struct {
	err     error
	calls   int
	gotNote string
	gotID   string
}

func (r *scriptedRetrier) Retry(_ context.Context, _, id string, _ bpm.Actor, note string) (*bpm.Instance, error) {
	r.calls++
	r.gotID, r.gotNote = id, note
	return &bpm.Instance{ID: id, Status: bpm.StatusPending}, r.err
}

type scriptedRecall struct {
	err      error
	gotAdmin bool
	gotNote  string
}

func (s *scriptedRecall) Startable(context.Context, string, string, map[string]any) ([]bpm.StartableProcess, error) {
	return nil, nil
}
func (s *scriptedRecall) SubmitWith(context.Context, string, string, string, map[string]any, bpm.Actor, bpm.SubmitOptions) (*bpm.Instance, error) {
	return nil, nil
}
func (s *scriptedRecall) Recall(_ context.Context, _, id, note string, actor bpm.Actor) (*bpm.Instance, error) {
	s.gotAdmin, s.gotNote = actor.IsAdmin, note
	return &bpm.Instance{ID: id, Status: bpm.StatusCancelled}, s.err
}
func (s *scriptedRecall) Delegate(context.Context, string, string, string, string, bpm.Actor) (*bpm.Instance, error) {
	return nil, nil
}

type listSpy struct {
	memProcessRepo
	gotStuck bool
}

func (l *listSpy) ListProcessInstances(_ context.Context, _, _ string, _ *int, _ string, stuck bool, _ int) ([]bpm.ProcessInstanceRow, error) {
	l.gotStuck = stuck
	return []bpm.ProcessInstanceRow{{ID: "i1", Stuck: true, LastError: "boom"}}, nil
}

func retryRouter(svc *services.ApprovalService, admin bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewApprovalHandler(svc, zap.NewNop())
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("authClaims", &auth.Claims{
			Subject:   auth.Subject{UserID: "admin-1", TenantID: "t-1", IsAdmin: admin},
			TokenType: auth.TokenTypeAccess,
		})
		c.Set("tenantID", "t-1")
		c.Next()
	})
	h.RegisterAdminRoutes(r.Group(""))
	return r
}

func TestApprovalRetryAPI(t *testing.T) {
	for _, tc := range []struct {
		name     string
		admin    bool
		retryErr error
		wantCode int
		wantErr  string
		wantCall bool
	}{
		{"stuck hồ sơ is retried", true, nil, http.StatusOK, "", true},
		{"waiting for a person", true, bpm.ErrNotStuck, http.StatusConflict, "APPROVAL_NOT_STUCK", true},
		{"already finished", true, bpm.ErrNotPending, http.StatusUnprocessableEntity, "APPROVAL_NOT_PENDING", true},
		{"unknown id", true, bpm.ErrInstanceNotFound, http.StatusNotFound, "APPROVAL_INSTANCE_NOT_FOUND", true},
		{"a non-admin never reaches the runner", false, nil, http.StatusUnprocessableEntity, "APPROVAL_NOT_AUTHORIZED", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &scriptedRetrier{err: tc.retryErr}
			svc := services.NewApprovalService(nil, nil, nil).WithRetrier(rt)
			w := sendJSON(retryRouter(svc, tc.admin), http.MethodPost, "/admin/approval-instances/ai-1/retry", `{"note":"đã sửa"}`)
			if w.Code != tc.wantCode {
				t.Fatalf("status %d %s; want %d", w.Code, w.Body.String(), tc.wantCode)
			}
			if tc.wantErr != "" && !jsonHas(w.Body.Bytes(), "code", tc.wantErr) {
				t.Fatalf("body %s; want code %s", w.Body.String(), tc.wantErr)
			}
			if (rt.calls == 1) != tc.wantCall {
				t.Fatalf("runner calls = %d; want call=%v", rt.calls, tc.wantCall)
			}
			if tc.wantCall && (rt.gotID != "ai-1" || rt.gotNote != "đã sửa") {
				t.Fatalf("runner saw id %q note %q", rt.gotID, rt.gotNote)
			}
		})
	}
}

func TestApprovalCancelAPI(t *testing.T) {
	rc := &scriptedRecall{}
	svc := services.NewApprovalService(nil, nil, nil).WithRequestFlow(rc, nil)
	w := sendJSON(retryRouter(svc, true), http.MethodPost, "/admin/approval-instances/ai-1/cancel", `{"note":"không cần nữa"}`)
	if w.Code != http.StatusOK || !rc.gotAdmin || rc.gotNote != "không cần nữa" {
		t.Fatalf("cancel = %d %s admin=%v note=%q; want 200 through the recall path as an admin", w.Code, w.Body.String(), rc.gotAdmin, rc.gotNote)
	}
	if w := sendJSON(retryRouter(svc, false), http.MethodPost, "/admin/approval-instances/ai-1/cancel", `{}`); w.Code == http.StatusOK {
		t.Fatalf("a non-admin cancelled a hồ sơ: %d", w.Code)
	}
}

func TestApprovalListStuckFilter(t *testing.T) {
	spy := &listSpy{}
	svc := services.NewApprovalService(spy, nil, nil)
	r := retryRouter(svc, true)
	for query, want := range map[string]bool{"": false, "?stuck=true": true, "?stuck=1": true, "?stuck=false": false} {
		spy.gotStuck = !want
		w := sendJSON(r, http.MethodGet, "/admin/approval-processes/p-1/instances"+query, "")
		if w.Code != http.StatusOK || spy.gotStuck != want {
			t.Fatalf("%q = %d (stuck reached the repository as %v); want 200 and %v", query, w.Code, spy.gotStuck, want)
		}
	}
	if w := sendJSON(r, http.MethodGet, "/admin/approval-processes/p-1/instances?stuck=maybe", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("stuck=maybe = %d; want 400", w.Code)
	}
}

// jsonHas reports whether the JSON object body has key == want (as a string).
func jsonHas(body []byte, key, want string) bool {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return false
	}
	got, _ := m[key].(string)
	return got == want
}
