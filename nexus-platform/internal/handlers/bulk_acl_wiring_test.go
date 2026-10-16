package handlers

// bulk_acl_wiring_test.go — BULK-ACL-TEST: default-build regression lock (no DB, no build tag)
// proving POST /:entity/bulk sits behind the SAME module-level RequireACL gate as the rest of
// the dynamic record surface. The route is currently registered on the recordGroup inside
// RegisterAPIRoutes (router.go), so it inherits the group's RequireACL; these tests wire the
// REAL RegisterAPIRoutes (not a synthetic chain) so that if someone later splits /bulk out of
// that group — mounting it on the plain protected group without the ACL stage — the ungranted
// test below flips from 403 to a non-403 and goes red.
//
// It reuses the stubVerifier / claimsFor / aclWiringChecker fixtures from acl_wiring_test.go
// (same package): the ACL decision is exercised through the real middleware chain
// (AuthMiddleware → RequireTenant → RequireACL) without minting JWTs or touching MySQL — a
// refused request is aborted 403 BEFORE the record handler runs, so a nil RecordService is
// never dereferenced on the deny path.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
)

// buildBulkACLEngine wires the REAL RegisterAPIRoutes with the given ACL checker and a nil
// RecordService (the tests only exercise the middleware chain in front of the handler).
// gin.Recovery() is mounted so a request that legitimately PASSES the ACL stage and reaches
// the nil-service handler turns into a 500 instead of crashing the test process — mirroring
// import_route_wiring_test.go.
func buildBulkACLEngine(checker ACLChecker) *gin.Engine {
	gin.SetMode(gin.TestMode)
	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		"member": claimsFor("t1", "role-1", false),
	}}
	r := gin.New()
	r.Use(gin.Recovery())
	api := r.Group("/api/v1")
	RegisterAPIRoutes(api, ver, checker, APIHandlers{
		Record: NewDynamicRecordHandler(nil, zap.NewNop()),
	}, zap.NewNop())
	return r
}

// doBulkACLReq issues POST /api/v1/contacts/bulk with a well-formed bulk body and the given
// bearer token, returning the recorder.
func doBulkACLReq(r *gin.Engine, token string) *httptest.ResponseRecorder {
	body := bytes.NewBufferString(`{"records":[{"name":"X"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/contacts/bulk", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestBulkACLWiring_UngrantedMemberIs403Forbidden is the regression lock: a NON-admin whose
// role lacks the "create" grant on the entity (the checker allows only view on contacts) must
// be refused 403/FORBIDDEN on POST /:entity/bulk — POST maps to action "create" in the shared
// methodAction table, exactly like plain POST /:entity. If the /bulk route is ever mounted
// outside the RequireACL'd recordGroup, this request would reach the handler instead and the
// status would no longer be 403.
func TestBulkACLWiring_UngrantedMemberIs403Forbidden(t *testing.T) {
	checker := &aclWiringChecker{allowTenant: "t1", allowRole: "role-1", allowEntity: "contacts", allowAction: "view"}
	r := buildBulkACLEngine(checker)

	rec := doBulkACLReq(r, "member")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ungranted bulk create status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	var errResp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal 403 body: %v (%s)", err, rec.Body.String())
	}
	if errResp.Code != "FORBIDDEN" {
		t.Fatalf("403 code = %q, want FORBIDDEN (body=%s)", errResp.Code, rec.Body.String())
	}
}

// TestBulkACLWiring_GrantedMemberPassesACLStage is the control proving the 403 above is the
// ACL decision on the real route — not an auth or routing artifact: the SAME request with a
// role that DOES hold the "create" grant clears the ACL stage and reaches the record handler.
// The handler then panics on the intentionally-nil service (recovered by gin → 500); what
// matters is that the ACL stage no longer refuses it (any 401/403 here would mean the request
// never got past the middleware chain).
func TestBulkACLWiring_GrantedMemberPassesACLStage(t *testing.T) {
	checker := &aclWiringChecker{allowTenant: "t1", allowRole: "role-1", allowEntity: "contacts", allowAction: "create"}
	r := buildBulkACLEngine(checker)

	rec := doBulkACLReq(r, "member")
	if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
		t.Fatalf("granted bulk create was refused by the middleware chain: status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	t.Logf("granted bulk create status=%d (expected ~500 from nil service — ACL stage passed, OK)", rec.Code)
}
