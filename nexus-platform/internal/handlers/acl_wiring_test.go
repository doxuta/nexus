package handlers

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

// stubVerifier is a TokenVerifier that maps a fixed bearer string to a pre-built claim set, so
// the ACL chain can be exercised through the REAL middleware (AuthMiddleware → RequireTenant →
// RequireACL) without minting real JWTs. The token string selects which principal is presented.
type stubVerifier struct {
	byToken map[string]*auth.Claims
}

func (s *stubVerifier) VerifyAccessToken(tok string) (*auth.Claims, error) {
	if c, ok := s.byToken[tok]; ok {
		return c, nil
	}
	return nil, auth.ErrTokenInvalid
}

func claimsFor(tenantID, roleID string, isAdmin bool) *auth.Claims {
	return &auth.Claims{
		Subject:   auth.Subject{UserID: "u", TenantID: tenantID, RoleID: roleID, IsAdmin: isAdmin},
		TokenType: auth.TokenTypeAccess,
	}
}

// aclWiringChecker is a fake ACLChecker that allows exactly one (tenant, role, entity, action)
// tuple and denies everything else — enough to prove the middleware is mounted in the right chain
// position and consulted with arguments derived from the verified claim and the request.
type aclWiringChecker struct {
	allowTenant, allowRole, allowEntity, allowAction string
}

func (c *aclWiringChecker) Authorize(_ context.Context, tenantID, roleID, entity, action string) (bool, string, error) {
	allowed := tenantID == c.allowTenant && roleID == c.allowRole &&
		entity == c.allowEntity && action == c.allowAction
	if allowed {
		// The wiring tests only care about allow/deny; return a benign "all" level.
		return true, "all", nil
	}
	return false, "", nil
}

// buildACLChainEngine reproduces the production protected-chain order from RegisterAPIRoutes
// (Auth → Tenant → ACL → Business Logic, spec §4) onto a /:entity route whose business handler
// simply returns 200. A request that the ACL stage allows reaches the 200 handler; a refused one
// is aborted with 403 before it — so the handler running (200) vs not (403) IS the ACL decision.
func buildACLChainEngine(checker ACLChecker) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		"member": claimsFor("t1", "role-1", false),
		"admin":  claimsFor("t1", "", true),
		"other":  claimsFor("t1", "role-2", false),
	}}

	api := r.Group("/api/v1")
	protected := api.Group("")
	protected.Use(middleware.AuthMiddleware(ver, zap.NewNop()))
	protected.Use(middleware.RequireTenant(zap.NewNop()))

	recordGroup := protected.Group("")
	recordGroup.Use(middleware.RequireACL(checker, zap.NewNop()))
	business := func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) }
	recordGroup.GET("/:entity", business)
	recordGroup.POST("/:entity", business)
	recordGroup.PUT("/:entity/:id", business)
	recordGroup.DELETE("/:entity", business)
	return r
}

// doACLReq issues a request with the given bearer token and returns the recorder.
func doACLReq(r *gin.Engine, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// D2 (wired): a non-admin WITH the matching grant passes the ACL stage → reaches the handler (200).
func TestACLWiring_GrantedMemberPasses(t *testing.T) {
	checker := &aclWiringChecker{allowTenant: "t1", allowRole: "role-1", allowEntity: "contacts", allowAction: "view"}
	r := buildACLChainEngine(checker)
	rec := doACLReq(r, http.MethodGet, "/api/v1/contacts", "member")
	if rec.Code != http.StatusOK {
		t.Fatalf("granted member status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

// D2 (wired): a non-admin LACKING the grant is refused with 403 by the ACL stage.
func TestACLWiring_UngrantedMemberIs403(t *testing.T) {
	checker := &aclWiringChecker{allowTenant: "t1", allowRole: "role-1", allowEntity: "contacts", allowAction: "view"}
	r := buildACLChainEngine(checker)
	rec := doACLReq(r, http.MethodDelete, "/api/v1/contacts", "other") // role-2 has no grant
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ungranted member status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
}

// D3 (wired): an admin BYPASSES the ACL stage for an action no role grants → reaches the handler.
func TestACLWiring_AdminBypasses(t *testing.T) {
	checker := &aclWiringChecker{allowTenant: "none", allowRole: "none", allowEntity: "none", allowAction: "none"}
	r := buildACLChainEngine(checker)
	rec := doACLReq(r, http.MethodDelete, "/api/v1/contacts", "admin")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status = %d, want 200 (ACL bypass → handler) (body=%s)", rec.Code, rec.Body.String())
	}
}

// (wired): a request with NO token is refused at the Auth stage (401) before ACL.
func TestACLWiring_NoTokenIs401(t *testing.T) {
	checker := &aclWiringChecker{allowTenant: "t1", allowRole: "role-1", allowEntity: "contacts", allowAction: "view"}
	r := buildACLChainEngine(checker)
	rec := doACLReq(r, http.MethodGet, "/api/v1/contacts", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", rec.Code)
	}
}
