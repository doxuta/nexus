package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

type ownerLevelChecker struct{}

func (ownerLevelChecker) Authorize(context.Context, string, string, string, string) (bool, string, error) {
	return true, "owner", nil
}

// TestScopeOf_IncludeSharesOnlyForView pins the first key of the read-only share design
// (specs/051-record-manual-share): scopeOf arms IncludeShares only when the ACL stage authorized the
// request as "view" — a GET, or a route pinned to "view". Every write (method-mapped or pinned to
// edit/delete) and a route with no ACL stage at all get false.
func TestScopeOf_IncludeSharesOnlyForView(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name, method, fixed string
		noACL, want         bool
	}{
		{name: "GET → view → shares", method: http.MethodGet, want: true},
		{name: "PUT → edit → no shares", method: http.MethodPut},
		{name: "DELETE → delete → no shares", method: http.MethodDelete},
		{name: "POST → create → no shares", method: http.MethodPost},
		{name: "GET pinned to edit → no shares", method: http.MethodGet, fixed: "edit"},
		{name: "DELETE pinned to view → shares", method: http.MethodDelete, fixed: "view", want: true},
		{name: "no ACL stage → no shares", method: http.MethodGet, noACL: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			r.Use(func(c *gin.Context) {
				c.Set("tenantID", "t1")
				c.Set("authClaims", &auth.Claims{Subject: auth.Subject{UserID: "u1", TenantID: "t1", RoleID: "r1"}})
				c.Next()
			})
			switch {
			case tc.noACL:
			case tc.fixed != "":
				r.Use(middleware.RequireACLAction(ownerLevelChecker{}, nil, tc.fixed))
			default:
				r.Use(middleware.RequireACL(ownerLevelChecker{}, nil))
			}
			var got services.RecordScope
			r.Handle(tc.method, "/:entity/:id", func(c *gin.Context) { got = scopeOf(c); c.Status(http.StatusNoContent) })
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(tc.method, "/widgets/rec-1", nil))
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status %d", rec.Code)
			}
			if got.IncludeShares != tc.want {
				t.Fatalf("IncludeShares = %v; want %v (scope %+v)", got.IncludeShares, tc.want, got)
			}
		})
	}
}
