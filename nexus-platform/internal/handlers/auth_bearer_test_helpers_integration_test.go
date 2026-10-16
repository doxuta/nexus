//go:build integration

package handlers

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/auth"
)

// This file centralizes the A2 test plumbing that lets the protected-route integration tests
// authenticate the way production does: a verified Bearer ACCESS token whose claim carries the
// tenant id, instead of the legacy (pre-A2) X-Tenant-Id header. The shared router builders
// (newFullRouter / newAdminRouter) mount the real AuthMiddleware → RequireTenant chain via
// handlers.RegisterAPIRoutes using itAccessVerifier as the verifier, and the per-test request
// helpers attach a token minted by tokenForTenant — so the tenant a handler sees is ALWAYS the
// one inside the signed token, never anything a client could put in a header/query/body.

var (
	itTokenOnce sync.Once
	itTokenSvc  *auth.TokenService
)

// accessVerifier is the package-shared access-token verifier+issuer for the protected-route
// integration tests. It is built once from itJWTSecret (the same secret the auth suite uses),
// so a token it issues verifies inside the AuthMiddleware mounted by the test routers.
func accessVerifier(t *testing.T) *auth.TokenService {
	t.Helper()
	itTokenOnce.Do(func() {
		svc, err := auth.NewTokenService(itJWTSecret, 15*time.Minute, 168*time.Hour)
		if err != nil {
			t.Fatalf("accessVerifier: build token service: %v", err)
		}
		itTokenSvc = svc
	})
	return itTokenSvc
}

// tokenForTenant mints a valid access token whose claim carries the given tenant id (and a
// throwaway admin user id, so admin-gated routes added later still see a principal). This is how
// the integration tests put a tenant "into the request" post-A2: the value is signed, so the
// AuthMiddleware verifies it and lifts tenant into context exactly as a real login would.
func tokenForTenant(t *testing.T, tenant string) string {
	t.Helper()
	tok, err := accessVerifier(t).IssueAccessToken(auth.Subject{
		UserID:   uuid.NewString(),
		TenantID: tenant,
		IsAdmin:  true,
	})
	if err != nil {
		t.Fatalf("tokenForTenant(%q): %v", tenant, err)
	}
	return tok
}

// setAuth attaches the Authorization header for a request acting as `tenant`. An EMPTY tenant
// attaches NO header at all — that is the "unauthenticated / no token" case the protected routes
// must reject with 401 (it replaces the old "missing X-Tenant-Id → 400" semantics).
func setAuth(t *testing.T, req *http.Request, tenant string) {
	t.Helper()
	if tenant == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+tokenForTenant(t, tenant))
}
