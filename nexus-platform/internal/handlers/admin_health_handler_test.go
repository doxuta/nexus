package handlers

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/emaillog"
	"nexus/platform/internal/inboundhook"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

type hHealthJobs struct{ tenant string }

func (f *hHealthJobs) FailedByTypeSince(_ context.Context, t string, _ time.Time) ([]jobs.JobTypeFailure, error) {
	f.tenant = t
	return nil, nil
}
func (f *hHealthJobs) StuckQueuedForTenant(context.Context, string, time.Time) (int, error) {
	return 0, nil
}

type hHealthRuns struct{}

func (hHealthRuns) FailedRunsSince(context.Context, string, time.Time, int) (int, []workflow.RuleFailure, error) {
	return 0, nil, nil
}
func (hHealthRuns) LastFailedRun(context.Context, string, string, time.Time) (string, error) {
	return "", nil
}

type hHealthMail struct{}

func (hHealthMail) FailedSummarySince(context.Context, string, time.Time, time.Time) (emaillog.FailedSummary, error) {
	return emaillog.FailedSummary{}, nil
}

type hHealthHooks struct{}

func (hHealthHooks) FailedByHookSince(context.Context, string, time.Time) ([]inboundhook.HookFailure, error) {
	return []inboundhook.HookFailure{{HookID: "h", Name: "Hook", Count: 1, LastAt: time.Now()}}, nil
}

func TestAdminHealth_GateAndTenantPin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &hHealthJobs{}
	claims := func(user, tenant string, admin bool) *auth.Claims {
		return &auth.Claims{Subject: auth.Subject{UserID: user, TenantID: tenant, RoleID: "r1", IsAdmin: admin}, TokenType: auth.TokenTypeAccess}
	}
	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		"tok-admin": claims("u-adm", "t1", true),
		"tok-user":  claims("u-usr", "t1", false),
	}}
	r := gin.New()
	svc := services.NewHealthService(store, hHealthRuns{}, hHealthMail{}, hHealthHooks{}, nil, true)
	RegisterAPIRoutes(r.Group("/api/v1"), ver, nil, APIHandlers{Health: NewAdminHealthHandler(svc, zap.NewNop())}, zap.NewNop(),
		WithOperatorTier(middleware.RequirePlatformOperator(&operatorRepoFake{operatorID: "nobody"}, zap.NewNop())))

	if w := jobsReq(t, r, http.MethodGet, "/api/v1/admin/health", "tok-user"); w.Code != http.StatusForbidden {
		t.Fatalf("staff GET = %d, want 403", w.Code)
	}
	w := jobsReq(t, r, http.MethodGet, "/api/v1/admin/health?tenantId=t2", "tok-admin")
	if w.Code != http.StatusOK || store.tenant != "t1" {
		t.Fatalf("admin GET = %d tenant %q body %s", w.Code, store.tenant, w.Body)
	}
	body := w.Body.String()
	for _, k := range []string{`"status"`, `"jobs"`, `"workflowRuns"`, `"emails"`, `"inboundHooks"`, `"doctor":{"status":"unknown"`} {
		if !strings.Contains(body, k) {
			t.Errorf("body lacks %s: %s", k, body)
		}
	}
	for _, bad := range []string{"payload", "payloadHead", "error\":"} {
		if strings.Contains(body, bad) {
			t.Errorf("body leaks %q: %s", bad, body)
		}
	}
}
