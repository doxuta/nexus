package handlers

// specs/068-tenant-backup-restore T039a — the operator aims a WRITE at another tenant through
// ?targetTenant=; the HTTP freeze middleware only sees the operator's own (factory) tenant, so
// resolveCrossTenantTarget judges the TARGET's freeze itself: after the operator check and the slug
// resolution (never revealing the freeze to an unauthorised caller), writes only (GET/HEAD pass).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
)

type targetFreeze map[string]time.Time

func (f targetFreeze) FrozenUntil(_ context.Context, id string) (time.Time, bool, error) {
	u, ok := f[id]
	return u, ok, nil
}

func freezeProbeRouter(gate operatorGate, tenants tenantSlugResolver, frozen tenantFreezeChecker, calls *int) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", "factory")
		c.Set("authClaims", &auth.Claims{Subject: auth.Subject{UserID: "op-1", TenantID: "factory", IsAdmin: true}})
		c.Next()
	})
	h := func(c *gin.Context) {
		id, ok := resolveCrossTenantTarget(c, zap.NewNop(), gate, tenants, frozen, "test", "factory", c.Query("targetTenant"))
		if !ok {
			return
		}
		*calls++ // stands for the installer / backfill being invoked
		c.JSON(http.StatusOK, gin.H{"tenant": id})
	}
	r.POST("/probe", h)
	r.GET("/probe", h)
	return r
}

func TestResolveCrossTenantTarget_FrozenTarget(t *testing.T) {
	until := time.Now().Add(5 * time.Minute)
	tenants := func() *fakeTenantResolver {
		return &fakeTenantResolver{t: t, bySlug: map[string]*auth.Tenant{
			"qa-x": {ID: "tn-x"}, "qa-y": {ID: "tn-y"}}}
	}
	tests := []struct {
		name      string
		user      *auth.User
		method    string
		slug      string
		frozen    tenantFreezeChecker
		wantCode  int
		wantCalls int
	}{
		{"write into a frozen target is refused", liveOperator(), http.MethodPost, "qa-x", targetFreeze{"tn-x": until}, 503, 0},
		{"read of a frozen target still runs", liveOperator(), http.MethodGet, "qa-x", targetFreeze{"tn-x": until}, 200, 1},
		{"unfrozen target as before", liveOperator(), http.MethodPost, "qa-y", targetFreeze{"tn-x": until}, 200, 1},
		{"nil checker keeps the old behaviour", liveOperator(), http.MethodPost, "qa-x", nil, 200, 1},
		// The freeze must not leak to a caller who is not an operator: same 403 as ever.
		{"non-operator on a frozen target still sees 403", &auth.User{ID: "op-1", IsAdmin: true, Status: "active"},
			http.MethodPost, "qa-x", targetFreeze{"tn-x": until}, 403, 0},
		{"unknown slug on freeze-enabled gate is 404 not 503", liveOperator(), http.MethodPost, "nope", targetFreeze{"tn-x": until}, 404, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			r := freezeProbeRouter(&fakeOperatorGate{t: t, user: tc.user}, tenants(), tc.frozen, &calls)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(tc.method, "/probe?targetTenant="+tc.slug, nil))
			if w.Code != tc.wantCode || calls != tc.wantCalls {
				t.Fatalf("status %d calls %d, want %d/%d (%s)", w.Code, calls, tc.wantCode, tc.wantCalls, w.Body.String())
			}
			if tc.wantCode == 503 {
				if !strings.Contains(w.Body.String(), `"code":"TENANT_MAINTENANCE"`) || w.Header().Get("Retry-After") == "" {
					t.Fatalf("503 must carry TENANT_MAINTENANCE + Retry-After: %v %s", w.Header(), w.Body.String())
				}
			}
		})
	}
}
