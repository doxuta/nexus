package handlers

// record_purge_route_test.go — specs/056 D1: POST /:entity/:id/purge is OPERATOR-ONLY and
// FAIL-CLOSED. No DB, no build tag; same 403-vs-500 mechanics as operator_tier_wiring_test.go
// (nil RecordService ⇒ a request that passes every gate panics in the handler ⇒ Recovery 500).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
)

func buildPurgeRouteEngine(t *testing.T, withOperatorTier bool) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		"tok-member": claimsFor("t1", "role-1", false),
		"tok-admin":  {Subject: auth.Subject{UserID: "u-adm", TenantID: "t1", RoleID: "r1", IsAdmin: true}, TokenType: auth.TokenTypeAccess},
		"tok-op":     {Subject: auth.Subject{UserID: "u-op", TenantID: "t1", RoleID: "r1", IsAdmin: true}, TokenType: auth.TokenTypeAccess},
	}}
	var opts []RouteOption
	if withOperatorTier {
		opts = append(opts, WithOperatorTier(middleware.RequirePlatformOperator(&operatorRepoFake{operatorID: "u-op"}, zap.NewNop())))
	}
	r := gin.New()
	r.Use(gin.Recovery())
	// A checker that grants EVERYTHING to the member: the purge wall must not be the entity ACL
	// (a customer role with the delete grant is still not an operator).
	checker := &aclWiringChecker{allowTenant: "t1", allowRole: "role-1", allowEntity: "contacts", allowAction: "delete"}
	RegisterAPIRoutes(r.Group("/api/v1"), ver, checker, APIHandlers{
		Record: NewDynamicRecordHandler(nil, zap.NewNop()),
	}, zap.NewNop(), opts...)
	return r
}

func purgeReq(r *gin.Engine, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/contacts/rec-1/purge", strings.NewReader(`{"dryRun":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestPurgeRoute_OperatorOnly(t *testing.T) {
	r := buildPurgeRouteEngine(t, true)
	tests := []struct {
		name  string
		token string
		want  int // 403 = refused before the handler; 500 = handler reached (nil svc)
	}{
		{"staff with the delete grant", "tok-member", http.StatusForbidden},
		{"customer admin (not operator)", "tok-admin", http.StatusForbidden},
		{"platform operator", "tok-op", http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if w := purgeReq(r, tt.token); w.Code != tt.want {
				t.Fatalf("status = %d; want %d (body=%s)", w.Code, tt.want, w.Body.String())
			}
		})
	}
	// The two refusals are byte-identical (anti-enumeration: RequireAdmin and the operator gate
	// share one body), so a customer admin cannot tell "operator-only" from any other wall.
	if a, b := purgeReq(r, "tok-member").Body.String(), purgeReq(r, "tok-admin").Body.String(); a != b {
		t.Fatalf("refusal bodies differ:\n member=%s\n admin=%s", a, b)
	}
}

// Fail-closed: a server built without the operator tier does not have the route at all — it must
// never fall back to "any admin may purge".
func TestPurgeRoute_AbsentWithoutOperatorTier(t *testing.T) {
	r := buildPurgeRouteEngine(t, false)
	for _, tok := range []string{"tok-admin", "tok-op"} {
		if w := purgeReq(r, tok); w.Code != http.StatusNotFound {
			t.Fatalf("%s without operator tier: status = %d; want 404 (route not registered)", tok, w.Code)
		}
	}
}
