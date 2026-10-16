package handlers

// httptest — AdminJobsHandler ("Việc nền", DAILY-QUEUE #31/#32) through the REAL route builder, so
// the gate under test is the one production mounts:
//   * the routes sit on adminGated, NOT builderGated — a customer's own tenant-admin (is_admin, not
//     a platform operator) reaches them while the operator tier is wired (specs/034 puts that
//     persona outside the builder subset: on builderGated it would get 403);
//   * a non-admin gets 403 and the store is never touched;
//   * the tenant is the VERIFIED one from the token, never a query parameter;
//   * wire shape {data:[…camelCase…], pagination:{limit, nextCursor}}, filter validation (400),
//     cursor errors (400) and store failures (500).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// hFakeJobStore is an in-memory stand-in for the tenant-facing jobs store: rows per tenant, and a
// record of the last call so the tests can see what reached it.
type hFakeJobStore struct {
	rows       map[string][]jobs.TenantJob // tenant → rows
	listErr    error
	calls      int
	lastTenant string
	lastFilter jobs.TenantJobFilter
}

func (f *hFakeJobStore) ListForTenant(_ context.Context, tenantID string, flt jobs.TenantJobFilter) ([]jobs.TenantJob, string, error) {
	f.calls++
	f.lastTenant, f.lastFilter = tenantID, flt
	if f.listErr != nil {
		return nil, "", f.listErr
	}
	return f.rows[tenantID], "", nil
}

func (f *hFakeJobStore) GetForTenant(_ context.Context, tenantID, id string) (*jobs.TenantJob, error) {
	f.calls++
	f.lastTenant = tenantID
	for _, r := range f.rows[tenantID] {
		if r.ID == id {
			return &r, nil
		}
	}
	return nil, jobs.ErrJobNotFound
}

func (f *hFakeJobStore) RequeueFinished(_ context.Context, tenantID, id, from string, _ time.Time) (bool, error) {
	f.calls++
	for i, r := range f.rows[tenantID] {
		if r.ID == id && r.Status == from {
			f.rows[tenantID][i].Status = jobs.StatusQueued
			return true, nil
		}
	}
	return false, nil
}

func strPtr(s string) *string { return &s }

// buildJobsEngine mounts the jobs routes through RegisterAPIRoutes with the operator tier wired,
// exactly like main.go. Tokens: plain tenant-admin of t1 (NOT an operator), an admin of t2, and a
// non-admin of t1.
func buildJobsEngine(t *testing.T, svc *services.BackgroundJobService) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	claims := func(user, tenant string, admin bool) *auth.Claims {
		return &auth.Claims{Subject: auth.Subject{UserID: user, TenantID: tenant, RoleID: "r1", IsAdmin: admin}, TokenType: auth.TokenTypeAccess}
	}
	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		"tok-admin":    claims("u-adm", "t1", true),
		"tok-admin-t2": claims("u-adm2", "t2", true),
		"tok-user":     claims("u-usr", "t1", false),
	}}
	r := gin.New()
	r.Use(gin.Recovery())
	RegisterAPIRoutes(r.Group("/api/v1"), ver, nil, APIHandlers{
		Jobs: NewAdminJobsHandler(svc, zap.NewNop()),
	}, zap.NewNop(),
		// Nobody is an operator here: a route on builderGated would answer 403 to every token.
		WithOperatorTier(middleware.RequirePlatformOperator(&operatorRepoFake{operatorID: "nobody"}, zap.NewNop())),
	)
	return r
}

func jobsReq(t *testing.T, r *gin.Engine, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAdminJobsList_GateAndTenantPin(t *testing.T) {
	failedAt := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	store := &hFakeJobStore{rows: map[string][]jobs.TenantJob{
		"t1": {{ID: "job-1", Type: "outbound_webhook", Status: jobs.StatusFailed, Attempts: 1, MaxAttempts: 10,
			LastError:  strPtr("services.OutboundWebhookJobHandler: workflow.HTTPWebhookSender.Deliver: status 422"),
			WorkflowID: strPtr("wf-1"), RunAt: failedAt, CreatedAt: failedAt, FinishedAt: &failedAt}},
		"t2": {{ID: "job-other-tenant", Type: "outbound_webhook", Status: jobs.StatusFailed}},
	}}
	r := buildJobsEngine(t, services.NewBackgroundJobService(store))

	// Non-admin: refused by the admin gate before anything runs.
	if w := jobsReq(t, r, http.MethodGet, "/api/v1/admin/jobs", "tok-user"); w.Code != http.StatusForbidden || store.calls != 0 {
		t.Fatalf("non-admin GET /admin/jobs = %d (store calls %d); want 403 and no store call", w.Code, store.calls)
	}

	// The customer's own tenant-admin (not an operator) gets their rows — and only theirs, even
	// when the query string names another tenant.
	w := jobsReq(t, r, http.MethodGet, "/api/v1/admin/jobs?tenantId=t2", "tok-admin")
	if w.Code != http.StatusOK {
		t.Fatalf("tenant-admin GET /admin/jobs = %d %s; want 200 (adminGated, not builderGated)", w.Code, w.Body)
	}
	if store.lastTenant != "t1" {
		t.Fatalf("store queried tenant %q; want the verified t1", store.lastTenant)
	}
	var body struct {
		Data       []map[string]any `json:"data"`
		Pagination map[string]any   `json:"pagination"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data) != 1 || body.Data[0]["id"] != "job-1" {
		t.Fatalf("data = %v; want exactly t1's job-1", body.Data)
	}
	row := body.Data[0]
	for _, k := range []string{"jobType", "status", "attempts", "maxAttempts", "lastError", "workflowId", "runAt", "createdAt", "finishedAt"} {
		if _, ok := row[k]; !ok {
			t.Fatalf("row lacks %q: %v", k, row)
		}
	}
	if _, leaked := row["payload"]; leaked {
		t.Fatalf("row carries the payload: %v", row)
	}
	if !strings.HasSuffix(row["lastError"].(string), "status 422") {
		t.Fatalf("lastError = %v; want the stored text verbatim", row["lastError"])
	}
	if body.Pagination["limit"] != float64(50) || body.Pagination["nextCursor"] != nil {
		t.Fatalf("pagination = %v; want limit 50 (default) and nextCursor null", body.Pagination)
	}

	// Another tenant's admin sees their own row, not t1's.
	w = jobsReq(t, r, http.MethodGet, "/api/v1/admin/jobs", "tok-admin-t2")
	if w.Code != http.StatusOK || store.lastTenant != "t2" || strings.Contains(w.Body.String(), "job-1") {
		t.Fatalf("t2 admin = %d tenant %q body %s; want only t2's rows", w.Code, store.lastTenant, w.Body)
	}
}

func TestAdminJobsList_FiltersAndErrors(t *testing.T) {
	cases := []struct {
		name     string
		query    string
		storeErr error
		status   int
		code     string
		reached  bool
		want     jobs.TenantJobFilter
	}{
		{name: "filters pass through", query: "?type=outbound_webhook&status=failed&limit=10&after=abc",
			status: http.StatusOK, reached: true,
			want: jobs.TenantJobFilter{Type: "outbound_webhook", Status: jobs.StatusFailed, Limit: 10, After: "abc"}},
		{name: "every status is a valid filter", query: "?status=queued", status: http.StatusOK, reached: true,
			want: jobs.TenantJobFilter{Status: jobs.StatusQueued, Limit: 50}},
		{name: "unknown status", query: "?status=exploded", status: http.StatusBadRequest, code: "JOB_FILTER_INVALID"},
		{name: "type too long", query: "?type=" + strings.Repeat("x", 65), status: http.StatusBadRequest, code: "JOB_FILTER_INVALID"},
		{name: "bad cursor", query: "?after=zzz", storeErr: fmt.Errorf("jobs: %w", dynrepo.ErrInvalidCursor),
			status: http.StatusBadRequest, code: "INVALID_CURSOR", reached: true},
		{name: "store failure", storeErr: errors.New("db down"), status: http.StatusInternalServerError,
			code: "JOBS_QUERY_FAILED", reached: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &hFakeJobStore{listErr: tc.storeErr}
			w := jobsReq(t, buildJobsEngine(t, services.NewBackgroundJobService(store)), http.MethodGet, "/api/v1/admin/jobs"+tc.query, "tok-admin")
			if w.Code != tc.status {
				t.Fatalf("status = %d %s; want %d", w.Code, w.Body, tc.status)
			}
			if tc.code != "" && !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("body %s lacks code %s", w.Body, tc.code)
			}
			if (store.calls > 0) != tc.reached {
				t.Fatalf("store reached = %v; want %v", store.calls > 0, tc.reached)
			}
			if tc.status == http.StatusOK && store.lastFilter != tc.want {
				t.Fatalf("filter = %+v; want %+v", store.lastFilter, tc.want)
			}
			if tc.status == http.StatusOK && !strings.Contains(w.Body.String(), `"data":[]`) {
				t.Fatalf("an empty page must be data:[] (never null): %s", w.Body)
			}
		})
	}
}

// hJobAuditSpy captures the audit events the retry route emits.
type hJobAuditSpy struct{ events []audit.Event }

func (s *hJobAuditSpy) Record(_ context.Context, evt audit.Event) { s.events = append(s.events, evt) }

// TestAdminJobsRetry pins POST /admin/jobs/:id/retry ("Gửi lại", #32) through the real routes: the
// same adminGated gate as the list, its OWN tenant check (an id is all it takes to aim at another
// tenant's job — the list's filter cannot protect this path), the verified actor in the audit row,
// and each refusal's status.
func TestAdminJobsRetry(t *testing.T) {
	newStore := func() *hFakeJobStore {
		return &hFakeJobStore{rows: map[string][]jobs.TenantJob{
			"t1": {
				{ID: "job-failed", Type: "outbound_webhook", Status: jobs.StatusFailed, Attempts: 10, MaxAttempts: 10,
					LastError: strPtr("status 422")},
				{ID: "job-done", Type: "outbound_webhook", Status: jobs.StatusDone, Attempts: 1, MaxAttempts: 10},
				{ID: "job-timer-done", Type: "workflow_timer", Status: jobs.StatusDone, Attempts: 1, MaxAttempts: 5},
			},
			"t2": {{ID: "job-t2", Type: "outbound_webhook", Status: jobs.StatusFailed, MaxAttempts: 10}},
		}}
	}
	cases := []struct {
		name     string
		token    string
		id       string
		queueOff bool
		status   int
		code     string
	}{
		{name: "tenant-admin resends its failed job", token: "tok-admin", id: "job-failed", status: http.StatusOK},
		{name: "non-admin is refused before anything runs", token: "tok-user", id: "job-failed", status: http.StatusForbidden, code: "FORBIDDEN"},
		{name: "another tenant's job id is not found", token: "tok-admin", id: "job-t2", status: http.StatusNotFound, code: "JOB_NOT_FOUND"},
		{name: "a delivered webhook is resent (#256)", token: "tok-admin", id: "job-done", status: http.StatusOK},
		{name: "a delivered timer is a conflict", token: "tok-admin", id: "job-timer-done", status: http.StatusConflict, code: "JOB_NOT_RETRYABLE"},
		{name: "queue switched off", token: "tok-admin", id: "job-failed", queueOff: true, status: http.StatusServiceUnavailable, code: "JOBS_DISABLED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore()
			spy := &hJobAuditSpy{}
			svc := services.NewBackgroundJobService(store).WithAuditor(spy).WithQueueEnabled(!tc.queueOff)
			w := jobsReq(t, buildJobsEngine(t, svc), http.MethodPost, "/api/v1/admin/jobs/"+tc.id+"/retry", tc.token)
			if w.Code != tc.status {
				t.Fatalf("status = %d %s; want %d", w.Code, w.Body, tc.status)
			}
			if tc.code != "" {
				if !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
					t.Fatalf("body %s lacks code %s", w.Body, tc.code)
				}
				if len(spy.events) != 0 || store.rows["t2"][0].Status != jobs.StatusFailed {
					t.Fatalf("a refused resend changed state: audit %d, t2 job %s", len(spy.events), store.rows["t2"][0].Status)
				}
				if tc.status == http.StatusForbidden && store.calls != 0 {
					t.Fatalf("the gate let a non-admin reach the store (%d calls)", store.calls)
				}
				return
			}
			var body struct {
				Data struct {
					Job map[string]any `json:"job"`
				} `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Data.Job["id"] != tc.id || body.Data.Job["status"] != jobs.StatusQueued || body.Data.Job["attempts"] != float64(0) {
				t.Fatalf("job = %v; want the job back in the queue with attempts 0", body.Data.Job)
			}
			if store.lastTenant != "t1" || len(spy.events) != 1 || spy.events[0].ActorUserID != "u-adm" || spy.events[0].RecordID != tc.id {
				t.Fatalf("tenant %q, audit %+v; want t1 and one row by the verified u-adm", store.lastTenant, spy.events)
			}
		})
	}
}
