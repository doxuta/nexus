//go:build integration

// Integration test for the pack-install ledger repository against REAL MySQL: prove RecordInstall
// → ListInstalls round-trips through the sys_pack_installs table (migration 000034), tenant-scoped
// and newest-first, with the JSON result column preserved.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/packinstall/ -race -v
//
// Builds and drops its OWN throwaway database (never nexus_platform). GORM-DSN + migrate-DSN come
// from the SINGLE TEST_DATABASE_URL source (trap B7). Requires MySQL 8 root + the migrate CLI.
package packinstall

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"nexus/platform/internal/services/vertical_pack"
)

var piDBCounter int64

func piUniqueDBName() string {
	n := atomic.AddInt64(&piDBCounter, 1)
	return fmt.Sprintf("nexus_pi_it_%d_%d", os.Getpid(), n)
}

// piSplitDSN splits TEST_DATABASE_URL so GORM and migrate target the SAME throwaway DB.
func piSplitDSN(t *testing.T) (creds, host, params string) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		base = "root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local"
	}
	at := strings.Index(base, "@tcp(")
	if at < 0 {
		t.Fatalf("piSplitDSN: unexpected DSN shape %q", base)
	}
	creds = base[:at]
	rest := base[at:]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		t.Fatalf("piSplitDSN: no db segment in DSN %q", base)
	}
	host = rest[:slash+1]
	if q := strings.Index(rest, "?"); q >= 0 {
		params = rest[q:]
	}
	return creds, host, params
}

func piAdminDSN(t *testing.T) string { c, h, p := piSplitDSN(t); return c + h + p }
func piDSN(t *testing.T, db string) string {
	c, h, p := piSplitDSN(t)
	return c + h + db + p
}
func piMigrateDSN(t *testing.T, db string) string {
	c, h, p := piSplitDSN(t)
	return "mysql://" + c + h + db + p
}

func piMigrateBin(t *testing.T) string {
	if p, err := exec.LookPath("migrate"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		cand := filepath.Join(home, "go", "bin", "migrate")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return ""
}

func piMigrationsDir() string {
	if d := os.Getenv("MIGRATIONS_DIR"); d != "" {
		return d
	}
	return "../../migrations"
}

// piSetup builds a fresh throwaway DB, migrates it up, and returns a GORM handle bound to it.
func piSetup(t *testing.T) *gorm.DB {
	t.Helper()
	dbName := piUniqueDBName()

	admin, err := gorm.Open(gormmysql.Open(piAdminDSN(t)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Skipf("pi: MySQL not reachable (%v) — skipping", err)
	}
	if sqlDB, err := admin.DB(); err != nil || sqlDB.Ping() != nil {
		t.Skipf("pi: MySQL not pingable — skipping")
	}
	if err := admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`").Error; err != nil {
		t.Fatalf("pi: drop throwaway DB: %v", err)
	}
	if err := admin.Exec("CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci").Error; err != nil {
		t.Fatalf("pi: create throwaway DB: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	migrateBin := piMigrateBin(t)
	if migrateBin == "" {
		t.Skip("pi: migrate binary not found — skipping")
	}
	cmd := exec.Command(migrateBin, "-path", piMigrationsDir(), "-database", piMigrateDSN(t, dbName), "up")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pi: migrate up: %v\n%s", err, out)
	}

	gdb, err := gorm.Open(gormmysql.Open(piDSN(t, dbName)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("pi: open gorm: %v", err)
	}
	t.Cleanup(func() {
		if d, err := gdb.DB(); err == nil {
			_ = d.Close()
		}
	})
	return gdb
}

func TestPackInstall_RecordAndList(t *testing.T) {
	gdb := piSetup(t)
	repo := New(gdb)
	ctx := context.Background()

	const tenantA = "11111111-1111-1111-1111-111111111111"
	const tenantB = "22222222-2222-2222-2222-222222222222"

	// Two installs into A (older then newer), one into B.
	older := vertical_pack.InstallRecord{
		ID: "aaaaaaaa-0000-0000-0000-000000000001", TenantID: tenantA,
		Slug: "crm", Version: "1.0.0", InstalledAt: time.Now().Add(-time.Hour),
		Result: map[string]any{"entitiesCreated": []any{"crm_accounts"}},
	}
	newer := vertical_pack.InstallRecord{
		ID: "aaaaaaaa-0000-0000-0000-000000000002", TenantID: tenantA,
		Slug: "crm", Version: "1.1.0", InstalledAt: time.Now(),
		Result: map[string]any{"entitiesCreated": []any{"crm_accounts", "crm_contacts"}},
	}
	other := vertical_pack.InstallRecord{
		ID: "bbbbbbbb-0000-0000-0000-000000000001", TenantID: tenantB,
		Slug: "clinic", Version: "1.0.0", InstalledAt: time.Now(),
	}
	for _, rec := range []vertical_pack.InstallRecord{older, newer, other} {
		if err := repo.RecordInstall(ctx, rec); err != nil {
			t.Fatalf("RecordInstall(%s): %v", rec.ID, err)
		}
	}

	// A: two rows, newest first, tenant-scoped (no B row leaks in).
	aRows, err := repo.ListInstalls(ctx, tenantA)
	if err != nil {
		t.Fatalf("ListInstalls(A): %v", err)
	}
	if len(aRows) != 2 {
		t.Fatalf("A rows = %d, want 2", len(aRows))
	}
	if aRows[0].Version != "1.1.0" || aRows[1].Version != "1.0.0" {
		t.Errorf("A rows not newest-first: got %q then %q", aRows[0].Version, aRows[1].Version)
	}
	if aRows[0].Slug != "crm" {
		t.Errorf("A newest slug = %q, want crm", aRows[0].Slug)
	}
	// Result JSON round-tripped.
	if aRows[0].Result == nil {
		t.Errorf("A newest Result is nil, want the stored JSON map")
	}
	// tenant_id is not echoed in the payload (json:"-") but is populated on the returned struct.
	if aRows[0].TenantID != tenantA {
		t.Errorf("A row TenantID = %q, want %q", aRows[0].TenantID, tenantA)
	}

	// B: exactly its own single row.
	bRows, err := repo.ListInstalls(ctx, tenantB)
	if err != nil {
		t.Fatalf("ListInstalls(B): %v", err)
	}
	if len(bRows) != 1 || bRows[0].Slug != "clinic" {
		t.Errorf("B rows = %+v, want exactly [clinic]", bRows)
	}
}

// specs/069 T015: ListItems carries live_id + protected back, and ListItemKeysOtherSlugs returns
// only the SAME tenant's rows of OTHER slugs.
func TestPackItems_ListItemsLiveIDProtectedAndOtherSlugs(t *testing.T) {
	gdb := piSetup(t)
	repo := New(gdb)
	ctx := context.Background()

	const tenantA = "11111111-1111-1111-1111-111111111111"
	const tenantB = "22222222-2222-2222-2222-222222222222"
	const liveID = "cccccccc-0000-0000-0000-000000000001"

	save := func(tenant, slug string, items ...vertical_pack.PackItem) {
		t.Helper()
		if err := repo.SaveItems(ctx, tenant, slug, "1.0.0", items); err != nil {
			t.Fatalf("SaveItems(%s/%s): %v", tenant, slug, err)
		}
	}
	rule := func(name, live string) vertical_pack.PackItem {
		return vertical_pack.PackItem{Type: vertical_pack.ItemWorkflow, Key: name, Content: []byte(`{"name":"` + name + `"}`), Wrote: true, LiveID: live}
	}
	save(tenantA, "crm", rule("with-live-id", liveID), rule("no-live-id", ""))
	save(tenantA, "ticketing", rule("tk-rule", ""))
	save(tenantB, "ticketing", rule("other-tenant-rule", ""))
	if err := gdb.Exec("UPDATE sys_pack_items SET protected = 1 WHERE tenant_id = ? AND pack_slug = 'crm' AND item_key = 'no-live-id'", tenantA).Error; err != nil {
		t.Fatalf("flag protected: %v", err)
	}

	rows, err := repo.ListItems(ctx, tenantA, "crm")
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	got := map[string]vertical_pack.StoredPackItem{}
	for _, r := range rows {
		got[r.Key] = r
	}
	if len(got) != 2 || got["with-live-id"].LiveID != liveID || got["with-live-id"].Protected {
		t.Errorf("with-live-id row = %+v", got["with-live-id"])
	}
	if got["no-live-id"].LiveID != "" || !got["no-live-id"].Protected {
		t.Errorf("no-live-id row = %+v (want empty live id, protected)", got["no-live-id"])
	}

	others, err := repo.ListItemKeysOtherSlugs(ctx, tenantA, "crm")
	if err != nil {
		t.Fatalf("ListItemKeysOtherSlugs: %v", err)
	}
	if len(others) != 1 || others[0].Slug != "ticketing" || others[0].Key != "tk-rule" {
		t.Errorf("other-slug rows = %+v, want exactly tenant A's ticketing/tk-rule (tenant B and the same slug excluded)", others)
	}
	if none, err := repo.ListItemKeysOtherSlugs(ctx, "99999999-9999-9999-9999-999999999999", "crm"); err != nil || len(none) != 0 {
		t.Errorf("unknown tenant sees %v (err %v), want nothing", none, err)
	}
}

// Issue #30 item 4: PacksOf names every pack that ships one item, sorted, from the tenant's own ledger rows only.
func TestPackItems_PacksOfIsTenantScopedAndSorted(t *testing.T) {
	gdb := piSetup(t)
	repo := New(gdb)
	ctx := context.Background()
	const tenantA = "11111111-1111-1111-1111-111111111111"
	const tenantB = "22222222-2222-2222-2222-222222222222"
	table := func(key string) []vertical_pack.PackItem {
		return []vertical_pack.PackItem{{Type: vertical_pack.ItemEntity, Key: key, Content: []byte(`{}`), Wrote: true}}
	}
	for _, s := range []struct{ tenant, slug, key string }{
		{tenantA, "ticketing", "contacts"}, {tenantA, "crm", "contacts"}, {tenantB, "lms", "contacts"}, {tenantA, "crm", "deals"},
	} {
		if err := repo.SaveItems(ctx, s.tenant, s.slug, "1.0.0", table(s.key)); err != nil {
			t.Fatalf("SaveItems: %v", err)
		}
	}
	if got, err := repo.PacksOf(ctx, tenantA, vertical_pack.ItemEntity, "contacts"); err != nil || strings.Join(got, ",") != "crm,ticketing" {
		t.Errorf("PacksOf(A, contacts) = %v, %v; want [crm ticketing]", got, err)
	}
	if got, err := repo.PacksOf(ctx, tenantA, vertical_pack.ItemLayout, "contacts"); err != nil || len(got) != 0 {
		t.Errorf("another item type = %v, %v; want none", got, err)
	}
	if got, err := repo.PacksOf(ctx, tenantB, vertical_pack.ItemEntity, "deals"); err != nil || len(got) != 0 {
		t.Errorf("tenant B sees tenant A's pack: %v, %v", got, err)
	}
}
