package handlers

// specs/055 FR-008 default-build regression lock (no DB): RegisterAPIRoutes with WithSessionGuard
// makes the LIVE auth stage refuse a token whose login session was ended. Wired through the REAL
// route builder, so dropping the option (or its plumbing to AuthMiddlewareLive) flips this red.
// Reuses stubVerifier / claimsFor / aclWiringChecker / wiringLiveUsers / doClaimReq (same package).

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
)

type wiringSessions struct{ ended map[string]bool }

func (w *wiringSessions) SessionEnded(_ context.Context, _, _, sessionID string) (bool, error) {
	return w.ended[sessionID], nil
}

func buildSessionGuardEngine(sessions *wiringSessions) *gin.Engine {
	gin.SetMode(gin.TestMode)
	admin := claimsFor("t1", "", true)
	admin.SessionID = "s-ended"
	ver := &stubVerifier{byToken: map[string]*auth.Claims{"admin": admin}}
	r := gin.New()
	r.Use(gin.Recovery())
	opts := []RouteOption{WithLiveSubject(&wiringLiveUsers{user: &auth.User{Status: "active", IsAdmin: true}})}
	if sessions != nil {
		opts = append(opts, WithSessionGuard(sessions))
	}
	RegisterAPIRoutes(r.Group("/api/v1"), ver, nil, APIHandlers{
		User: NewAdminUserHandler(nil, zap.NewNop()),
	}, zap.NewNop(), opts...)
	return r
}

func TestSessionGuardWiring_EndedSessionIs401(t *testing.T) {
	r := buildSessionGuardEngine(&wiringSessions{ended: map[string]bool{"s-ended": true}})
	if rec := doClaimReq(r, http.MethodGet, "/api/v1/admin/users", "admin"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("ended session on a protected route = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
}

// Control: without the option the same token is not refused by the auth stage — the 401 above is
// the session guard, not a routing artifact.
func TestSessionGuardWiring_WithoutOptionTokenPasses(t *testing.T) {
	r := buildSessionGuardEngine(nil)
	if rec := doClaimReq(r, http.MethodGet, "/api/v1/admin/users", "admin"); rec.Code == http.StatusUnauthorized {
		t.Fatalf("without WithSessionGuard the auth stage refused the token: %d", rec.Code)
	}
}
