//go:build integration

// RM-18 tenant-leak hardening — integration tests.
//
// These tests prove THREE invariants that 000010_tenant_id_not_null closes:
//
//  1. Migration cycle: up → down → up on a clean throwaway DB is idempotent and leaves
//     both tables NOT NULL.
//  2. Backfill proof: seeding a NULL-tenant_id sys_fields row BEFORE applying 000010 up
//     causes the backfill UPDATE to fix the NULL, and the subsequent MODIFY NOT NULL
//     succeeds.
//  3. GetFieldByID cross-tenant: after NOT NULL is enforced and the OR-NULL hatch is
//     removed, requesting a field id that belongs to tenant A while authenticating as
//     tenant B returns ErrFieldNotFound (never the cross-tenant row).
//  4. ListFields tenant scoping: ListFields with a mismatched tenantID returns an empty
//     slice (never the other tenant's fields), even when the entityID is known.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/nexus_rm18_test?charset=utf8mb4&parseTime=True&loc=Local" \
//	  go test -tags=integration ./internal/metadata/ -run TestRM18
//
// Requires a reachable MySQL 8 root connection. Each test creates (and drops) its own
// throwaway database so it never touches nexus_platform. The migrate binary must be on PATH.
package metadata

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// rm18MigrationsDir is the path to the migrations folder relative to the package dir
// (internal/metadata → ../../migrations). Tests run from the package dir.
const rm18MigrationsDir = "../../migrations"

// rm18Tenant constants for fixture seeding.
const (
	rm18TenantA = "aaaaaaaa-1111-1111-1111-aaaaaaaaaaaa"
	rm18TenantB = "bbbbbbbb-2222-2222-2222-bbbbbbbbbbbb"
)

// rm18DBCounter gives each test a unique throwaway database name so the GORM connection and the
// migrate CLI always target the SAME database and parallel/interleaved tests never collide.
var rm18DBCounter int64

// rm18UniqueDBName returns a process+test-unique throwaway DB name. It is ALWAYS a generated name
// (never the dev/CI database carried in TEST_DATABASE_URL), so a botched run can never touch real
// data — which is why no "refusing to run against nexus_platform" guard is needed.
func rm18UniqueDBName() string {
	n := atomic.AddInt64(&rm18DBCounter, 1)
	return fmt.Sprintf("nexus_rm18_it_%d_%d", os.Getpid(), n)
}

// rm18SplitDSN breaks the base go-sql-driver DSN "user:pass@tcp(host:port)/db?params" into its
// credentials ("user:pass"), host segment ("@tcp(host:port)/"), and query ("?params"). The base is
// TEST_DATABASE_URL — the SAME source CI uses — so the GORM DSN and the migrate DSN can never
// diverge onto different databases (the split-brain that made these tests red in CI: GORM followed
// TEST_DATABASE_URL while migrate read a separate MIGRATE_DATABASE_URL). Falls back to a local root
// DSN when TEST_DATABASE_URL is unset. Mirrors the established a4 harness (handlers/a4_*_test.go).
func rm18SplitDSN(t *testing.T) (creds, host, params string) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		base = "root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local"
	}
	at := strings.Index(base, "@tcp(")
	if at < 0 {
		t.Fatalf("rm18SplitDSN: unexpected DSN shape %q (want user:pass@tcp(host:port)/db?params)", base)
	}
	creds = base[:at]
	rest := base[at:] // "@tcp(host:port)/db?params"
	slash := strings.Index(rest, "/")
	if slash < 0 {
		t.Fatalf("rm18SplitDSN: no db segment in DSN %q", base)
	}
	host = rest[:slash+1] // "@tcp(host:port)/"
	if q := strings.Index(rest, "?"); q >= 0 {
		params = rest[q:] // "?params"
	}
	return creds, host, params
}

// rm18AdminDSN is a go-sql-driver DSN with NO database selected (for CREATE/DROP DATABASE).
func rm18AdminDSN(t *testing.T) string {
	creds, host, params := rm18SplitDSN(t)
	return creds + host + params // "user:pass@tcp(host:port)/?params"
}

// rm18GormDSN is the go-sql-driver DSN (gorm) pointing at the named throwaway DB.
func rm18GormDSN(t *testing.T, dbName string) string {
	creds, host, params := rm18SplitDSN(t)
	return creds + host + dbName + params
}

// rm18MigrateDSN is the golang-migrate connection string (mysql:// scheme) for the named throwaway
// DB — built from the SAME source as rm18GormDSN so both always point at dbName, carrying CI's
// multiStatements=true through for the multi-statement 000010 migration.
func rm18MigrateDSN(t *testing.T, dbName string) string {
	creds, host, params := rm18SplitDSN(t)
	return "mysql://" + creds + host + dbName + params
}

// newRM18DB creates a per-test unique throwaway database and returns a GORM handle bound to it plus
// the database name (threaded into the migrate helpers so GORM + migrate target the same DB). The
// suite is SKIPPED (not failed) when MySQL is unreachable, so it degrades gracefully off-CI.
func newRM18DB(t *testing.T) (*gorm.DB, string) {
	t.Helper()

	dbName := rm18UniqueDBName()

	// Admin connection (no DB selected) to create/drop the throwaway database.
	admin, err := gorm.Open(gormmysql.Open(rm18AdminDSN(t)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Skipf("rm18: MySQL not reachable (%v) — skipping", err)
	}
	if sqlDB, err := admin.DB(); err != nil || sqlDB.Ping() != nil {
		t.Skipf("rm18: MySQL not pingable — skipping")
	}
	if err := admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`").Error; err != nil {
		t.Fatalf("rm18: drop throwaway DB: %v", err)
	}
	if err := admin.Exec("CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci").Error; err != nil {
		t.Fatalf("rm18: create throwaway DB: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	// GORM connection bound to the throwaway database.
	gdb, err := gorm.Open(gormmysql.Open(rm18GormDSN(t, dbName)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("rm18: open throwaway DB %q: %v", dbName, err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb, dbName
}

// rm18MigrateBin locates the golang-migrate CLI: PATH, then ~/go/bin/migrate.
func rm18MigrateBin(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("migrate"); err == nil {
		return p
	}
	if home, _ := os.UserHomeDir(); home != "" {
		cand := home + "/go/bin/migrate"
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return ""
}

// migrateCmd runs the migrate CLI against the named throwaway database.
func migrateCmd(t *testing.T, dbName string, args ...string) {
	t.Helper()
	migrateBin := rm18MigrateBin(t)
	if migrateBin == "" {
		t.Skip("rm18: migrate binary not found on PATH — skipping migration test")
	}
	cmdArgs := append([]string{"-path", rm18MigrationsDir, "-database", rm18MigrateDSN(t, dbName)}, args...)
	cmd := exec.Command(migrateBin, cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rm18: migrate %v: %v\n%s", args, err, out)
	}
}

// migrateToVersion migrates the named throwaway DB to the given version.
func migrateToVersion(t *testing.T, dbName string, version int) {
	t.Helper()
	migrateCmd(t, dbName, "goto", fmt.Sprintf("%d", version))
}

// ---------------------------------------------------------------------------
// Test 1: migration up→down→up cycle
// ---------------------------------------------------------------------------

// TestRM18_MigrationCycle verifies that migration 000010 applies cleanly on a fresh DB,
// reverses cleanly on down, and re-applies cleanly (idempotent backfill).
func TestRM18_MigrationCycle(t *testing.T) {
	gdb, dbName := newRM18DB(t)

	// Apply all migrations up to 000009 (pre-RM18 state).
	migrateToVersion(t, dbName, 9)

	// Seed one entity + one field (both with correct tenant_id — normal state).
	now := time.Now().Truncate(time.Second)
	entID := "rm18-cycle-ent-1"
	fldID := "rm18-cycle-fld-1"
	gdb.Exec(`INSERT INTO sys_entities (id, tenant_id, name, label, table_name, created_at, updated_at)
	          VALUES (?, ?, ?, ?, ?, ?, ?)`,
		entID, rm18TenantA, "rm18_cycle_widget", "Widget", "obj_rm18_cycle_widget", now, now)
	gdb.Exec(`INSERT INTO sys_fields (id, entity_id, tenant_id, name, label, field_type, created_at, updated_at)
	          VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		fldID, entID, rm18TenantA, "title", "Title", "text", now, now)

	t.Cleanup(func() {
		gdb.Exec("DELETE FROM sys_fields WHERE id = ?", fldID)
		gdb.Exec("DELETE FROM sys_entities WHERE id = ?", entID)
	})

	// Apply 000010 up.
	migrateToVersion(t, dbName, 10)

	// Verify sys_fields.tenant_id is NOT NULL.
	var isNullable string
	gdb.Raw(`SELECT IS_NULLABLE FROM information_schema.COLUMNS
	         WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_fields' AND COLUMN_NAME = 'tenant_id'`).
		Scan(&isNullable)
	if isNullable != "NO" {
		t.Errorf("sys_fields.tenant_id: IS_NULLABLE = %q; want NO (NOT NULL)", isNullable)
	}

	// Verify sys_relationships.tenant_id is NOT NULL.
	gdb.Raw(`SELECT IS_NULLABLE FROM information_schema.COLUMNS
	         WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_relationships' AND COLUMN_NAME = 'tenant_id'`).
		Scan(&isNullable)
	if isNullable != "NO" {
		t.Errorf("sys_relationships.tenant_id: IS_NULLABLE = %q; want NO (NOT NULL)", isNullable)
	}

	// Down to 000009.
	migrateToVersion(t, dbName, 9)

	// Verify columns are nullable again after down.
	gdb.Raw(`SELECT IS_NULLABLE FROM information_schema.COLUMNS
	         WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_fields' AND COLUMN_NAME = 'tenant_id'`).
		Scan(&isNullable)
	if isNullable != "YES" {
		t.Errorf("after down: sys_fields.tenant_id: IS_NULLABLE = %q; want YES (NULL)", isNullable)
	}

	// Re-apply 000010 (second up — backfill is idempotent since tenant_id already set).
	migrateToVersion(t, dbName, 10)

	gdb.Raw(`SELECT IS_NULLABLE FROM information_schema.COLUMNS
	         WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_fields' AND COLUMN_NAME = 'tenant_id'`).
		Scan(&isNullable)
	if isNullable != "NO" {
		t.Errorf("after second up: sys_fields.tenant_id: IS_NULLABLE = %q; want NO", isNullable)
	}
}

// ---------------------------------------------------------------------------
// Test 2: backfill proof — NULL-seeded row is fixed before MODIFY NOT NULL
// ---------------------------------------------------------------------------

// TestRM18_BackfillFixesNullBeforeModify proves the backfill UPDATE in migration 000010 up
// rescues a sys_fields row that has tenant_id = NULL (as 000004 left them) so the
// subsequent MODIFY NOT NULL succeeds instead of failing with MySQL-1138.
func TestRM18_BackfillFixesNullBeforeModify(t *testing.T) {
	gdb, dbName := newRM18DB(t)

	// Apply up to 000009 (tenant_id is still NULLABLE).
	migrateToVersion(t, dbName, 9)

	now := time.Now().Truncate(time.Second)
	entID := "rm18-backfill-ent"
	fldID := "rm18-backfill-fld"

	// Insert entity with a valid tenant_id.
	gdb.Exec(`INSERT INTO sys_entities (id, tenant_id, name, label, table_name, created_at, updated_at)
	          VALUES (?, ?, ?, ?, ?, ?, ?)`,
		entID, rm18TenantA, "rm18_backfill_widget", "Widget", "obj_rm18_backfill_widget", now, now)

	// Insert a field row with tenant_id = NULL (simulates the pre-000004 state or a bad INSERT).
	gdb.Exec(`INSERT INTO sys_fields (id, entity_id, tenant_id, name, label, field_type, created_at, updated_at)
	          VALUES (?, ?, NULL, ?, ?, ?, ?, ?)`,
		fldID, entID, "status", "Status", "text", now, now)

	t.Cleanup(func() {
		gdb.Exec("DELETE FROM sys_fields WHERE id = ?", fldID)
		gdb.Exec("DELETE FROM sys_entities WHERE id = ?", entID)
	})

	// Confirm the NULL is really there before 000010.
	var beforeTenant *string
	gdb.Raw("SELECT tenant_id FROM sys_fields WHERE id = ?", fldID).Scan(&beforeTenant)
	if beforeTenant != nil {
		t.Fatalf("test setup: expected NULL tenant_id before migration, got %q", *beforeTenant)
	}

	// Apply 000010 up — backfill must fix the NULL and MODIFY must succeed.
	migrateToVersion(t, dbName, 10)

	// After up, the field must have tenant_id = rm18TenantA (backfilled from entity).
	var afterTenant string
	gdb.Raw("SELECT tenant_id FROM sys_fields WHERE id = ?", fldID).Scan(&afterTenant)
	if afterTenant != rm18TenantA {
		t.Errorf("after backfill: tenant_id = %q; want %q", afterTenant, rm18TenantA)
	}

	// Confirm NOT NULL is now enforced: attempt to set tenant_id = NULL must fail.
	result := gdb.Exec("UPDATE sys_fields SET tenant_id = NULL WHERE id = ?", fldID)
	if result.Error == nil {
		t.Errorf("expected MySQL error when setting tenant_id = NULL after NOT NULL enforcement, got nil")
	}
}

// ---------------------------------------------------------------------------
// Test 3: GetFieldByID cross-tenant returns ErrFieldNotFound (no leak)
// ---------------------------------------------------------------------------

// TestRM18_GetFieldByID_CrossTenantReturnsNotFound proves that after the OR-NULL hatch
// is removed from GetFieldByID, requesting a field that belongs to tenant A while
// passing tenant B returns ErrFieldNotFound — not the cross-tenant row.
func TestRM18_GetFieldByID_CrossTenantReturnsNotFound(t *testing.T) {
	gdb, dbName := newRM18DB(t)

	// Apply all migrations to head (includes 000010).
	migrateCmd(t, dbName, "up")

	now := time.Now().Truncate(time.Second)
	entA := "rm18-xtenant-ent-a"
	fldA := "rm18-xtenant-fld-a"

	// Seed entity + field for tenant A.
	gdb.Exec(`INSERT INTO sys_entities (id, tenant_id, name, label, table_name, created_at, updated_at)
	          VALUES (?, ?, ?, ?, ?, ?, ?)`,
		entA, rm18TenantA, "rm18_xtenant_widget", "Widget", "obj_rm18_xtenant_widget", now, now)
	gdb.Exec(`INSERT INTO sys_fields (id, entity_id, tenant_id, name, label, field_type, created_at, updated_at)
	          VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		fldA, entA, rm18TenantA, "title", "Title", "text", now, now)

	t.Cleanup(func() {
		gdb.Exec("DELETE FROM sys_fields WHERE id = ?", fldA)
		gdb.Exec("DELETE FROM sys_entities WHERE id = ?", entA)
	})

	repo := NewRepository(gdb)
	ctx := context.Background()

	// Correct tenant can retrieve the field.
	got, err := repo.GetFieldByID(ctx, rm18TenantA, fldA)
	if err != nil {
		t.Fatalf("same-tenant GetFieldByID: unexpected error: %v", err)
	}
	if got.ID != fldA {
		t.Fatalf("same-tenant GetFieldByID: got id %q; want %q", got.ID, fldA)
	}

	// Wrong tenant (B) must get ErrFieldNotFound — never the cross-tenant row.
	_, err = repo.GetFieldByID(ctx, rm18TenantB, fldA)
	if err != ErrFieldNotFound {
		t.Errorf("cross-tenant GetFieldByID: err = %v; want ErrFieldNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Test 4: ListFields tenant scoping — mismatched tenant returns empty slice
// ---------------------------------------------------------------------------

// TestRM18_ListFields_TenantScopedReturnsEmpty proves that ListFields with a tenantID
// that does not match the entity's owning tenant returns an empty slice — not the other
// tenant's fields. This defends against a caller who resolves the entityID correctly but
// supplies the wrong tenantID (defense-in-depth, RM-18).
func TestRM18_ListFields_TenantScopedReturnsEmpty(t *testing.T) {
	gdb, dbName := newRM18DB(t)

	// Apply all migrations to head.
	migrateCmd(t, dbName, "up")

	now := time.Now().Truncate(time.Second)
	entA := "rm18-listscope-ent-a"
	fldA1 := "rm18-listscope-fld-a1"
	fldA2 := "rm18-listscope-fld-a2"

	// Seed one entity + two fields for tenant A.
	gdb.Exec(`INSERT INTO sys_entities (id, tenant_id, name, label, table_name, created_at, updated_at)
	          VALUES (?, ?, ?, ?, ?, ?, ?)`,
		entA, rm18TenantA, "rm18_listscope_widget", "Widget", "obj_rm18_listscope_widget", now, now)
	gdb.Exec(`INSERT INTO sys_fields (id, entity_id, tenant_id, name, label, field_type, created_at, updated_at)
	          VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		fldA1, entA, rm18TenantA, "alpha", "Alpha", "text", now, now)
	gdb.Exec(`INSERT INTO sys_fields (id, entity_id, tenant_id, name, label, field_type, created_at, updated_at)
	          VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		fldA2, entA, rm18TenantA, "beta", "Beta", "text", now, now)

	t.Cleanup(func() {
		gdb.Exec("DELETE FROM sys_fields WHERE id IN (?, ?)", fldA1, fldA2)
		gdb.Exec("DELETE FROM sys_entities WHERE id = ?", entA)
	})

	repo := NewRepository(gdb)
	ctx := context.Background()

	// Correct tenant A sees both fields.
	fieldsA, err := repo.ListFields(ctx, rm18TenantA, entA)
	if err != nil {
		t.Fatalf("ListFields tenant A: %v", err)
	}
	if len(fieldsA) != 2 {
		t.Errorf("ListFields tenant A: got %d fields; want 2", len(fieldsA))
	}

	// Wrong tenant B with the same entityID gets an empty slice — never tenant A's fields.
	fieldsB, err := repo.ListFields(ctx, rm18TenantB, entA)
	if err != nil {
		t.Fatalf("ListFields tenant B: %v", err)
	}
	if len(fieldsB) != 0 {
		t.Errorf("ListFields cross-tenant: got %d fields; want 0 (no cross-tenant leak)", len(fieldsB))
		for _, f := range fieldsB {
			t.Logf("  leaked field: id=%q name=%q tenant_id=%q", f.ID, f.Name, f.TenantID)
		}
	}
}
