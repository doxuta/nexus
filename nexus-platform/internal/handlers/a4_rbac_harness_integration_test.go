//go:build integration

// A4 RBAC integration harness — the shared plumbing every A4 integration test in this package
// builds on. Run with:
//
//	go test -tags=integration ./internal/handlers/
//
// It requires a reachable MySQL 8 (the docker-compose `mysql` service, `make docker-up`) AND the
// golang-migrate CLI on PATH or at $GOPATH/bin/migrate (set A4_MIGRATE_BIN to override).
//
// WHY A DEDICATED, MIGRATED DATABASE (coverage point T7, "bài học A1"):
// the rest of the integration suite AutoMigrates the GORM-managed system tables, which is fine for
// A1/A2 but would NOT exercise the hand-written A4 migration (000005_create_rbac_a4) — the FK
// users.role_id → acl_roles ON DELETE SET NULL, the acl_role_actions ON DELETE CASCADE, and the
// tenant/name unique keys all live in that SQL, not in GORM tags. So this harness stands up a
// SEPARATE, throwaway database and brings it up with the REAL `migrate up` (golang-migrate),
// applying migrations 000001..000005 exactly as production deploy does, BEFORE any ACL test runs.
// A bug in the migration (missing FK, wrong cascade) therefore fails these tests rather than being
// papered over by AutoMigrate.
//
// The migrated DB is named nexus_a4_it (override via A4_TEST_DB). It is dropped + recreated for
// each test (idempotent across reruns) and the schema is asserted to be at version >= 7, dirty=0
// before any test proceeds.
package handlers

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	"nexus/platform/internal/shares"
	"nexus/platform/internal/view"
)

// a4DBCounter gives each migrated-DB test a unique throwaway database name so tests never collide
// on the same schema/rows even if the runner interleaves them. The base name is overridable via
// A4_TEST_DB (a per-test numeric suffix is always appended).
var a4DBCounter int64

// a4BaseDBName is the throwaway-database prefix the A4 integration suite migrates and runs against.
// It is deliberately separate from the dev DB (nexus_platform) so a `migrate up` from a clean slate
// is meaningful and a botched run never touches real data.
func a4BaseDBName() string {
	if v := os.Getenv("A4_TEST_DB"); v != "" {
		return v
	}
	return "nexus_a4_it"
}

// a4UniqueDBName returns a process+test-unique throwaway DB name (base + pid + counter).
func a4UniqueDBName() string {
	n := atomic.AddInt64(&a4DBCounter, 1)
	return fmt.Sprintf("%s_%d_%d", a4BaseDBName(), os.Getpid(), n)
}

// splitDSN breaks the package go-sql-driver dsn() "user:pass@tcp(host:port)/db?params" into its
// credentials ("user:pass"), the "@tcp(host:port)/" host segment, and the "?params" query (or "").
// Both A4 DSN builders below reuse it so host/credentials stay in lockstep with dsn().
func splitDSN(t *testing.T) (creds, host, params string) {
	t.Helper()
	base := dsn()
	at := strings.Index(base, "@tcp(")
	if at < 0 {
		t.Fatalf("splitDSN: unexpected dsn shape %q", base)
	}
	creds = base[:at]
	rest := base[at:] // "@tcp(host:port)/db?params"
	slash := strings.Index(rest, "/")
	if slash < 0 {
		t.Fatalf("splitDSN: no db segment in dsn %q", base)
	}
	host = rest[:slash+1] // "@tcp(host:port)/"
	if q := strings.Index(rest, "?"); q >= 0 {
		params = rest[q:] // "?params"
	}
	return creds, host, params
}

// a4AdminDSN is a DSN with NO database selected (so DROP/CREATE DATABASE works).
func a4AdminDSN(t *testing.T) string {
	creds, host, params := splitDSN(t)
	return creds + host + params // "user:pass@tcp(host)/?params"
}

// a4GormDSN is the go-sql-driver DSN (for gorm/sqlx) pointing at the named throwaway DB.
func a4GormDSN(t *testing.T, dbName string) string {
	creds, host, params := splitDSN(t)
	return creds + host + dbName + params
}

// a4MigratedDSN is the golang-migrate connection string (mysql:// scheme) for the named throwaway DB.
func a4MigratedDSN(t *testing.T, dbName string) string {
	creds, host, params := splitDSN(t)
	return "mysql://" + creds + host + dbName + params
}

// a4MigrateBin locates the golang-migrate CLI: A4_MIGRATE_BIN, then PATH, then $GOPATH/bin/migrate.
func a4MigrateBin(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("A4_MIGRATE_BIN"); v != "" {
		return v
	}
	if p, err := exec.LookPath("migrate"); err == nil {
		return p
	}
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			gopath = filepath.Join(home, "go")
		}
	}
	cand := filepath.Join(gopath, "bin", "migrate")
	if _, err := os.Stat(cand); err == nil {
		return cand
	}
	return "" // signal "not found" → suite skips
}

// migrationsDir resolves the repo migrations/ directory from this test file's package
// (internal/handlers → ../../migrations).
func migrationsDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", "..", "migrations"))
}

// a4SetupMigratedDB creates a per-test throwaway DB and brings it up with the REAL golang-migrate
// CLI (T7). It asserts the resulting schema_migrations is version >= 7, dirty 0 — i.e. ALL of
// 000001..000007 applied cleanly through the production migration tool, not AutoMigrate — so the
// A4 RBAC tables (acl_roles, acl_role_actions), the users.role_id → acl_roles FK, AND the
// tenants.deleted_at column (000007) all come from the real SQL migration. It returns gorm + sqlx
// handles bound to the migrated DB. The suite is SKIPPED (not failed) when MySQL is unreachable or
// the migrate CLI is absent, so it degrades gracefully off-CI.
//
// HISTORY (bug #1/#4, now closed): the GORM model auth.Tenant declares a soft-delete column
// (DeletedAt gorm.DeletedAt) that migration 000003 did NOT create on the `tenants` table (only
// `users` got deleted_at). This harness previously reconciled that drift by AutoMigrating
// auth.Tenant after the real migration — which silently re-added the column and DEFEATED the
// regression net (a broken/reverted 000007 would still pass). Migration 000007 now adds
// tenants.deleted_at for real, so the AutoMigrate reconcile line is GONE: the harness is 100%
// migration-driven and a future tenants-schema drift fails the suite instead of being masked.
func a4SetupMigratedDB(t *testing.T) (*gorm.DB, *sqlx.DB) {
	t.Helper()

	migrateBin := a4MigrateBin(t)
	if migrateBin == "" {
		t.Skip("golang-migrate CLI not found (set A4_MIGRATE_BIN) — skipping A4 migration-backed integration")
	}

	dbName := a4UniqueDBName()

	// 1) Create the per-test throwaway DB via an admin connection (no DB selected).
	admin, err := sqlx.Connect("mysql", a4AdminDSN(t))
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping A4 integration", err)
	}
	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`"); err != nil {
		t.Fatalf("drop test db: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("create test db: %v", err)
	}
	_ = admin.Close()
	// Always drop the throwaway DB at the end (kept tidy across runs).
	t.Cleanup(func() {
		if a, err := sqlx.Connect("mysql", a4AdminDSN(t)); err == nil {
			a.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
			_ = a.Close()
		}
	})

	// 2) Run the REAL migration up (golang-migrate) — this is T7: not AutoMigrate. The A4 RBAC
	//    tables + the users.role_id FK are created here, from migrations/000005_create_rbac_a4.
	cmd := exec.Command(migrateBin, "-path", migrationsDir(t), "-database", a4MigratedDSN(t, dbName), "up")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("golang-migrate up failed: %v\n%s", err, out)
	}
	t.Logf("golang-migrate up (db=%s):\n%s", dbName, out)

	// 3) Connect gorm + sqlx to the migrated DB.
	gdb, err := gorm.Open(gormmysql.Open(a4GormDSN(t, dbName)), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open migrated db: %v", err)
	}
	sx, err := sqlx.Connect("mysql", a4GormDSN(t, dbName))
	if err != nil {
		t.Fatalf("sqlx open migrated db: %v", err)
	}

	// 4) Assert the migration tool left the schema at the latest version, dirty=0 (T7 acceptance) —
	//    proving the WHOLE schema, including the A4 RBAC tables (000005) AND tenants.deleted_at
	//    (000007), applied cleanly via golang-migrate, not AutoMigrate. The floor is 7 (000007 =
	//    the migration that adds tenants.deleted_at — the column the GORM auth.Tenant soft-delete
	//    field requires); the assertion is >= 7 rather than == 7 so it stays green as later
	//    migrations land. dirty=false is required either way: a dirty row means a migration
	//    half-applied. There is deliberately NO AutoMigrate reconcile step after this: the harness
	//    depends ENTIRELY on the real migration chain, so a future drift (e.g. 000007 broken or
	//    reverted, model gains a column without a migration) FAILS the suite instead of being
	//    silently papered over (bài học A1/A2: AutoMigrate-trong-test KHÔNG phải migration).
	var ver struct {
		Version int
		Dirty   bool
	}
	if err := sx.Get(&ver, "SELECT version, dirty FROM schema_migrations LIMIT 1"); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if ver.Version < 7 || ver.Dirty {
		t.Fatalf("schema_migrations = version %d dirty %v; want version >= 7 dirty false (migrations 000001..000007 did not all apply cleanly — note 000007 adds tenants.deleted_at)", ver.Version, ver.Dirty)
	}

	t.Cleanup(func() {
		sqlDB, _ := gdb.DB()
		if sqlDB != nil {
			_ = sqlDB.Close()
		}
		_ = sx.Close()
	})
	return gdb, sx
}

// a4Router is the full production /api/v1 surface wired EXACTLY as cmd/server/main.go does, but
// with the A4 ACL stage ENABLED (a real services.ACLService over auth.RoleRepository passed as the
// aclChecker) and the W1 revoker wired into the user service. This is what lets the ACL tests drive
// the genuine Auth → Tenant → ACL → handler chain and the role/W1 tests hit real CRUD.
type a4Router struct {
	engine   *gin.Engine
	gdb      *gorm.DB
	sx       *sqlx.DB
	tokenSvc *auth.TokenService
	roleRepo *auth.RoleRepository
}

// newA4Router builds the migrated-DB-backed production router with ACL enabled.
// extra lets a suite mount more handlers on the same production chain (#43: notes, files, approvals,
// relations), each wired by the caller exactly like main.go.
func newA4Router(t *testing.T, gdb *gorm.DB, sx *sqlx.DB, extra ...func(*APIHandlers)) *a4Router {
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

	// specs/055: the SAME ended-session guard main.go shares between the auth service and the live
	// Auth stage (WithSessionGuard below) — ending a session refuses its token on the next request.
	sessionGuard := services.NewCachedSessionGuard(refreshRepo, services.DefaultSessionGuardTTL)
	authSvc := services.NewAuthService(userRepo, refreshRepo, tokenSvc).WithSessionGuard(sessionGuard)
	// W1: revoke a user's refresh tokens when an admin changes role_id/is_admin or deletes them.
	// #14 claim-freshness: the SAME cached live-row loader main.go wires — the user service
	// invalidates it on every committed user mutation, the Auth stage reads through it — so
	// every A4 suite (ACL decision table, W1, TOCTOU, this file's login flows) runs on the
	// production auth stage, and a4_claim_freshness_integration_test.go proves the next-request
	// effect end to end.
	userClaims := services.NewCachedUserLoader(userRepo, services.DefaultUserClaimTTL)
	userSvc := services.NewUserService(userRepo).WithTokenRevoker(refreshRepo).WithSessionEnder(authSvc).WithClaimCache(userClaims)
	roleSvc := services.NewRoleService(roleRepo)
	aclSvc := services.NewACLService(roleRepo)
	teamRepo := auth.NewTeamRepository(gdb)
	teamSvc := services.NewTeamService(teamRepo)

	meta := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sx)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	handlers := APIHandlers{
		Auth:     NewAuthHandler(authSvc, zap.NewNop()),
		Metadata: NewMetadataHandler(gdb, zap.NewNop()),
		Admin:    NewAdminEntityHandler(services.NewEntityService(dyn, meta), services.NewFieldService(dyn, meta), zap.NewNop()),
		User:     NewAdminUserHandler(userSvc, zap.NewNop()).WithSessionManager(authSvc),
		Role:     NewAdminRoleHandler(roleSvc, zap.NewNop()),
		Team:     NewAdminTeamHandler(teamSvc, zap.NewNop()),
		// Wired like main.go:890 (WithACLAuthorizer): Get's canEdit probe (#334) and ?expand's target-ACL
		// re-check both need it; without it they silently skip, which production never does.
		Record: NewDynamicRecordHandler(services.NewRecordService(meta, dyn).WithACLAuthorizer(aclSvc), zap.NewNop()),
		// specs/051 manual record shares — wired like main.go (the gate re-resolves "edit" through
		// the real ACL service; dyn is the grantee probe + label fetcher; no auditor in the harness).
		RecordShare: NewRecordShareHandler(shares.NewService(shares.NewRepository(sx),
			services.NewRecordService(meta, dyn).WithACLAuthorizer(aclSvc), dyn, nil).WithTeams(teamRepo), zap.NewNop()).WithUserLabels(dyn).WithTeams(teamRepo),
		// DAILY-QUEUE #224: the team picker of the share dialog (any signed-in user).
		UserDirectory: NewUserDirectoryHandler(services.NewUserDirectoryService(userRepo), zap.NewNop()).WithTeams(teamRepo),
		// W9b: saved-view CRUD (protected group) + recycle-bin restore rides the Record handler's
		// RequireACLAction("delete") gate under this ACL-enabled router.
		SavedView: NewSavedViewHandler(services.NewViewService(view.NewSQLRepository(sx), meta, zap.NewNop()), zap.NewNop()),
	}
	for _, add := range extra {
		add(&handlers)
	}
	RegisterAPIRoutes(r.Group("/api/v1"), tokenSvc, aclSvc, handlers, zap.NewNop(), WithLiveSubject(userClaims), WithSessionGuard(sessionGuard))

	return &a4Router{engine: r, gdb: gdb, sx: sx, tokenSvc: tokenSvc, roleRepo: roleRepo}
}

// --- seeding helpers (all against the migrated DB) ---------------------------------------------

// a4SeedTenant inserts a tenant row (so the FK from acl_roles/users is satisfied).
//
// It uses a RAW INSERT of exactly the migrated columns rather than gdb.Create(&auth.Tenant{}) on
// purpose: it keeps the seed independent of the GORM model's column set, so the harness stays
// honest to whatever the production migration chain actually built. (Historically this also dodged
// a real bug — the migrated `tenants` table lacked the `deleted_at` column the GORM model declares,
// making GORM Create fail with "Unknown column 'deleted_at'". Migration 000007 has since added that
// column, so a GORM Create would now work too; the raw INSERT is retained for the explicit-columns
// honesty it gives.)
func a4SeedTenant(t *testing.T, gdb *gorm.DB, id, slug, name string) {
	t.Helper()
	if err := gdb.Exec(
		"INSERT INTO tenants (id, slug, name, status, created_at, updated_at) VALUES (?, ?, ?, 'active', ?, ?)",
		id, slug, name, time.Now(), time.Now(),
	).Error; err != nil {
		t.Fatalf("seed tenant %s: %v", slug, err)
	}
}

// a4SeedRole inserts an acl_roles row plus its (entity, action) grants directly (bypassing the
// service) so a test can set up an arbitrary grant matrix. Returns the role id.
func a4SeedRole(t *testing.T, gdb *gorm.DB, tenantID, name string, grants []auth.RoleAction) string {
	t.Helper()
	roleID := uuid.NewString()
	now := time.Now()
	role := auth.Role{ID: roleID, TenantID: tenantID, Name: name, Label: name, CreatedAt: now, UpdatedAt: now}
	if err := gdb.Create(&role).Error; err != nil {
		t.Fatalf("seed role %s: %v", name, err)
	}
	for i := range grants {
		grants[i].ID = uuid.NewString()
		grants[i].RoleID = roleID
		if grants[i].AccessLevel == "" {
			grants[i].AccessLevel = "all"
		}
		grants[i].CreatedAt = now
		grants[i].UpdatedAt = now
	}
	if len(grants) > 0 {
		if err := gdb.Create(&grants).Error; err != nil {
			t.Fatalf("seed grants for %s: %v", name, err)
		}
	}
	return roleID
}

// grant is a tiny builder for an acl_role_actions row (access_level defaults to 'all').
func grant(entity, action string) auth.RoleAction {
	return auth.RoleAction{EntityName: entity, Action: action}
}

// a4SeedUser inserts a user (with a real bcrypt hash for the shared password) carrying an optional
// role_id and is_admin flag, so a real login mints a token whose claim carries that role/admin.
// Returns the user id.
func a4SeedUser(t *testing.T, gdb *gorm.DB, tenantID, email string, isAdmin bool, roleID *string) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(authPassword), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	id := uuid.NewString()
	now := time.Now()
	u := auth.User{
		ID: id, TenantID: tenantID, Email: email, PasswordHash: string(hash),
		FullName: email, Status: "active", IsAdmin: isAdmin, RoleID: roleID,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("seed user %s: %v", email, err)
	}
	return id
}

// login performs a real login through the router and returns the token pair, failing on non-200.
func (rt *a4Router) login(t *testing.T, tenantSlug, email string) loginResponse {
	t.Helper()
	return itLogin(t, rt.engine, tenantSlug, email, authPassword)
}

// createEntity registers a dynamic entity (and its obj_ table) via the admin API as the given admin
// token — so the dynamic /:entity record routes the ACL tests hit actually dispatch.
func (rt *a4Router) createEntity(t *testing.T, adminToken, name string) {
	t.Helper()
	body := map[string]any{
		"name":  name,
		"label": name,
		"fields": []map[string]any{
			{"name": "title", "label": "Title", "fieldType": "text", "isRequired": true},
		},
	}
	w := doAuthed(t, rt.engine, "POST", "/api/v1/admin/entities", adminToken, body, nil)
	if w.Code != 201 {
		t.Fatalf("create entity %s: %d %s", name, w.Code, w.Body.String())
	}
}

// a4DropEntity tears down a dynamic entity + its obj_ table across the given tenants (cleanup).
func a4DropEntity(sx *sqlx.DB, tenantIDs []string, name string) {
	for _, ten := range tenantIDs {
		sx.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		         WHERE e.tenant_id = ? AND e.name = ?`, ten, name)
		sx.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name = ?`, ten, name)
	}
	sx.Exec("DROP TABLE IF EXISTS `obj_" + name + "`")
}
