package handlers

// admin_publish_handler_test.go — specs/063-draft-publish T031. Routes sit on builderGated; a
// non-builder gets the SAME 403 bytes as every other /admin route; boundary input is refused before
// any query; error codes map to the documented HTTP statuses.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// Every /admin/publish route is registered, and each one is behind RequireAdmin AND the operator tier.
func TestAdminPublishRoutes_AreBuilderGated(t *testing.T) {
	staff := &auth.Claims{Subject: auth.Subject{UserID: "s", TenantID: "t1"}, TokenType: auth.TokenTypeAccess}
	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		"tok-admin": {Subject: auth.Subject{UserID: "a", TenantID: "t1", IsAdmin: true}, TokenType: auth.TokenTypeAccess},
		"tok-staff": staff,
	}}
	gin.SetMode(gin.TestMode)
	operatorRefusals := 0
	r := gin.New()
	pass := func(c *gin.Context) { c.Next() }
	RegisterAPIRoutes(r.Group("/api/v1"), ver, nil, allHandlers(t), zap.NewNop(),
		WithLiveSubject(&stubLiveUsers{}),
		WithSessionGuard(neverEnded{}),
		// An operator tier that refuses everyone: a route reaching it is behind the tier.
		WithOperatorTier(func(c *gin.Context) {
			operatorRefusals++
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"code": "OPERATOR_TIER"})
		}),
		WithDataRateLimiter(pass), WithAuthRateLimiter(pass), WithSSORateLimiter(pass), WithAssistantRateLimiter(pass),
	)
	var routes []gin.RouteInfo
	for _, rt := range r.Routes() {
		if strings.HasPrefix(rt.Path, "/api/v1/admin/publish") {
			routes = append(routes, rt)
		}
	}
	want := map[string]bool{
		"GET /api/v1/admin/publish/state": true, "GET /api/v1/admin/publish/changes": true,
		"GET /api/v1/admin/publish/versions": true, "GET /api/v1/admin/publish/versions/:id": true, "POST /api/v1/admin/publish/baseline": true,
		"POST /api/v1/admin/publish": true, "DELETE /api/v1/admin/publish/changes/:id": true, "DELETE /api/v1/admin/publish/changes": true,
		"POST /api/v1/admin/publish/versions/:id/retry-schema": true,                                                           // S7-9
		"GET /api/v1/admin/publish/versions/:id/rollback-plan": true, "POST /api/v1/admin/publish/versions/:id/rollback": true, // S7-7
		"POST /api/v1/admin/publish/versions/:id/revert": true, // S7-7b
	}
	if len(routes) != len(want) {
		t.Fatalf("registered publish routes = %d, want %d", len(routes), len(want))
	}
	for _, rt := range routes {
		if !want[rt.Method+" "+rt.Path] {
			t.Errorf("unexpected route %s %s", rt.Method, rt.Path)
		}
		path := strings.Replace(rt.Path, ":id", "00000000-0000-0000-0000-000000000001", 1)
		for tok, wantBody := range map[string]string{"tok-admin": "OPERATOR_TIER", "tok-staff": "FORBIDDEN"} {
			req := httptest.NewRequest(rt.Method, path, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer "+tok)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), wantBody) {
				t.Errorf("%s %s with %s: %d %s, want 403 %s (admin → operator tier, staff → RequireAdmin)", rt.Method, rt.Path, tok, w.Code, w.Body.String(), wantBody)
			}
		}
	}
	if operatorRefusals != len(routes) {
		t.Errorf("operator tier ran %d times, want once per route (%d)", operatorRefusals, len(routes))
	}
}

type stubLiveUsers struct{}

func (stubLiveUsers) GetUserByID(_ context.Context, tenantID, id string) (*auth.User, error) {
	admin := id == "a"
	return &auth.User{ID: id, TenantID: tenantID, Status: "active", IsAdmin: admin}, nil
}

// publishEngine serves the handler with a service that has NO database: everything asserted below
// must be decided before the first query.
func publishEngine(callerOf func(c *gin.Context) metaversion.Caller) *gin.Engine {
	gin.SetMode(gin.TestMode)
	svc := services.NewPublishService(nil, nil, nil, metaversion.NewRegistry(services.NewWorkflowDocAdapter()), services.NewDraftGate(nil), nil, nil)
	h := NewAdminPublishHandler(svc, zap.NewNop())
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", "t1")
		c.Request = c.Request.WithContext(metaversion.WithCaller(c.Request.Context(), callerOf(c)))
		c.Next()
	})
	h.RegisterRoutes(&r.RouterGroup)
	return r
}

// The 403 for a non-builder is byte-identical with middleware.RequireAdmin's (anti-enumeration).
func TestAdminPublish_NonBuilderGets403IdenticalToRequireAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ref := gin.New()
	ref.Use(middleware.RequireAdmin(zap.NewNop()))
	ref.GET("/x", func(c *gin.Context) { c.Status(200) })
	rw := httptest.NewRecorder()
	ref.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rw.Code != http.StatusForbidden {
		t.Fatalf("reference RequireAdmin = %d", rw.Code)
	}

	r := publishEngine(func(*gin.Context) metaversion.Caller { return metaversion.Caller{UserID: "u", TenantID: "t1"} }) // legacy admin / staff: not a builder
	for _, rt := range []struct{ method, path string }{
		{"GET", "/admin/publish/state"}, {"GET", "/admin/publish/changes"}, {"POST", "/admin/publish/baseline"},
		{"POST", "/admin/publish"}, {"DELETE", "/admin/publish/changes/00000000-0000-0000-0000-000000000001"}, {"DELETE", "/admin/publish/changes"},
		{"POST", "/admin/publish/versions/00000000-0000-0000-0000-000000000001/retry-schema"},
	} {
		req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(`{"changesetHash":"`+strings.Repeat("a", 64)+`"}`))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden || w.Body.String() != rw.Body.String() {
			t.Errorf("%s %s: %d %q, want 403 with RequireAdmin's exact body %q", rt.method, rt.path, w.Code, w.Body.String(), rw.Body.String())
		}
	}
}

// FR-080 / boundary: bad input is 422 INVALID_BODY / 404, never a 500, and before any query.
func TestAdminPublish_BoundaryInput(t *testing.T) {
	r := publishEngine(func(*gin.Context) metaversion.Caller {
		return metaversion.Caller{UserID: "b", TenantID: "t1", IsBuilder: true}
	})
	good := strings.Repeat("a", 64)
	cases := []struct {
		name, method, path, body string
		wantStatus               int
		wantCode                 string
	}{
		{"note longer than 500 characters", "POST", "/admin/publish", `{"changesetHash":"` + good + `","note":"` + strings.Repeat("x", 501) + `"}`, 422, "INVALID_BODY"},
		{"hash not 64 hex", "POST", "/admin/publish", `{"changesetHash":"nope"}`, 422, "INVALID_BODY"},
		{"hash missing", "POST", "/admin/publish", `{}`, 422, "INVALID_BODY"},
		{"body not JSON", "POST", "/admin/publish", `{`, 400, "INVALID_BODY"},
		{"baseline note too long", "POST", "/admin/publish/baseline", `{"note":"` + strings.Repeat("x", 501) + `"}`, 422, "INVALID_BODY"},
		{"discard-all without a hash", "DELETE", "/admin/publish/changes", ``, 422, "INVALID_BODY"},
		{"discard-all with a short hash", "DELETE", "/admin/publish/changes?changesetHash=abc", ``, 422, "INVALID_BODY"},
		{"change id that is not a UUID", "DELETE", "/admin/publish/changes/not-a-uuid", ``, 404, "DRAFT_NOT_FOUND"},
		{"retry-schema version id that is not a UUID", "POST", "/admin/publish/versions/not-a-uuid/retry-schema", ``, 404, "VERSION_NOT_FOUND"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tc.wantStatus || !strings.Contains(w.Body.String(), `"code":"`+tc.wantCode+`"`) {
				t.Fatalf("%s %s: %d %s, want %d %s", tc.method, tc.path, w.Code, w.Body.String(), tc.wantStatus, tc.wantCode)
			}
		})
	}
}

// The documented status of each publish error code (plan "API contracts").
func TestPublishErrorCodeStatuses(t *testing.T) {
	for code, want := range map[string]int{
		"ALREADY_DELIVERED": 409, "NOT_DELIVERED": 409, "DRAFTS_CHANGED": 409, "PUBLISH_CONFLICT": 409, "NOTHING_TO_PUBLISH": 409,
		"PUBLISH_BUSY": 409, "PROJECT_DELIVERED_INSTALL_BLOCKED": 409,
		"PUBLISH_INVALID": 422, "INVALID_BODY": 422,
		"DRAFT_NOT_FOUND": 404, "FORBIDDEN": 403,
		"DRAFTS_PENDING": 409, "HEAD_MOVED": 409, "PLAN_CHANGED": 409, "ROLLBACK_NAME_TAKEN": 409, "ROLLBACK_NO_SNAPSHOT": 422, "ROLLBACK_USE_REVERT": 422, // S7-7
		"VERSION_NOT_FOUND": 404, "SCHEMA_RETRY_NOT_NEEDED": 409, "FIELD_PENDING_DELETE": 409, // S7-9
	} {
		if got := statusForCode(code); got != want {
			t.Errorf("statusForCode(%s) = %d, want %d", code, got, want)
		}
	}
}
