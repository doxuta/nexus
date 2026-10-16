package middleware

// tenant_freeze_test.go — specs/068-tenant-backup-restore T035. RequireTenantWritable refuses writes
// of a frozen tenant (503 TENANT_MAINTENANCE + Retry-After), lets reads through, and exempts ONLY the
// closed (method, route) list freezeExempt — never a prefix.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type fakeFreeze struct {
	until map[string]time.Time
	err   error
	known bool // with err: report the freeze as still known (FR-030a)
}

func (f *fakeFreeze) FrozenUntil(_ context.Context, tenantID string) (time.Time, bool, error) {
	u, ok := f.until[tenantID]
	if f.err != nil {
		if f.known && ok {
			return u, true, f.err
		}
		return time.Time{}, false, f.err
	}
	if !ok || !u.After(time.Now()) {
		return time.Time{}, false, nil
	}
	return u, true, nil
}

func freezeRouter(chk TenantFreezeChecker, tenant string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	api.Use(func(c *gin.Context) {
		c.Set(ctxKeyTenantID, tenant)
		c.Next()
	})
	api.Use(RequireTenantWritable(chk, api.BasePath(), zap.NewNop()))
	ok := func(c *gin.Context) { c.Status(http.StatusOK) }
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		api.Handle(m, "/:entity", ok)
	}
	// Exempt shapes (as registered in production) and look-alikes that must NOT be exempt.
	api.PUT("/auth/me/password", ok)
	api.POST("/auth/logout-all", ok)
	api.DELETE("/auth/sessions/:id", ok)
	api.POST("/auth/mfa/enroll", ok)
	api.POST("/auth/mfa/activate", ok)
	api.POST("/auth/mfa/disable", ok)
	api.POST("/assistant/ask", ok)
	api.DELETE("/admin/api-keys/:id", ok)
	api.DELETE("/admin/users/:id/sessions/:sessionId", ok)
	api.PUT("/admin/users/:id", ok)
	api.POST("/admin/inbound-hooks/:id/rotate", ok)
	api.DELETE("/admin/credentials/:id", ok)
	api.PATCH("/admin/sso/providers/:id", ok)
	api.DELETE("/admin/workspace-sessions/current", ok)
	api.DELETE("/preview-sessions/current", ok)
	api.POST("/auth/not-a-real-route", ok)
	api.POST("/admin/users", ok)
	api.POST("/admin/api-keys", ok)
	api.PUT("/admin/inbound-hooks/:id", ok)
	return r
}

func TestRequireTenantWritable_Table(t *testing.T) {
	until := time.Now().Add(5 * time.Minute)
	frozen := &fakeFreeze{until: map[string]time.Time{"t1": until}}
	lapsed := &fakeFreeze{until: map[string]time.Time{"t1": time.Now().Add(-time.Second)}}
	errNoKnown := &fakeFreeze{until: map[string]time.Time{}, err: errors.New("db down")}
	errKnown := &fakeFreeze{until: map[string]time.Time{"t1": until}, err: errors.New("db down"), known: true}

	tests := []struct {
		name         string
		chk          TenantFreezeChecker
		tenant       string
		method, path string
		want         int
	}{
		{"GET passes while frozen", frozen, "t1", "GET", "/api/v1/contacts", 200},
		{"HEAD passes while frozen", frozen, "t1", "HEAD", "/api/v1/contacts", 200},
		{"POST refused", frozen, "t1", "POST", "/api/v1/contacts", 503},
		{"PUT refused", frozen, "t1", "PUT", "/api/v1/contacts", 503},
		{"PATCH refused", frozen, "t1", "PATCH", "/api/v1/contacts", 503},
		{"DELETE refused", frozen, "t1", "DELETE", "/api/v1/contacts", 503},
		{"other tenant passes", frozen, "t2", "POST", "/api/v1/contacts", 200},
		{"lapsed passes", lapsed, "t1", "POST", "/api/v1/contacts", 200},
		{"checker error, nothing known -> pass", errNoKnown, "t1", "POST", "/api/v1/contacts", 200},
		{"checker error, freeze known -> refused", errKnown, "t1", "POST", "/api/v1/contacts", 503},
		{"exempt password change", frozen, "t1", "PUT", "/api/v1/auth/me/password", 200},
		{"exempt logout-all", frozen, "t1", "POST", "/api/v1/auth/logout-all", 200},
		{"exempt own session end", frozen, "t1", "DELETE", "/api/v1/auth/sessions/abc", 200},
		{"exempt mfa enroll", frozen, "t1", "POST", "/api/v1/auth/mfa/enroll", 200},
		{"exempt mfa activate", frozen, "t1", "POST", "/api/v1/auth/mfa/activate", 200},
		{"exempt mfa disable", frozen, "t1", "POST", "/api/v1/auth/mfa/disable", 200},
		{"exempt assistant ask", frozen, "t1", "POST", "/api/v1/assistant/ask", 200},
		{"exempt revoke api key", frozen, "t1", "DELETE", "/api/v1/admin/api-keys/k1", 200},
		{"exempt end user session", frozen, "t1", "DELETE", "/api/v1/admin/users/u/sessions/s", 200},
		{"exempt lock user account", frozen, "t1", "PUT", "/api/v1/admin/users/u", 200},
		{"exempt rotate hook secret", frozen, "t1", "POST", "/api/v1/admin/inbound-hooks/h/rotate", 200},
		{"exempt delete credential", frozen, "t1", "DELETE", "/api/v1/admin/credentials/c", 200},
		{"exempt sso provider patch", frozen, "t1", "PATCH", "/api/v1/admin/sso/providers/p", 200},
		{"exempt close workspace session", frozen, "t1", "DELETE", "/api/v1/admin/workspace-sessions/current", 200},
		{"exempt end preview", frozen, "t1", "DELETE", "/api/v1/preview-sessions/current", 200},
		// Same prefix, NOT on the list -> refused (a prefix match would let these through).
		{"fake /auth route refused", frozen, "t1", "POST", "/api/v1/auth/not-a-real-route", 503},
		{"create user refused", frozen, "t1", "POST", "/api/v1/admin/users", 503},
		{"create api key refused", frozen, "t1", "POST", "/api/v1/admin/api-keys", 503},
		{"update inbound hook refused", frozen, "t1", "PUT", "/api/v1/admin/inbound-hooks/h", 503},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			freezeRouter(tc.chk, tc.tenant).ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code != tc.want {
				t.Fatalf("%s %s: %d, want %d (%s)", tc.method, tc.path, w.Code, tc.want, w.Body.String())
			}
			if tc.want != 503 {
				return
			}
			var body struct {
				Code    string            `json:"code"`
				Error   string            `json:"error"`
				Details map[string]string `json:"details"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != "TENANT_MAINTENANCE" || body.Error == "" || body.Details["until"] == "" {
				t.Fatalf("body: %+v", body)
			}
			if s, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || s < 1 {
				t.Fatalf("Retry-After %q", w.Header().Get("Retry-After"))
			}
		})
	}
}

// Every freezeExempt entry is a real (method, route) shape — pinned against the production router in
// handlers/router_freeze_integration_test.go; here only the closed-map shape is checked.
func TestFreezeExempt_IsExactMatchMap(t *testing.T) {
	if len(freezeExempt) == 0 {
		t.Fatal("freezeExempt must not be empty")
	}
	for k := range freezeExempt {
		if k.method == "" || k.path == "" || k.path[0] != '/' {
			t.Fatalf("bad entry %+v", k)
		}
	}
}
