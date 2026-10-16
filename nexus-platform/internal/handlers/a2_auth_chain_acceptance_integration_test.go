//go:build integration

// A2 acceptance — AuthMiddleware wired into the chain + tenant derived strictly from the JWT
// claim. Run with:
//
//	go test -tags=integration ./internal/handlers/
//
// These tests stand up the COMPLETE production wiring via handlers.RegisterAPIRoutes (the same
// builder cmd/server/main.go uses): the public /auth/* group, then the protected group fronted by
// AuthMiddleware → RequireTenant carrying the metadata/admin/dynamic-record handlers. A real login
// mints the Bearer access token, so the whole path — login → token → middleware verify → handler →
// tenant-scoped query — is exercised end to end against MySQL 8.
//
// Acceptance covered (../specs/001-auth-security/spec.md §A2, §4, §5):
//   - a protected route reads tenant FROM THE CLAIM only (Bearer from a real login works);
//   - spoofing the tenant via header / query / body has NO effect (the verified claim wins,
//     and a client-supplied tenant for someone else's data is ignored — isolation holds);
//   - middleware ORDERING is enforced: an unauthenticated request to a protected route is
//     rejected with 401 BEFORE the handler runs, while the public /auth/login stays reachable
//     without a token (auth gate sits before business logic, never in front of the login).
package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/config"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

const a2Entity = "it_a2_notes"

// a2TenantBEmail is the active tenant-B user this suite seeds + logs in to obtain a tenant-B token.
const a2TenantBEmail = "user-b@it-globex.test"

// seedAndLoginTenantB inserts an active user in tenant B (the base fixtures seed users in tenant A
// only), logs it in, and returns its access token — the credential whose verified claim scopes
// requests to tenant B. The seeded user is removed on cleanup.
func seedAndLoginTenantB(t *testing.T, r *gin.Engine, gdb *gorm.DB) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(authPassword), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt hash: %v", err)
	}
	userB := auth.User{
		ID: uuid.NewString(), TenantID: authTenantBID, Email: a2TenantBEmail,
		PasswordHash: string(hash), FullName: "User B", Status: "active",
		IsAdmin: true, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := gdb.Create(&userB).Error; err != nil {
		t.Fatalf("seed tenant-B user: %v", err)
	}
	t.Cleanup(func() {
		gdb.Where("tenant_id = ?", authTenantBID).Delete(&auth.RefreshToken{})
		gdb.Unscoped().Where("id = ?", userB.ID).Delete(&auth.User{})
	})
	login := itLogin(t, r, authTenantBSlug, a2TenantBEmail, authPassword)
	return login.Data.AccessToken
}

// newProductionRouter wires the FULL /api/v1 surface exactly as main.go does — auth handler on the
// public group, metadata/admin/dynamic-record handlers on the protected group behind
// AuthMiddleware → RequireTenant — over the real auth + metadata stacks against MySQL 8. It returns
// the engine plus a sqlx handle for fixture cleanup. The JWT env is set here so the verifier's
// secret stays in lockstep with the tokens issued by login.
func newProductionRouter(t *testing.T, gdb *gorm.DB) (*gin.Engine, *sqlx.DB) {
	t.Helper()
	sx, err := sqlx.Connect("mysql", dsn())
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping", err)
	}

	// Real A1 auth wiring (env-loaded JWT config → token service → repos → auth service → handler),
	// mirroring newAuthRouter so login issues tokens the mounted middleware verifies with one secret.
	t.Setenv("JWT_SECRET", itJWTSecret)
	t.Setenv("JWT_ACCESS_TTL", "15m")
	t.Setenv("JWT_REFRESH_TTL", "168h")
	jwtCfg, err := config.LoadJWTConfig()
	if err != nil {
		t.Fatalf("config.LoadJWTConfig: %v", err)
	}
	tokenSvc, err := auth.NewTokenService(jwtCfg.JWTSecret, jwtCfg.AccessTTL, jwtCfg.RefreshTTL)
	if err != nil {
		t.Fatalf("auth.NewTokenService: %v", err)
	}
	authSvc := services.NewAuthService(auth.NewUserRepository(gdb), auth.NewRefreshTokenRepository(gdb), tokenSvc)

	meta := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sx)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), tokenSvc, nil, APIHandlers{
		Auth:     NewAuthHandler(authSvc, zap.NewNop()),
		Metadata: NewMetadataHandler(gdb, zap.NewNop()),
		Admin:    NewAdminEntityHandler(services.NewEntityService(dyn, meta), services.NewFieldService(dyn, meta), zap.NewNop()),
		Record:   NewDynamicRecordHandler(services.NewRecordService(meta, dyn), zap.NewNop()),
	}, zap.NewNop())
	return r, sx
}

// a2GormDB opens a GORM connection for the A2 acceptance suite, skipping when MySQL is unreachable.
func a2GormDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(gormmysql.Open(dsn()), &gorm.Config{})
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping A2 acceptance", err)
	}
	return gdb
}

// doAuthed issues a JSON request carrying the given Bearer access token, plus any extra headers
// (used to inject a spoofed X-Tenant-Id). An empty token sends no Authorization header.
func doAuthed(t *testing.T, r *gin.Engine, method, path, token string, body any, extraHeaders map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		reader = bytes.NewReader(buf)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func cleanupA2Entity(sx *sqlx.DB) {
	for _, ten := range []string{authTenantAID, authTenantBID} {
		sx.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		         WHERE e.tenant_id = ? AND e.name = ?`, ten, a2Entity)
		sx.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name = ?`, ten, a2Entity)
	}
	sx.Exec("DROP TABLE IF EXISTS `obj_" + a2Entity + "`")
}

// TestA2_ProtectedRouteReadsTenantFromClaimOnly is the headline A2 acceptance: after a real login,
// the Bearer access token is the ONLY thing that scopes a protected route to a tenant. It proves
// (1) the protected route works with the login token, (2) a header/query/body tenant spoof for a
// DIFFERENT tenant is ignored — the claim wins, isolation holds — and (3) middleware ordering: an
// unauthenticated request to the same protected route is rejected with 401 before the handler runs.
func TestA2_ProtectedRouteReadsTenantFromClaimOnly(t *testing.T) {
	gdb := a2GormDB(t)
	cleanupFixtures := seedAuthFixtures(t, gdb)
	t.Cleanup(cleanupFixtures)

	r, sx := newProductionRouter(t, gdb)
	cleanupA2Entity(sx)
	t.Cleanup(func() { cleanupA2Entity(sx) })

	// Log in as tenant A (alice) and tenant B (a seeded user we add for this test).
	loginA := itLogin(t, r, authTenantASlug, authUserEmail, authPassword)
	tokenA := loginA.Data.AccessToken
	tokenB := seedAndLoginTenantB(t, r, gdb)

	entityBody := map[string]any{
		"name":  a2Entity,
		"label": "Ghi chú",
		"fields": []map[string]any{
			{"name": "title", "label": "Tiêu đề", "fieldType": "text", "isRequired": true},
		},
	}

	// 1) CLAIM-ONLY: with tenant A's token, the protected admin + record routes work and operate
	//    under tenant A. No X-Tenant-Id / ?tenantId is sent anywhere — the tenant is the claim.
	if w := doAuthed(t, r, http.MethodPost, "/api/v1/admin/entities", tokenA, entityBody, nil); w.Code != http.StatusCreated {
		t.Fatalf("create entity as A: %d %s", w.Code, w.Body.String())
	}
	// Tenant B registers the same entity (shared obj_ table, separated by tenant_id).
	if w := doAuthed(t, r, http.MethodPost, "/api/v1/admin/entities", tokenB, entityBody, nil); w.Code != http.StatusCreated {
		t.Fatalf("create entity as B: %d %s", w.Code, w.Body.String())
	}
	// A inserts a private record.
	wA := doAuthed(t, r, http.MethodPost, "/api/v1/"+a2Entity, tokenA, map[string]any{"title": "A-secret"}, nil)
	if wA.Code != http.StatusCreated {
		t.Fatalf("A insert: %d %s", wA.Code, wA.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(wA.Body.Bytes(), &created)
	idA := created.Data.ID

	// 2a) SPOOF VIA HEADER: tenant B's token + an X-Tenant-Id header claiming tenant A must NOT
	//     let B see A's data. The verified claim (B) wins; the header is ignored. B sees 0 records.
	spoofHeader := map[string]string{"X-Tenant-Id": authTenantAID}
	wList := doAuthed(t, r, http.MethodGet, "/api/v1/"+a2Entity, tokenB, nil, spoofHeader)
	if wList.Code != http.StatusOK {
		t.Fatalf("B list with spoof header: %d %s", wList.Code, wList.Body.String())
	}
	var list struct {
		Pagination struct {
			Total int `json:"total"`
		} `json:"pagination"`
	}
	json.Unmarshal(wList.Body.Bytes(), &list)
	if list.Pagination.Total != 0 {
		t.Errorf("spoof via X-Tenant-Id header leaked %d records to tenant B; want 0", list.Pagination.Total)
	}

	// 2b) SPOOF VIA QUERY: ?tenantId=A on the metadata route, authenticated as B, must list B's
	//     entities only (B registered a2Entity), never A's — the query param is ignored.
	wMeta := doAuthed(t, r, http.MethodGet, "/api/v1/metadata/entities?tenantId="+authTenantAID, tokenB, nil, nil)
	if wMeta.Code != http.StatusOK {
		t.Fatalf("B metadata with spoof query: %d %s", wMeta.Code, wMeta.Body.String())
	}
	var meta struct {
		Meta struct {
			TenantID string `json:"tenantId"`
		} `json:"meta"`
	}
	json.Unmarshal(wMeta.Body.Bytes(), &meta)
	if meta.Meta.TenantID != authTenantBID {
		t.Errorf("metadata tenant came from ?tenantId=, not the claim: got %q, want %q (B)", meta.Meta.TenantID, authTenantBID)
	}

	// 2c) SPOOF VIA BODY + HEADER: B tries to read A's record by id, with a header AND body field
	//     claiming tenant A. Must be 404 — the record is invisible to B regardless of spoof inputs.
	wGet := doAuthed(t, r, http.MethodGet, "/api/v1/"+a2Entity+"/"+idA, tokenB, map[string]any{"tenant_id": authTenantAID}, spoofHeader)
	if wGet.Code != http.StatusNotFound {
		t.Errorf("B reading A's record with spoof header+body: %d; want 404 (isolation breached)", wGet.Code)
	}

	// Sanity: tenant A, using only its claim, still sees its own record.
	wAList := doAuthed(t, r, http.MethodGet, "/api/v1/"+a2Entity, tokenA, nil, nil)
	json.Unmarshal(wAList.Body.Bytes(), &list)
	if list.Pagination.Total != 1 {
		t.Errorf("tenant A sees %d records via its own claim; want 1", list.Pagination.Total)
	}

	// 3) MIDDLEWARE ORDERING: an unauthenticated request to a protected route is rejected with 401
	//    BEFORE the handler runs (auth gate precedes business logic), while the PUBLIC /auth/login
	//    is reachable without a token (the auth gate must not sit in front of the credential entry).
	if w := doAuthed(t, r, http.MethodGet, "/api/v1/"+a2Entity, "", nil, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated protected route: %d; want 401 (auth must run before handler)", w.Code)
	}
	if w := doAuthed(t, r, http.MethodPost, "/api/v1/admin/entities", "", entityBody, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated admin route: %d; want 401", w.Code)
	}
	// Public login still works with NO token — proves the auth gate is on the protected group only.
	if w := postLogin(t, r, `{"tenant":"`+authTenantASlug+`","email":"`+authUserEmail+`","password":"`+authPassword+`"}`); w.Code != http.StatusOK {
		t.Errorf("public /auth/login behind no token: %d; want 200 (must stay public)", w.Code)
	}
}
