package handlers

// specs/065 lát D (T075): POST/DELETE /admin/tenants/:id/owner-tier sit behind the operator gate on
// the REAL route group - a plain tenant admin gets the byte-identical 403, an operator gets the
// documented body shape (counts + role id, never an e-mail), and the service's refusals map to
// 404 / 422.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

func otRouteRouter(svc tenantProvisioner, userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", "factory")
		c.Set("authClaims", &auth.Claims{Subject: auth.Subject{UserID: userID, TenantID: "factory", IsAdmin: true}})
		c.Next()
	})
	NewAdminTenantHandler(svc, &operatorRepoFake{operatorID: "op-1"}, zap.NewNop()).RegisterRoutes(r.Group("/api/v1"))
	return r
}

func otRouteDo(r *gin.Engine, method string) (int, string) {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, "/api/v1/admin/tenants/t-9/owner-tier", nil))
	return w.Code, w.Body.String()
}

func TestOwnerTierRoutes_OperatorGate(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		f := &fakeProvisioner{}
		if code, body := otRouteDo(otRouteRouter(f, "plain-admin"), method); code != http.StatusForbidden || f.gotTarget != "" {
			t.Fatalf("%s as plain admin = %d %s (service reached: %q), want 403 and service untouched", method, code, body, f.gotTarget)
		}
	}

	f := &fakeProvisioner{}
	code, body := otRouteDo(otRouteRouter(f, "op-1"), http.MethodPost)
	if code != http.StatusOK || !f.gotEnabled || f.gotCaller != "factory" || f.gotTarget != "t-9" {
		t.Fatalf("POST as operator = %d %s (enabled=%v caller=%q target=%q)", code, body, f.gotEnabled, f.gotCaller, f.gotTarget)
	}
	for _, want := range []string{`"converted":2`, `"skippedWithRole":1`, `"ownerRoleId":"role-1"`, `"revokeFailed":0`} {
		if !strings.Contains(body, want) {
			t.Errorf("POST body %s lacks %s", body, want)
		}
	}
	code, body = otRouteDo(otRouteRouter(f, "op-1"), http.MethodDelete)
	if code != http.StatusOK || f.gotEnabled || !strings.Contains(body, `"reverted":3`) || strings.Contains(body, "ownerRoleId") {
		t.Fatalf("DELETE as operator = %d %s (enabled=%v)", code, body, f.gotEnabled)
	}
}

func TestOwnerTierRoutes_ServiceRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"self", &services.ValidationError{Code: "TENANT_SELF_OWNER_TIER", Msg: "x"}, http.StatusUnprocessableEntity},
		{"unknown", &services.ValidationError{Code: "TENANT_NOT_FOUND", Msg: "x"}, http.StatusNotFound},
		{"boom", errors.New("boom"), http.StatusInternalServerError},
	} {
		code, body := otRouteDo(otRouteRouter(&fakeProvisioner{err: tc.err}, "op-1"), http.MethodPost)
		if code != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, code, body, tc.want)
		}
	}
}
