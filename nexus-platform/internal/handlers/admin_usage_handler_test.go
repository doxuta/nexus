package handlers

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
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
	"nexus/platform/internal/usage"
)

type hFakeUsageStore struct {
	calls  int
	tenant string
	err    error
}

func (f *hFakeUsageStore) CountersForDay(_ context.Context, tenant string, _, _ time.Time) (map[usage.Key]int64, error) {
	f.calls++
	f.tenant = tenant
	if f.err != nil {
		return nil, f.err
	}
	return map[usage.Key]int64{{Metric: usage.WorkflowRuns}: 4}, nil
}
func (f *hFakeUsageStore) Upsert(context.Context, []usage.Row, usage.Mode) error { return nil }
func (f *hFakeUsageStore) Range(_ context.Context, tenant, _, _ string) ([]usage.DayValue, error) {
	f.tenant = tenant
	return nil, nil
}
func (f *hFakeUsageStore) LastRollupAt(context.Context, string, string) (*time.Time, error) {
	return nil, nil
}

func buildUsageEngine(t *testing.T, store *hFakeUsageStore) *gin.Engine {
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
		Usage: NewAdminUsageHandler(services.NewUsageService(store, true), zap.NewNop()),
	}, zap.NewNop(),
		// Nobody is an operator: a route mounted on builderGated would answer 403 to every token.
		WithOperatorTier(middleware.RequirePlatformOperator(&operatorRepoFake{operatorID: "nobody"}, zap.NewNop())),
	)
	return r
}

func TestAdminUsage_GateAndTenantPin(t *testing.T) {
	store := &hFakeUsageStore{}
	r := buildUsageEngine(t, store)

	if w := jobsReq(t, r, http.MethodGet, "/api/v1/admin/usage", "tok-user"); w.Code != http.StatusForbidden || store.calls != 0 {
		t.Fatalf("staff GET = %d (store calls %d); want 403 and no store call", w.Code, store.calls)
	}
	w := jobsReq(t, r, http.MethodGet, "/api/v1/admin/usage?tenantId=t2", "tok-admin")
	if w.Code != http.StatusOK {
		t.Fatalf("tenant-admin GET = %d %s; want 200 (adminGated, not builderGated)", w.Code, w.Body)
	}
	if store.tenant != "t1" {
		t.Fatalf("store queried %q; want the verified t1", store.tenant)
	}
	var body struct {
		Data struct {
			Timezone      string           `json:"timezone"`
			RollupEnabled bool             `json:"rollupEnabled"`
			Days          []map[string]any `json:"days"`
			Totals        map[string]any   `json:"totals"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data.Days) != 30 || !body.Data.RollupEnabled || body.Data.Timezone == "" {
		t.Fatalf("body = %s", w.Body)
	}
	for _, k := range []string{"day", "workflowRuns", "workflowRunsFailed", "emailsSent", "emailsFailed", "webhooksDelivered", "webhooksFailed", "hooksReceived", "hooksFailed"} {
		if _, ok := body.Data.Days[0][k]; !ok {
			t.Errorf("day lacks %q: %v", k, body.Data.Days[0])
		}
	}
	jobsReq(t, r, http.MethodGet, "/api/v1/admin/usage", "tok-admin-t2")
	if store.tenant != "t2" {
		t.Errorf("t2 admin queried tenant %q", store.tenant)
	}
}

func TestAdminUsage_Errors(t *testing.T) {
	cases := []struct {
		name, query string
		storeErr    error
		status      int
		code        string
	}{
		{name: "reversed", query: "?from=2026-10-10&to=2026-10-01", status: 400, code: "USAGE_RANGE_INVALID"},
		{name: "bad format", query: "?from=yesterday", status: 400, code: "USAGE_RANGE_INVALID"},
		{name: "future", query: "?to=2999-01-01", status: 400, code: "USAGE_RANGE_INVALID"},
		{name: "too long", query: "?from=2020-01-01", status: 400, code: "USAGE_RANGE_INVALID"},
		{name: "store failure", storeErr: errors.New("db down"), status: 500, code: "USAGE_QUERY_FAILED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := buildUsageEngine(t, &hFakeUsageStore{err: tc.storeErr})
			w := jobsReq(t, r, http.MethodGet, "/api/v1/admin/usage"+tc.query, "tok-admin")
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("got %d %s; want %d %s", w.Code, w.Body, tc.status, tc.code)
			}
		})
	}
}
