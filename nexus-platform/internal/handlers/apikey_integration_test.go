//go:build integration

// APIKEY-1 integration — full machine-identity round-trip on real MySQL through the REAL
// migration chain (000045 sys_api_keys included) and the production middleware order:
//
//	issue (admin JWT) → use the raw secret as Bearer on the dynamic /:entity surface (the key
//	rides Auth → Tenant → ACL exactly like a JWT, scoped by its role's grants) → per-action
//	deny (no delete grant → 403) → admin surface deny (IsAdmin floor → 403) → tenant isolation
//	(key of tenant A never sees tenant B rows in the SHARED obj_ table) → last_used_at
//	write-behind lands → revoke → the SAME request now answers the SAME opaque 401.
//
// Run:  go test -tags=integration ./internal/handlers/ -run TestAPIKey_MachineIdentity
package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/config"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

// newAPIKeyITRouter mirrors newA4Router (production wiring, ACL enabled) but swaps the Bearer
// verifier for the COMPOSITE (JWT + API key) and mounts the /admin/api-keys handler — i.e. the
// exact cmd/server/main.go shape after APIKEY-1.
// opts are appended after the production options (e.g. WithDataRateLimiter for #52).
func newAPIKeyITRouter(t *testing.T, gdb *gorm.DB, sx *sqlx.DB, opts ...RouteOption) *gin.Engine {
	t.Helper()

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

	userRepo := auth.NewUserRepository(gdb)
	refreshRepo := auth.NewRefreshTokenRepository(gdb)
	roleRepo := auth.NewRoleRepository(gdb)
	apiKeyRepo := auth.NewAPIKeyRepository(gdb)

	authSvc := services.NewAuthService(userRepo, refreshRepo, tokenSvc)
	apiKeySvc := services.NewAPIKeyService(apiKeyRepo, roleRepo)
	aclSvc := services.NewACLService(roleRepo)

	meta := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sx)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), services.NewCompositeVerifier(tokenSvc, apiKeySvc), aclSvc, APIHandlers{
		Auth:   NewAuthHandler(authSvc, zap.NewNop()),
		Admin:  NewAdminEntityHandler(services.NewEntityService(dyn, meta), services.NewFieldService(dyn, meta), zap.NewNop()),
		Record: NewDynamicRecordHandler(services.NewRecordService(meta, dyn), zap.NewNop()),
		APIKey: NewAdminAPIKeyHandler(apiKeySvc, zap.NewNop()),
	}, zap.NewNop(), append([]RouteOption{WithLiveSubject(services.NewCachedUserLoader(userRepo, services.DefaultUserClaimTTL))}, opts...)...)
	// WithLiveSubject mirrors production (cmd/server/main.go): the round-trip below is the only
	// end-to-end lock that an `nxk_` principal (no user row) is carved out of the #14 live re-read.
	return r
}

// itCreateEntity registers a dynamic entity (title text field) via the admin API.
func itCreateEntity(t *testing.T, r *gin.Engine, adminTok, name string) {
	t.Helper()
	w := doAuthed(t, r, http.MethodPost, "/api/v1/admin/entities", adminTok, map[string]any{
		"name": name, "label": name,
		"fields": []map[string]any{{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true}},
	}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create entity %s: %d %s", name, w.Code, w.Body.String())
	}
}

func TestAPIKey_MachineIdentity_RoundTrip(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	r := newAPIKeyITRouter(t, gdb, sx)

	const entity = "apikey_it_items"
	tenantA, tenantB := "ten-key-aaaa", "ten-key-bbbb"
	a4SeedTenant(t, gdb, tenantA, "key-ten-a", "Key Tenant A")
	a4SeedTenant(t, gdb, tenantB, "key-ten-b", "Key Tenant B")
	// Machine role in tenant A: view + create on the entity — deliberately NO delete, so the
	// per-action ACL deny is provable through the key.
	roleA := a4SeedRole(t, gdb, tenantA, "machine-role", []auth.RoleAction{
		grant(entity, "view"), grant(entity, "create"),
	})
	a4SeedUser(t, gdb, tenantA, "admin-a@key.test", true, nil)
	a4SeedUser(t, gdb, tenantB, "admin-b@key.test", true, nil)
	t.Cleanup(func() { a4DropEntity(sx, []string{tenantA, tenantB}, entity) })

	adminATok := itLogin(t, r, "key-ten-a", "admin-a@key.test", authPassword).Data.AccessToken
	adminBTok := itLogin(t, r, "key-ten-b", "admin-b@key.test", authPassword).Data.AccessToken

	// Same entity name in BOTH tenants → one shared obj_ table, rows split by tenant_id: the
	// strongest possible cross-tenant probe for the key's claims.
	itCreateEntity(t, r, adminATok, entity)
	itCreateEntity(t, r, adminBTok, entity)
	wB := doAuthed(t, r, http.MethodPost, "/api/v1/"+entity, adminBTok, map[string]any{"title": "tenant-b-secret-row"}, nil)
	if wB.Code != http.StatusCreated {
		t.Fatalf("seed tenant B record: %d %s", wB.Code, wB.Body.String())
	}

	// ---- issue (admin JWT) --------------------------------------------------------------
	wIssue := doAuthed(t, r, http.MethodPost, "/api/v1/admin/api-keys", adminATok,
		map[string]any{"name": "integration machine", "roleId": roleA}, nil)
	if wIssue.Code != http.StatusCreated {
		t.Fatalf("issue key: %d %s", wIssue.Code, wIssue.Body.String())
	}
	var issued struct {
		Data struct {
			APIKey struct {
				ID        string `json:"id"`
				KeyPrefix string `json:"keyPrefix"`
			} `json:"apiKey"`
			Secret string `json:"secret"`
		} `json:"data"`
	}
	if err := json.Unmarshal(wIssue.Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode issue: %v", err)
	}
	secret, keyID := issued.Data.Secret, issued.Data.APIKey.ID
	if !strings.HasPrefix(secret, "nxk_") {
		t.Fatalf("secret %q lacks nxk_ prefix", secret)
	}

	// Hash-only storage on the REAL table: the raw secret must not be in sys_api_keys.
	var storedHash string
	if err := sx.Get(&storedHash, "SELECT key_hash FROM sys_api_keys WHERE id = ?", keyID); err != nil {
		t.Fatalf("read stored hash: %v", err)
	}
	if storedHash == secret || storedHash != auth.HashRefreshToken(secret) {
		t.Fatalf("stored hash wrong: raw-persisted=%v", storedHash == secret)
	}
	// The admin list never echoes the secret.
	wList := doAuthed(t, r, http.MethodGet, "/api/v1/admin/api-keys", adminATok, nil, nil)
	if wList.Code != http.StatusOK || strings.Contains(wList.Body.String(), secret) || strings.Contains(wList.Body.String(), storedHash) {
		t.Fatalf("list leaked secret/hash (code %d)", wList.Code)
	}

	// ---- use the key on the dynamic surface (the crown mechanism) ------------------------
	wCreate := doAuthed(t, r, http.MethodPost, "/api/v1/"+entity, secret, map[string]any{"title": "from-machine"}, nil)
	if wCreate.Code != http.StatusCreated {
		t.Fatalf("key create record: %d %s", wCreate.Code, wCreate.Body.String())
	}
	var created recordCreateEnvelope
	_ = json.Unmarshal(wCreate.Body.Bytes(), &created)

	wGet := doAuthed(t, r, http.MethodGet, "/api/v1/"+entity, secret, nil, nil)
	if wGet.Code != http.StatusOK || !strings.Contains(wGet.Body.String(), "from-machine") {
		t.Fatalf("key list: %d %s", wGet.Code, wGet.Body.String())
	}
	// 🔒 Tenant isolation: tenant B's row lives in the SAME obj_ table but must be invisible.
	if strings.Contains(wGet.Body.String(), "tenant-b-secret-row") {
		t.Fatal("API key of tenant A saw a tenant B row — cross-tenant leak")
	}
	// 🔒 Per-action ACL applies to keys: the role has no delete grant → 403.
	wDel := doAuthed(t, r, http.MethodDelete, "/api/v1/"+entity+"/"+created.Data.ID, secret, nil, nil)
	if wDel.Code != http.StatusForbidden {
		t.Fatalf("key delete without grant: %d, want 403", wDel.Code)
	}
	// 🔒 IsAdmin floor: the key can never reach /admin/* — not even the api-key surface itself.
	wAdm := doAuthed(t, r, http.MethodGet, "/api/v1/admin/api-keys", secret, nil, nil)
	if wAdm.Code != http.StatusForbidden {
		t.Fatalf("key on /admin: %d, want 403", wAdm.Code)
	}

	// ---- last_used_at write-behind lands (async — poll briefly) --------------------------
	deadline := time.Now().Add(3 * time.Second)
	for {
		var lastUsed *time.Time
		if err := sx.Get(&lastUsed, "SELECT last_used_at FROM sys_api_keys WHERE id = ?", keyID); err != nil {
			t.Fatalf("read last_used_at: %v", err)
		}
		if lastUsed != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("last_used_at never set after a successful verify")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// ---- cross-tenant admin: tenant B cannot see or revoke tenant A's key ----------------
	wBRevoke := doAuthed(t, r, http.MethodDelete, "/api/v1/admin/api-keys/"+keyID, adminBTok, nil, nil)
	if wBRevoke.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant revoke: %d, want 404", wBRevoke.Code)
	}

	// ---- revoke → fail-closed on the next request, same opaque 401 -----------------------
	wRevoke := doAuthed(t, r, http.MethodDelete, "/api/v1/admin/api-keys/"+keyID, adminATok, nil, nil)
	if wRevoke.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", wRevoke.Code, wRevoke.Body.String())
	}
	wAfter := doAuthed(t, r, http.MethodGet, "/api/v1/"+entity, secret, nil, nil)
	if wAfter.Code != http.StatusUnauthorized || !strings.Contains(wAfter.Body.String(), "UNAUTHORIZED") {
		t.Fatalf("revoked key: %d %s, want the same opaque 401 as a bad JWT", wAfter.Code, wAfter.Body.String())
	}
	// Anti-enumeration: the revoked-key body is byte-identical to the garbage-JWT body.
	wGarbage := doAuthed(t, r, http.MethodGet, "/api/v1/"+entity, "garbage-token", nil, nil)
	if wGarbage.Body.String() != wAfter.Body.String() {
		t.Fatalf("401 bodies differ (revoked key %q vs bad JWT %q) — enumeration oracle", wAfter.Body.String(), wGarbage.Body.String())
	}
}
