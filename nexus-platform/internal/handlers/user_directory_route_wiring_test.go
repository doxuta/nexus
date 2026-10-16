package handlers

// user_directory_route_wiring_test.go — unit test (no DB, no build-tag) that empirically proves
// the new static "/users/directory" segment does NOT collide with the dynamic /:entity[/:id]
// dispatcher mounted on the SAME protected group, in EITHER direction:
//
//  1. GET /api/v1/users/directory reaches UserDirectoryHandler (200, its own envelope) — not the
//     record dispatcher's GET /:entity/:id (entity="users", id="directory").
//  2. GET /api/v1/leads/42 (an ordinary two-segment dynamic-record path) still reaches
//     DynamicRecordHandler, unaffected by the new route.
//
// Mirrors the proven pattern in import_route_wiring_test.go: a nil-service DynamicRecordHandler
// panics on any real dispatch, recovered by gin.Recovery() into a 500 — so "500, not my handler's
// shape" is the decisive signal for (2), and "200, my shape" is decisive for (1).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// buildUserDirectoryWiringEngine wires UserDirectory + Record (nil service) through the REAL
// RegisterAPIRoutes, using a stub verifier so no JWT signing is needed.
func buildUserDirectoryWiringEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		"tok": claimsFor("t1", "role-1", false), // non-admin — this endpoint must not need admin
	}}

	repo := &fakeUserDirectoryRepo{rows: []auth.DirectoryUser{{ID: "u1", FullName: "A", Email: "a@x.test"}}, total: 1}
	dirH := NewUserDirectoryHandler(services.NewUserDirectoryService(repo), zap.NewNop())

	// nil RecordService: any REAL dispatch into it panics on nil-deref; gin.Recovery() turns that
	// into a 500. That is the fixture's whole point — proving a request landed there at all.
	recordH := NewDynamicRecordHandler(nil, zap.NewNop())

	r := gin.New()
	r.Use(gin.Recovery())
	api := r.Group("/api/v1")
	RegisterAPIRoutes(api, ver, nil, APIHandlers{
		UserDirectory: dirH,
		Record:        recordH,
	}, zap.NewNop())
	return r
}

func TestUserDirectoryRouteWiring_NoRegistrationPanic(t *testing.T) {
	buildUserDirectoryWiringEngine(t)
}

// TestUserDirectoryRouteWiring_UsersDirectoryHitsDirectoryHandler proves GET /users/directory
// resolves to UserDirectoryHandler, not DynamicRecordHandler's GET /:entity/:id (entity="users",
// id="directory") — which is nil-serviced and would panic->500 if it were dispatched there instead.
func TestUserDirectoryRouteWiring_UsersDirectoryHitsDirectoryHandler(t *testing.T) {
	r := buildUserDirectoryWiringEngine(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/directory", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /users/directory status = %d, want 200 (must reach UserDirectoryHandler, not the nil-serviced record dispatcher); body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"fullName"`) {
		t.Fatalf("body = %s, want the directory envelope (fullName key present)", rec.Body.String())
	}
}

// TestUserDirectoryRouteWiring_OrdinaryEntityRouteUnaffected proves an ordinary dynamic-record
// path (GET /:entity/:id) still reaches DynamicRecordHandler — the new static route must not
// shadow it. The nil RecordService panics on dispatch, recovered to 500 by gin.Recovery(): that
// 500 (not a 404 or the directory's 200 shape) is proof the request WAS routed to the record
// handler, exactly like TestImportRoutWiring_PostEntityRouteNotImport already establishes for the
// sibling /:entity/import route.
func TestUserDirectoryRouteWiring_OrdinaryEntityRouteUnaffected(t *testing.T) {
	r := buildUserDirectoryWiringEngine(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/leads/42", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET /:entity/:id status = %d, want 500 (nil-serviced record handler dispatch, proving the route reached it); body=%s", rec.Code, rec.Body.String())
	}
}
