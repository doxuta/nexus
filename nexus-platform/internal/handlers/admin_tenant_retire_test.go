package handlers

// DELETE /admin/tenants/:id status mapping. The end-to-end behaviour (login dies, webhooks die,
// the operator gate refuses a non-operator) is measured against the live engine; what is easy to
// get wrong in code review is which refusal becomes which HTTP status, so that is what this pins.
//
// TENANT_SELF_RETIRE in particular must NOT be a 403: it is not a permission problem. The caller
// is a platform operator and is fully entitled to retire workspaces — just not the one they are
// standing in, because that locks every operator out of the factory tenant and out of this very
// endpoint, leaving only SQL, which is the hole this route exists to close.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

type fakeProvisioner struct {
	err        error
	gotCaller  string
	gotTarget  string
	retireCall int

	gotEnabled bool // SetOwnerTier capture

	// Purge capture — the handler's job is to default dryRun to TRUE when the key is absent,
	// so the fake records what actually reached the service.
	purgeCall    int
	gotActor     string
	gotConfirm   string
	gotDryRun    bool
	purgeReport  *services.PurgeReport
	purgeErrOnly error
}

func (f *fakeProvisioner) Provision(context.Context, services.ProvisionTenantInput) (*services.ProvisionedTenant, error) {
	return nil, nil
}

func (f *fakeProvisioner) ListTenants(context.Context) ([]services.TenantListItem, error) {
	return nil, nil
}

func (f *fakeProvisioner) RetireTenant(_ context.Context, callerTenantID, tenantID string) error {
	f.retireCall++
	f.gotCaller, f.gotTarget = callerTenantID, tenantID
	return f.err
}

func (f *fakeProvisioner) SetOwnerTier(_ context.Context, callerTenantID, targetTenantID string, enabled bool) (*services.OwnerTierResult, error) {
	f.gotCaller, f.gotTarget, f.gotEnabled = callerTenantID, targetTenantID, enabled
	if f.err != nil {
		return nil, f.err
	}
	return &services.OwnerTierResult{Converted: 2, SkippedWithRole: 1, Reverted: 3, OwnerRoleID: "role-1", RevokeFailed: 0}, nil
}

func (f *fakeProvisioner) PurgeTenant(_ context.Context, callerTenantID, actorUserID, tenantID, confirmSlug string, dryRun bool) (*services.PurgeReport, error) {
	f.purgeCall++
	f.gotCaller, f.gotActor, f.gotTarget = callerTenantID, actorUserID, tenantID
	f.gotConfirm, f.gotDryRun = confirmSlug, dryRun
	if f.purgeErrOnly != nil {
		return nil, f.purgeErrOnly
	}
	if f.purgeReport != nil {
		return f.purgeReport, nil
	}
	return &services.PurgeReport{TenantID: tenantID, Slug: confirmSlug, DryRun: dryRun}, nil
}

// retireRouter mounts Retire directly with a verified operator claim. The operator middleware is
// exercised for real by the live run and by the tenant-isolation integration suite; here the
// subject is injected so the status mapping is what is under test.
func retireRouter(svc tenantProvisioner) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", "factory-tenant")
		c.Set("authClaims", &auth.Claims{Subject: auth.Subject{
			UserID: "op-1", TenantID: "factory-tenant", IsAdmin: true,
		}})
		c.Next()
	})
	h := NewAdminTenantHandler(svc, nil, zap.NewNop())
	r.DELETE("/api/v1/admin/tenants/:id", h.Retire)
	return r
}

func doRetire(r *gin.Engine, id string) (int, map[string]any) {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/v1/admin/tenants/"+id, nil))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func TestRetireTenant_StatusMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
		code string
	}{
		{"success", nil, http.StatusNoContent, ""},
		{
			"self-retire is a foot-gun, not a permission problem",
			&services.ValidationError{Code: "TENANT_SELF_RETIRE", Msg: "không thể xoá chính workspace bạn đang đứng"},
			http.StatusUnprocessableEntity, "TENANT_SELF_RETIRE",
		},
		{
			"unknown or already retired",
			&services.ValidationError{Code: "TENANT_NOT_FOUND", Msg: "không tìm thấy workspace"},
			http.StatusNotFound, "TENANT_NOT_FOUND",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doRetire(retireRouter(&fakeProvisioner{err: tc.err}), "target-1")
			if code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", code, tc.want, body)
			}
			if tc.code != "" && body["code"] != tc.code {
				t.Errorf("code = %v, want %s", body["code"], tc.code)
			}
		})
	}
}

// TestRetireTenant_PassesTheCallersOwnTenant: the service decides the self-retire refusal, so the
// handler must hand it the CALLER's tenant from the verified claim. If this ever came from a
// header or body, the guard could be sidestepped by lying about where you are standing.
func TestRetireTenant_PassesTheCallersOwnTenant(t *testing.T) {
	svc := &fakeProvisioner{}
	if code, _ := doRetire(retireRouter(svc), "customer-9"); code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", code)
	}
	if svc.retireCall != 1 {
		t.Fatalf("service called %d times, want once", svc.retireCall)
	}
	if svc.gotCaller != "factory-tenant" {
		t.Errorf("caller tenant = %q, want the verified claim's tenant", svc.gotCaller)
	}
	if svc.gotTarget != "customer-9" {
		t.Errorf("target = %q, want the path param", svc.gotTarget)
	}
}
