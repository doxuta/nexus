package handlers

// operator_tier_wiring_test.go — EASE Nước 6 (../specs/034-customer-admin-tier/spec.md §2): proves the
// two-tier /admin split is WIRED, not just designed. No DB, no build tag.
//
// Assertion mechanics (same panic-rationale as import_route_wiring_test.go): handlers are
// built with nil services, so a request that PASSES the gates reaches the handler and panics
// on the nil deref → gin.Recovery turns it into a 500. The distinguishing signal is therefore
// 403 (operator gate blocked BEFORE the handler) vs 500 (gate let it through). Ugly on
// purpose — it needs zero service fakes and cannot false-pass.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
)

// operatorRepoFake satisfies middleware.RequirePlatformOperator's repo parameter: exactly one
// user id carries the operator flag.
type operatorRepoFake struct{ operatorID string }

func (f *operatorRepoFake) GetUserByID(_ context.Context, tenantID, id string) (*auth.User, error) {
	return &auth.User{ID: id, TenantID: tenantID, IsAdmin: true, IsPlatformOperator: id == f.operatorID, Status: "active"}, nil
}

func buildOperatorTierEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	adminClaims := &auth.Claims{Subject: auth.Subject{UserID: "u-adm", TenantID: "t1", RoleID: "r1", IsAdmin: true}, TokenType: auth.TokenTypeAccess}
	opClaims := &auth.Claims{Subject: auth.Subject{UserID: "u-op", TenantID: "t1", RoleID: "r1", IsAdmin: true}, TokenType: auth.TokenTypeAccess}
	ver := &stubVerifier{byToken: map[string]*auth.Claims{"tok-admin": adminClaims, "tok-op": opClaims}}

	r := gin.New()
	r.Use(gin.Recovery()) // nil-svc handler panics become 500 (the "gate passed" signal)
	api := r.Group("/api/v1")
	RegisterAPIRoutes(api, ver, nil, APIHandlers{
		Admin: NewAdminEntityHandler(nil, nil, zap.NewNop()), // BUILDER surface → operator tier
		User:  NewAdminUserHandler(nil, zap.NewNop()),        // GOVERNANCE surface → stays tenant-admin
	}, zap.NewNop(),
		WithOperatorTier(middleware.RequirePlatformOperator(&operatorRepoFake{operatorID: "u-op"}, zap.NewNop())),
	)
	return r
}

func opTierReq(t *testing.T, r *gin.Engine, method, path, token string) int {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

func TestOperatorTier_BuilderRoutesRequireOperator(t *testing.T) {
	r := buildOperatorTierEngine(t)

	// Plain tenant-admin: the builder surface is GONE (403 from the operator gate, handler
	// never runs — no panic despite the nil service).
	if code := opTierReq(t, r, http.MethodPost, "/api/v1/admin/entities", "tok-admin"); code != http.StatusForbidden {
		t.Fatalf("plain admin on builder route = %d; want 403 (operator tier)", code)
	}

	// Operator: the gate passes and the nil-svc handler is REACHED (500 via Recovery) —
	// anything but 403 proves the tier admitted the operator.
	if code := opTierReq(t, r, http.MethodPost, "/api/v1/admin/entities", "tok-op"); code == http.StatusForbidden {
		t.Fatalf("operator on builder route = 403; the tier must admit operators")
	}
}

func TestOperatorTier_GovernanceStaysTenantAdmin(t *testing.T) {
	r := buildOperatorTierEngine(t)

	// Plain tenant-admin keeps governance: /admin/users must NOT be operator-blocked. The
	// nil-svc handler is reached (non-403) — the regression this pins is a future "move
	// everything behind operator" mistake that would lock customers out of their own users.
	if code := opTierReq(t, r, http.MethodGet, "/api/v1/admin/users", "tok-admin"); code == http.StatusForbidden {
		t.Fatalf("plain admin on governance route = 403; governance must stay tenant-admin")
	}
}

func TestOperatorTier_AbsentOptionKeepsLegacySingleTier(t *testing.T) {
	// Without WithOperatorTier every /admin route keeps the coarse RequireAdmin gate — the
	// degrade-safe default that keeps every pre-slice test harness meaningful.
	gin.SetMode(gin.TestMode)
	adminClaims := &auth.Claims{Subject: auth.Subject{UserID: "u-adm", TenantID: "t1", RoleID: "r1", IsAdmin: true}, TokenType: auth.TokenTypeAccess}
	ver := &stubVerifier{byToken: map[string]*auth.Claims{"tok-admin": adminClaims}}
	r := gin.New()
	r.Use(gin.Recovery())
	api := r.Group("/api/v1")
	RegisterAPIRoutes(api, ver, nil, APIHandlers{Admin: NewAdminEntityHandler(nil, nil, zap.NewNop())}, zap.NewNop())
	if code := opTierReq(t, r, http.MethodPost, "/api/v1/admin/entities", "tok-admin"); code == http.StatusForbidden {
		t.Fatalf("legacy (no option) admin on builder route = 403; want single-tier pass-through")
	}
}
