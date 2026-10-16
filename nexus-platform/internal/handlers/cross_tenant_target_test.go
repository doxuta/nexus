package handlers

// The cross-tenant gate is now ONE body shared by three surfaces (install, install-diff, ACL
// backfill). That is the point — three copies of a security rule are three things that drift — so
// the properties worth pinning are the ones a copy would most easily lose.
//
// ORDER is the subtle one: the operator check must run BEFORE the slug is resolved. Reversed, a
// tenant admin could probe which customer slugs exist by comparing 404 against 403, which is
// exactly the enumeration the uniform refusals are built to prevent.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
)

func liveOperator() *auth.User {
	return &auth.User{ID: "op-1", IsAdmin: true, IsPlatformOperator: true, Status: "active"}
}

func crossTenantRouter(gate operatorGate, tenants tenantSlugResolver, sink *string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", "factory")
		c.Set("authClaims", &auth.Claims{Subject: auth.Subject{
			UserID: "op-1", TenantID: "factory", IsAdmin: true,
		}})
		c.Next()
	})
	r.POST("/probe", func(c *gin.Context) {
		id, ok := resolveCrossTenantTarget(c, zap.NewNop(), gate, tenants, nil, "test", "factory", c.Query("targetTenant"))
		if !ok {
			return
		}
		*sink = id
		c.JSON(http.StatusOK, gin.H{"tenant": id})
	})
	return r
}

func doProbe(r *gin.Engine, slug string) int {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/probe?targetTenant="+slug, nil))
	return w.Code
}

func TestResolveCrossTenantTarget_OperatorReachesTheTarget(t *testing.T) {
	var got string
	gate := &fakeOperatorGate{t: t, user: liveOperator()}
	tenants := &fakeTenantResolver{t: t, bySlug: map[string]*auth.Tenant{"khachb": {ID: "tn-khachb"}}}

	if code := doProbe(crossTenantRouter(gate, tenants, &got), "khachb"); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got != "tn-khachb" {
		t.Errorf("resolved tenant = %q; want tn-khachb — the write would have landed in the wrong tenant", got)
	}
}

// The anti-enumeration property: a NON-operator is refused BEFORE the slug is looked at, so 404 and
// 403 cannot be diffed to learn which customers exist.
func TestResolveCrossTenantTarget_NonOperatorNeverReachesTheResolver(t *testing.T) {
	var got string
	gate := &fakeOperatorGate{t: t, user: &auth.User{ID: "op-1", IsAdmin: true, IsPlatformOperator: false, Status: "active"}}
	tenants := &fakeTenantResolver{t: t, failIfCalled: true} // trips the test if consulted

	if code := doProbe(crossTenantRouter(gate, tenants, &got), "khachb"); code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", code)
	}
	if got != "" {
		t.Error("a non-operator resolved a target tenant")
	}
}

func TestResolveCrossTenantTarget_UnknownAndInactiveAreOneAnswer(t *testing.T) {
	var got string
	gate := &fakeOperatorGate{t: t, user: liveOperator()}
	tenants := &fakeTenantResolver{t: t, bySlug: map[string]*auth.Tenant{}} // every slug is unknown

	if code := doProbe(crossTenantRouter(gate, tenants, &got), "khong-co-that"); code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

// Unwired deps must fail CLOSED — an operator feature that is not configured refuses everyone
// rather than falling back to the caller's own tenant, which would silently write in the wrong place.
func TestResolveCrossTenantTarget_UnwiredFailsClosed(t *testing.T) {
	var got string
	if code := doProbe(crossTenantRouter(nil, nil, &got), "khachb"); code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when the feature is unwired", code)
	}
	if got != "" {
		t.Error("an unwired gate still resolved a tenant")
	}
}

// A caller whose user row vanished (deleted since the token was minted) gets the uniform 403, not a
// 500 — a distinguishable error is itself a signal.
func TestResolveCrossTenantTarget_DeletedCallerGetsTheUniform403(t *testing.T) {
	var got string
	gate := &fakeOperatorGate{t: t, err: auth.ErrUserNotFound}
	tenants := &fakeTenantResolver{t: t, failIfCalled: true}

	if code := doProbe(crossTenantRouter(gate, tenants, &got), "khachb"); code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", code)
	}
}
