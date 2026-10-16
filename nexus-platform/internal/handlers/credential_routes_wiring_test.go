package handlers

// credential_routes_wiring_test.go — ../specs/050-named-credentials S1b (tasks T021): the vault is
// mounted on adminGated (customer admin, no operator flag) through the REAL RegisterAPIRoutes, with
// the dynamic record routes present and the operator tier wired like production.
//
// The two tests read as a PAIR (spec §"Duyệt S0" finding 1): before the vault existed,
// /admin/credentials fell into the record route /:entity/:id and the record ACL answered a
// non-admin 403 — so "staff ⇒ 403" alone is green without any vault. The wall only counts when the
// SAME path answers the customer admin 200/201.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

func buildCredentialWiringEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		"tok-staff":  {Subject: auth.Subject{UserID: "u-staff", TenantID: "t1", RoleID: "r1"}, TokenType: auth.TokenTypeAccess},
		"tok-cadmin": {Subject: auth.Subject{UserID: "u-adm", TenantID: "t1", RoleID: "r1", IsAdmin: true}, TokenType: auth.TokenTypeAccess},
	}}
	deny := &aclWiringChecker{} // grants nothing: a record-route fall-through answers staff 403
	svc := services.NewCredentialService(newHFakeCredentialStore(), hTestKeyring(t), false)
	r := gin.New()
	r.Use(gin.Recovery())
	RegisterAPIRoutes(r.Group("/api/v1"), ver, deny, APIHandlers{
		Record:     NewDynamicRecordHandler(nil, zap.NewNop()),
		Credential: NewAdminCredentialHandler(svc, zap.NewNop()),
	}, zap.NewNop(),
		// u-adm is NOT the operator: a builderGated mount would refuse it 403.
		WithOperatorTier(middleware.RequirePlatformOperator(&operatorRepoFake{operatorID: "u-op"}, zap.NewNop())),
	)
	return r
}

func credWiringReq(r *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

const credWiringBody = `{"name":"pay","label":"Pay","baseUrl":"https://pay.example.vn","authType":"header","secret":"tok"}`

func TestAdminCredentialRoutes_CustomerAdminAllowed(t *testing.T) {
	r := buildCredentialWiringEngine(t)
	if rec := credWiringReq(r, http.MethodPost, "/api/v1/admin/credentials", "tok-cadmin", credWiringBody); rec.Code != http.StatusCreated {
		t.Fatalf("customer admin POST /admin/credentials = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	rec := credWiringReq(r, http.MethodGet, "/api/v1/admin/credentials", "tok-cadmin", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"credentials"`) {
		t.Fatalf("customer admin GET /admin/credentials = %d, want 200 with the vault list (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestAdminCredentialRoutes_NonAdminForbidden(t *testing.T) {
	r := buildCredentialWiringEngine(t)
	for _, tc := range []struct {
		method, path, body string
		admin              int    // what the customer admin gets on the SAME path
		adminCode          string // vault error code proving it is the vault route
	}{
		{http.MethodGet, "/api/v1/admin/credentials", "", http.StatusOK, ""},
		{http.MethodPost, "/api/v1/admin/credentials", credWiringBody, http.StatusCreated, ""},
		{http.MethodGet, "/api/v1/admin/credentials/some-id", "", http.StatusNotFound, "CREDENTIAL_NOT_FOUND"},
		{http.MethodPut, "/api/v1/admin/credentials/some-id", `{"label":"x"}`, http.StatusNotFound, "CREDENTIAL_NOT_FOUND"},
		{http.MethodDelete, "/api/v1/admin/credentials/some-id", "", http.StatusNotFound, "CREDENTIAL_NOT_FOUND"},
		{http.MethodPost, "/api/v1/admin/credentials/some-id/rotate", `{"generateSecret":true}`, http.StatusNotFound, "CREDENTIAL_NOT_FOUND"},
	} {
		if rec := credWiringReq(r, tc.method, tc.path, "tok-staff", tc.body); rec.Code != http.StatusForbidden {
			t.Errorf("staff %s %s = %d, want 403", tc.method, tc.path, rec.Code)
		}
		// The pair: the SAME path must be the vault route for the customer admin, never the
		// record fall-through (which answers an admin ENTITY_NOT_FOUND / a 5xx).
		rec := credWiringReq(r, tc.method, tc.path, "tok-cadmin", tc.body)
		if rec.Code != tc.admin || !strings.Contains(rec.Body.String(), tc.adminCode) {
			t.Errorf("customer admin %s %s = %d, want %d %s — not the vault route (body=%s)",
				tc.method, tc.path, rec.Code, tc.admin, tc.adminCode, rec.Body.String())
		}
	}
}
