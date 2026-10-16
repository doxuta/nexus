//go:build integration

// specs/062-customer-workspace T076 — the project registry's storage against MySQL 8: upsert on the
// one-profile-per-tenant key, tenant-pinned reads, the build-step counts (soft-deleted rows and
// admins excluded), and the guarantee that the number of statements does not grow with the number
// of customers.
//
//	go test -tags=integration ./internal/project/
package project

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func pjDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(gormmysql.Open(testDSN), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping integration test", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping integration test", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func mustExec(t *testing.T, db *gorm.DB, q string, args ...any) {
	t.Helper()
	if err := db.Exec(q, args...).Error; err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func newTenant(t *testing.T, db *gorm.DB, branding any) string {
	t.Helper()
	id := uuid.NewString()
	mustExec(t, db, "INSERT INTO tenants (id, slug, name, branding) VALUES (?, ?, ?, ?)", id, "t-"+id[:8], "Khách "+id[:4], branding)
	return id
}

// seedTenant gives a tenant: apps live + 1 deleted, entities live + 1 deleted, layouts live + 1
// deleted, workflows, approvals, roles, reports, dashboards, users (admin + staff + inactive +
// deleted) and a live + a revoked refresh token.
func seedTenant(t *testing.T, db *gorm.DB, id string, apps, entities, layouts int) {
	t.Helper()
	for i := 0; i < apps; i++ {
		mustExec(t, db, "INSERT INTO sys_apps (id, tenant_id, name, label) VALUES (?,?,?,?)", uuid.NewString(), id, fmt.Sprintf("app%d", i), "App")
	}
	mustExec(t, db, "INSERT INTO sys_apps (id, tenant_id, name, label, deleted_at) VALUES (?,?,?,?,NOW())", uuid.NewString(), id, "gone", "Gone")
	for i := 0; i < entities; i++ {
		mustExec(t, db, "INSERT INTO sys_entities (id, tenant_id, name, label, table_name) VALUES (?,?,?,?,?)",
			uuid.NewString(), id, fmt.Sprintf("e%d", i), "E", fmt.Sprintf("obj_%s_%d", id[:8], i))
	}
	mustExec(t, db, "INSERT INTO sys_entities (id, tenant_id, name, label, table_name, deleted_at) VALUES (?,?,?,?,?,NOW())",
		uuid.NewString(), id, "gone", "G", "obj_gone_"+id[:8])
	for i := 0; i < layouts; i++ {
		mustExec(t, db, "INSERT INTO sys_layouts (id, tenant_id, entity_name, view_type) VALUES (?,?,?,?)", uuid.NewString(), id, fmt.Sprintf("e%d", i), "form")
	}
	mustExec(t, db, "INSERT INTO sys_layouts (id, tenant_id, entity_name, view_type, deleted_at) VALUES (?,?,?,?,NOW())", uuid.NewString(), id, "gone", "form")
	mustExec(t, db, "INSERT INTO sys_workflows (id, tenant_id, name, entity_name, trigger_event, actions) VALUES (?,?,?,?,?,?)",
		uuid.NewString(), id, "w", "e0", "after_create", "[]")
	mustExec(t, db, "INSERT INTO sys_workflows (id, tenant_id, name, entity_name, trigger_event, actions, deleted_at) VALUES (?,?,?,?,?,?,NOW())",
		uuid.NewString(), id, "w2", "e0", "after_create", "[]")
	mustExec(t, db, "INSERT INTO sys_approval_processes (id, tenant_id, name, entity_name, trigger_event, steps) VALUES (?,?,?,?,?,?)",
		uuid.NewString(), id, "ap", "e0", "manual", "[]")
	mustExec(t, db, "INSERT INTO acl_roles (id, tenant_id, name, label) VALUES (?,?,?,?)", uuid.NewString(), id, "r1", "R1")
	mustExec(t, db, "INSERT INTO acl_roles (id, tenant_id, name, label) VALUES (?,?,?,?)", uuid.NewString(), id, "r2", "R2")
	mustExec(t, db, "INSERT INTO sys_reports (id, tenant_id, name, entity_name, type, config) VALUES (?,?,?,?,?,?)",
		uuid.NewString(), id, "rp", "e0", "tabular", "{}")
	mustExec(t, db, "INSERT INTO sys_dashboards (id, tenant_id, name, widgets) VALUES (?,?,?,?)", uuid.NewString(), id, "db", "[]")

	admin, staff := uuid.NewString(), uuid.NewString()
	mustExec(t, db, "INSERT INTO users (id, tenant_id, email, password_hash, status, is_admin) VALUES (?,?,?,?,?,1)", admin, id, "a-"+admin[:6]+"@x.vn", "h", "active")
	mustExec(t, db, "INSERT INTO users (id, tenant_id, email, password_hash, status, is_admin) VALUES (?,?,?,?,?,0)", staff, id, "s-"+staff[:6]+"@x.vn", "h", "active")
	mustExec(t, db, "INSERT INTO users (id, tenant_id, email, password_hash, status, is_admin) VALUES (?,?,?,?,?,0)", uuid.NewString(), id, "i-"+id[:6]+"@x.vn", "h", "disabled")
	mustExec(t, db, "INSERT INTO users (id, tenant_id, email, password_hash, status, is_admin, deleted_at) VALUES (?,?,?,?,?,0,NOW())", uuid.NewString(), id, "d-"+id[:6]+"@x.vn", "h", "active")

	mustExec(t, db, "INSERT INTO refresh_tokens (id, tenant_id, user_id, token_hash, expires_at, last_used_at) VALUES (?,?,?,?,?,?)",
		uuid.NewString(), id, staff, strings.Repeat("a", 63)+id[:1], time.Now().Add(time.Hour), time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC))
	// a LATER use on a revoked token must not count as activity
	mustExec(t, db, "INSERT INTO refresh_tokens (id, tenant_id, user_id, token_hash, expires_at, last_used_at, revoked_at) VALUES (?,?,?,?,?,?,NOW())",
		uuid.NewString(), id, staff, strings.Repeat("b", 63)+id[:1], time.Now().Add(time.Hour), time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC))
}

func cleanupTenants(t *testing.T, db *gorm.DB, ids ...string) {
	t.Cleanup(func() {
		for _, tbl := range []string{"sys_project_profiles", "refresh_tokens", "users", "sys_apps", "sys_entities", "sys_layouts",
			"sys_workflows", "sys_approval_processes", "acl_roles", "sys_reports", "sys_dashboards"} {
			for _, id := range ids {
				if err := db.Exec("DELETE FROM "+tbl+" WHERE tenant_id = ?", id).Error; err != nil {
					t.Errorf("cleanup %s: %v", tbl, err)
				}
			}
		}
		for _, id := range ids {
			if err := db.Exec("DELETE FROM tenants WHERE id = ?", id).Error; err != nil {
				t.Errorf("cleanup tenants: %v", err)
			}
		}
	})
}

func TestUpsert_OneProfilePerTenant_UpdatesOnlyTheListedColumns(t *testing.T) {
	db := pjDB(t)
	repo := NewRepository(db)
	tid := newTenant(t, db, nil)
	cleanupTenants(t, db, tid)
	ctx := context.Background()

	ind := "Phòng khám"
	if err := repo.Upsert(ctx, &Profile{ID: uuid.NewString(), TenantID: tid, Industry: &ind, CreatedBy: "u1", UpdatedBy: "u1"}, "industry", "updated_by"); err != nil {
		t.Fatal(err)
	}
	// a health-check from another request: same tenant, a different id, different columns
	st := HealthWarn
	if err := repo.Upsert(ctx, &Profile{ID: uuid.NewString(), TenantID: tid, HealthStatus: &st, HealthJSON: []byte(`{"critical":0,"warn":1}`), UpdatedBy: "u2"},
		"health_status", "health_json", "updated_by"); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := db.Raw("SELECT COUNT(*) FROM sys_project_profiles WHERE tenant_id = ?", tid).Scan(&n).Error; err != nil || n != 1 {
		t.Fatalf("want exactly one profile row, got %d (%v)", n, err)
	}
	p, err := repo.Get(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	if p.Industry == nil || *p.Industry != ind {
		t.Fatalf("the health upsert clobbered the industry: %+v", p.Industry)
	}
	if p.HealthStatus == nil || *p.HealthStatus != HealthWarn || p.UpdatedBy != "u2" {
		t.Fatalf("health columns not written: %+v", p)
	}
	if p.CreatedBy != "u1" {
		t.Fatalf("created_by must survive the conflict path, got %q", p.CreatedBy)
	}
}

func TestUpsert_ResurrectsASoftDeletedProfile(t *testing.T) {
	db := pjDB(t)
	repo := NewRepository(db)
	tid := newTenant(t, db, nil)
	cleanupTenants(t, db, tid)
	ctx := context.Background()

	a := "a"
	if err := repo.Upsert(ctx, &Profile{ID: uuid.NewString(), TenantID: tid, Industry: &a, UpdatedBy: "u1"}, "industry", "updated_by"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "UPDATE sys_project_profiles SET deleted_at = NOW() WHERE tenant_id = ?", tid)
	if _, err := repo.Get(ctx, tid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a soft-deleted profile must be hidden, got %v", err)
	}
	b := "b"
	if err := repo.Upsert(ctx, &Profile{ID: uuid.NewString(), TenantID: tid, Industry: &b, UpdatedBy: "u2"}, "industry", "updated_by"); err != nil {
		t.Fatal(err)
	}
	p, err := repo.Get(ctx, tid)
	if err != nil {
		t.Fatalf("an upsert must make the row visible again: %v", err)
	}
	if p.Industry == nil || *p.Industry != b {
		t.Fatalf("industry = %v, want b", p.Industry)
	}
}

func TestListByTenants_OnlyTheAskedOnes_AndGetMisses(t *testing.T) {
	db := pjDB(t)
	repo := NewRepository(db)
	a, b := newTenant(t, db, nil), newTenant(t, db, nil)
	cleanupTenants(t, db, a, b)
	ctx := context.Background()
	for _, id := range []string{a, b} {
		if err := repo.Upsert(ctx, &Profile{ID: uuid.NewString(), TenantID: id}, "updated_by"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := repo.ListByTenants(ctx, []string{a})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[a].TenantID != a {
		t.Fatalf("want only tenant a, got %v", got)
	}
	if empty, err := repo.ListByTenants(ctx, nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty ask must be empty, got %v %v", empty, err)
	}
	if _, err := repo.Get(ctx, uuid.NewString()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get of a tenant without profile = %v, want ErrNotFound", err)
	}
}

func TestTenantRow_RetiredIsNotFound_SuspendedIsReturned(t *testing.T) {
	db := pjDB(t)
	repo := NewRepository(db)
	live, suspended, retired := newTenant(t, db, nil), newTenant(t, db, nil), newTenant(t, db, nil)
	cleanupTenants(t, db, live, suspended, retired)
	mustExec(t, db, "UPDATE tenants SET status = 'suspended' WHERE id = ?", suspended)
	mustExec(t, db, "UPDATE tenants SET deleted_at = NOW() WHERE id = ?", retired)
	ctx := context.Background()
	if r, err := repo.TenantRow(ctx, suspended); err != nil || r.Status != "suspended" {
		t.Fatalf("suspended = %+v %v", r, err)
	}
	if _, err := repo.TenantRow(ctx, retired); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retired = %v, want ErrNotFound", err)
	}
	if _, err := repo.TenantRow(ctx, live); err != nil {
		t.Fatal(err)
	}
}

func TestSignals_CountsAreExactAndTenantPinned(t *testing.T) {
	db := pjDB(t)
	repo := NewRepository(db)
	a := newTenant(t, db, `{"appName":"Phòng khám A"}`)
	b := newTenant(t, db, "null") // JSON null = no branding document
	c := newTenant(t, db, nil)    // untouched: every signal zero
	cleanupTenants(t, db, a, b, c)
	seedTenant(t, db, a, 2, 3, 1)
	seedTenant(t, db, b, 1, 1, 0)

	got, err := repo.Signals(context.Background(), []string{a, b, c})
	if err != nil {
		t.Fatal(err)
	}
	wantA := Counts{Apps: 2, Entities: 3, CustomLayouts: 1, Workflows: 1, Approvals: 1, Roles: 2, Users: 2, StaffUsers: 1, Reports: 1, Dashboards: 1}
	if got[a].Counts != wantA {
		t.Fatalf("a counts = %+v, want %+v", got[a].Counts, wantA)
	}
	wantB := Counts{Apps: 1, Entities: 1, CustomLayouts: 0, Workflows: 1, Approvals: 1, Roles: 2, Users: 2, StaffUsers: 1, Reports: 1, Dashboards: 1}
	if got[b].Counts != wantB {
		t.Fatalf("b counts = %+v, want %+v", got[b].Counts, wantB)
	}
	if got[c].Counts != (Counts{}) || got[c].Branded || got[c].LastActiveAt != nil {
		t.Fatalf("c must be all zero, got %+v", got[c])
	}
	if !got[a].Branded || got[b].Branded {
		t.Fatalf("branded: a=%v (want true) b(JSON null)=%v (want false)", got[a].Branded, got[b].Branded)
	}
	// last active = the live token's use (Oct 1), NOT the later use of the revoked one (Oct 5)
	if at := got[a].LastActiveAt; at == nil || at.UTC().Day() != 1 {
		t.Fatalf("a lastActive = %v, want 2026-10-01", at)
	}
	// asking for only c must not see a's rows (no cross-tenant bleed)
	only, err := repo.Signals(context.Background(), []string{c})
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || only[c].Counts != (Counts{}) {
		t.Fatalf("Signals([c]) = %+v", only)
	}
}

// The statement count of the list read is a function of the code, not of the data: 3 customers and
// 50 customers cost the same number of statements (FR-027).
func TestListReads_StatementCountDoesNotGrowWithTenants(t *testing.T) {
	db := pjDB(t)
	var stmts atomic.Int64
	count := func(*gorm.DB) { stmts.Add(1) }
	if err := db.Callback().Query().After("gorm:query").Register("pj_count_query", count); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Row().After("gorm:row").Register("pj_count_row", count); err != nil {
		t.Fatal(err)
	}
	repo := NewRepository(db)

	measure := func(n int) int64 {
		ids := make([]string, n)
		for i := range ids {
			ids[i] = uuid.NewString() // unknown tenants are fine: the statements still run
		}
		stmts.Store(0)
		if _, err := repo.ListByTenants(context.Background(), ids); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.Signals(context.Background(), ids); err != nil {
			t.Fatal(err)
		}
		return stmts.Load()
	}
	small, large := measure(3), measure(50)
	if small == 0 {
		t.Fatal("the counter saw no statements — the callbacks are not wired, the test proves nothing")
	}
	if small != large {
		t.Fatalf("statements grew with tenants: 3 -> %d, 50 -> %d", small, large)
	}
	// 1 profile + 9 counts + 1 users... = see countQueries; the budget is the plan's, not a guess.
	if want := int64(1 + len(countQueries) + 4); small != want {
		t.Fatalf("statements = %d, want %d (profiles + %d counts + last-active + branded + 2 backup)", small, want, len(countQueries))
	}
}

func TestCreateInTx_RollsBackWithTheCaller(t *testing.T) {
	db := pjDB(t)
	repo := NewRepository(db)
	tid := newTenant(t, db, nil)
	cleanupTenants(t, db, tid)
	ctx := context.Background()

	rollback := errors.New("force rollback")
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := repo.CreateInTx(ctx, tx, &Profile{ID: uuid.NewString(), TenantID: tid}); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("tx error = %v", err)
	}
	if _, err := repo.Get(ctx, tid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a rolled-back profile must not exist, got %v", err)
	}
}

// specs/073 so3 / 068 T100: backup signal = last ready time + "latest finished attempt failed", per tenant.
func TestSignals_BackupSignal(t *testing.T) {
	db := pjDB(t)
	repo := NewRepository(db)
	a := newTenant(t, db, nil) // ready then failed => lastFailed
	b := newTenant(t, db, nil) // failed then ready => not failed
	c := newTenant(t, db, nil) // never backed up
	cleanupTenants(t, db, a, b, c)
	ins := func(tenant, status, at string) {
		mustExec(t, db, "INSERT INTO sys_tenant_backups (id, tenant_id, kind, status, format_version, schema_version, created_at, finished_at) "+
			"VALUES (UUID(), ?, 'manual', ?, 1, 1, ?, ?)", tenant, status, at, at)
	}
	ins(a, "ready", "2026-10-01 10:00:00")
	ins(a, "failed", "2026-10-02 10:00:00")
	ins(b, "failed", "2026-10-01 10:00:00")
	ins(b, "ready", "2026-10-02 10:00:00")
	t.Cleanup(func() { db.Exec("DELETE FROM sys_tenant_backups WHERE tenant_id IN ?", []string{a, b, c}) })
	got, err := repo.Signals(context.Background(), []string{a, b, c})
	if err != nil {
		t.Fatal(err)
	}
	if !got[a].Backup.LastFailed || got[a].Backup.LastReadyAt == nil || got[a].Backup.LastReadyAt.UTC().Day() != 1 {
		t.Fatalf("a = %+v", got[a].Backup)
	}
	if got[b].Backup.LastFailed || got[b].Backup.LastReadyAt == nil || got[b].Backup.LastReadyAt.UTC().Day() != 2 {
		t.Fatalf("b = %+v", got[b].Backup)
	}
	if got[c].Backup.LastFailed || got[c].Backup.LastReadyAt != nil {
		t.Fatalf("c = %+v", got[c].Backup)
	}
}
