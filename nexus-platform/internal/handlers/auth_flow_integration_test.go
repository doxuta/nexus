//go:build integration

// End-to-end integration tests for the COMPLETE A1 auth surface against a real MySQL 8
// (the docker-compose `mysql` service). Run with:
//
//	go test -tags=integration ./internal/handlers/
//
// They require a reachable MySQL 8 with the dev DB (`make docker-up` from nexus-platform/).
// Set TEST_DATABASE_URL to override the DSN; otherwise the local compose DSN is used (see
// dsn() in admin_entity_handler_integration_test.go, shared across this package).
//
// Scope (../specs/001-auth-security/spec.md §3 A1 coverage map — the AUTHORITATIVE list of A1 endpoints):
//
//	| POST | /auth/login   | public            |  ← happy + failure covered in auth_login_integration_test.go
//	| POST | /auth/refresh | public (valid rt) |  ← here
//	| POST | /auth/logout  | auth              |  ← here
//	| GET  | /auth/me      | auth              |  ← here
//
// register / forgot / reset / change-password are explicitly OUT of A1 by spec (user CRUD is
// A3 /admin/users; there is no password_reset table in A1 — confirmed by the migration scope
// note on task t_7fdaa6bc). This file therefore covers the full A1 chain end to end —
// login → me → refresh → logout — plus, for every endpoint, at least one happy path and one
// failure path, and the tenant-isolation guarantees that hold inside the auth surface itself.
//
// Everything runs against the SAME real stack the login suite wires (newAuthRouter): env-loaded
// JWT config → token service → GORM-backed user + refresh-token repositories → auth service →
// handler, mounted on /api/v1. The refresh_tokens table is GORM-managed (internal/auth/
// refresh_token.go) and created by the real golang-migrate chain (seedAuthFixtures no longer
// AutoMigrates — bug #1/#4 regression net), so the rotation/revocation paths exercise the
// production server-side liveness check against the migrated schema, not a stub.
//
// NOTE: A1 has no AuthMiddleware yet (that lands in A2). Per the handler contract, /auth/me
// self-verifies the Bearer ACCESS token and /auth/logout takes the refresh token in the body —
// this suite asserts those A1 semantics exactly, so it stays valid until A2 moves verification
// into middleware (at which point these tests are the spec to re-point, per slice A2's
// "rewrite ALL integration tests to login→Bearer, keep each test's intent").
package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
)

// ---- small request helpers (login lives in postLogin / loginResponse in the login suite) ----

// getMe issues GET /api/v1/auth/me with an optional Bearer access token. An empty token sends
// no Authorization header at all (the missing-credential case).
func getMe(t *testing.T, r *gin.Engine, accessToken string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// postRefresh issues POST /api/v1/auth/refresh with the given JSON body.
func postRefresh(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// postLogout issues POST /api/v1/auth/logout with the given JSON body.
func postLogout(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// refreshResponse mirrors the /auth/refresh success envelope: {"data": {accessToken, refreshToken}}.
type refreshResponse struct {
	Data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	} `json:"data"`
	Error string `json:"error"`
	Code  string `json:"code"`
}

// meResponse mirrors the /auth/me success envelope: {"data": {id, tenantId, email, ...}}.
type meResponse struct {
	Data struct {
		ID           string `json:"id"`
		TenantID     string `json:"tenantId"`
		Email        string `json:"email"`
		FullName     string `json:"fullName"`
		IsAdmin      bool   `json:"isAdmin"`
		PasswordHash string `json:"passwordHash"`
		Password     string `json:"password"`
	} `json:"data"`
	Error string `json:"error"`
	Code  string `json:"code"`
}

// itLogin performs a login and returns the decoded success envelope, failing the test on any
// non-200 so the chained-flow tests can assume a valid starting token pair.
func itLogin(t *testing.T, r *gin.Engine, tenantSlug, email, password string) loginResponse {
	t.Helper()
	w := postLogin(t, r, `{"tenant":"`+tenantSlug+`","email":"`+email+`","password":"`+password+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("login(%s/%s): got %d, want 200; body=%s", tenantSlug, email, w.Code, w.Body.String())
	}
	var resp loginResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode login response: %v; body=%s", err, w.Body.String())
	}
	if resp.Data.AccessToken == "" || resp.Data.RefreshToken == "" {
		t.Fatalf("login returned empty token(s): %s", w.Body.String())
	}
	return resp
}

// TestA1FullAuthFlow walks the entire A1 chain end to end against real DB rows, asserting the
// happy path of every §3 endpoint and that each step's output feeds the next:
//
//	login → me (access works) → refresh (rotate) → me (new access works) → logout (revoke).
//
// Along the way it pins the security-critical invariants of the chain: the rotated refresh
// token is single-use (the old one is dead), the access token survives logout (only refresh is
// revoked, spec §5), and no response ever leaks a password hash.
func TestA1FullAuthFlow(t *testing.T) {
	gdb := authGormDB(t)
	cleanup := seedAuthFixtures(t, gdb)
	t.Cleanup(cleanup)

	r, tokenSvc := newAuthRouter(t, gdb)

	// 1) LOGIN — obtain the initial token pair (tenant A admin user "alice").
	login := itLogin(t, r, authTenantASlug, authUserEmail, authPassword)
	access1, refresh1 := login.Data.AccessToken, login.Data.RefreshToken

	// 2) ME with the login access token → 200 with the seeded user, no hash leak.
	wMe := getMe(t, r, access1)
	if wMe.Code != http.StatusOK {
		t.Fatalf("me(access1): got %d, want 200; body=%s", wMe.Code, wMe.Body.String())
	}
	var me meResponse
	if err := json.Unmarshal(wMe.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode me: %v; body=%s", err, wMe.Body.String())
	}
	if me.Data.Email != authUserEmail {
		t.Errorf("me.email: got %q, want %q", me.Data.Email, authUserEmail)
	}
	if me.Data.TenantID != authTenantAID {
		t.Errorf("me.tenantId: got %q, want %q", me.Data.TenantID, authTenantAID)
	}
	if !me.Data.IsAdmin {
		t.Error("me.isAdmin=false; seeded user is admin")
	}
	if me.Data.PasswordHash != "" || me.Data.Password != "" || containsHash(wMe.Body.String()) {
		t.Errorf("me response leaks a password field/hash: %s", wMe.Body.String())
	}
	// The me-subject must match the user_id embedded in the access token (same principal).
	claims1, err := tokenSvc.VerifyAccessToken(access1)
	if err != nil {
		t.Fatalf("verify access1: %v", err)
	}
	if me.Data.ID != claims1.UserID {
		t.Errorf("me.id %q != access token uid %q", me.Data.ID, claims1.UserID)
	}

	// 3) REFRESH — exchange refresh1 for a fresh access + a ROTATED refresh token.
	wRef := postRefresh(t, r, `{"refreshToken":"`+refresh1+`"}`)
	if wRef.Code != http.StatusOK {
		t.Fatalf("refresh(refresh1): got %d, want 200; body=%s", wRef.Code, wRef.Body.String())
	}
	var ref refreshResponse
	if err := json.Unmarshal(wRef.Body.Bytes(), &ref); err != nil {
		t.Fatalf("decode refresh: %v; body=%s", err, wRef.Body.String())
	}
	access2, refresh2 := ref.Data.AccessToken, ref.Data.RefreshToken
	if access2 == "" || refresh2 == "" {
		t.Fatalf("refresh returned empty token(s): %s", wRef.Body.String())
	}
	// Rotation must actually rotate: the new refresh token differs from the presented one.
	if refresh2 == refresh1 {
		t.Error("refresh token was not rotated (refresh2 == refresh1)")
	}
	// The new access token must verify and carry the SAME subject (no escalation, no drift).
	claims2, err := tokenSvc.VerifyAccessToken(access2)
	if err != nil {
		t.Fatalf("verify access2: %v", err)
	}
	if claims2.UserID != claims1.UserID || claims2.TenantID != claims1.TenantID || claims2.IsAdmin != claims1.IsAdmin {
		t.Errorf("rotated access subject drifted: got %+v, want uid=%s tid=%s adm=%v",
			claims2.Subject, claims1.UserID, claims1.TenantID, claims1.IsAdmin)
	}

	// 3a) SINGLE-USE: replaying the now-rotated refresh1 must fail — its server-side row is
	//     revoked, so liveness lookup misses and the endpoint returns a generic 401.
	wReplay := postRefresh(t, r, `{"refreshToken":"`+refresh1+`"}`)
	if wReplay.Code != http.StatusUnauthorized {
		t.Errorf("replay of rotated refresh1: got %d, want 401; body=%s", wReplay.Code, wReplay.Body.String())
	}

	// 4) ME with the NEW access token still works (the rotated access is a valid credential).
	wMe2 := getMe(t, r, access2)
	if wMe2.Code != http.StatusOK {
		t.Fatalf("me(access2): got %d, want 200; body=%s", wMe2.Code, wMe2.Body.String())
	}

	// 5) LOGOUT — revoke the live refresh2. 204, idempotent, body-less.
	wOut := postLogout(t, r, `{"refreshToken":"`+refresh2+`"}`)
	if wOut.Code != http.StatusNoContent {
		t.Fatalf("logout(refresh2): got %d, want 204; body=%s", wOut.Code, wOut.Body.String())
	}

	// 5a) After logout the refresh token is dead: a refresh with it must 401.
	wAfter := postRefresh(t, r, `{"refreshToken":"`+refresh2+`"}`)
	if wAfter.Code != http.StatusUnauthorized {
		t.Errorf("refresh after logout: got %d, want 401; body=%s", wAfter.Code, wAfter.Body.String())
	}

	// 5b) Logout is idempotent — logging the (already-dead) token out again is still 204,
	//     so the endpoint never signals whether the token was live (anti-probe, spec §5).
	wOut2 := postLogout(t, r, `{"refreshToken":"`+refresh2+`"}`)
	if wOut2.Code != http.StatusNoContent {
		t.Errorf("idempotent logout: got %d, want 204; body=%s", wOut2.Code, wOut2.Body.String())
	}

	// 5c) Spec §5: logout revokes only the REFRESH side. The access token (access2) issued
	//     before logout is short-lived but still valid until its own expiry — /auth/me must
	//     still accept it (this documents the deliberate ≤TTL revocation latency).
	wMeAfterLogout := getMe(t, r, access2)
	if wMeAfterLogout.Code != http.StatusOK {
		t.Errorf("me(access2) after logout: got %d, want 200 (access survives logout per §5); body=%s",
			wMeAfterLogout.Code, wMeAfterLogout.Body.String())
	}
}

// TestA1MeFailures drives the /auth/me failure paths: a request must be rejected with 401
// whenever the Bearer ACCESS token is absent, malformed, signed by the wrong secret, expired,
// or is actually a REFRESH token (type pinning — a refresh token is NOT a valid access
// credential). Every rejection is a generic 401 with no hash leak.
func TestA1MeFailures(t *testing.T) {
	gdb := authGormDB(t)
	cleanup := seedAuthFixtures(t, gdb)
	t.Cleanup(cleanup)

	r, _ := newAuthRouter(t, gdb)

	// A real login gives us a genuine refresh token to use in the "refresh-as-access" case.
	login := itLogin(t, r, authTenantASlug, authUserEmail, authPassword)

	// A token signed by a DIFFERENT secret/issuer must not verify against the router's secret.
	foreignSvc, err := auth.NewTokenService("a-totally-different-secret-0123456789", 15*time.Minute, 168*time.Hour)
	if err != nil {
		t.Fatalf("foreign token service: %v", err)
	}
	foreignAccess, err := foreignSvc.IssueAccessToken(auth.Subject{
		UserID: uuid.NewString(), TenantID: authTenantAID, IsAdmin: true,
	})
	if err != nil {
		t.Fatalf("issue foreign access: %v", err)
	}

	// An ALREADY-EXPIRED access token signed by the CORRECT secret (negative TTL) — verifies
	// the expiry path specifically, distinct from a signature failure.
	expiredSvc, err := auth.NewTokenService(itJWTSecret, -1*time.Minute, 168*time.Hour)
	if err != nil {
		t.Fatalf("expired token service: %v", err)
	}
	expiredAccess, err := expiredSvc.IssueAccessToken(auth.Subject{
		UserID: uuid.NewString(), TenantID: authTenantAID, IsAdmin: true,
	})
	if err != nil {
		t.Fatalf("issue expired access: %v", err)
	}

	cases := []struct {
		name  string
		token string // "" => no Authorization header at all
	}{
		{"no Authorization header", ""},
		{"garbage token", "not-a-jwt"},
		{"foreign-signed access token", foreignAccess},
		{"expired access token", expiredAccess},
		{"refresh token used as access (type pinning)", login.Data.RefreshToken},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := getMe(t, r, tc.token)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("got %d, want 401; body=%s", w.Code, w.Body.String())
			}
			if containsHash(w.Body.String()) {
				t.Errorf("me rejection leaks a hash: %s", w.Body.String())
			}
		})
	}
}

// TestA1RefreshFailures drives the /auth/refresh failure paths: a generic 401 for any
// invalid / expired / wrong-type / unknown-but-well-signed refresh token, and a 400 for a
// malformed JSON body. No rejection ever returns a token or leaks a hash.
func TestA1RefreshFailures(t *testing.T) {
	gdb := authGormDB(t)
	cleanup := seedAuthFixtures(t, gdb)
	t.Cleanup(cleanup)

	r, _ := newAuthRouter(t, gdb)

	login := itLogin(t, r, authTenantASlug, authUserEmail, authPassword)

	// A refresh token that is cryptographically VALID for the router's secret but was never
	// stored server-side (minted out-of-band) — must fail the liveness check → 401. This is
	// the case that proves refresh isn't just JWT-verified but also DB-backed/revocable.
	unstoredSvc, err := auth.NewTokenService(itJWTSecret, 15*time.Minute, 168*time.Hour)
	if err != nil {
		t.Fatalf("unstored token service: %v", err)
	}
	unstoredRefresh, err := unstoredSvc.IssueRefreshToken(auth.Subject{
		UserID: uuid.NewString(), TenantID: authTenantAID, IsAdmin: false,
	})
	if err != nil {
		t.Fatalf("issue unstored refresh: %v", err)
	}

	// An expired (but correctly signed) refresh token.
	expiredSvc, err := auth.NewTokenService(itJWTSecret, -2*time.Minute, -1*time.Minute)
	if err != nil {
		t.Fatalf("expired refresh service: %v", err)
	}
	expiredRefresh, err := expiredSvc.IssueRefreshToken(auth.Subject{
		UserID: uuid.NewString(), TenantID: authTenantAID, IsAdmin: false,
	})
	if err != nil {
		t.Fatalf("issue expired refresh: %v", err)
	}

	type tc struct {
		name     string
		body     string
		wantCode int
	}
	cases := []tc{
		{"empty refresh token", `{"refreshToken":""}`, http.StatusUnauthorized},
		{"garbage refresh token", `{"refreshToken":"not-a-jwt"}`, http.StatusUnauthorized},
		{"access token used as refresh (type pinning)", `{"refreshToken":"` + login.Data.AccessToken + `"}`, http.StatusUnauthorized},
		{"well-signed but never-stored refresh", `{"refreshToken":"` + unstoredRefresh + `"}`, http.StatusUnauthorized},
		{"expired refresh token", `{"refreshToken":"` + expiredRefresh + `"}`, http.StatusUnauthorized},
		{"malformed JSON body", `{not-json`, http.StatusBadRequest},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := postRefresh(t, r, c.body)
			if w.Code != c.wantCode {
				t.Fatalf("got %d, want %d; body=%s", w.Code, c.wantCode, w.Body.String())
			}
			var resp refreshResponse
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if resp.Data.AccessToken != "" || resp.Data.RefreshToken != "" {
				t.Errorf("refresh rejection returned a token: %s", w.Body.String())
			}
			if containsHash(w.Body.String()) {
				t.Errorf("refresh rejection leaks a hash: %s", w.Body.String())
			}
		})
	}
}

// TestA1LogoutBehaviour covers the /auth/logout contract directly: a malformed body is a 400,
// while a garbage / unknown / empty refresh token is a 204 no-op (idempotent, anti-probe — the
// endpoint never reveals whether the token existed, spec §5).
func TestA1LogoutBehaviour(t *testing.T) {
	gdb := authGormDB(t)
	cleanup := seedAuthFixtures(t, gdb)
	t.Cleanup(cleanup)

	r, _ := newAuthRouter(t, gdb)

	type tc struct {
		name     string
		body     string
		wantCode int
	}
	cases := []tc{
		{"empty refresh token is a no-op", `{"refreshToken":""}`, http.StatusNoContent},
		{"garbage refresh token is a no-op", `{"refreshToken":"not-a-jwt"}`, http.StatusNoContent},
		{"unknown but well-formed body is a no-op", `{"refreshToken":"x.y.z"}`, http.StatusNoContent},
		{"malformed JSON body is a 400", `{not-json`, http.StatusBadRequest},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := postLogout(t, r, c.body)
			if w.Code != c.wantCode {
				t.Fatalf("got %d, want %d; body=%s", w.Code, c.wantCode, w.Body.String())
			}
		})
	}
}

// TestA1TenantIsolationAcrossFlow is the trust-boundary test the spec calls out (§5/§6
// "forge-tenant"): a credential minted for tenant A must be useless for resolving anything in
// tenant B, ACROSS the whole auth surface — me, refresh, and logout. It seeds a second active
// user in tenant B, logs both in, and proves their tokens never cross the tenant line.
//
// Because A1's /auth/me and /auth/refresh take the tenant FROM THE VERIFIED CLAIM (not from any
// client-supplied header/query), the isolation here is enforced by the token subject + the
// tenant-scoped repository lookups — exactly the boundary A2 will extend to every route.
func TestA1TenantIsolationAcrossFlow(t *testing.T) {
	gdb := authGormDB(t)
	cleanup := seedAuthFixtures(t, gdb)
	t.Cleanup(cleanup)

	// Add an active user in tenant B (the base fixtures only seed users in tenant A). Track it
	// so we can remove it on cleanup without disturbing the shared fixture teardown.
	bobBEmail := "user-b@it-globex.test"
	hash, err := bcrypt.GenerateFromPassword([]byte(authPassword), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt hash: %v", err)
	}
	userB := auth.User{
		ID: uuid.NewString(), TenantID: authTenantBID, Email: bobBEmail,
		PasswordHash: string(hash), FullName: "User B", Status: "active",
		IsAdmin: false, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := gdb.Create(&userB).Error; err != nil {
		t.Fatalf("seed tenant-B user: %v", err)
	}
	t.Cleanup(func() {
		// Remove the refresh tokens we minted for B, then the user. (The shared cleanup already
		// nukes tenant B's tenant row + any tenant-scoped tokens, but be explicit/idempotent.)
		gdb.Where("tenant_id = ?", authTenantBID).Delete(&auth.RefreshToken{})
		gdb.Unscoped().Where("id = ?", userB.ID).Delete(&auth.User{})
	})

	r, tokenSvc := newAuthRouter(t, gdb)

	// Log both principals in: alice in tenant A (admin), userB in tenant B (non-admin).
	loginA := itLogin(t, r, authTenantASlug, authUserEmail, authPassword)
	loginB := itLogin(t, r, authTenantBSlug, bobBEmail, authPassword)

	// Sanity: the two access tokens carry DIFFERENT tenant ids — the trust boundary is in the
	// claim, set from the resolved (tenant_id, email) row, never from the request.
	claimsA, err := tokenSvc.VerifyAccessToken(loginA.Data.AccessToken)
	if err != nil {
		t.Fatalf("verify A access: %v", err)
	}
	claimsB, err := tokenSvc.VerifyAccessToken(loginB.Data.AccessToken)
	if err != nil {
		t.Fatalf("verify B access: %v", err)
	}
	if claimsA.TenantID != authTenantAID || claimsB.TenantID != authTenantBID {
		t.Fatalf("tenant claims wrong: A=%q (want %q) B=%q (want %q)",
			claimsA.TenantID, authTenantAID, claimsB.TenantID, authTenantBID)
	}
	if claimsA.TenantID == claimsB.TenantID {
		t.Fatal("tenant A and tenant B resolved to the same tenant_id — isolation broken")
	}

	// /auth/me: each token resolves ONLY its own tenant's user. We assert the returned user is
	// the right principal in the right tenant; a token can never surface the other tenant's user.
	t.Run("me resolves own-tenant user only", func(t *testing.T) {
		wA := getMe(t, r, loginA.Data.AccessToken)
		wB := getMe(t, r, loginB.Data.AccessToken)
		if wA.Code != http.StatusOK || wB.Code != http.StatusOK {
			t.Fatalf("me A=%d B=%d, want 200/200", wA.Code, wB.Code)
		}
		var meA, meB meResponse
		_ = json.Unmarshal(wA.Body.Bytes(), &meA)
		_ = json.Unmarshal(wB.Body.Bytes(), &meB)
		if meA.Data.TenantID != authTenantAID || meA.Data.Email != authUserEmail {
			t.Errorf("me(A) crossed tenants: %+v", meA.Data)
		}
		if meB.Data.TenantID != authTenantBID || meB.Data.Email != bobBEmail {
			t.Errorf("me(B) crossed tenants: %+v", meB.Data)
		}
		if meA.Data.ID == meB.Data.ID {
			t.Error("me(A) and me(B) returned the same user id — tenant isolation broken")
		}
	})

	// Forge-tenant on /auth/me: a token whose subject claims tenant B's id but a user_id that
	// only exists in tenant A must NOT resolve — the repository lookup is pinned to
	// (claim.tenant_id, claim.user_id), so a forged tenant id finds no matching row → 401.
	t.Run("forged-tenant access token cannot read across tenants", func(t *testing.T) {
		forged, err := tokenSvc.IssueAccessToken(auth.Subject{
			UserID:   claimsA.UserID, // alice, who lives in tenant A
			TenantID: authTenantBID,  // ...but the claim lies that she is in tenant B
			IsAdmin:  true,
		})
		if err != nil {
			t.Fatalf("issue forged access: %v", err)
		}
		w := getMe(t, r, forged)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("forged-tenant me: got %d, want 401 (user_id not in claimed tenant); body=%s",
				w.Code, w.Body.String())
		}
	})

	// /auth/refresh: tenant B's refresh token rotates ONLY within tenant B. The new access
	// token it yields must still be a tenant-B subject — refreshing can never migrate a session
	// into another tenant, because the subject is rebuilt from the stored row + verified claim.
	t.Run("refresh stays within the token's own tenant", func(t *testing.T) {
		w := postRefresh(t, r, `{"refreshToken":"`+loginB.Data.RefreshToken+`"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("refresh(B): got %d, want 200; body=%s", w.Code, w.Body.String())
		}
		var ref refreshResponse
		_ = json.Unmarshal(w.Body.Bytes(), &ref)
		c, err := tokenSvc.VerifyAccessToken(ref.Data.AccessToken)
		if err != nil {
			t.Fatalf("verify refreshed B access: %v", err)
		}
		if c.TenantID != authTenantBID {
			t.Errorf("refreshed B access tenant: got %q, want %q", c.TenantID, authTenantBID)
		}
		if c.UserID != claimsB.UserID {
			t.Errorf("refreshed B access user drifted: got %q, want %q", c.UserID, claimsB.UserID)
		}
	})

	// Cross-tenant logout cannot revoke another tenant's session: logging out tenant A's refresh
	// token (a tenant-A-scoped revoke) must leave tenant B's refresh token fully usable. We use
	// loginA's STILL-LIVE refresh token (loginB's was rotated above, so re-login B for a fresh one).
	t.Run("logout is tenant-scoped and cannot revoke another tenant", func(t *testing.T) {
		freshB := itLogin(t, r, authTenantBSlug, bobBEmail, authPassword)

		// Log out tenant A's refresh token (scoped to tenant A by its own claim).
		wOut := postLogout(t, r, `{"refreshToken":"`+loginA.Data.RefreshToken+`"}`)
		if wOut.Code != http.StatusNoContent {
			t.Fatalf("logout(A): got %d, want 204; body=%s", wOut.Code, wOut.Body.String())
		}

		// Tenant A's refresh token is now dead.
		wDeadA := postRefresh(t, r, `{"refreshToken":"`+loginA.Data.RefreshToken+`"}`)
		if wDeadA.Code != http.StatusUnauthorized {
			t.Errorf("A refresh after A logout: got %d, want 401; body=%s", wDeadA.Code, wDeadA.Body.String())
		}

		// Tenant B's fresh refresh token is UNAFFECTED — the A-scoped logout never touched it.
		wLiveB := postRefresh(t, r, `{"refreshToken":"`+freshB.Data.RefreshToken+`"}`)
		if wLiveB.Code != http.StatusOK {
			t.Errorf("B refresh after A logout: got %d, want 200 (A's logout must not revoke B); body=%s",
				wLiveB.Code, wLiveB.Body.String())
		}
	})
}

// compile-time guard: ensure the suite references the gorm import even if a future edit drops
// the only direct use (keeps the harness honest about what it depends on).
var _ = func(db *gorm.DB) *gorm.DB { return db }
