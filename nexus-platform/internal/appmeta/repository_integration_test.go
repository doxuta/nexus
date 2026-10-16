//go:build integration

// Integration tests for the sys_apps/sys_app_nav_items repository. Run with:
//
//	go test -tags=integration -race ./internal/appmeta/
//
// They require a reachable MySQL 8 with migration 000042 applied. Set TEST_DATABASE_URL to
// override the DSN; otherwise the local compose DSN is used (same convention as internal/view).
// Covered here (real MySQL, per spec §6): 2-tenant isolation, UNIQUE(tenant_id, name) + the
// tombstone-rename freeing the slug slot, the default-clear transaction, nav round-trip, and
// the INV-4 cascades — dashboard delete (dashboard.SQLRepository tx) and entity soft-delete
// (metadata.Repository GORM tx) both purging referencing nav rows.
package appmeta_test

import (
	"context"
	"errors"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/metadata"
)

// testDSN returns the DSN of the throwaway migrated database provisioned by TestMain (see
// main_test.go). Before #84 this read TEST_DATABASE_URL directly, which broke against a db-less
// DSN ("No database selected"); TestMain now creates+migrates a throwaway DB and points
// packageTestDSN at it.
func testDSN() string {
	return packageTestDSN
}

func testDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("mysql", testDSN())
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping integration test", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

const (
	tenantA = "aaaa1111-0000-0000-0000-00000000ap01"
	tenantB = "bbbb2222-0000-0000-0000-00000000ap02"
)

// cleanTenants scrubs both test tenants from every table this suite touches.
func cleanTenants(t *testing.T, db *sqlx.DB) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		"DELETE FROM sys_app_nav_items WHERE tenant_id IN (?, ?)",
		"DELETE FROM sys_apps WHERE tenant_id IN (?, ?)",
		"DELETE FROM sys_dashboards WHERE tenant_id IN (?, ?)",
		"DELETE FROM sys_entities WHERE tenant_id IN (?, ?)",
	} {
		if _, err := db.ExecContext(ctx, q, tenantA, tenantB); err != nil {
			t.Fatalf("clean: %v", err)
		}
	}
}

func TestAppRepo_UniqueSlugAndTenantIsolation(t *testing.T) {
	db := testDB(t)
	cleanTenants(t, db)
	t.Cleanup(func() { cleanTenants(t, db) })
	repo := appmeta.NewSQLRepository(db)
	ctx := context.Background()

	idA, err := repo.Create(ctx, appmeta.CreateInput{TenantID: tenantA, Name: "sales", Label: "Sales A"})
	if err != nil {
		t.Fatalf("create tenant-a sales: %v", err)
	}
	// Same slug in the SAME tenant → duplicate.
	if _, err := repo.Create(ctx, appmeta.CreateInput{TenantID: tenantA, Name: "sales", Label: "dup"}); !errors.Is(err, appmeta.ErrAppDuplicateName) {
		t.Fatalf("same-tenant dup err = %v; want ErrAppDuplicateName", err)
	}
	// Same slug in ANOTHER tenant → fine (unique is per tenant).
	if _, err := repo.Create(ctx, appmeta.CreateInput{TenantID: tenantB, Name: "sales", Label: "Sales B"}); err != nil {
		t.Fatalf("cross-tenant same slug: %v", err)
	}

	// Isolation: tenant B lists only its own app; cross-tenant Get is not-found.
	appsB, err := repo.List(ctx, tenantB)
	if err != nil || len(appsB) != 1 || appsB[0].Label != "Sales B" {
		t.Fatalf("tenant-b list = %+v (%v); want exactly its own app", appsB, err)
	}
	if _, err := repo.GetByID(ctx, tenantB, idA); !errors.Is(err, appmeta.ErrAppNotFound) {
		t.Fatalf("cross-tenant GetByID err = %v; want ErrAppNotFound", err)
	}
	if _, err := repo.GetByName(ctx, tenantB, "ghost"); !errors.Is(err, appmeta.ErrAppNotFound) {
		t.Fatalf("unknown GetByName err = %v; want ErrAppNotFound", err)
	}

	// Soft-delete tombstones the slug slot → the SAME name can be recreated.
	if err := repo.SoftDelete(ctx, tenantA, idA); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := repo.Create(ctx, appmeta.CreateInput{TenantID: tenantA, Name: "sales", Label: "Sales A v2"}); err != nil {
		t.Fatalf("recreate after soft-delete: %v (tombstone should free the slug slot)", err)
	}
}

func TestAppRepo_DefaultClearAndNavRoundTrip(t *testing.T) {
	db := testDB(t)
	cleanTenants(t, db)
	t.Cleanup(func() { cleanTenants(t, db) })
	repo := appmeta.NewSQLRepository(db)
	ctx := context.Background()

	id1, err := repo.Create(ctx, appmeta.CreateInput{TenantID: tenantA, Name: "sales", Label: "Sales", IsDefault: true})
	if err != nil {
		t.Fatalf("create sales: %v", err)
	}
	id2, err := repo.Create(ctx, appmeta.CreateInput{TenantID: tenantA, Name: "support", Label: "Support", IsDefault: true})
	if err != nil {
		t.Fatalf("create support: %v", err)
	}
	// ≤1 default per tenant: the second default create cleared the first.
	a1, err := repo.GetByID(ctx, tenantA, id1)
	if err != nil || a1.IsDefault {
		t.Fatalf("app1 after second default = %+v (%v); want IsDefault=false", a1, err)
	}
	a2, err := repo.GetByID(ctx, tenantA, id2)
	if err != nil || !a2.IsDefault {
		t.Fatalf("app2 = %+v (%v); want IsDefault=true", a2, err)
	}

	// Nav round-trip: replace-all keeps slice order; NavCount reflects it; re-replace reorders.
	ref1, ref2 := uuid.NewString(), uuid.NewString()
	if err := repo.ReplaceNav(ctx, tenantA, id1, []appmeta.NavItemInput{
		{ItemType: appmeta.ItemTypeEntity, RefID: ref1, Label: "Tab 1"},
		{ItemType: appmeta.ItemTypeEntity, RefID: ref2},
	}); err != nil {
		t.Fatalf("replace nav: %v", err)
	}
	items, err := repo.ListNav(ctx, tenantA, id1)
	if err != nil || len(items) != 2 || items[0].RefID != ref1 || items[1].RefID != ref2 {
		t.Fatalf("nav = %+v (%v); want [ref1 ref2] in order", items, err)
	}
	if items[0].Label == nil || *items[0].Label != "Tab 1" || items[1].Label != nil {
		t.Fatalf("labels = %+v; want override then NULL", items)
	}
	a1, err = repo.GetByID(ctx, tenantA, id1)
	if err != nil || a1.NavCount != 2 {
		t.Fatalf("NavCount = %d (%v); want 2", a1.NavCount, err)
	}
	if err := repo.ReplaceNav(ctx, tenantA, id1, []appmeta.NavItemInput{
		{ItemType: appmeta.ItemTypeEntity, RefID: ref2},
	}); err != nil {
		t.Fatalf("re-replace nav: %v", err)
	}
	items, err = repo.ListNav(ctx, tenantA, id1)
	if err != nil || len(items) != 1 || items[0].RefID != ref2 {
		t.Fatalf("nav after re-replace = %+v (%v); want [ref2]", items, err)
	}

	// Tenant isolation on nav reads.
	if items, err := repo.ListNav(ctx, tenantB, id1); err != nil || len(items) != 0 {
		t.Fatalf("cross-tenant nav = %+v (%v); want empty", items, err)
	}

	// App soft-delete removes its nav rows (same tx).
	if err := repo.SoftDelete(ctx, tenantA, id1); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	var n int
	if err := db.GetContext(ctx, &n, "SELECT COUNT(*) FROM sys_app_nav_items WHERE tenant_id = ? AND app_id = ?", tenantA, id1); err != nil {
		t.Fatalf("count nav: %v", err)
	}
	if n != 0 {
		t.Fatalf("nav rows after app delete = %d; want 0", n)
	}
}

// TestAppRepo_CascadeOnRefDelete pins INV-4 end-to-end against the REAL delete paths: deleting
// a dashboard (dashboard.SQLRepository.Delete tx) and soft-deleting an entity
// (metadata.Repository.SoftDeleteEntity GORM tx) must remove the nav rows referencing them.
func TestAppRepo_CascadeOnRefDelete(t *testing.T) {
	db := testDB(t)
	cleanTenants(t, db)
	t.Cleanup(func() { cleanTenants(t, db) })
	repo := appmeta.NewSQLRepository(db)
	ctx := context.Background()

	appID, err := repo.Create(ctx, appmeta.CreateInput{TenantID: tenantA, Name: "sales", Label: "Sales"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	// Real dashboard row via the real repo.
	dashRepo := dashboard.NewSQLRepository(db)
	dashID, err := dashRepo.Create(ctx, dashboard.CreateInput{TenantID: tenantA, Name: "KPI", Widgets: []byte("[]")})
	if err != nil {
		t.Fatalf("create dashboard: %v", err)
	}

	// Real entity row via raw insert (the metadata repo's create path also runs DDL; the
	// cascade under test lives in SoftDeleteEntity, which only needs the registry row).
	entityID := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO sys_entities (id, tenant_id, name, label, table_name, created_at, updated_at)
		VALUES (?, ?, 'cascade_probe', 'Cascade Probe', 'obj_cascade_probe', NOW(), NOW())`, entityID, tenantA); err != nil {
		t.Fatalf("insert entity row: %v", err)
	}

	if err := repo.ReplaceNav(ctx, tenantA, appID, []appmeta.NavItemInput{
		{ItemType: appmeta.ItemTypeEntity, RefID: entityID},
		{ItemType: appmeta.ItemTypeDashboard, RefID: dashID},
	}); err != nil {
		t.Fatalf("replace nav: %v", err)
	}

	// Dashboard delete → its nav row disappears; the entity's row stays.
	if err := dashRepo.Delete(ctx, tenantA, dashID); err != nil {
		t.Fatalf("delete dashboard: %v", err)
	}
	items, err := repo.ListNav(ctx, tenantA, appID)
	if err != nil || len(items) != 1 || items[0].RefID != entityID {
		t.Fatalf("nav after dashboard delete = %+v (%v); want only the entity ref", items, err)
	}

	// Entity soft-delete (GORM tx incl. tombstone + relationship sweep) → its nav row disappears.
	gdb, err := gorm.Open(gormmysql.Open(testDSN()), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Skipf("GORM not connectable (%v) — skipping entity-cascade leg", err)
	}
	metaRepo := metadata.NewRepository(gdb)
	if err := metaRepo.SoftDeleteEntity(ctx, tenantA, entityID); err != nil {
		t.Fatalf("soft delete entity: %v", err)
	}
	items, err = repo.ListNav(ctx, tenantA, appID)
	if err != nil || len(items) != 0 {
		t.Fatalf("nav after entity delete = %+v (%v); want empty (INV-4)", items, err)
	}
}

// TestRepository_HomeDashboard: set → read back → preserved across a nil update → cleared by the
// shared dashboard-delete cascade, never visible cross-tenant (real JSON_SET/JSON_REMOVE).
func TestRepository_HomeDashboard(t *testing.T) {
	db := testDB(t)
	cleanTenants(t, db)
	t.Cleanup(func() { cleanTenants(t, db) })
	ctx := context.Background()
	repo := appmeta.NewSQLRepository(db)

	appID, err := repo.Create(ctx, appmeta.CreateInput{TenantID: tenantA, Name: "home_probe", Label: "Home"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	dashRepo := dashboard.NewSQLRepository(db)
	dashID, err := dashRepo.Create(ctx, dashboard.CreateInput{TenantID: tenantA, Name: "KPI", Widgets: []byte("[]")})
	if err != nil {
		t.Fatalf("create dashboard: %v", err)
	}

	set := func(v *string) {
		t.Helper()
		if err := repo.Update(ctx, tenantA, appID, appmeta.UpdateInput{Label: "Home", HomeDashboardID: v}); err != nil {
			t.Fatalf("update: %v", err)
		}
	}
	home := func() string {
		t.Helper()
		a, err := repo.GetByID(ctx, tenantA, appID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return a.HomeDashboardID
	}

	set(&dashID)
	if got := home(); got != dashID {
		t.Fatalf("home = %q; want %q", got, dashID)
	}
	if _, err := repo.GetByID(ctx, tenantB, appID); !errors.Is(err, appmeta.ErrAppNotFound) {
		t.Fatalf("cross-tenant read err = %v; want ErrAppNotFound", err)
	}
	set(nil) // an old client that never sends the key
	if got := home(); got != dashID {
		t.Fatalf("after nil update home = %q; want unchanged %q", got, dashID)
	}
	if err := dashRepo.Delete(ctx, tenantA, dashID); err != nil {
		t.Fatalf("delete dashboard: %v", err)
	}
	if got := home(); got != "" {
		t.Fatalf("after dashboard delete home = %q; want cleared", got)
	}
	empty := ""
	set(&dashID)
	set(&empty)
	if got := home(); got != "" {
		t.Fatalf("after explicit clear home = %q; want empty", got)
	}
}

// Home queues + per-role default live in the same options JSON as the home dashboard and must not
// clobber each other (JSON_SET/CAST AS JSON against real MySQL).
func TestRepository_HomeQueuesAndRoleDefault(t *testing.T) {
	db := testDB(t)
	cleanTenants(t, db)
	t.Cleanup(func() { cleanTenants(t, db) })
	ctx := context.Background()
	repo := appmeta.NewSQLRepository(db)
	appID, err := repo.Create(ctx, appmeta.CreateInput{TenantID: tenantA, Name: "queues_probe", Label: "Q"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	dash := "d-1"
	qs, rs := []string{"q-1", "q-2"}, []string{"r-1"}
	if err := repo.Update(ctx, tenantA, appID, appmeta.UpdateInput{Label: "Q", HomeDashboardID: &dash, HomeQueueIDs: &qs, DefaultRoleIDs: &rs}); err != nil {
		t.Fatalf("update: %v", err)
	}
	a, _ := repo.GetByID(ctx, tenantA, appID)
	if len(a.HomeQueueIDs) != 2 || a.HomeQueueIDs[1] != "q-2" || len(a.DefaultRoleIDs) != 1 || a.HomeDashboardID != "d-1" {
		t.Fatalf("got queues=%v roles=%v home=%q", a.HomeQueueIDs, a.DefaultRoleIDs, a.HomeDashboardID)
	}
	if err := repo.Update(ctx, tenantA, appID, appmeta.UpdateInput{Label: "Q"}); err != nil { // nil keeps all
		t.Fatalf("update nil: %v", err)
	}
	a, _ = repo.GetByID(ctx, tenantA, appID)
	if len(a.HomeQueueIDs) != 2 {
		t.Fatalf("nil update must keep queues, got %v", a.HomeQueueIDs)
	}
	none := []string{}
	if err := repo.Update(ctx, tenantA, appID, appmeta.UpdateInput{Label: "Q", HomeQueueIDs: &none}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	a, _ = repo.GetByID(ctx, tenantA, appID)
	if len(a.HomeQueueIDs) != 0 || len(a.DefaultRoleIDs) != 1 || a.HomeDashboardID != "d-1" {
		t.Fatalf("clear queues only: queues=%v roles=%v home=%q", a.HomeQueueIDs, a.DefaultRoleIDs, a.HomeDashboardID)
	}
}
