package handlers

// POST /auth/logout-all — "đăng xuất mọi thiết bị". The behaviour that matters (device B can no
// longer refresh after device A revokes) is proven end-to-end over the live API; what these tests
// pin is the property that is easy to break later by adding a convenience: identity must come
// from the VERIFIED CLAIM and nowhere else, so no one can ever end another user's sessions.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// logoutAllRouter builds an engine with BOTH the public auth routes (to log in) and the protected
// group behind the real AuthMiddleware → RequireTenant chain, so an unverified request is refused
// by exactly the chain production uses rather than by a test shortcut.
func logoutAllRouter(t *testing.T) (*gin.Engine, string, *stubRefreshRepo) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	const password = "s3cret-pw"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	repo := &stubUserRepo{
		tenant: &auth.Tenant{ID: "t-1", Slug: "acme", Status: "active"},
		user: &auth.User{
			ID: "u-1", TenantID: "t-1", Email: "alice@acme.test",
			PasswordHash: string(hash), Status: "active",
		},
	}
	tokens, err := auth.NewTokenService("super-secret-test-key-0123456789", 15*60*time.Second, 7*24*60*60*time.Second)
	if err != nil {
		t.Fatalf("token service: %v", err)
	}
	refresh := newStubRefreshRepo()
	h := NewAuthHandler(services.NewAuthService(repo, refresh, tokens), zap.NewNop())

	r := gin.New()
	api := r.Group("/api/v1")
	h.RegisterRoutes(api)

	prot := r.Group("/api/v1")
	prot.Use(middleware.AuthMiddleware(tokens, zap.NewNop()))
	prot.Use(middleware.RequireTenant(zap.NewNop()))
	h.RegisterProtectedRoutes(prot)
	return r, password, refresh
}

// liveFor counts refresh tokens of (tenant,user) that are still usable.
func (s *stubRefreshRepo) liveFor(tenantID, userID string) int {
	n := 0
	for _, rt := range s.rows {
		if rt.TenantID == tenantID && rt.UserID == userID && rt.RevokedAt == nil {
			n++
		}
	}
	return n
}

func postWithAuth(r *gin.Engine, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestLogoutAll_RevokesEverySessionOfTheCaller(t *testing.T) {
	r, password, refresh := logoutAllRouter(t)
	access, _ := doLogin(t, r, password)
	doLogin(t, r, password) // a second "device"

	if n := refresh.liveFor("t-1", "u-1"); n != 2 {
		t.Fatalf("live sessions before = %d, want the two logins", n)
	}
	if w := postWithAuth(r, "/api/v1/auth/logout-all", access, "{}"); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d (%s), want 204", w.Code, w.Body.String())
	}
	if n := refresh.liveFor("t-1", "u-1"); n != 0 {
		t.Errorf("live sessions after = %d, want every device revoked", n)
	}
}

func TestLogoutAll_RequiresAVerifiedCaller(t *testing.T) {
	r, _, refresh := logoutAllRouter(t)

	for _, tc := range []struct{ name, token string }{
		{"no token", ""},
		{"garbage token", "not-a-jwt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := postWithAuth(r, "/api/v1/auth/logout-all", tc.token, "{}")
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", w.Code)
			}
		})
	}
	if n := refresh.liveFor("t-1", "u-1"); n != 0 {
		t.Errorf("no one logged in, yet %d sessions exist", n)
	}
}

// TestLogoutAll_IgnoresAnyUserIdInTheBody is the regression guard that matters: the endpoint takes
// no body, and if someone later "helpfully" binds one, this fails. Ending another user's sessions
// is a denial-of-service against them.
func TestLogoutAll_IgnoresAnyUserIdInTheBody(t *testing.T) {
	r, password, refresh := logoutAllRouter(t)
	access, _ := doLogin(t, r, password)

	// A session belonging to a DIFFERENT user, which must survive untouched.
	other := &auth.RefreshToken{TenantID: "t-1", UserID: "u-2", TokenHash: "other-hash"}
	if err := refresh.Store(context.Background(), other); err != nil {
		t.Fatalf("seed other user session: %v", err)
	}

	w := postWithAuth(r, "/api/v1/auth/logout-all", access, `{"userId":"u-2","tenantId":"other-tenant"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d (%s), want 204", w.Code, w.Body.String())
	}
	if n := refresh.liveFor("t-1", "u-1"); n != 0 {
		t.Errorf("caller's own sessions survived (%d live)", n)
	}
	if n := refresh.liveFor("t-1", "u-2"); n != 1 {
		t.Fatalf("another user's session was revoked — the body steered whose sessions die")
	}
}
