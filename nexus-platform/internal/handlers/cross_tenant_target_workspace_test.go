package handlers

// specs/062-customer-workspace T011: a builder workspace-session token may NEVER aim an operation
// at another tenant. Its Subject's tenant is the customer, so the operator gate would be asked
// about the wrong tenant — and a session that could hop onwards would turn one opened customer
// into every customer. The refusal comes BEFORE opGate and the slug resolver are touched (same
// order that keeps a home non-operator from probing slugs), with the same 403 body.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
)

func TestResolveCrossTenantTarget_WorkspaceSessionTokenIsRefusedFirst(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gate := &fakeOperatorGate{t: t, user: liveOperator(), failIfCalled: true}
	tenants := &fakeTenantResolver{t: t, failIfCalled: true}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", "customer-1")
		c.Set("authClaims", &auth.Claims{Subject: auth.Subject{
			UserID: "builder-1", TenantID: "customer-1", IsAdmin: true, IsPlatformOperator: true,
			ActorUserID: "builder-1", ActorTenantID: "factory", WorkspaceSessionID: "ws-1",
		}})
		c.Next()
	})
	reached := false
	r.POST("/probe", func(c *gin.Context) {
		if _, ok := resolveCrossTenantTarget(c, zap.NewNop(), gate, tenants, nil, "test", "customer-1", c.Query("targetTenant")); !ok {
			return
		}
		reached = true
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/probe?targetTenant=khachb", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if want := `{"code":"FORBIDDEN","error":"Yêu cầu quyền quản trị viên"}`; w.Body.String() != want {
		t.Errorf("body = %s, want byte-identical to RequireAdmin: %s", w.Body.String(), want)
	}
	if reached {
		t.Error("a workspace session token resolved a cross-tenant target")
	}
	if gate.calls != 0 || tenants.calls != 0 {
		t.Errorf("opGate calls=%d resolver calls=%d, want 0 and 0 (the refusal must come first)", gate.calls, tenants.calls)
	}
}
