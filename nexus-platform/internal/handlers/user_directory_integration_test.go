//go:build integration

// GET /users/directory — DB-backed acceptance (docs/NEXUS-MOT-XUONG-2026-07-29.md §0b: the
// owner-change FE-gap). Proves, against a REAL migrated MySQL 8, the three guarantees that live
// in the SQL WHERE clause (auth.UserRepository.ListDirectory) and therefore CANNOT be proven by a
// fake-repo unit test: tenant isolation, disabled-user exclusion, and ?q= substring matching.
//
// Run with: go test -tags=integration ./internal/handlers/ -race -run TestUserDirectoryIntegration
// Requires a reachable MySQL 8 (docker-compose `mysql` service / `make docker-up`). Reuses
// setupMigratedDB/throwawayDSN (rm25_27_toctou_concurrency_integration_test.go) so this suite
// runs against its OWN throwaway, freshly-migrated database — never the shared dev DB.
package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"go.uber.org/zap"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// seedDirTenant inserts a tenant row via a raw INSERT (mirrors a4SeedTenant's explicit-columns
// honesty — independent of whatever GORM model fields the auth.Tenant struct happens to declare).
func seedDirTenant(t *testing.T, gdb *gorm.DB, slug string) string {
	t.Helper()
	id := uuid.NewString()
	if err := gdb.Exec(
		"INSERT INTO tenants (id, slug, name, status, created_at, updated_at) VALUES (?, ?, ?, 'active', NOW(), NOW())",
		id, slug, slug,
	).Error; err != nil {
		t.Fatalf("seed tenant %s: %v", slug, err)
	}
	return id
}

// seedDirUser inserts a user via GORM Create (CreatedAt/UpdatedAt auto-populated by convention).
func seedDirUser(t *testing.T, gdb *gorm.DB, tenantID, email, fullName, status string) {
	t.Helper()
	u := auth.User{
		ID: uuid.NewString(), TenantID: tenantID, Email: email, PasswordHash: "x",
		FullName: fullName, Status: status, IsAdmin: false,
	}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("seed user %s: %v", email, err)
	}
}

// dirDirectoryEmails performs GET /api/v1/users/directory?<query> as tokA and returns the set of
// emails present in the response.
func dirDirectoryEmails(t *testing.T, r *gin.Engine, query string) map[string]bool {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/directory"+query, nil)
	req.Header.Set("Authorization", "Bearer tokA")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /users/directory%s status = %d, want 200; body=%s", query, rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data []struct {
			Email string `json:"email"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode %s: %v; body=%s", query, err, rec.Body.String())
	}
	out := map[string]bool{}
	for _, u := range envelope.Data {
		out[u.Email] = true
	}
	return out
}

func TestUserDirectoryIntegration_TenantIsolation_DisabledFilter_QSearch(t *testing.T) {
	sx, dbName := setupMigratedDB(t)
	gdb, err := gorm.Open(gormmysql.Open(throwawayDSN(t, dbName)), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open %s: %v", dbName, err)
	}
	sqlDB, _ := gdb.DB()
	t.Cleanup(func() {
		if sqlDB != nil {
			_ = sqlDB.Close()
		}
	})
	_ = sx // sx (sqlx handle) is only needed to trigger setupMigratedDB's migration + cleanup

	tenantA := seedDirTenant(t, gdb, "dir-tenant-a")
	tenantB := seedDirTenant(t, gdb, "dir-tenant-b")

	seedDirUser(t, gdb, tenantA, "alice@a.test", "Alice Nguyen", "active")
	seedDirUser(t, gdb, tenantA, "bob@a.test", "Bob Tran", "active")
	seedDirUser(t, gdb, tenantA, "carol@a.test", "Carol Le", "disabled") // must NEVER appear
	seedDirUser(t, gdb, tenantB, "dave@b.test", "Dave Pham", "active")   // different tenant, must NEVER appear
	seedDirUser(t, gdb, tenantA, "nguyenvana@a.test", "Nguyễn Văn A", "active")

	userRepo := auth.NewUserRepository(gdb)
	h := NewUserDirectoryHandler(services.NewUserDirectoryService(userRepo), zap.NewNop())

	gin.SetMode(gin.TestMode)
	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		"tokA": claimsFor(tenantA, "role-1", false), // non-admin caller — the whole point of this endpoint
	}}
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), ver, nil, APIHandlers{UserDirectory: h}, zap.NewNop())

	// 1) Tenant isolation + disabled-user exclusion: tenant A's caller sees Alice + Bob only.
	got := dirDirectoryEmails(t, r, "?limit=50")
	if !got["alice@a.test"] || !got["bob@a.test"] {
		t.Errorf("expected alice+bob present, got %v", got)
	}
	if got["carol@a.test"] {
		t.Errorf("disabled user carol leaked into the directory: %v", got)
	}
	if got["dave@b.test"] {
		t.Errorf("tenant B's user dave leaked into tenant A's directory: %v", got)
	}
	if len(got) != 3 {
		t.Errorf("expected exactly 3 active tenant-A users (alice, bob, nguyenvana), got %v", got)
	}

	// 2) ?q= matches by email substring (all three active tenant-A seed users share the @a.test
	// domain — alice, bob, AND nguyenvana).
	got = dirDirectoryEmails(t, r, "?q=a.test")
	if len(got) != 3 || !got["alice@a.test"] || !got["bob@a.test"] || !got["nguyenvana@a.test"] {
		t.Errorf("q=a.test (email domain substring) = %v, want exactly alice+bob+nguyenvana", got)
	}

	// 3) ?q= matches by full-name substring, and NEVER surfaces a disabled user even when q
	//    matches their name — disabled-exclusion and q-filter compose with AND, not OR.
	got = dirDirectoryEmails(t, r, "?q=Bob")
	if len(got) != 1 || !got["bob@a.test"] {
		t.Errorf("q=Bob (full-name substring) = %v, want exactly bob@a.test", got)
	}
	got = dirDirectoryEmails(t, r, "?q=Carol")
	if len(got) != 0 {
		t.Errorf("q=Carol (disabled user's own name) = %v, want empty — disabled must stay excluded", got)
	}

	// 4) Non-matching q returns an empty (not error) result.
	got = dirDirectoryEmails(t, r, "?q=zzz-no-such-person")
	if len(got) != 0 {
		t.Errorf("q=zzz-no-such-person = %v, want empty", got)
	}

	// 5) ?q= is case-INSENSITIVE — a real user types lowercase. This is the part the doc comment
	// on auth.UserRepository.ListDirectory only ASSERTS from the table's collation; prove it
	// empirically rather than trust the inference (a wrong assumption here means the picker
	// silently returns nothing for the common case of a lowercase-typed query).
	got = dirDirectoryEmails(t, r, "?q=bob")
	if len(got) != 1 || !got["bob@a.test"] {
		t.Errorf("q=bob (lowercase, full-name is \"Bob Tran\") = %v, want exactly bob@a.test — utf8mb4_unicode_ci should fold case", got)
	}
	got = dirDirectoryEmails(t, r, "?q=BOB")
	if len(got) != 1 || !got["bob@a.test"] {
		t.Errorf("q=BOB (uppercase) = %v, want exactly bob@a.test", got)
	}

	// 6) ?q= WITHOUT Vietnamese diacritics against a name that HAS them ("Nguyễn Văn A") — proves
	// (or disproves) accent-folding under utf8mb4_unicode_ci empirically. Founder's real data is
	// Vietnamese, so this is a genuine usability question, not a formality. OBSERVED (this run):
	// "nguyen" (no diacritics) matches BOTH nguyenvana@a.test (diacritic form "Nguyễn Văn A",
	// folded to the plain base letters) AND alice@a.test (her full name is literally "Alice
	// Nguyen" — a plain-ASCII substring hit, unrelated to folding). Both are correct: this
	// confirms utf8mb4_unicode_ci DOES fold accents for search purposes, using a fixture where
	// the same surname coincidentally appears in both transliterated and diacritic form — a
	// realistic Vietnamese-names scenario, not a contrived one. If this assertion ever needs to
	// shrink, that is a real product regression to document, not silently discover later.
	got = dirDirectoryEmails(t, r, "?q=nguyen")
	if len(got) != 2 || !got["nguyenvana@a.test"] || !got["alice@a.test"] {
		t.Errorf("q=nguyen (no diacritics) = %v, want exactly alice@a.test + nguyenvana@a.test — utf8mb4_unicode_ci should fold base-letter accents", got)
	}
}

// TestUserDirectoryIntegration_SoftDeletedUserExcluded: a user removed through the REAL admin
// delete path (auth.UserRepository.SoftDeleteUser — GORM soft delete: deleted_at set, status
// stays 'active') must vanish from the directory. The directory feeds the `user`-field picker
// (UserPicker) and owner reassignment; the record-time check (dynrepo.BatchActiveUserExists)
// requires deleted_at IS NULL, so a deleted person listed here is one the picker offers and Save
// then refuses with 422 RECORD_USER_INVALID. ListDirectory queries Table("users"), which does NOT
// get GORM's soft-delete scope — the filter has to be explicit.
func TestUserDirectoryIntegration_SoftDeletedUserExcluded(t *testing.T) {
	_, dbName := setupMigratedDB(t)
	gdb, err := gorm.Open(gormmysql.Open(throwawayDSN(t, dbName)), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open %s: %v", dbName, err)
	}
	sqlDB, _ := gdb.DB()
	t.Cleanup(func() {
		if sqlDB != nil {
			_ = sqlDB.Close()
		}
	})

	tenantA := seedDirTenant(t, gdb, "dir-softdel-a")
	seedDirUser(t, gdb, tenantA, "alice@a.test", "Alice Nguyen", "active")
	seedDirUser(t, gdb, tenantA, "gone@a.test", "Gone Person", "active")

	userRepo := auth.NewUserRepository(gdb)
	var gone auth.User
	if err := gdb.Where("tenant_id = ? AND email = ?", tenantA, "gone@a.test").First(&gone).Error; err != nil {
		t.Fatalf("load gone@a.test: %v", err)
	}
	if err := userRepo.SoftDeleteUser(t.Context(), tenantA, gone.ID); err != nil {
		t.Fatalf("SoftDeleteUser: %v", err)
	}

	h := NewUserDirectoryHandler(services.NewUserDirectoryService(userRepo), zap.NewNop())
	gin.SetMode(gin.TestMode)
	ver := &stubVerifier{byToken: map[string]*auth.Claims{"tokA": claimsFor(tenantA, "role-1", false)}}
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), ver, nil, APIHandlers{UserDirectory: h}, zap.NewNop())

	got := dirDirectoryEmails(t, r, "?limit=50")
	if got["gone@a.test"] {
		t.Errorf("soft-deleted user gone@a.test is still offered by the directory: %v", got)
	}
	if len(got) != 1 || !got["alice@a.test"] {
		t.Errorf("directory = %v, want exactly alice@a.test", got)
	}
	if got = dirDirectoryEmails(t, r, "?q=Gone"); len(got) != 0 {
		t.Errorf("q=Gone (deleted user's own name) = %v, want empty", got)
	}
}
