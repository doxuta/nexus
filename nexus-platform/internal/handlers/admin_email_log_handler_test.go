package handlers

// httptest — AdminEmailLogHandler (specs/066 L1b, "Nhật ký email") through the REAL route builder:
// adminGated (the customer's own admin, NOT an operator) reaches it, staff get 403 before the store
// runs, the tenant is the verified one, and the wire shape / error codes match the contract.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/emaillog"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

type hFakeEmailLogStore struct {
	rows       map[string][]emaillog.Row
	err        error
	calls      int
	lastTenant string
	lastFilter emaillog.Filter
	// L2 (resend): rows by "<tenant>/<id>" for Get/LockResend, and what they were asked.
	byTenantID  map[string]*emaillog.Row
	gets, locks int
}

func (f *hFakeEmailLogStore) ListForTenant(_ context.Context, tenantID string, flt emaillog.Filter) ([]emaillog.Row, string, error) {
	f.calls++
	f.lastTenant, f.lastFilter = tenantID, flt
	if f.err != nil {
		return nil, "", f.err
	}
	return f.rows[tenantID], "", nil
}

func buildEmailLogEngine(t *testing.T, svc *services.EmailLogService) *gin.Engine {
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
		EmailLog: NewAdminEmailLogHandler(svc, zap.NewNop()),
	}, zap.NewNop(),
		// Nobody is an operator: a route mounted on builderGated would answer 403 to every token.
		WithOperatorTier(middleware.RequirePlatformOperator(&operatorRepoFake{operatorID: "nobody"}, zap.NewNop())),
	)
	return r
}

func TestAdminEmailLogList_GateAndTenantPin(t *testing.T) {
	at := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	store := &hFakeEmailLogStore{rows: map[string][]emaillog.Row{
		"t1": {{ID: "log-1", CreatedAt: at, Source: "workflow", Status: "failed", RecipientMasked: "a***@gmail.com",
			SubjectTemplate: strPtr("Chào {{name}}"), WorkflowName: strPtr("Chào khách"), Attempts: 5, LastError: strPtr("x: [redacted]")}},
		"t2": {{ID: "log-other", Status: "sent"}},
	}}
	r := buildEmailLogEngine(t, services.NewEmailLogService(store).WithMailConfigured(true).WithQueueEnabled(false))

	if w := jobsReq(t, r, http.MethodGet, "/api/v1/admin/email-log", "tok-user"); w.Code != http.StatusForbidden || store.calls != 0 {
		t.Fatalf("staff GET = %d (store calls %d); want 403 and no store call", w.Code, store.calls)
	}
	w := jobsReq(t, r, http.MethodGet, "/api/v1/admin/email-log?tenantId=t2", "tok-admin")
	if w.Code != http.StatusOK {
		t.Fatalf("tenant-admin GET = %d %s; want 200 (adminGated, not builderGated)", w.Code, w.Body)
	}
	if store.lastTenant != "t1" {
		t.Fatalf("store queried %q; want the verified t1", store.lastTenant)
	}
	var body struct {
		Data       []map[string]any `json:"data"`
		Pagination map[string]any   `json:"pagination"`
		Meta       map[string]any   `json:"meta"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) != 1 || body.Data[0]["id"] != "log-1" {
		t.Fatalf("data = %v; want exactly t1's row", body.Data)
	}
	for _, k := range []string{"createdAt", "source", "status", "recipientMasked", "subjectTemplate", "workflowName", "attempts", "lastError", "entityName", "recordId"} {
		if _, ok := body.Data[0][k]; !ok {
			t.Errorf("row lacks %q: %v", k, body.Data[0])
		}
	}
	if body.Meta["mailConfigured"] != true || body.Meta["queueEnabled"] != false {
		t.Errorf("meta = %v; want mailConfigured=true queueEnabled=false", body.Meta)
	}
	if body.Pagination["limit"] != float64(50) || body.Pagination["nextCursor"] != nil {
		t.Errorf("pagination = %v", body.Pagination)
	}
	if w := jobsReq(t, r, http.MethodGet, "/api/v1/admin/email-log", "tok-admin-t2"); store.lastTenant != "t2" || strings.Contains(w.Body.String(), "log-1") {
		t.Errorf("t2 admin saw %s (tenant %q)", w.Body, store.lastTenant)
	}
}

func TestAdminEmailLogList_FiltersAndErrors(t *testing.T) {
	cases := []struct {
		name, query string
		storeErr    error
		status      int
		code        string
		reached     bool
		wantStatus  string
	}{
		{name: "status passes through", query: "?status=failed&limit=10&after=abc", status: 200, reached: true, wantStatus: "failed"},
		{name: "bad status", query: "?status=bogus", status: 400, code: "EMAIL_LOG_FILTER_INVALID"},
		{name: "foreign cursor", query: "?after=zzz", storeErr: dynrepo.ErrInvalidCursor, status: 400, code: "INVALID_CURSOR", reached: true},
		{name: "store failure", storeErr: errors.New("db down"), status: 500, code: "EMAIL_LOG_QUERY_FAILED", reached: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &hFakeEmailLogStore{err: tc.storeErr}
			r := buildEmailLogEngine(t, services.NewEmailLogService(store))
			w := jobsReq(t, r, http.MethodGet, "/api/v1/admin/email-log"+tc.query, "tok-admin")
			if w.Code != tc.status || (store.calls > 0) != tc.reached {
				t.Fatalf("status %d reached=%v (calls %d), want %d / %v: %s", w.Code, store.calls > 0, store.calls, tc.status, tc.reached, w.Body)
			}
			if tc.code != "" && !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
				t.Errorf("body %s lacks code %s", w.Body, tc.code)
			}
			if tc.wantStatus != "" && store.lastFilter.Status != tc.wantStatus {
				t.Errorf("filter = %+v", store.lastFilter)
			}
		})
	}
}
