//go:build integration

// End-to-end integration tests for the A1 auth foundation against a real MySQL 8
// (the docker-compose `mysql` service). Run with:
//
//	go test -tags=integration ./internal/handlers/
//
// They require a reachable MySQL 8 with the dev DB (`make docker-up` from nexus-platform/).
// Set TEST_DATABASE_URL to override the DSN; otherwise the local compose DSN is used (see
// dsn() in admin_entity_handler_integration_test.go, shared across this package).
//
// Coverage (A1 ONLY — no AuthMiddleware / X-Tenant-Id, per the slice scope):
//   - config.LoadJWTConfig reads + validates the JWT settings from the environment;
//   - auth.TokenService issue→verify roundtrip with the env-loaded secret/TTLs;
//   - handlers.AuthHandler POST /auth/login against DB-backed user rows: a valid login
//     returns a token pair (no password-hash leak) whose access token verifies back to the
//     authenticated subject, and every rejection (wrong password, unknown user, cross-tenant,
//     disabled account, missing field, malformed body) maps to the correct status code.
//
// The fixed system tables `tenants` and `users` are GORM-managed (see internal/auth/user.go);
// they come from the real golang-migrate chain (run `make migrate-up` against the dev DB), and the
// harness seeds rows with real bcrypt digests so the flow exercises the production code paths end
// to end, not a stub. It does NOT AutoMigrate (see seedAuthFixtures — bug #1/#4 regression net).
package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/config"
	"nexus/platform/internal/services"
)

// Stable identifiers for the seeded auth fixtures. UUID-shaped to match the CHAR(36)
// columns and to stay clearly distinct from any real data in the dev DB.
const (
	authTenantAID   = "a1a1a1a1-0000-0000-0000-000000000001"
	authTenantBID   = "a1a1a1a1-0000-0000-0000-000000000002"
	authTenantASlug = "it-acme"
	authTenantBSlug = "it-globex"

	authUserEmail = "alice@it-acme.test"
	authPassword  = "Sup3r-Secret-Pw!"

	// A real (non-placeholder, >=32 char) secret so config.LoadJWTConfig accepts it.
	itJWTSecret = "integration-test-secret-0123456789"
)

// authGormDB opens a GORM connection to the dev DB, skipping (not failing) when MySQL
// is unreachable so the suite degrades gracefully in environments without docker.
func authGormDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(gormmysql.Open(dsn()), &gorm.Config{})
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping A1 integration test", err)
	}
	return gdb
}

// seedAuthFixtures inserts a clean fixture set into the fixed tenants/users tables of the
// migration-driven dev DB: two tenants (A active, B active) and, in tenant A, one active user
// (alice) plus one disabled user (bob). Returns a cleanup that removes exactly the seeded rows.
// Idempotent: it deletes any leftovers from a prior failed run before inserting.
//
// It does NOT AutoMigrate. The tenants/users/refresh_tokens schema comes ENTIRELY from the real
// golang-migrate chain (000003 auth_a1 + 000007 tenants.deleted_at), exactly as production deploys
// it — run `make migrate-up` against the dev DB before this suite. Removing the former
// `AutoMigrate(&auth.Tenant{}, &auth.User{}, &auth.RefreshToken{})` line is deliberate (bug #1/#4):
// AutoMigrate silently re-added the tenants.deleted_at column the GORM model needs, which DEFEATED
// the regression net — a broken or reverted 000007 would still have passed. Now a real
// tenants-schema drift fails the GORM Create below ("Unknown column 'deleted_at'") instead of being
// masked. (Lesson A1/A2: AutoMigrate-in-test is NOT a migration.)
func seedAuthFixtures(t *testing.T, gdb *gorm.DB) func() {
	t.Helper()

	clean := func() {
		// Hard-delete (Unscoped) so soft-delete doesn't leave rows that collide on re-seed.
		// refresh_tokens first (it references the seeded users/tenants conceptually); the table
		// has no GORM-managed FK so order is not strictly enforced, but we clear it for hygiene.
		gdb.Where("tenant_id IN ?", []string{authTenantAID, authTenantBID}).Delete(&auth.RefreshToken{})
		gdb.Unscoped().Where("tenant_id IN ?", []string{authTenantAID, authTenantBID}).Delete(&auth.User{})
		gdb.Unscoped().Where("id IN ?", []string{authTenantAID, authTenantBID}).Delete(&auth.Tenant{})
	}
	clean()

	now := time.Now()
	tenants := []auth.Tenant{
		{ID: authTenantAID, Slug: authTenantASlug, Name: "Acme (IT)", Status: "active", CreatedAt: now, UpdatedAt: now},
		{ID: authTenantBID, Slug: authTenantBSlug, Name: "Globex (IT)", Status: "active", CreatedAt: now, UpdatedAt: now},
	}
	if err := gdb.Create(&tenants).Error; err != nil {
		t.Fatalf("seed tenants: %v", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(authPassword), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt hash: %v", err)
	}
	users := []auth.User{
		{
			ID: uuid.NewString(), TenantID: authTenantAID, Email: authUserEmail,
			PasswordHash: string(hash), FullName: "Alice IT", Status: "active",
			IsAdmin: true, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: uuid.NewString(), TenantID: authTenantAID, Email: "bob@it-acme.test",
			PasswordHash: string(hash), FullName: "Bob Disabled", Status: "disabled",
			IsAdmin: false, CreatedAt: now, UpdatedAt: now,
		},
	}
	if err := gdb.Create(&users).Error; err != nil {
		t.Fatalf("seed users: %v", err)
	}

	return clean
}

// newAuthRouter wires the REAL A1 stack — env-loaded JWT config → token service → GORM-backed
// user repository → auth service → handler — onto /api/v1, exactly as main.go does. Returns the
// router and the token service so JWT verification can be asserted against the same secret/TTLs.
func newAuthRouter(t *testing.T, gdb *gorm.DB) (*gin.Engine, *auth.TokenService) {
	t.Helper()

	// Drive config loading FROM THE ENVIRONMENT (task acceptance: "config loading from env").
	// t.Setenv auto-restores the prior value when the test finishes.
	t.Setenv("JWT_SECRET", itJWTSecret)
	t.Setenv("JWT_ACCESS_TTL", "15m")
	t.Setenv("JWT_REFRESH_TTL", "168h")

	jwtCfg, err := config.LoadJWTConfig()
	if err != nil {
		t.Fatalf("config.LoadJWTConfig from env: %v", err)
	}
	if jwtCfg.JWTSecret != itJWTSecret || jwtCfg.AccessTTL != 15*time.Minute || jwtCfg.RefreshTTL != 168*time.Hour {
		t.Fatalf("LoadJWTConfig mismatch: got secret=%q access=%s refresh=%s",
			jwtCfg.JWTSecret, jwtCfg.AccessTTL, jwtCfg.RefreshTTL)
	}

	tokenSvc, err := auth.NewTokenService(jwtCfg.JWTSecret, jwtCfg.AccessTTL, jwtCfg.RefreshTTL)
	if err != nil {
		t.Fatalf("auth.NewTokenService: %v", err)
	}
	userRepo := auth.NewUserRepository(gdb)
	refreshRepo := auth.NewRefreshTokenRepository(gdb)
	authSvc := services.NewAuthService(userRepo, refreshRepo, tokenSvc)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/v1")
	NewAuthHandler(authSvc, zap.NewNop()).RegisterRoutes(g)
	return r, tokenSvc
}

// postLogin issues POST /api/v1/auth/login with a JSON body. The tenant here is the request
// body slug — A1's login is the pre-auth entry point and intentionally takes NO X-Tenant-Id
// header (that header belongs to the post-A2 middleware path, which is out of scope here).
func postLogin(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// loginResponse mirrors the handler's success envelope: {"data": {accessToken, refreshToken, user}}.
type loginResponse struct {
	Data struct {
		AccessToken  string          `json:"accessToken"`
		RefreshToken string          `json:"refreshToken"`
		User         json.RawMessage `json:"user"`
	} `json:"data"`
	Error string `json:"error"`
	Code  string `json:"code"`
}

// TestA1LoginRoundTrip is the headline end-to-end case: a valid {tenant, email, password}
// against real DB-backed user data returns 200 with a token pair, the access token verifies
// back to the authenticated subject (JWT issue→verify roundtrip through the live handler),
// and the password hash never appears anywhere in the response (anti-leak, spec §5).
func TestA1LoginRoundTrip(t *testing.T) {
	gdb := authGormDB(t)
	cleanup := seedAuthFixtures(t, gdb)
	t.Cleanup(cleanup)

	r, tokenSvc := newAuthRouter(t, gdb)

	w := postLogin(t, r, `{"tenant":"`+authTenantASlug+`","email":"`+authUserEmail+`","password":"`+authPassword+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("login: got %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var resp loginResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode login response: %v; body=%s", err, w.Body.String())
	}
	if resp.Data.AccessToken == "" || resp.Data.RefreshToken == "" {
		t.Fatalf("missing token(s): access=%q refresh=%q", resp.Data.AccessToken, resp.Data.RefreshToken)
	}

	// Access token must verify with the SAME service the handler used, and carry the subject
	// of the seeded user (tenant A, admin) — proving the issue→verify roundtrip end to end.
	claims, err := tokenSvc.VerifyAccessToken(resp.Data.AccessToken)
	if err != nil {
		t.Fatalf("VerifyAccessToken on issued token: %v", err)
	}
	if claims.TenantID != authTenantAID {
		t.Errorf("access token tenant: got %q, want %q", claims.TenantID, authTenantAID)
	}
	if claims.UserID == "" {
		t.Error("access token has empty user id")
	}
	if !claims.IsAdmin {
		t.Error("access token IsAdmin=false; seeded user is admin")
	}

	// The refresh token must verify as a refresh token and NOT as an access token (type pinning).
	if _, err := tokenSvc.VerifyRefreshToken(resp.Data.RefreshToken); err != nil {
		t.Errorf("VerifyRefreshToken on issued refresh token: %v", err)
	}
	if _, err := tokenSvc.VerifyAccessToken(resp.Data.RefreshToken); err == nil {
		t.Error("refresh token verified as an access token; token-type pinning breached")
	}

	// The password hash must never be serialized — assert against the raw body, not just the struct.
	if got := w.Body.String(); containsHash(got) {
		t.Errorf("login response leaks a bcrypt hash: %s", got)
	}
	// The user view echoes the email but no password field at all.
	var user struct {
		Email        string `json:"email"`
		PasswordHash string `json:"passwordHash"`
		Password     string `json:"password"`
	}
	_ = json.Unmarshal(resp.Data.User, &user)
	if user.Email != authUserEmail {
		t.Errorf("user.email: got %q, want %q", user.Email, authUserEmail)
	}
	if user.PasswordHash != "" || user.Password != "" {
		t.Errorf("user view exposes a password field: %+v", user)
	}
}

// TestA1LoginRejections drives every authentication-failure path against real DB rows and
// asserts the generic, anti-enumeration mapping: wrong password / unknown user / unknown
// tenant / cross-tenant / disabled account all collapse to a single 401 with the SAME code,
// while bad input (missing field, malformed JSON) is a 400.
func TestA1LoginRejections(t *testing.T) {
	gdb := authGormDB(t)
	cleanup := seedAuthFixtures(t, gdb)
	t.Cleanup(cleanup)

	r, _ := newAuthRouter(t, gdb)

	cases := []struct {
		name     string
		body     string
		wantCode int
		wantErr  string // expected response "code" field
	}{
		{
			name:     "wrong password",
			body:     `{"tenant":"` + authTenantASlug + `","email":"` + authUserEmail + `","password":"wrong-password"}`,
			wantCode: http.StatusUnauthorized, wantErr: "INVALID_CREDENTIALS",
		},
		{
			name:     "unknown user in real tenant",
			body:     `{"tenant":"` + authTenantASlug + `","email":"nobody@it-acme.test","password":"` + authPassword + `"}`,
			wantCode: http.StatusUnauthorized, wantErr: "INVALID_CREDENTIALS",
		},
		{
			name:     "unknown tenant",
			body:     `{"tenant":"no-such-tenant","email":"` + authUserEmail + `","password":"` + authPassword + `"}`,
			wantCode: http.StatusUnauthorized, wantErr: "INVALID_CREDENTIALS",
		},
		{
			// Cross-tenant isolation: alice exists in tenant A only. Logging in via tenant B's
			// slug must fail — the user lookup is pinned to the resolved tenant_id, never global.
			name:     "cross-tenant (user A via tenant B slug)",
			body:     `{"tenant":"` + authTenantBSlug + `","email":"` + authUserEmail + `","password":"` + authPassword + `"}`,
			wantCode: http.StatusUnauthorized, wantErr: "INVALID_CREDENTIALS",
		},
		{
			// Disabled account: bob has a valid password but Status != "active".
			name:     "disabled account",
			body:     `{"tenant":"` + authTenantASlug + `","email":"bob@it-acme.test","password":"` + authPassword + `"}`,
			wantCode: http.StatusUnauthorized, wantErr: "INVALID_CREDENTIALS",
		},
		{
			// RM-06: well-formed JSON but semantically incomplete → 422, not 400.
			name:     "missing password field",
			body:     `{"tenant":"` + authTenantASlug + `","email":"` + authUserEmail + `"}`,
			wantCode: http.StatusUnprocessableEntity, wantErr: "LOGIN_FIELD_REQUIRED",
		},
		{
			// RM-06: well-formed JSON with empty required field → 422, not 400.
			name:     "empty tenant",
			body:     `{"tenant":"","email":"` + authUserEmail + `","password":"` + authPassword + `"}`,
			wantCode: http.StatusUnprocessableEntity, wantErr: "LOGIN_FIELD_REQUIRED",
		},
		{
			name:     "malformed JSON body",
			body:     `{not-json`,
			wantCode: http.StatusBadRequest, wantErr: "INVALID_BODY",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := postLogin(t, r, tc.body)
			if w.Code != tc.wantCode {
				t.Fatalf("got %d, want %d; body=%s", w.Code, tc.wantCode, w.Body.String())
			}
			// A rejection must NEVER carry a token or a password hash.
			var resp loginResponse
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if resp.Data.AccessToken != "" || resp.Data.RefreshToken != "" {
				t.Errorf("rejection returned a token: %s", w.Body.String())
			}
			if containsHash(w.Body.String()) {
				t.Errorf("rejection response leaks a hash: %s", w.Body.String())
			}
			if tc.wantErr != "" && resp.Code != tc.wantErr {
				t.Errorf("error code: got %q, want %q; body=%s", resp.Code, tc.wantErr, w.Body.String())
			}
		})
	}
}

// TestA1LoadJWTConfigFromEnv exercises the env-driven config layer directly (acceptance:
// "config loading from env"): a well-formed environment yields the expected JWTConfig, and
// the documented fail-fast cases (placeholder secret, too-short secret, refresh < access)
// each return an error rather than booting a misconfigured service (spec §5).
func TestA1LoadJWTConfigFromEnv(t *testing.T) {
	t.Run("valid env", func(t *testing.T) {
		t.Setenv("JWT_SECRET", itJWTSecret)
		t.Setenv("JWT_ACCESS_TTL", "10m")
		t.Setenv("JWT_REFRESH_TTL", "240h")
		cfg, err := config.LoadJWTConfig()
		if err != nil {
			t.Fatalf("valid env should load: %v", err)
		}
		if cfg.JWTSecret != itJWTSecret || cfg.AccessTTL != 10*time.Minute || cfg.RefreshTTL != 240*time.Hour {
			t.Errorf("loaded cfg mismatch: %+v", cfg)
		}
	})

	t.Run("defaults when TTLs unset", func(t *testing.T) {
		t.Setenv("JWT_SECRET", itJWTSecret)
		t.Setenv("JWT_ACCESS_TTL", "")
		t.Setenv("JWT_REFRESH_TTL", "")
		cfg, err := config.LoadJWTConfig()
		if err != nil {
			t.Fatalf("missing TTLs should fall back to defaults: %v", err)
		}
		if cfg.AccessTTL != 15*time.Minute || cfg.RefreshTTL != 7*24*time.Hour {
			t.Errorf("default TTLs wrong: access=%s refresh=%s", cfg.AccessTTL, cfg.RefreshTTL)
		}
	})

	t.Run("placeholder secret rejected", func(t *testing.T) {
		t.Setenv("JWT_SECRET", "changeme")
		t.Setenv("JWT_ACCESS_TTL", "15m")
		t.Setenv("JWT_REFRESH_TTL", "168h")
		if _, err := config.LoadJWTConfig(); err == nil {
			t.Error("placeholder secret must be rejected")
		}
	})

	t.Run("short secret rejected", func(t *testing.T) {
		t.Setenv("JWT_SECRET", "tooshort")
		t.Setenv("JWT_ACCESS_TTL", "15m")
		t.Setenv("JWT_REFRESH_TTL", "168h")
		if _, err := config.LoadJWTConfig(); err == nil {
			t.Error("too-short secret must be rejected")
		}
	})

	t.Run("refresh shorter than access rejected", func(t *testing.T) {
		t.Setenv("JWT_SECRET", itJWTSecret)
		t.Setenv("JWT_ACCESS_TTL", "1h")
		t.Setenv("JWT_REFRESH_TTL", "10m")
		if _, err := config.LoadJWTConfig(); err == nil {
			t.Error("refresh < access must be rejected")
		}
	})
}

// containsHash reports whether s contains a bcrypt digest prefix ($2a$/$2b$/$2y$), used to
// guard every response body against accidentally serializing the stored password hash.
func containsHash(s string) bool {
	for _, p := range []string{"$2a$", "$2b$", "$2y$"} {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
