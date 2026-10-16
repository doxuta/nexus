package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/auth"
)

// setClaims puts verified claims into the context the way AuthMiddleware would, so RequireAdmin
// (which reads is_admin via IsAdminFromContext → ClaimsFromContext) sees a realistic principal.
func setClaims(c *gin.Context, isAdmin bool) {
	c.Set(ctxKeyClaims, &auth.Claims{
		Subject: auth.Subject{
			UserID:   "user-1",
			TenantID: "tenant-1",
			IsAdmin:  isAdmin,
		},
		TokenType: auth.TokenTypeAccess,
	})
	c.Set(ctxKeyTenantID, "tenant-1")
}

// runRequireAdmin executes RequireAdmin on a fresh context whose claims carry the given
// is_admin flag, reporting whether the downstream handler ran.
func runRequireAdmin(isAdmin bool, setUp bool) (*httptest.ResponseRecorder, bool) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/users", nil)
	if setUp {
		setClaims(c, isAdmin)
	}

	nextRan := false
	RequireAdmin(nil)(c)
	if !c.IsAborted() {
		nextRan = true
	}
	return rec, nextRan
}

func TestRequireAdmin_AllowsAdmin(t *testing.T) {
	rec, nextRan := runRequireAdmin(true, true)
	if !nextRan {
		t.Fatalf("admin request was aborted; want it to pass through to the handler")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no abort written); body=%s", rec.Code, rec.Body.String())
	}
}

func TestRequireAdmin_RejectsNonAdminWith403(t *testing.T) {
	rec, nextRan := runRequireAdmin(false, true)
	if nextRan {
		t.Fatalf("non-admin request reached the handler; want it refused")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// A request that never passed AuthMiddleware (no claims in context) must also be refused with
// 403 — RequireAdmin fails closed rather than treating an absent claim as admin.
func TestRequireAdmin_RejectsMissingClaimsWith403(t *testing.T) {
	rec, nextRan := runRequireAdmin(false, false)
	if nextRan {
		t.Fatalf("request with no claims reached the handler; want it refused")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}
