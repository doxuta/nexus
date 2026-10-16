package handlers

// claim_freshness_wiring_test.go — queue #14 (claim-freshness) default-build regression lock (no
// DB, no build tag): RegisterAPIRoutes with WithLiveSubject mounts the LIVE auth stage, so the
// role/admin/status a protected request runs under come from the user ROW, not from the (up to
// 15-minute-old) token claims. Wired through the REAL RegisterAPIRoutes so that if the option is
// ever dropped from the builder, the "demoted admin still passes /admin" case flips green→red.
//
// Reuses stubVerifier / claimsFor / aclWiringChecker from acl_wiring_test.go (same package).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
)

// wiringLiveUsers serves one fixed row (or error) for every lookup.
type wiringLiveUsers struct {
	user *auth.User
	err  error
}

func (w *wiringLiveUsers) GetUserByID(context.Context, string, string) (*auth.User, error) {
	return w.user, w.err
}

// buildClaimFreshnessEngine wires the real router with the ACL stage + an admin-gated resource
// (the user handler over a nil service — the gate refuses before the handler runs on the deny
// path; gin.Recovery turns a pass-through into a 500 so the process never crashes).
func buildClaimFreshnessEngine(users *wiringLiveUsers) *gin.Engine {
	gin.SetMode(gin.TestMode)
	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		"admin":  claimsFor("t1", "", true),          // token STILL says admin
		"member": claimsFor("t1", "role-old", false), // token STILL says role-old
	}}
	checker := &aclWiringChecker{allowTenant: "t1", allowRole: "role-new", allowEntity: "contacts", allowAction: "view"}
	r := gin.New()
	r.Use(gin.Recovery())
	opts := []RouteOption{}
	if users != nil {
		opts = append(opts, WithLiveSubject(users))
	}
	RegisterAPIRoutes(r.Group("/api/v1"), ver, checker, APIHandlers{
		User:   NewAdminUserHandler(nil, zap.NewNop()),
		Record: NewDynamicRecordHandler(nil, zap.NewNop()),
	}, zap.NewNop(), opts...)
	return r
}

func doClaimReq(r *gin.Engine, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// Demoted admin: token says admin, row says not → /admin/* is refused 403 on the very next request.
func TestClaimFreshnessWiring_DemotedAdminIs403OnAdminRoute(t *testing.T) {
	r := buildClaimFreshnessEngine(&wiringLiveUsers{user: &auth.User{Status: "active", IsAdmin: false}})
	rec := doClaimReq(r, http.MethodGet, "/api/v1/admin/users", "admin")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("demoted admin on /admin/users = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
}

// Control for the case above: WITHOUT the option the stale token claim still passes the admin
// gate (pre-#14 behaviour) — so the 403 above is the live re-read, not a routing artifact.
func TestClaimFreshnessWiring_WithoutOptionStaleClaimPasses(t *testing.T) {
	r := buildClaimFreshnessEngine(nil)
	rec := doClaimReq(r, http.MethodGet, "/api/v1/admin/users", "admin")
	if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
		t.Fatalf("without WithLiveSubject the admin gate refused a token-admin: %d (body=%s)", rec.Code, rec.Body.String())
	}
}

// Role change: token says role-old (no grant), row says role-new (granted) → the ACL stage
// authorizes the ROW's role and the request reaches the handler.
func TestClaimFreshnessWiring_RowRoleFeedsACLStage(t *testing.T) {
	newRole := "role-new"
	r := buildClaimFreshnessEngine(&wiringLiveUsers{user: &auth.User{Status: "active", RoleID: &newRole}})
	rec := doClaimReq(r, http.MethodGet, "/api/v1/contacts", "member")
	if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
		t.Fatalf("row role did not reach the ACL stage: %d (body=%s)", rec.Code, rec.Body.String())
	}
}

// Disabled row → 401 on a protected route, even though the token verifies fine.
func TestClaimFreshnessWiring_DisabledRowIs401(t *testing.T) {
	r := buildClaimFreshnessEngine(&wiringLiveUsers{user: &auth.User{Status: "disabled", IsAdmin: true}})
	rec := doClaimReq(r, http.MethodGet, "/api/v1/contacts", "member")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled row on protected route = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
}
