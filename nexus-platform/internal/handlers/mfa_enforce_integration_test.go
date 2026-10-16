//go:build integration

// MFA-ENFORCE integration suite: prove against REAL MySQL that an account which enrolled in
// MFA while the key was present is FAIL-CLOSED at login (503 MFA_UNAVAILABLE) once
// MFA_SECRET_ENC_KEY is lost — never silently downgraded to one-step login — and that the two
// operational recoveries both work: restoring the key resumes the normal two-step flow, and
// the MFA_FAIL_OPEN=true INFRA-EMERGENCY override permits (loudly logged) one-step logins.
// Run with:
//
//	go test -tags=integration -race ./internal/handlers/ -run TestMFAEnforceIT
package handlers

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/config"
	"nexus/platform/internal/services"
)

const (
	mfaEnfTenantID   = "b4ea0000-0000-0000-0000-0000000000e1"
	mfaEnfTenantSlug = "mfa-enforce-it-tenant"
)

// newMFAEnforceITRouter builds the auth surface wired EXACTLY like cmd/server/main.go when
// MFA_SECRET_ENC_KEY is ABSENT (the "key lost" server state): config loaded from the
// environment (Enabled=false), the enrollment reader wired UNCONDITIONALLY, WithMFA skipped.
func newMFAEnforceITRouter(t *testing.T, gdb *gorm.DB, failOpen bool) *gin.Engine {
	t.Helper()

	t.Setenv("JWT_SECRET", itJWTSecret)
	t.Setenv("JWT_ACCESS_TTL", "15m")
	t.Setenv("JWT_REFRESH_TTL", "168h")
	t.Setenv("MFA_SECRET_ENC_KEY", "") // the key is LOST/absent
	if failOpen {
		t.Setenv("MFA_FAIL_OPEN", "true")
	} else {
		t.Setenv("MFA_FAIL_OPEN", "")
	}
	jwtCfg, err := config.LoadJWTConfig()
	if err != nil {
		t.Fatalf("config.LoadJWTConfig: %v", err)
	}
	tokenSvc, err := auth.NewTokenService(jwtCfg.JWTSecret, jwtCfg.AccessTTL, jwtCfg.RefreshTTL)
	if err != nil {
		t.Fatalf("auth.NewTokenService: %v", err)
	}
	mfaCfg, err := config.LoadMFAConfig()
	if err != nil {
		t.Fatalf("config.LoadMFAConfig: %v", err)
	}
	if mfaCfg.Enabled {
		t.Fatal("expected MFA config Enabled=false with MFA_SECRET_ENC_KEY absent")
	}
	if mfaCfg.FailOpen != failOpen {
		t.Fatalf("mfaCfg.FailOpen = %v; want %v (MFA_FAIL_OPEN env plumbing)", mfaCfg.FailOpen, failOpen)
	}

	authSvc := services.NewAuthService(auth.NewUserRepository(gdb), auth.NewRefreshTokenRepository(gdb), tokenSvc).
		WithMFAEnrollmentReader(auth.NewUserMFARepository(gdb), mfaCfg.FailOpen)
	// WithMFA deliberately NOT called — mirrors main.go's !mfaCfg.Enabled branch.

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), tokenSvc, nil, APIHandlers{
		Auth: NewAuthHandler(authSvc, zap.NewNop()),
	}, zap.NewNop())
	return r
}

// TestMFAEnforceIT_EnrolledThenKeyLost_FailClosed_ThenKeyRestored walks the full operational
// story on one real database:
//  1. key present: user enrolls + activates MFA; login is two-step (mfaPending).
//  2. key LOST (reader-only wiring): login with the CORRECT password → 503 MFA_UNAVAILABLE,
//     no token material of any kind — the fail-open hole this slice closes.
//  3. a never-enrolled user on the SAME degraded server logs in one-step normally.
//  4. key RESTORED: the enrolled user's login is two-step again — nothing was mutated by the
//     blocked attempts (restore key → everything resumes).
func TestMFAEnforceIT_EnrolledThenKeyLost_FailClosed_ThenKeyRestored(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	a4SeedTenant(t, gdb, mfaEnfTenantID, mfaEnfTenantSlug, "MFA Enforce IT Tenant")
	mfaITCleanup(t, gdb, mfaEnfTenantID)
	a4SeedUser(t, gdb, mfaEnfTenantID, "alice@"+mfaEnfTenantSlug, false, nil)
	a4SeedUser(t, gdb, mfaEnfTenantID, "carol@"+mfaEnfTenantSlug, false, nil)

	// 1. Key present: enroll + activate through the real HTTP surface.
	rKeyed := newMFAITRouter(t, gdb)
	pre := itLogin(t, rKeyed, mfaEnfTenantSlug, "alice@"+mfaEnfTenantSlug, authPassword)
	mfaEnrollAndActivate(t, rKeyed, pre.Data.AccessToken)
	mfaLoginExpectPending(t, rKeyed, mfaEnfTenantSlug, "alice@"+mfaEnfTenantSlug, authPassword)

	// 2. Key lost: the SAME database, a server booted without the key.
	rLost := newMFAEnforceITRouter(t, gdb, false)
	w := mfaPostJSON(t, rLost, "/api/v1/auth/login",
		`{"tenant":"`+mfaEnfTenantSlug+`","email":"alice@`+mfaEnfTenantSlug+`","password":"`+authPassword+`"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("login(enrolled, key lost): got %d, want 503; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"MFA_UNAVAILABLE"`) {
		t.Fatalf("login(enrolled, key lost) body missing MFA_UNAVAILABLE: %s", w.Body.String())
	}
	for _, forbidden := range []string{"accessToken", "refreshToken", "pendingToken"} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Fatalf("fail-closed 503 leaked %q: %s", forbidden, w.Body.String())
		}
	}

	// 3. A never-enrolled user is untouched by the degradation.
	carol := itLogin(t, rLost, mfaEnfTenantSlug, "carol@"+mfaEnfTenantSlug, authPassword)
	if carol.Data.AccessToken == "" {
		t.Fatal("non-enrolled user must still log in one-step on the degraded server")
	}

	// 4. Key restored: the enrolled account resumes the two-step flow untouched.
	rRestored := newMFAITRouter(t, gdb)
	mfaLoginExpectPending(t, rRestored, mfaEnfTenantSlug, "alice@"+mfaEnfTenantSlug, authPassword)
}

// TestMFAEnforceIT_KeyLost_FailOpen_OneStepLogin proves the INFRA-EMERGENCY override end to
// end: MFA_FAIL_OPEN=true on the key-less server lets the enrolled account log in one-step
// with a full working token pair.
func TestMFAEnforceIT_KeyLost_FailOpen_OneStepLogin(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	a4SeedTenant(t, gdb, mfaEnfTenantID, mfaEnfTenantSlug, "MFA Enforce IT Tenant")
	mfaITCleanup(t, gdb, mfaEnfTenantID)
	a4SeedUser(t, gdb, mfaEnfTenantID, "alice@"+mfaEnfTenantSlug, false, nil)

	rKeyed := newMFAITRouter(t, gdb)
	pre := itLogin(t, rKeyed, mfaEnfTenantSlug, "alice@"+mfaEnfTenantSlug, authPassword)
	mfaEnrollAndActivate(t, rKeyed, pre.Data.AccessToken)

	rFailOpen := newMFAEnforceITRouter(t, gdb, true)
	out := itLogin(t, rFailOpen, mfaEnfTenantSlug, "alice@"+mfaEnfTenantSlug, authPassword)
	if out.Data.AccessToken == "" || out.Data.RefreshToken == "" {
		t.Fatalf("fail-open login must return a full one-step session, got: %+v", out.Data)
	}
}
