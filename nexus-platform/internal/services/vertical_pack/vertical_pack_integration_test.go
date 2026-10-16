//go:build integration

// Integration test for the vertical seed-pack (M1): export a tenant's metadata as a reusable
// pack and install it into ANOTHER tenant, proving the round trip reproduces the schema, that
// re-install is idempotent, and that install is strictly tenant-scoped (no cross-tenant bleed).
//
// It wires the PRODUCTION composition against REAL MySQL:
//
//	real metadata.Repository (GORM, sys_*) + real metadata.LayoutRepository
//	+ real auth.RoleRepository + real dynrepo (sqlx, obj_*)
//	-> real EntityService / FieldService / LayoutService / RoleService
//	-> vertical_pack.Service (export readers = repos; install writers = services)
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/vertical_pack/ -race -v
//
// Each test builds and drops its OWN throwaway database (never nexus_platform). Requires a
// reachable MySQL 8 root connection and the golang-migrate CLI on PATH (or ~/go/bin/migrate).
package vertical_pack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/ddl"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
	"nexus/platform/internal/workqueue"
)

const vpMigrationsDirDefault = "../../../migrations"

// vpMigrationsDir returns the migrations path for the throwaway-DB setup. It honors the
// VP_MIGRATIONS_DIR env override so the test can run against an isolated, deduplicated migration
// chain when the shared migrations/ dir has a transient parallel-track number collision (two
// uncommitted siblings claiming the same NNNNNN) that would otherwise make `migrate up` fail with
// "duplicate migration file". With no override it uses the in-repo migrations dir.
func vpMigrationsDir() string {
	if v := os.Getenv("VP_MIGRATIONS_DIR"); v != "" {
		return v
	}
	return vpMigrationsDirDefault
}

const (
	vpTenantA = "aaaaaaaa-1a1a-1a1a-1a1a-aaaaaaaaaaaa"
	vpTenantB = "bbbbbbbb-1b1b-1b1b-1b1b-bbbbbbbbbbbb"
	vpTenantC = "cccccccc-1c1c-1c1c-1c1c-cccccccccccc"
)

var vpDBCounter int64

func vpUniqueDBName() string {
	n := atomic.AddInt64(&vpDBCounter, 1)
	return fmt.Sprintf("nexus_vp_it_%d_%d", os.Getpid(), n)
}

// vpSplitDSN splits TEST_DATABASE_URL so GORM, sqlx, and migrate all target the SAME throwaway DB.
func vpSplitDSN(t *testing.T) (creds, host, params string) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		base = "root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local"
	}
	at := strings.Index(base, "@tcp(")
	if at < 0 {
		t.Fatalf("vpSplitDSN: unexpected DSN shape %q", base)
	}
	creds = base[:at]
	rest := base[at:]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		t.Fatalf("vpSplitDSN: no db segment in DSN %q", base)
	}
	host = rest[:slash+1]
	if q := strings.Index(rest, "?"); q >= 0 {
		params = rest[q:]
	}
	return creds, host, params
}

func vpAdminDSN(t *testing.T) string { c, h, p := vpSplitDSN(t); return c + h + p }
func vpDSN(t *testing.T, db string) string {
	c, h, p := vpSplitDSN(t)
	return c + h + db + p
}
func vpMigrateDSN(t *testing.T, db string) string {
	c, h, p := vpSplitDSN(t)
	return "mysql://" + c + h + db + p
}

func vpMigrateBin(t *testing.T) string {
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

// vpSetup builds a fresh throwaway DB, runs ALL migrations up, and returns a GORM + sqlx handle
// bound to it. Skips (not fails) when MySQL or the migrate CLI is unavailable.
func vpSetup(t *testing.T) (*gorm.DB, *sqlx.DB) {
	t.Helper()
	dbName := vpUniqueDBName()

	admin, err := gorm.Open(gormmysql.Open(vpAdminDSN(t)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Skipf("vp: MySQL not reachable (%v) — skipping", err)
	}
	if sqlDB, err := admin.DB(); err != nil || sqlDB.Ping() != nil {
		t.Skipf("vp: MySQL not pingable — skipping")
	}
	if err := admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`").Error; err != nil {
		t.Fatalf("vp: drop throwaway DB: %v", err)
	}
	if err := admin.Exec("CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci").Error; err != nil {
		t.Fatalf("vp: create throwaway DB: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	migrateBin := vpMigrateBin(t)
	if migrateBin == "" {
		t.Skip("vp: migrate binary not found — skipping")
	}
	cmd := exec.Command(migrateBin, "-path", vpMigrationsDir(), "-database", vpMigrateDSN(t, dbName), "up")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("vp: migrate up: %v\n%s", err, out)
	}

	gdb, err := gorm.Open(gormmysql.Open(vpDSN(t, dbName)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("vp: open gorm: %v", err)
	}
	sdb, err := sqlx.Connect("mysql", vpDSN(t, dbName))
	if err != nil {
		t.Fatalf("vp: open sqlx: %v", err)
	}
	t.Cleanup(func() {
		if d, err := gdb.DB(); err == nil {
			_ = d.Close()
		}
		_ = sdb.Close()
	})

	// Seed the tenant rows the test installs into. acl_roles.tenant_id has an FK to
	// tenants(id) (fk_acl_roles_tenant), so role install would 1452 without these. Entities/
	// fields/layouts carry no such FK, but roles do — seed all three test tenants up front.
	for i, tid := range []string{vpTenantA, vpTenantB, vpTenantC} {
		slug := fmt.Sprintf("vp-it-%d-%d", os.Getpid(), i)
		if err := gdb.Exec(
			"INSERT INTO tenants (id, slug, name, status) VALUES (?, ?, ?, 'active')",
			tid, slug, "VP IT Tenant "+slug,
		).Error; err != nil {
			t.Fatalf("vp: seed tenant %s: %v", tid, err)
		}
	}
	return gdb, sdb
}

// vpBuild wires the vertical-pack service over the production composition for a throwaway DB.
func vpBuild(gdb *gorm.DB, sdb *sqlx.DB) *Service {
	metaRepo := metadata.NewRepository(gdb)
	layoutRepo := metadata.NewLayoutRepository(gdb)
	roleRepo := auth.NewRoleRepository(gdb)
	dyn := dynrepo.New(sdb)

	mc := services.NewMetaCache(metaRepo, services.DefaultMetaTTL) // as cmd/server: a field added at (re)install is visible to the validators at once
	entitySvc := services.NewEntityService(dyn, metaRepo).WithMetaCache(mc)
	fieldSvc := services.NewFieldService(dyn, metaRepo).WithMetaCache(mc)
	layoutSvc := services.NewLayoutService(metaRepo, layoutRepo, zap.NewNop())
	roleSvc := services.NewRoleService(roleRepo)
	relationSvc := services.NewRelationService(dyn, metaRepo)
	// Mirror cmd/server/main.go's production RecordService wiring: the deepened CRM pack's
	// samples need WithSequenceClaimer (auto_number assignSequences fails closed without it —
	// RECORD_AUTONUMBER_UNAVAILABLE would abort install), and reads in the tests need
	// WithFieldValueFetcher (lookup) + WithRollupMeta/WithRollupLinked (?rollup=, incl. N-N).
	recordSvc := services.NewRecordService(metaRepo, dyn).
		WithLabelFetcher(dyn).
		WithFieldValueFetcher(dyn).
		WithRollupMeta(metaRepo).
		WithRollupLinked(dyn).
		WithSequenceClaimer(dyn)
	dashboardRepo := dashboard.NewSQLRepository(sdb)
	dashboardSvc := services.NewDashboardService(dashboardRepo, recordSvc, zap.NewNop())

	return NewService(Deps{
		EntityReader:       metaRepo,
		LayoutReader:       layoutRepo,
		RoleReader:         roleRepo,
		EntityWriter:       entitySvc,
		EntityExists:       metaRepo,
		FieldWriter:        fieldSvc,
		LayoutWriter:       layoutSvc,
		RoleWriter:         roleSvc,
		RelationWriter:     relationSvc,
		RecordWriter:       recordSvc,
		Linker:             relationSvc,
		RecordExists:       dyn,
		RecordFetcher:      dyn,
		DashboardWriter:    dashboardSvc,
		RelationshipReader: metaRepo, // W8-BE-1: N-N export from the registry
		RecordLister:       dyn,      // W8-BE-1: sample export (?includeData=true)
		ObjProber:          dyn,      // W8-BE-2: dry-run unique-slot conflict probe
	})
}

// vpBuildStore mirrors vpBuild but also wires an in-memory install ledger so a test can assert the
// installer records one row per successful install. Returns the service and the store to inspect.
// (Using an in-package fake avoids importing internal/packinstall — which imports this package —
// into this internal test; the real GORM ledger is covered by packinstall's own integration test.)
func vpBuildStore(gdb *gorm.DB, sdb *sqlx.DB) (*Service, *fakeInstallStore) {
	metaRepo := metadata.NewRepository(gdb)
	layoutRepo := metadata.NewLayoutRepository(gdb)
	roleRepo := auth.NewRoleRepository(gdb)
	dyn := dynrepo.New(sdb)

	mc := services.NewMetaCache(metaRepo, services.DefaultMetaTTL) // as cmd/server: a field added at (re)install is visible to the validators at once
	entitySvc := services.NewEntityService(dyn, metaRepo).WithMetaCache(mc)
	fieldSvc := services.NewFieldService(dyn, metaRepo).WithMetaCache(mc)
	layoutSvc := services.NewLayoutService(metaRepo, layoutRepo, zap.NewNop())
	roleSvc := services.NewRoleService(roleRepo)
	relationSvc := services.NewRelationService(dyn, metaRepo)
	recordSvc := services.NewRecordService(metaRepo, dyn).
		WithLabelFetcher(dyn).
		WithFieldValueFetcher(dyn).
		WithRollupMeta(metaRepo).
		WithRollupLinked(dyn).
		WithSequenceClaimer(dyn)
	dashboardRepo := dashboard.NewSQLRepository(sdb)
	dashboardSvc := services.NewDashboardService(dashboardRepo, recordSvc, zap.NewNop())

	store := &fakeInstallStore{}
	svc := NewService(Deps{
		EntityReader:       metaRepo,
		LayoutReader:       layoutRepo,
		RoleReader:         roleRepo,
		EntityWriter:       entitySvc,
		EntityExists:       metaRepo,
		FieldWriter:        fieldSvc,
		LayoutWriter:       layoutSvc,
		RoleWriter:         roleSvc,
		RelationWriter:     relationSvc,
		RecordWriter:       recordSvc,
		Linker:             relationSvc,
		RecordExists:       dyn,
		RecordFetcher:      dyn,
		DashboardWriter:    dashboardSvc,
		RelationshipReader: metaRepo,
		RecordLister:       dyn,
		InstallStore:       store,
		ObjProber:          dyn,
	})
	return svc, store
}

// fakeInstallStore is an in-memory installStore for tests: it records every RecordInstall call and
// serves them tenant-scoped, newest-first, from ListInstalls.
type fakeInstallStore struct {
	mu         sync.Mutex
	recs       []InstallRecord
	failRecord bool // when true, RecordInstall returns an error (fail-open coverage)
}

func (f *fakeInstallStore) RecordInstall(_ context.Context, rec InstallRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRecord {
		return fmt.Errorf("simulated ledger insert failure")
	}
	f.recs = append(f.recs, rec)
	return nil
}

func (f *fakeInstallStore) ListInstalls(_ context.Context, tenantID string) ([]InstallRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []InstallRecord
	for i := len(f.recs) - 1; i >= 0; i-- {
		if f.recs[i].TenantID == tenantID {
			out = append(out, f.recs[i])
		}
	}
	return out, nil
}

// vpSeedUserID is the installer user ID used in integration tests (a stable placeholder UUID).
const vpSeedUserID = "00000000-0000-0000-0000-000000000001"

// seedPack is the pack the test installs into tenant A (then exports and re-installs into B). It
// exercises every resource kind: ordinary/unique/picklist/currency fields, a relation field
// (parent→child), a list layout, and a role with grants.
func seedPack() *Pack {
	return &Pack{
		Name: "TestVertical",
		Entities: []PackEntity{
			{
				Name:  "vp_companies",
				Label: "Công ty",
				Fields: []PackField{
					{Name: "name", Label: "Tên", FieldType: "text", IsRequired: true},
					{Name: "tax_code", Label: "MST", FieldType: "text", IsUnique: true},
					{Name: "tier", Label: "Hạng", FieldType: "picklist", Options: map[string]any{
						"values": []any{"A", "B", "C"},
					}},
				},
			},
			{
				Name:  "vp_people",
				Label: "Người",
				Fields: []PackField{
					{Name: "full_name", Label: "Họ tên", FieldType: "text", IsRequired: true},
					{Name: "email", Label: "Email", FieldType: "email", IsUnique: true},
					// relation → vp_companies (FK-on-child)
					{Name: "company", Label: "Công ty", FieldType: "relation", Options: map[string]any{
						"relatedEntity": "vp_companies",
					}},
				},
			},
		},
		Layouts: []PackLayout{
			{EntityName: "vp_companies", ViewType: "list", LayoutJSON: map[string]any{
				"version": 1,
				"columns": []any{
					map[string]any{"field": "name"},
					map[string]any{"field": "tier"},
				},
				"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
			}},
		},
		Roles: []PackRole{
			{
				Name:  "vp_viewer",
				Label: "Người xem",
				Actions: []PackRoleAction{
					{EntityName: "vp_companies", Action: "view", AccessLevel: "all"},
					{EntityName: "vp_people", Action: "view", AccessLevel: "team"},
				},
			},
		},
	}
}

// TestVerticalPack_ExportInstall_RoundTrip is the core test: install a pack into tenant A, EXPORT
// it from A, INSTALL the export into tenant B, and assert B's entities/fields/relationship/layout/
// role match. Then re-install into B (idempotent: nothing created), and confirm tenant C (which
// never installed) sees NONE of the entities (tenant-isolation).
func TestVerticalPack_ExportInstall_RoundTrip(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc := vpBuild(gdb, sdb)
	ctx := context.Background()

	// --- Install the seed pack into tenant A. ---
	if _, err := svc.Install(ctx, vpTenantA, vpSeedUserID, seedPack()); err != nil {
		t.Fatalf("install into A: %v", err)
	}

	// --- Export from A (all entities). ---
	pack, _, err := svc.Export(ctx, vpTenantA, nil, false, nil)
	if err != nil {
		t.Fatalf("export from A: %v", err)
	}
	if len(pack.Entities) != 2 {
		t.Fatalf("export: got %d entities, want 2", len(pack.Entities))
	}
	// The relation must appear in the denormalized Relationships projection.
	foundRel := false
	for _, r := range pack.Relationships {
		if r.ChildEntity == "vp_people" && r.Name == "company" && r.ParentEntity == "vp_companies" {
			foundRel = true
		}
	}
	if !foundRel {
		t.Errorf("export: relationship vp_people.company -> vp_companies missing; got %+v", pack.Relationships)
	}
	// Layout + role survived the export.
	if len(pack.Layouts) == 0 {
		t.Errorf("export: no layouts in pack")
	}
	if len(pack.Roles) == 0 {
		t.Errorf("export: no roles in pack")
	}

	// --- Install the EXPORT into tenant B. ---
	res, err := svc.Install(ctx, vpTenantB, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("install export into B: %v", err)
	}
	if len(res.EntitiesCreated) != 2 {
		t.Errorf("install B: created %v entities, want 2", res.EntitiesCreated)
	}
	if len(res.RelationFieldsAdded) != 1 {
		t.Errorf("install B: relation fields added %v, want 1", res.RelationFieldsAdded)
	}

	// --- Verify B's metadata matches. ---
	metaRepo := metadata.NewRepository(gdb)
	bEntities, err := metaRepo.ListEntities(vpTenantB)
	if err != nil {
		t.Fatalf("list B entities: %v", err)
	}
	if len(bEntities) != 2 {
		t.Fatalf("B has %d entities, want 2", len(bEntities))
	}
	byName := map[string]metadata.EntityDefinition{}
	for _, e := range bEntities {
		byName[e.Name] = e
	}
	people, ok := byName["vp_people"]
	if !ok {
		t.Fatalf("B missing vp_people")
	}
	pf, err := metaRepo.ListFields(ctx, vpTenantB, people.ID)
	if err != nil {
		t.Fatalf("list B vp_people fields: %v", err)
	}
	fieldNames := map[string]metadata.FieldDefinition{}
	for _, f := range pf {
		fieldNames[f.Name] = f
	}
	if _, ok := fieldNames["company"]; !ok {
		t.Errorf("B vp_people missing relation field 'company'; got %v", keys(fieldNames))
	}
	if f, ok := fieldNames["email"]; !ok || !f.IsUnique {
		t.Errorf("B vp_people 'email' missing or not unique: %+v", f)
	}
	// The relation FK column must physically exist on B's obj_vp_people table.
	if !vpColumnExists(t, sdb, "obj_vp_people", "company_id") {
		t.Errorf("B obj_vp_people missing FK column company_id")
	}

	// B's layout for vp_companies:list installed.
	layoutRepo := metadata.NewLayoutRepository(gdb)
	bLayouts, err := layoutRepo.GetByEntity(ctx, vpTenantB, "vp_companies")
	if err != nil {
		t.Fatalf("list B layouts: %v", err)
	}
	if len(bLayouts) == 0 {
		t.Errorf("B has no layout for vp_companies")
	}
	// B's role installed.
	roleRepo := auth.NewRoleRepository(gdb)
	bRoles, err := roleRepo.ListRoles(ctx, vpTenantB)
	if err != nil {
		t.Fatalf("list B roles: %v", err)
	}
	if len(bRoles) != 1 || bRoles[0].Name != "vp_viewer" {
		t.Errorf("B roles = %+v, want exactly [vp_viewer]", bRoles)
	}

	// --- Idempotent re-install into B: nothing new created. ---
	res2, err := svc.Install(ctx, vpTenantB, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("re-install into B: %v", err)
	}
	if len(res2.EntitiesCreated) != 0 {
		t.Errorf("re-install B: created %v entities, want 0 (idempotent)", res2.EntitiesCreated)
	}
	if len(res2.EntitiesSkipped) != 2 {
		t.Errorf("re-install B: skipped %v entities, want 2", res2.EntitiesSkipped)
	}
	if len(res2.FieldsSkipped) != 1 {
		t.Errorf("re-install B: relation fields skipped %v, want 1", res2.FieldsSkipped)
	}
	if len(res2.RolesSkipped) != 1 {
		t.Errorf("re-install B: roles skipped %v, want 1", res2.RolesSkipped)
	}
	// Still exactly 2 entities for B (no duplicates).
	bEntities2, _ := metaRepo.ListEntities(vpTenantB)
	if len(bEntities2) != 2 {
		t.Errorf("after re-install B has %d entities, want 2 (no dup)", len(bEntities2))
	}

	// --- Tenant isolation: tenant C never installed → sees NO entities/roles. ---
	cEntities, err := metaRepo.ListEntities(vpTenantC)
	if err != nil {
		t.Fatalf("list C entities: %v", err)
	}
	if len(cEntities) != 0 {
		t.Errorf("tenant C must see 0 entities (isolation), got %d", len(cEntities))
	}
	cRoles, _ := roleRepo.ListRoles(ctx, vpTenantC)
	if len(cRoles) != 0 {
		t.Errorf("tenant C must see 0 roles (isolation), got %d", len(cRoles))
	}
	// And exporting from C is empty (its own scope only).
	cPack, _, err := svc.Export(ctx, vpTenantC, nil, false, nil)
	if err != nil {
		t.Fatalf("export from C: %v", err)
	}
	if len(cPack.Entities) != 0 {
		t.Errorf("export from C must be empty (isolation), got %d entities", len(cPack.Entities))
	}
}

// TestVerticalPack_InstallBuiltin installs the built-in CRM pack and asserts:
//   - every entity the pack declares is created (crmEntityCount)
//   - 10 relation fields added; FK columns materialised for contacts.account,
//     contacts.primary_for, opportunities.account, activities.account (primary_for is the 1-1)
//   - 5 deferred rollup fields added in Pass 2b
//   - B1 indexed generated columns present for each indexed picklist field
//   - N-N join table rel_crm_contacts_crm_opportunities created
//   - 31 sample records inserted (breakdown at the assertion)
//   - N-N link rows created (4 stakeholder links)
//   - 2 roles created
//
// crmEntityCount reads how many entities the CRM pack DECLARES, instead of restating a number
// here. A literal count rots on the next pack change and the failure looks like a bug in install
// (it did, twice: the file/valueLabels bump and again when crm_bank_txns landed). The contract
// these tests actually mean is "install creates everything the pack declares".
func crmEntityCount(t *testing.T) int {
	t.Helper()
	p, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin pack crm missing")
	}
	return len(p.Entities)
}

func TestVerticalPack_InstallBuiltin(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc := vpBuild(gdb, sdb)
	ctx := context.Background()

	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm pack not found")
	}
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, crm)
	if err != nil {
		t.Fatalf("install builtin crm: %v", err)
	}

	if want := crmEntityCount(t); len(res.EntitiesCreated) != want {
		t.Errorf("crm: created %v (%d), want %d — every entity the pack declares", res.EntitiesCreated, len(res.EntitiesCreated), want)
	}
	// 7 relation fields (contacts.account, contacts.primary_for, opportunities.account,
	// activities.account, quotes.account, quote_lines.quote, contracts.account).
	// 10, not 7: CRM 1.15.0 added the convert chain (crm_quotes.opportunity,
	// crm_contracts.quote) plus crm_quote_lines.product pointing at the new price list.
	if len(res.RelationFieldsAdded) != 10 {
		t.Errorf("crm: relation fields added %v, want 10", res.RelationFieldsAdded)
	}
	// 5 deferred rollup fields added in Pass 2b (the 4 pre-existing + crm_quotes.total).
	if len(res.DeferredFieldsAdded) != 5 {
		t.Errorf("crm: deferred fields added %v, want 5", res.DeferredFieldsAdded)
	}

	// FK columns must be materialised on the shared obj_* tables (primary_for is the 1-1
	// CASE-guarded column + tenant-scoped UNIQUE, same column-name convention).
	for _, tc := range []struct{ table, col string }{
		{"obj_crm_contacts", "account_id"},
		{"obj_crm_contacts", "primary_for_id"},
		{"obj_crm_opportunities", "account_id"},
		{"obj_crm_activities", "account_id"},
	} {
		if !vpColumnExists(t, sdb, tc.table, tc.col) {
			t.Errorf("crm: %s missing FK column %s", tc.table, tc.col)
		}
	}

	// B1: indexed generated columns for the 4 B1 fields.
	// DDL names the generated column after the field name (e.g. "stage" → col "stage",
	// index "idx_obj_crm_opportunities_stage").
	for _, tc := range []struct{ table, col string }{
		{"obj_crm_accounts", "industry"},
		{"obj_crm_opportunities", "stage"},
		{"obj_crm_leads", "status"},
		{"obj_crm_activities", "type"},
	} {
		if !vpColumnExists(t, sdb, tc.table, tc.col) {
			t.Errorf("crm: B1 indexed generated column %s.%s missing", tc.table, tc.col)
		}
	}

	// N-N join table created (canonical name via JoinParts alphabetical sort).
	joinTable, _, _, err := ddl.JoinParts("crm_contacts", "crm_opportunities")
	if err != nil {
		t.Fatalf("JoinParts: %v", err)
	}
	if !vpTableExists(t, sdb, joinTable) {
		t.Errorf("crm: N-N join table %q not created", joinTable)
	}
	if len(res.NNRelationsCreated) != 1 || res.NNRelationsCreated[0] != "crm_contacts_opportunities" {
		t.Errorf("crm: NNRelationsCreated = %v, want [crm_contacts_opportunities]", res.NNRelationsCreated)
	}

	// Sample records: 4 accounts + 4 contacts + 5 opportunities + 4 leads + 4 activities
	// + 2 quotes + 5 quote_lines + 3 contracts = 31.
	const wantSamples = 31
	if res.SamplesInserted != wantSamples {
		t.Errorf("crm: SamplesInserted = %d, want %d", res.SamplesInserted, wantSamples)
	}

	// N-N links: 4 stakeholder links.
	if res.LinksCreated != 4 {
		t.Errorf("crm: LinksCreated = %d, want 4", res.LinksCreated)
	}
	// Verify actual join-table rows in the DB.
	var linkCount int
	if err := sdb.Get(&linkCount,
		"SELECT COUNT(*) FROM `"+joinTable+"` WHERE tenant_id = ?", vpTenantA); err != nil {
		t.Fatalf("count join rows: %v", err)
	}
	if linkCount != 4 {
		t.Errorf("crm: join table has %d rows, want 4", linkCount)
	}

	// Two roles created.
	if len(res.RolesCreated) != 2 {
		t.Errorf("crm: roles created %v, want 2", res.RolesCreated)
	}

	// --- Re-install into the SAME tenant: must COMPLETE (skip-on-dup), not abort mid-seed. ---
	// Contacts carry the unique email → 4 RECORD_DUPLICATE rows skipped; their 4 stakeholder
	// links cascade-skip (endpoint id unknown). Entities without unique fields re-insert as
	// duplicates (accounts 4 + opportunities 5 + leads 4 + activities 4 + quotes 2 +
	// quote_lines 5 + contracts 3 = 27) per the documented partial-idempotency contract. The install reaching
	// Pass 7 (dashboard skip) proves it ran through to the end instead of 409ing partway.
	res2, err := svc.Install(ctx, vpTenantA, vpSeedUserID, crm)
	if err != nil {
		t.Fatalf("re-install builtin crm into same tenant: %v", err)
	}
	if want := crmEntityCount(t); len(res2.EntitiesCreated) != 0 || len(res2.EntitiesSkipped) != want {
		t.Errorf("re-install: entities created %v / skipped %v, want 0 / %d", res2.EntitiesCreated, res2.EntitiesSkipped, want)
	}
	if res2.SamplesSkipped != 4 {
		t.Errorf("re-install: SamplesSkipped = %d, want 4 (contacts on unique email)", res2.SamplesSkipped)
	}
	if res2.SamplesInserted != 27 {
		t.Errorf("re-install: SamplesInserted = %d, want 27 (non-unique rows duplicate)", res2.SamplesInserted)
	}
	if res2.LinksCreated != 0 || res2.LinksSkipped != 4 {
		t.Errorf("re-install: LinksCreated/LinksSkipped = %d/%d, want 0/4 (cascade-skip)", res2.LinksCreated, res2.LinksSkipped)
	}
	// Pass 7 actually ran (the install did not abort mid-seed): the dashboard is skipped by name.
	if len(res2.DashboardsSkipped) != 1 || res2.DashboardsSkipped[0] != "CRM Overview" {
		t.Errorf("re-install: DashboardsSkipped = %v, want [CRM Overview]", res2.DashboardsSkipped)
	}
	// No new join rows appeared.
	if err := sdb.Get(&linkCount,
		"SELECT COUNT(*) FROM `"+joinTable+"` WHERE tenant_id = ?", vpTenantA); err != nil {
		t.Fatalf("count join rows after re-install: %v", err)
	}
	if linkCount != 4 {
		t.Errorf("re-install: join table has %d rows, want 4 (unchanged)", linkCount)
	}
}

// vpTableExists reports whether a physical table exists in the current throwaway DB.
func vpTableExists(t *testing.T, db *sqlx.DB, table string) bool {
	t.Helper()
	var n int
	if err := db.Get(&n,
		`SELECT COUNT(*) FROM information_schema.TABLES
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table); err != nil {
		t.Fatalf("query table existence: %v", err)
	}
	return n > 0
}

// TestVerticalPack_InstallEmpty rejects a pack with no entities.
func TestVerticalPack_InstallEmpty(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc := vpBuild(gdb, sdb)
	if _, err := svc.Install(context.Background(), vpTenantA, vpSeedUserID, &Pack{}); err == nil {
		t.Fatal("install empty pack: want error, got nil")
	}
}

// TestVerticalPack_CRMDashboard installs the built-in CRM pack and asserts:
//   - InstallResult.DashboardsCreated contains exactly "CRM Overview"
//   - The dashboard row exists in sys_dashboards for the tenant
//   - A second install into a SEPARATE tenant (B, same entities → all skipped, no samples)
//     skips the dashboard by name when it is already present, producing DashboardsSkipped
//   - Running the dashboard via DashboardService.RunDashboard returns grouped + scalar results
//     for the count-by-stage (w3) and total-pipeline-value (w2) widgets
//
// Idempotency note: same-tenant re-install (unique email rows skipped, RECORD_DUPLICATE →
// SamplesSkipped) is covered by TestVerticalPack_InstallBuiltin; this test keeps the separate
// tenant (B) to assert the dashboard skip in isolation from sample duplication.
func TestVerticalPack_CRMDashboard(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc := vpBuild(gdb, sdb)
	ctx := context.Background()

	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm pack not found")
	}

	// --- Install into tenant A. ---
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, crm)
	if err != nil {
		t.Fatalf("install crm into A: %v", err)
	}
	if len(res.DashboardsCreated) != 1 || res.DashboardsCreated[0] != "CRM Overview" {
		t.Errorf("DashboardsCreated = %v, want [CRM Overview]", res.DashboardsCreated)
	}
	if len(res.DashboardsSkipped) != 0 {
		t.Errorf("DashboardsSkipped = %v, want empty on first install", res.DashboardsSkipped)
	}

	// --- Verify row exists in sys_dashboards for tenant A. ---
	dashRepo := dashboard.NewSQLRepository(sdb)
	rows, err := dashRepo.List(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list dashboards for A: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 dashboard row for A, got %d", len(rows))
	}
	if rows[0].Name != "CRM Overview" {
		t.Errorf("dashboard name = %q, want %q", rows[0].Name, "CRM Overview")
	}
	dashID := rows[0].ID

	// --- Tenant isolation: tenant B sees no dashboards before its own install. ---
	rowsB0, err := dashRepo.List(ctx, vpTenantB)
	if err != nil {
		t.Fatalf("list dashboards for B (pre-install): %v", err)
	}
	if len(rowsB0) != 0 {
		t.Errorf("tenant B should see 0 dashboards before install, got %d", len(rowsB0))
	}

	// --- Install into tenant B (first time → dashboard created). ---
	res2, err := svc.Install(ctx, vpTenantB, vpSeedUserID, crm)
	if err != nil {
		t.Fatalf("install crm into B: %v", err)
	}
	if len(res2.DashboardsCreated) != 1 || res2.DashboardsCreated[0] != "CRM Overview" {
		t.Errorf("B first install DashboardsCreated = %v, want [CRM Overview]", res2.DashboardsCreated)
	}

	// --- Install into tenant B a SECOND time → dashboard skipped by name, no duplicate. ---
	// Entities are all skipped (already exist); a pack carrying only entities + dashboard (no
	// samples) keeps this assertion about the DASHBOARD skip alone (same-tenant re-install WITH
	// samples is covered by TestVerticalPack_InstallBuiltin's skip-on-dup block).
	crmNS := &Pack{
		Name:       crm.Name,
		Entities:   crm.Entities,
		Dashboards: crm.Dashboards,
	}
	res3, err := svc.Install(ctx, vpTenantB, vpSeedUserID, crmNS)
	if err != nil {
		t.Fatalf("re-install crm (no-samples) into B: %v", err)
	}
	if len(res3.DashboardsCreated) != 0 {
		t.Errorf("re-install DashboardsCreated = %v, want empty (idempotent)", res3.DashboardsCreated)
	}
	if len(res3.DashboardsSkipped) != 1 || res3.DashboardsSkipped[0] != "CRM Overview" {
		t.Errorf("re-install DashboardsSkipped = %v, want [CRM Overview]", res3.DashboardsSkipped)
	}
	// Still exactly 1 dashboard for B (no duplicate row).
	rowsB2, _ := dashRepo.List(ctx, vpTenantB)
	if len(rowsB2) != 1 {
		t.Errorf("after re-install B has %d dashboard rows, want 1 (no dup)", len(rowsB2))
	}

	// --- Run the dashboard and assert the stage funnel (w3: count by stage) returns buckets. ---
	// The seed data (installed into A) has 5 opportunities across 5 distinct stages
	// (Proposal, Negotiation, Qualification, Prospecting, Closed Won) → 5 groups.
	recordSvc := services.NewRecordService(metadata.NewRepository(gdb), dynrepo.New(sdb))
	dashSvc := services.NewDashboardService(dashRepo, recordSvc, zap.NewNop())
	scope := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}

	runResult, err := dashSvc.RunDashboard(ctx, scope, vpTenantA, dashID, nil)
	if err != nil {
		t.Fatalf("RunDashboard: %v", err)
	}
	if len(runResult.Widgets) == 0 {
		t.Fatal("RunDashboard returned no widget results")
	}

	// vpFindWidget returns the WidgetRunResult with the given ID, or nil.
	vpFindWidget := func(widgets []services.WidgetRunResult, id string) *services.WidgetRunResult {
		for i := range widgets {
			if widgets[i].WidgetID == id {
				return &widgets[i]
			}
		}
		return nil
	}

	// w1: pipeline funnel — SUM(amount) crm_opportunities GROUP BY stage.
	// This is the headline widget: it exercises the currency JSON sub-path SUM combined with an
	// indexed generated-column GROUP BY in a single AggregateGrouped call. runWidget captures
	// per-widget errors silently (200 overall), so we must explicitly check Error before
	// inspecting Result.Groups — otherwise a broken SUM+groupby goes undetected.
	w1 := vpFindWidget(runResult.Widgets, "w1")
	if w1 == nil {
		t.Fatal("widget w1 (pipeline funnel) not found in run results")
	}
	if w1.Error != "" {
		t.Fatalf("widget w1 (pipeline funnel) error: %s (code: %s)", w1.Error, w1.Code)
	}
	if w1.Result == nil || !w1.Result.Grouped {
		t.Fatalf("widget w1: expected grouped result, got %+v", w1.Result)
	}
	// 5 seed opportunities span 5 distinct stage values → 5 groups.
	if len(w1.Result.Groups) != 5 {
		t.Errorf("widget w1 groups = %d, want 5 (one per seed stage)", len(w1.Result.Groups))
	}
	// Each group's SUM Value must be non-nil: nil would mean the currency sub-path SUM+groupby
	// is broken (aggregate engine failed to extract $.amount from the JSON column).
	for _, g := range w1.Result.Groups {
		if g.Value == nil {
			t.Errorf("widget w1 group %v: SUM value is nil — currency-sum+groupby path broken", g.Key)
		}
	}

	// w3: count-by-stage grouped bar — COUNT crm_opportunities GROUP BY stage.
	// Complements w1: same groupby, count-only (no field sub-path). Proves the indexed GROUP BY
	// resolves cleanly regardless of the metric field.
	w3 := vpFindWidget(runResult.Widgets, "w3")
	if w3 == nil {
		t.Fatal("widget w3 (count-by-stage) not found in run results")
	}
	if w3.Error != "" {
		t.Fatalf("widget w3 error: %s (code: %s)", w3.Error, w3.Code)
	}
	if w3.Result == nil || !w3.Result.Grouped {
		t.Fatalf("widget w3: expected grouped result, got %+v", w3.Result)
	}
	if len(w3.Result.Groups) != 5 {
		t.Errorf("widget w3 groups = %d, want 5 (one per seed stage)", len(w3.Result.Groups))
	}

	// w2: total pipeline value — SUM(amount) crm_opportunities (scalar KPI, no groupBy).
	// Proves the currency sub-path SUM path on an ungrouped aggregate (AggregateScalar).
	// Seed total: 75000 + 250000 + 15000 + 40000 + 120000 = 500000 VND.
	w2 := vpFindWidget(runResult.Widgets, "w2")
	if w2 == nil {
		t.Fatal("widget w2 (total pipeline value) not found in run results")
	}
	if w2.Error != "" {
		t.Fatalf("widget w2 error: %s (code: %s)", w2.Error, w2.Code)
	}
	if w2.Result == nil || w2.Result.Grouped {
		t.Fatalf("widget w2: expected scalar result, got %+v", w2.Result)
	}
	if w2.Result.Scalar.Value == nil {
		t.Error("widget w2 scalar value is nil, want non-nil SUM (500000 VND)")
	}
}

// TestVerticalPack_CRMDashboardEnrichment (DASHBOARD-ENRICH v1.5.0) installs the built-in CRM pack
// and asserts the THREE widgets added to "CRM Overview" for crm_contracts / crm_quotes:
//   - The persisted widget config (decoded from the sys_dashboards JSON column) carries w5/w6/w7
//     with the right entity/metric/groupBy — a broken config (wrong entity, non-indexed groupBy)
//     would have 422'd at install validation, so reaching this assertion already proves validity.
//   - Running the dashboard returns discriminating results on the seed data:
//     w5 (kpi   COUNT crm_contracts)            → scalar Count == 3 (3 seeded contracts)
//     w6 (bar   COUNT crm_contracts BY status)  → 3 groups {Hiệu lực, Nháp, Đã gia hạn}
//     w7 (bar   COUNT crm_quotes    BY status)  → 2 groups {Đã gửi, Nháp}
//
// A wrong entity or a non-indexed groupBy would surface as a per-widget Error (the aggregate
// engine 422s on an unindexed GROUP BY) or the wrong group count — this test would then fail,
// not silently pass.
func TestVerticalPack_CRMDashboardEnrichment(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc := vpBuild(gdb, sdb)
	ctx := context.Background()

	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm pack not found")
	}
	if _, err := svc.Install(ctx, vpTenantA, vpSeedUserID, crm); err != nil {
		t.Fatalf("install crm into A: %v", err)
	}

	dashRepo := dashboard.NewSQLRepository(sdb)
	rows, err := dashRepo.List(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list dashboards for A: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 dashboard row for A, got %d", len(rows))
	}
	dashID := rows[0].ID

	// --- Decode the persisted widget array and assert the enrichment widgets' config. ---
	// Widgets is the raw JSON column; the persisted shape is services.WidgetConfig (camelCase).
	var stored []struct {
		ID      string `json:"id"`
		Entity  string `json:"entity"`
		Metric  string `json:"metric"`
		GroupBy string `json:"groupBy"`
	}
	if err := json.Unmarshal(rows[0].Widgets, &stored); err != nil {
		t.Fatalf("decode stored widgets: %v", err)
	}
	byID := make(map[string]struct{ Entity, Metric, GroupBy string }, len(stored))
	for _, w := range stored {
		byID[w.ID] = struct{ Entity, Metric, GroupBy string }{w.Entity, w.Metric, w.GroupBy}
	}
	wantCfg := []struct {
		id, entity, metric, groupBy string
	}{
		{"w5", "crm_contracts", "count", ""},
		{"w6", "crm_contracts", "count", "status"},
		{"w7", "crm_quotes", "count", "status"},
	}
	for _, wc := range wantCfg {
		got, ok := byID[wc.id]
		if !ok {
			t.Fatalf("enrichment widget %q missing from stored dashboard config", wc.id)
		}
		if got.Entity != wc.entity || got.Metric != wc.metric || got.GroupBy != wc.groupBy {
			t.Errorf("widget %q config = {entity:%q metric:%q groupBy:%q}, want {%q %q %q}",
				wc.id, got.Entity, got.Metric, got.GroupBy, wc.entity, wc.metric, wc.groupBy)
		}
	}

	// --- Run the dashboard and assert the enrichment widgets return sensible results. ---
	recordSvc := services.NewRecordService(metadata.NewRepository(gdb), dynrepo.New(sdb))
	dashSvc := services.NewDashboardService(dashRepo, recordSvc, zap.NewNop())
	scope := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}

	runResult, err := dashSvc.RunDashboard(ctx, scope, vpTenantA, dashID, nil)
	if err != nil {
		t.Fatalf("RunDashboard: %v", err)
	}
	find := func(id string) *services.WidgetRunResult {
		for i := range runResult.Widgets {
			if runResult.Widgets[i].WidgetID == id {
				return &runResult.Widgets[i]
			}
		}
		return nil
	}
	// groupKeys returns the sorted set of non-nil group keys for a grouped widget result.
	groupKeys := func(w *services.WidgetRunResult) map[string]bool {
		set := map[string]bool{}
		for _, g := range w.Result.Groups {
			if g.Key != nil {
				set[*g.Key] = true
			}
		}
		return set
	}

	// w5: COUNT crm_contracts (scalar KPI). 3 seeded contracts.
	w5 := find("w5")
	if w5 == nil {
		t.Fatal("widget w5 (tổng hợp đồng) not found in run results")
	}
	if w5.Error != "" {
		t.Fatalf("widget w5 error: %s (code: %s)", w5.Error, w5.Code)
	}
	if w5.Result == nil || w5.Result.Grouped {
		t.Fatalf("widget w5: expected scalar result, got %+v", w5.Result)
	}
	if w5.Result.Scalar.Count != 3 {
		t.Errorf("widget w5 count = %d, want 3 (3 seeded contracts)", w5.Result.Scalar.Count)
	}

	// w6: COUNT crm_contracts GROUP BY status. Seed statuses: Hiệu lực / Nháp / Đã gia hạn.
	w6 := find("w6")
	if w6 == nil {
		t.Fatal("widget w6 (hợp đồng theo trạng thái) not found in run results")
	}
	if w6.Error != "" {
		t.Fatalf("widget w6 error: %s (code: %s) — indexed groupBy on crm_contracts.status broken?", w6.Error, w6.Code)
	}
	if w6.Result == nil || !w6.Result.Grouped {
		t.Fatalf("widget w6: expected grouped result, got %+v", w6.Result)
	}
	if len(w6.Result.Groups) != 3 {
		t.Errorf("widget w6 groups = %d, want 3 (one per seeded contract status)", len(w6.Result.Groups))
	}
	w6Keys := groupKeys(w6)
	for _, want := range []string{"Hiệu lực", "Nháp", "Đã gia hạn"} {
		if !w6Keys[want] {
			t.Errorf("widget w6 missing seeded contract status group %q (got %v)", want, w6Keys)
		}
	}

	// w7: COUNT crm_quotes GROUP BY status. Seed statuses: Đã gửi / Nháp.
	w7 := find("w7")
	if w7 == nil {
		t.Fatal("widget w7 (báo giá theo trạng thái) not found in run results")
	}
	if w7.Error != "" {
		t.Fatalf("widget w7 error: %s (code: %s) — indexed groupBy on crm_quotes.status broken?", w7.Error, w7.Code)
	}
	if w7.Result == nil || !w7.Result.Grouped {
		t.Fatalf("widget w7: expected grouped result, got %+v", w7.Result)
	}
	if len(w7.Result.Groups) != 2 {
		t.Errorf("widget w7 groups = %d, want 2 (one per seeded quote status)", len(w7.Result.Groups))
	}
	w7Keys := groupKeys(w7)
	for _, want := range []string{"Đã gửi", "Nháp"} {
		if !w7Keys[want] {
			t.Errorf("widget w7 missing seeded quote status group %q (got %v)", want, w7Keys)
		}
	}
}

// TestVerticalPack_FormulaWeightedValue installs the built-in CRM pack and asserts:
//  1. weighted_value is computed on every seeded crm_opportunities record.
//  2. A partial PUT (only probability changed) recomputes weighted_value from the stored
//     amount — proving the merge-before-image path survives the MySQL JSON round-trip.
//
// The tests are kept self-contained within tenant A; no cross-tenant concerns are
// exercised here (those belong to TestVerticalPack_ExportInstall_RoundTrip).
func TestVerticalPack_FormulaWeightedValue(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc := vpBuild(gdb, sdb)
	ctx := context.Background()

	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm pack not found")
	}
	if _, err := svc.Install(ctx, vpTenantA, vpSeedUserID, crm); err != nil {
		t.Fatalf("install crm: %v", err)
	}

	// Wire a RecordService directly (same deps as vpBuild) to call List and Update.
	metaRepo := metadata.NewRepository(gdb)
	recordSvc := services.NewRecordService(metaRepo, dynrepo.New(sdb))
	scope := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}

	// --- 1. Verify weighted_value on all 5 seeded opportunities. ---
	// The seeded records are: acme_q1 (75000 * 60 / 100 = 45000), globex_ent (250000 * 80 / 100 = 200000),
	// initech_pilot (15000 * 0 / 100 = 0), acme_upsell (40000 * 20 / 100 = 8000),
	// acme_renewal (120000 * 100 / 100 = 120000).
	listRes, err := recordSvc.List(ctx, scope, vpTenantA, "crm_opportunities", services.ListQuery{
		Page:  1,
		Limit: 20,
		Sort:  "created_at",
		Order: "asc",
	})
	if err != nil {
		t.Fatalf("list crm_opportunities: %v", err)
	}
	if len(listRes.Records) != 5 {
		t.Fatalf("expected 5 opportunities, got %d", len(listRes.Records))
	}

	// Build a map from amount → expected weighted_value for each seed record.
	// amount is stored as a currency object; extract the .amount sub-value for keying.
	type wantEntry struct {
		amount      float64
		probability float64
		wantWV      float64
	}
	wantTable := []wantEntry{
		{amount: 75000, probability: 60, wantWV: 45000},
		{amount: 250000, probability: 80, wantWV: 200000},
		{amount: 15000, probability: 0, wantWV: 0},
		{amount: 40000, probability: 20, wantWV: 8000},
		{amount: 120000, probability: 100, wantWV: 120000},
	}

	// vpExtractCurrencyAmount extracts the float64 amount from a JSON-round-tripped currency value.
	// After json.Unmarshal into map[string]any, the inner value is float64 (not json.Number).
	vpExtractCurrencyAmount := func(v any) float64 {
		if v == nil {
			return 0
		}
		m, ok := v.(map[string]any)
		if !ok {
			return 0
		}
		f, _ := m["amount"].(float64)
		return f
	}

	for _, rec := range listRes.Records {
		amount := vpExtractCurrencyAmount(rec.Data["amount"])
		probability, _ := rec.Data["probability"].(float64)
		wv := vpExtractCurrencyAmount(rec.Data["weighted_value"])

		// Find the matching wantEntry.
		var want *wantEntry
		for i := range wantTable {
			if wantTable[i].amount == amount && wantTable[i].probability == probability {
				want = &wantTable[i]
				break
			}
		}
		if want == nil {
			t.Errorf("unexpected opportunity: amount=%.0f probability=%.0f", amount, probability)
			continue
		}
		if wv != want.wantWV {
			t.Errorf("weighted_value for amount=%.0f prob=%.0f: got %.0f, want %.0f",
				amount, probability, wv, want.wantWV)
		}
	}

	// --- 2. Partial-PUT merge test: update only probability, assert weighted_value recomputes. ---
	// Pick the acme_q1 record (amount=75000, probability=60 → weighted_value=45000).
	// After setting probability=80, the new weighted_value should be 75000*80/100=60000.
	// The amount field is NOT included in the PUT payload — only the before-image fetch can supply it.
	var acmeID string
	for _, rec := range listRes.Records {
		if vpExtractCurrencyAmount(rec.Data["amount"]) == 75000 {
			acmeID = rec.ID
			break
		}
	}
	if acmeID == "" {
		t.Fatal("could not find acme_q1 record (amount=75000)")
	}

	// PUT with only probability — amount is intentionally absent from the payload.
	err = recordSvc.Update(ctx, scope, vpTenantA, "crm_opportunities", acmeID,
		map[string]any{"probability": float64(80)},
		nil, // no optimistic lock
	)
	if err != nil {
		t.Fatalf("partial PUT (probability only): %v", err)
	}

	// Re-fetch and assert.
	updated, err := recordSvc.Get(ctx, scope, vpTenantA, "crm_opportunities", acmeID, nil, nil)
	if err != nil {
		t.Fatalf("Get after partial PUT: %v", err)
	}
	wvAfter := vpExtractCurrencyAmount(updated.Data["weighted_value"])
	const wantAfterWV = 60000.0
	if wvAfter != wantAfterWV {
		t.Errorf("partial-PUT recompute: weighted_value = %.0f, want %.0f (merge-before-image broken)", wvAfter, wantAfterWV)
	}
	// Also confirm probability was stored correctly.
	probAfter, _ := updated.Data["probability"].(float64)
	if probAfter != 80 {
		t.Errorf("partial-PUT: probability stored as %.0f, want 80", probAfter)
	}
	// Amount must be unchanged.
	amtAfter := vpExtractCurrencyAmount(updated.Data["amount"])
	if amtAfter != 75000 {
		t.Errorf("partial-PUT: amount changed to %.0f, want 75000 (should not be touched)", amtAfter)
	}
}

// TestVerticalPack_CRMDeepened is the §5.4 gate for the deepened CRM pack
// (../specs/030-crm-pack-deepening/spec.md): auto_number (opp_code/lead_code), lookup (account_name/
// account_industry), rollup — FK-on-child sum/count/avg AND N-N count, all deferred via
// Pass 2b — 1-1 exclusivity (primary_for), the three readonly rejections, idempotent
// re-install (deferred fields skip on FIELD_ALREADY_EXISTS), and the relationship registry
// listing R1–R8 (R6/R7 added by QUOTE-LINE-ITEMS: crm_quotes.account, crm_quote_lines.quote;
// R8 added by CONTRACT-RENEWAL: crm_contracts.account).
// R1 (crm_contacts.account) and R4 (crm_contacts.primary_for) are TWO
// registry rows between the SAME (child crm_contacts → parent crm_accounts) pair — allowed
// because sys_relationships uniqueness is (tenant_id, name) + (tenant_id, join_table_name)
// (migration 000025), never the entity pair; this test pins that guarantee.
func TestVerticalPack_CRMDeepened(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc := vpBuild(gdb, sdb)
	ctx := context.Background()

	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm pack not found")
	}

	// --- #1: fresh-tenant install succeeds; 5 deferred rollup fields added in Pass 2b. ---
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, crm)
	if err != nil {
		t.Fatalf("install deepened crm: %v", err)
	}
	wantDeferred := map[string]bool{
		"crm_accounts.total_opp_value":        true,
		"crm_accounts.opportunity_count":      true,
		"crm_accounts.avg_opp_value":          true,
		"crm_opportunities.stakeholder_count": true,
		"crm_quotes.total":                    true,
	}
	if len(res.DeferredFieldsAdded) != 5 {
		t.Fatalf("DeferredFieldsAdded = %v, want exactly 5 entries", res.DeferredFieldsAdded)
	}
	for _, k := range res.DeferredFieldsAdded {
		if !wantDeferred[k] {
			t.Errorf("unexpected deferred field %q added", k)
		}
	}

	// Read-side services with the SAME production wiring as vpBuild's record writer.
	metaRepo := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sdb)
	recordSvc := services.NewRecordService(metaRepo, dyn).
		WithLabelFetcher(dyn).
		WithFieldValueFetcher(dyn).
		WithRollupMeta(metaRepo).
		WithRollupLinked(dyn).
		WithSequenceClaimer(dyn)
	scope := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}

	// vpAsInt normalizes a rollup COUNT value: RollupResult.ToJSONValue emits int64 at the
	// service layer; a JSON round-trip (HTTP path) would deliver float64. Accept both so the
	// assertion pins the NUMBER, not the carrier type.
	vpAsInt := func(v any) (int64, bool) {
		switch n := v.(type) {
		case int64:
			return n, true
		case float64:
			return int64(n), true
		}
		return 0, false
	}
	// vpCurrencyRollup unwraps a currency rollup value {amount:"<DECIMAL str>",currencyCode}
	// and parses the amount NUMERICALLY (guardrail #2: never hardcode the DECIMAL text —
	// MySQL's exact string, e.g. scale, is an implementation detail; the number is the contract).
	vpCurrencyRollup := func(t *testing.T, field string, v any) (float64, string, string) {
		t.Helper()
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%s: expected currency object, got %T (%v)", field, v, v)
		}
		amountStr, ok := m["amount"].(string)
		if !ok {
			t.Fatalf("%s: expected amount as exact DECIMAL string, got %T (%v)", field, m["amount"], m["amount"])
		}
		f, err := strconv.ParseFloat(amountStr, 64)
		if err != nil {
			t.Fatalf("%s: amount %q does not parse as a number: %v", field, amountStr, err)
		}
		code, _ := m["currencyCode"].(string)
		return f, amountStr, code
	}

	// Locate account ids by name (install does not expose the refMap).
	accList, err := recordSvc.List(ctx, scope, vpTenantA, "crm_accounts", services.ListQuery{
		Page: 1, Limit: 20, Sort: "created_at", Order: "asc",
	})
	if err != nil {
		t.Fatalf("list crm_accounts: %v", err)
	}
	if len(accList.Records) != 4 {
		t.Fatalf("expected 4 accounts, got %d", len(accList.Records))
	}
	accByName := map[string]string{}
	for _, r := range accList.Records {
		name, _ := r.Data["name"].(string)
		accByName[name] = r.ID
	}
	for _, want := range []string{"Acme Corp", "Globex Inc", "Initech", "Umbrella Health"} {
		if accByName[want] == "" {
			t.Fatalf("seed account %q not found; got %v", want, keys(accByName))
		}
	}

	rollupAll := []string{"total_opp_value", "opportunity_count", "avg_opp_value"}

	// --- #2a: acme rollups with explicit opt-in — count 3, sum 235000, avg 235000/3. ---
	acme, err := recordSvc.Get(ctx, scope, vpTenantA, "crm_accounts", accByName["Acme Corp"], nil, rollupAll)
	if err != nil {
		t.Fatalf("get acme with rollups: %v", err)
	}
	if n, ok := vpAsInt(acme.Data["opportunity_count"]); !ok || n != 3 {
		t.Errorf("acme opportunity_count = %v (%T), want 3", acme.Data["opportunity_count"], acme.Data["opportunity_count"])
	}
	sumVal, sumStr, sumCode := vpCurrencyRollup(t, "acme total_opp_value", acme.Data["total_opp_value"])
	t.Logf("acme total_opp_value exact DECIMAL text: %q", sumStr)
	if sumVal != 235000 {
		t.Errorf("acme total_opp_value = %v (%q), want 235000 (75000+40000+120000)", sumVal, sumStr)
	}
	if sumCode != "VND" {
		t.Errorf("acme total_opp_value currencyCode = %q, want VND", sumCode)
	}
	avgVal, avgStr, avgCode := vpCurrencyRollup(t, "acme avg_opp_value", acme.Data["avg_opp_value"])
	t.Logf("acme avg_opp_value exact DECIMAL text: %q", avgStr)
	if wantAvg := 235000.0 / 3.0; math.Abs(avgVal-wantAvg) > 0.01 {
		t.Errorf("acme avg_opp_value = %v (%q), want ≈%v", avgVal, avgStr, wantAvg)
	}
	if avgCode != "VND" {
		t.Errorf("acme avg_opp_value currencyCode = %q, want VND", avgCode)
	}

	// --- #2b: globex / initech single-opp rollups. ---
	for _, tc := range []struct {
		account string
		count   int64
		sum     float64
	}{
		{"Globex Inc", 1, 250000},
		{"Initech", 1, 15000},
	} {
		rec, err := recordSvc.Get(ctx, scope, vpTenantA, "crm_accounts", accByName[tc.account], nil, rollupAll)
		if err != nil {
			t.Fatalf("get %s with rollups: %v", tc.account, err)
		}
		if n, ok := vpAsInt(rec.Data["opportunity_count"]); !ok || n != tc.count {
			t.Errorf("%s opportunity_count = %v, want %d", tc.account, rec.Data["opportunity_count"], tc.count)
		}
		got, gotStr, _ := vpCurrencyRollup(t, tc.account+" total_opp_value", rec.Data["total_opp_value"])
		if got != tc.sum {
			t.Errorf("%s total_opp_value = %v (%q), want %v", tc.account, got, gotStr, tc.sum)
		}
	}

	// --- #2c: umbrella rollup empty-state — count 0, sum null, avg null. ---
	umb, err := recordSvc.Get(ctx, scope, vpTenantA, "crm_accounts", accByName["Umbrella Health"], nil, rollupAll)
	if err != nil {
		t.Fatalf("get umbrella with rollups: %v", err)
	}
	if n, ok := vpAsInt(umb.Data["opportunity_count"]); !ok || n != 0 {
		t.Errorf("umbrella opportunity_count = %v (%T), want 0", umb.Data["opportunity_count"], umb.Data["opportunity_count"])
	}
	if v := umb.Data["total_opp_value"]; v != nil {
		t.Errorf("umbrella total_opp_value = %v, want nil (no children → undefined sum)", v)
	}
	if v := umb.Data["avg_opp_value"]; v != nil {
		t.Errorf("umbrella avg_opp_value = %v, want nil", v)
	}

	// --- #2d: WITHOUT ?rollup= the aggregate fields must not resolve (opt-in contract, G12).
	plain, err := recordSvc.Get(ctx, scope, vpTenantA, "crm_accounts", accByName["Acme Corp"], nil, nil)
	if err != nil {
		t.Fatalf("get acme without rollups: %v", err)
	}
	for _, f := range rollupAll {
		if v, present := plain.Data[f]; present && v != nil {
			t.Errorf("without ?rollup=, %s = %v — must be absent or null (rollup is opt-in)", f, v)
		}
	}

	// --- #3: lookups ALWAYS resolve — every contact carries account_name. ---
	contacts, err := recordSvc.List(ctx, scope, vpTenantA, "crm_contacts", services.ListQuery{
		Page: 1, Limit: 20, Sort: "created_at", Order: "asc",
	})
	if err != nil {
		t.Fatalf("list crm_contacts: %v", err)
	}
	if len(contacts.Records) != 4 {
		t.Fatalf("expected 4 contacts, got %d", len(contacts.Records))
	}
	wantAccountName := map[string]string{
		"Alice": "Acme Corp",
		"Bob":   "Globex Inc",
		"Carol": "Initech",
		"Dave":  "Acme Corp",
	}
	for _, rec := range contacts.Records {
		first, _ := rec.Data["first_name"].(string)
		got, present := rec.Data["account_name"]
		if !present {
			t.Errorf("contact %s: lookup account_name key absent — lookups must always resolve (value or null)", first)
			continue
		}
		if want := wantAccountName[first]; got != any(want) {
			t.Errorf("contact %s: account_name = %v, want %q", first, got, want)
		}
	}

	// --- #4: opportunities — N-N stakeholder_count rollup, opp_code, account_industry. ---
	opps, err := recordSvc.List(ctx, scope, vpTenantA, "crm_opportunities", services.ListQuery{
		Page: 1, Limit: 20, Sort: "created_at", Order: "asc",
		Rollup: []string{"stakeholder_count"},
	})
	if err != nil {
		t.Fatalf("list crm_opportunities with stakeholder_count: %v", err)
	}
	if len(opps.Records) != 5 {
		t.Fatalf("expected 5 opportunities, got %d", len(opps.Records))
	}
	wantOpp := map[string]struct {
		stakeholders int64
		code         string
		industry     string
	}{
		"Acme Q1 Expansion":       {2, "OPP-00001", "Technology"},
		"Globex Enterprise Suite": {1, "OPP-00002", "Finance"},
		"Initech Pilot":           {1, "OPP-00003", "Retail"},
		"Acme Platform Upsell":    {0, "OPP-00004", "Technology"},
		"Acme Annual Renewal":     {0, "OPP-00005", "Technology"},
	}
	for _, rec := range opps.Records {
		name, _ := rec.Data["name"].(string)
		want, ok := wantOpp[name]
		if !ok {
			t.Errorf("unexpected opportunity %q", name)
			continue
		}
		if n, okInt := vpAsInt(rec.Data["stakeholder_count"]); !okInt || n != want.stakeholders {
			t.Errorf("opp %q stakeholder_count = %v (%T), want %d", name, rec.Data["stakeholder_count"], rec.Data["stakeholder_count"], want.stakeholders)
		}
		// G6: exact OPP-0000N codes hold on a pristine tenant only — this throwaway DB is one.
		if code, _ := rec.Data["opp_code"].(string); code != want.code {
			t.Errorf("opp %q opp_code = %v, want %q", name, rec.Data["opp_code"], want.code)
		}
		if ind, present := rec.Data["account_industry"]; !present || ind != any(want.industry) {
			t.Errorf("opp %q account_industry = %v, want %q", name, ind, want.industry)
		}
	}

	// leads: second, independent auto_number sequence — LEAD-0001..4 in insert order.
	leads, err := recordSvc.List(ctx, scope, vpTenantA, "crm_leads", services.ListQuery{
		Page: 1, Limit: 20, Sort: "created_at", Order: "asc",
	})
	if err != nil {
		t.Fatalf("list crm_leads: %v", err)
	}
	if len(leads.Records) != 4 {
		t.Fatalf("expected 4 leads, got %d", len(leads.Records))
	}
	wantLeadCode := map[string]string{
		"Eve Martinez": "LEAD-0001",
		"Frank Nguyen": "LEAD-0002",
		"Grace Chen":   "LEAD-0003",
		"Hank Wilson":  "LEAD-0004",
	}
	for _, rec := range leads.Records {
		full, _ := rec.Data["full_name"].(string)
		if code, _ := rec.Data["lead_code"].(string); code != wantLeadCode[full] {
			t.Errorf("lead %q lead_code = %v, want %q", full, rec.Data["lead_code"], wantLeadCode[full])
		}
	}

	// --- #5: 1-1 exclusivity — a second live contact claiming @acme is a 409. ---
	_, err = recordSvc.Create(ctx, scope, vpTenantA, vpSeedUserID, "crm_contacts", map[string]any{
		"first_name":  "Zed",
		"last_name":   "Intruder",
		"primary_for": accByName["Acme Corp"], // Alice already holds this slot
	})
	var ve *services.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("2nd primary_for claim: want ValidationError, got %v", err)
	}
	if ve.Code != "RECORD_RELATION_ALREADY_BOUND" {
		t.Errorf("2nd primary_for claim: code = %q, want RECORD_RELATION_ALREADY_BOUND", ve.Code)
	}
	if ve.Field != "primary_for" {
		t.Errorf("2nd primary_for claim: field = %q, want primary_for", ve.Field)
	}
	if f, _ := ve.Details["field"].(string); f != "primary_for" {
		t.Errorf("2nd primary_for claim: details.field = %q, want primary_for", f)
	}

	// --- #6: the three server-computed readonly rejections. ---
	for _, tc := range []struct {
		name     string
		entity   string
		payload  map[string]any
		wantCode string
	}{
		{
			name:     "auto_number opp_code in POST body",
			entity:   "crm_opportunities",
			payload:  map[string]any{"name": "Hack", "stage": "Prospecting", "opp_code": "OPP-99999"},
			wantCode: "RECORD_AUTONUMBER_READONLY",
		},
		{
			name:     "lookup account_name in POST body",
			entity:   "crm_contacts",
			payload:  map[string]any{"first_name": "Eva", "last_name": "Lie", "account_name": "Spoofed"},
			wantCode: "RECORD_LOOKUP_READONLY",
		},
		{
			name:     "rollup total_opp_value in POST body",
			entity:   "crm_accounts",
			payload:  map[string]any{"name": "Fake Co", "total_opp_value": float64(1)},
			wantCode: "RECORD_ROLLUP_READONLY",
		},
	} {
		_, err := recordSvc.Create(ctx, scope, vpTenantA, vpSeedUserID, tc.entity, tc.payload)
		var rve *services.ValidationError
		if !errors.As(err, &rve) {
			t.Errorf("%s: want ValidationError, got %v", tc.name, err)
			continue
		}
		if rve.Code != tc.wantCode {
			t.Errorf("%s: code = %q, want %q", tc.name, rve.Code, tc.wantCode)
		}
	}

	// --- #7: idempotent re-install — metadata only. Samples are explicitly NON-idempotent
	// (Install contract: seeding is a one-time bootstrap; contacts carry a unique email that
	// would collide), so mirror TestVerticalPack_CRMDashboard's precedent: re-install the pack
	// minus Samples/Links. Everything else must skip — including the 5 deferred fields, which
	// hit FIELD_ALREADY_EXISTS in Pass 2b — with no duplicate registry rows.
	crmMeta := &Pack{
		Name:            crm.Name,
		Entities:        crm.Entities,
		NNRelationships: crm.NNRelationships,
		DeferredFields:  crm.DeferredFields,
		Layouts:         crm.Layouts,
		Roles:           crm.Roles,
		Dashboards:      crm.Dashboards,
	}
	res2, err := svc.Install(ctx, vpTenantA, vpSeedUserID, crmMeta)
	if err != nil {
		t.Fatalf("re-install deepened crm (metadata only): %v", err)
	}
	if want := crmEntityCount(t); len(res2.EntitiesCreated) != 0 || len(res2.EntitiesSkipped) != want {
		t.Errorf("re-install: created %v / skipped %v, want 0 created / %d skipped", res2.EntitiesCreated, res2.EntitiesSkipped, want)
	}
	if len(res2.DeferredFieldsAdded) != 0 {
		t.Errorf("re-install: DeferredFieldsAdded = %v, want empty (idempotent skip)", res2.DeferredFieldsAdded)
	}
	skipped := make(map[string]bool, len(res2.FieldsSkipped))
	for _, k := range res2.FieldsSkipped {
		skipped[k] = true
	}
	for k := range wantDeferred {
		if !skipped[k] {
			t.Errorf("re-install: deferred field %q missing from FieldsSkipped (%v)", k, res2.FieldsSkipped)
		}
	}
	if len(res2.NNRelationsSkipped) != 1 || res2.NNRelationsSkipped[0] != "crm_contacts_opportunities" {
		t.Errorf("re-install: NNRelationsSkipped = %v, want [crm_contacts_opportunities]", res2.NNRelationsSkipped)
	}

	// --- #8: relationship registry lists R1–R8 with correct shape, no duplicates. ---
	rels, err := metaRepo.ListAllRelationships(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("ListAllRelationships: %v", err)
	}
	if len(rels) != 11 {
		t.Fatalf("registry has %d relationships, want 11 (R1–R8 + opportunity→quote→contract chain + quote_line→product; no dups after re-install); got %+v", len(rels), rels)
	}
	relByName := map[string]metadata.RelationshipDefinition{}
	for _, r := range rels {
		relByName[r.Name] = r
	}
	for _, tc := range []struct {
		name, relType, parent, child, inverse, fkCol string
	}{
		{"crm_contacts_account", "1-N", "crm_accounts", "crm_contacts", "contacts", "account_id"},
		{"crm_opportunities_account", "1-N", "crm_accounts", "crm_opportunities", "opportunities", "account_id"},
		{"crm_activities_account", "1-N", "crm_accounts", "crm_activities", "activities", "account_id"},
		{"crm_contacts_primary_for", "1-1", "crm_accounts", "crm_contacts", "primary_contact", "primary_for_id"},
		{"crm_contacts_opportunities", "N-N", "", "", "", ""},
		{"crm_quotes_account", "1-N", "crm_accounts", "crm_quotes", "quotes", "account_id"},
		{"crm_quote_lines_quote", "1-N", "crm_quotes", "crm_quote_lines", "lines", "quote_id"},
		{"crm_contracts_account", "1-N", "crm_accounts", "crm_contracts", "contracts", "account_id"},
	} {
		r, ok := relByName[tc.name]
		if !ok {
			t.Errorf("registry missing relationship %q; got %v", tc.name, keys(relByName))
			continue
		}
		if r.RelationshipType != tc.relType {
			t.Errorf("%s: relationshipType = %q, want %q", tc.name, r.RelationshipType, tc.relType)
		}
		if tc.relType == "N-N" {
			continue // N-N carries no inverse/fk; sides are asserted below via the pair count
		}
		if r.ParentEntityName != tc.parent || r.ChildEntityName != tc.child {
			t.Errorf("%s: parent/child = %q/%q, want %q/%q", tc.name, r.ParentEntityName, r.ChildEntityName, tc.parent, tc.child)
		}
		if r.InverseField != tc.inverse {
			t.Errorf("%s: inverseField = %q, want %q", tc.name, r.InverseField, tc.inverse)
		}
		if r.ChildFKColumn != tc.fkCol {
			t.Errorf("%s: childFkColumn = %q, want %q", tc.name, r.ChildFKColumn, tc.fkCol)
		}
	}
	// Guardrail #3 pinned: TWO relations between the SAME (crm_contacts → crm_accounts) pair.
	samePair := 0
	for _, r := range rels {
		if r.ChildEntityName == "crm_contacts" && r.ParentEntityName == "crm_accounts" {
			samePair++
		}
	}
	if samePair != 2 {
		t.Errorf("same-pair (crm_contacts→crm_accounts) registry rows = %d, want 2 (R1 account + R4 primary_for)", samePair)
	}
}

func vpColumnExists(t *testing.T, db *sqlx.DB, table, column string) bool {
	t.Helper()
	var n int
	if err := db.Get(&n,
		`SELECT COUNT(*) FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		table, column); err != nil {
		t.Fatalf("query column existence: %v", err)
	}
	return n > 0
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// w8AsInt normalizes a rollup COUNT value (int64 at the service layer, float64 over a JSON hop).
func w8AsInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return -1
}

// w8CurrencyAmount unwraps a currency rollup/value {amount:"<DECIMAL str>",currencyCode} and
// parses the amount NUMERICALLY (trap B8: never hardcode the exact DECIMAL text — the number is
// the contract, its scale is an implementation detail).
func w8CurrencyAmount(t *testing.T, field string, v any) float64 {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: expected currency object, got %T (%v)", field, v, v)
	}
	amountStr, ok := m["amount"].(string)
	if !ok {
		t.Fatalf("%s: expected amount as DECIMAL string, got %T (%v)", field, m["amount"], m["amount"])
	}
	f, err := strconv.ParseFloat(amountStr, 64)
	if err != nil {
		t.Fatalf("%s: amount %q does not parse as a number: %v", field, amountStr, err)
	}
	return f
}

// TestVerticalPack_W8_ExportRoundTrip_WithData is the definition-of-done for W8-BE-1 (the
// EXPORT-DEFERRED fix): install the deepened CRM into tenant A, EXPORT it (both metadata-only and
// with data), install the exported pack into a FRESH tenant B, and assert the exported pack is
// RE-INSTALLABLE with full metadata parity + computed values re-derived on B.
//
// Single-DB two-tenant is safe here because every obj_* unique index is (tenant_id, <col>)
// tenant-scoped — B can hold the same alice@acme.example.com as A without a cross-tenant collision.
func TestVerticalPack_W8_ExportRoundTrip_WithData(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc := vpBuild(gdb, sdb)
	ctx := context.Background()

	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm pack not found")
	}
	if _, err := svc.Install(ctx, vpTenantA, vpSeedUserID, crm); err != nil {
		t.Fatalf("install deepened crm into A: %v", err)
	}

	// --- Metadata-only export shape (?includeData=false). ---
	meta, _, err := svc.Export(ctx, vpTenantA, nil, false, nil)
	if err != nil {
		t.Fatalf("export metadata-only from A: %v", err)
	}
	if len(meta.Samples) != 0 {
		t.Errorf("metadata-only export must carry 0 samples, got %d", len(meta.Samples))
	}
	if meta.Manifest == nil || !isValidSemver(meta.Manifest.Version) {
		t.Errorf("export manifest missing/invalid: %+v", meta.Manifest)
	}
	// The four rollups plus the polymorphic_relation field are DEFERRED, none inline
	// (POLYMORPHIC-1: deferred so its targetEntities exist before the alphabetical install reaches it).
	wantDeferred := map[string]string{
		"crm_accounts.total_opp_value":        "rollup",
		"crm_accounts.opportunity_count":      "rollup",
		"crm_accounts.avg_opp_value":          "rollup",
		"crm_opportunities.stakeholder_count": "rollup",
		"crm_activities.related_to":           "polymorphic_relation",
		"crm_quotes.total":                    "rollup",
	}
	if len(meta.DeferredFields) != len(wantDeferred) {
		t.Fatalf("export DeferredFields = %d, want %d; got %+v", len(meta.DeferredFields), len(wantDeferred), meta.DeferredFields)
	}
	for _, df := range meta.DeferredFields {
		key := df.EntityName + "." + df.Field.Name
		wantType, ok := wantDeferred[key]
		if !ok {
			t.Errorf("unexpected deferred field %q", key)
		} else if df.Field.FieldType != wantType {
			t.Errorf("deferred %q type = %q, want %q", key, df.Field.FieldType, wantType)
		}
	}
	for _, e := range meta.Entities {
		for _, f := range e.Fields {
			if f.FieldType == "rollup" || f.FieldType == "polymorphic_relation" {
				t.Errorf("deferred-only field %q.%q (%s) leaked into inline Fields", e.Name, f.Name, f.FieldType)
			}
		}
	}
	// N-N exported from the registry.
	if len(meta.NNRelationships) != 1 || meta.NNRelationships[0].Name != "crm_contacts_opportunities" {
		t.Errorf("N-N export = %+v, want exactly [crm_contacts_opportunities]", meta.NNRelationships)
	}
	// Cardinality projection: primary_for → "1-1"; account → "1-N".
	relType := map[string]string{}
	for _, r := range meta.Relationships {
		relType[r.ChildEntity+"."+r.Name] = r.Type
	}
	if relType["crm_contacts.primary_for"] != "1-1" {
		t.Errorf("primary_for projection = %q, want 1-1", relType["crm_contacts.primary_for"])
	}
	if relType["crm_contacts.account"] != "1-N" {
		t.Errorf("account projection = %q, want 1-N", relType["crm_contacts.account"])
	}

	// --- Full export WITH data. ---
	pack, _, err := svc.Export(ctx, vpTenantA, nil, true, nil)
	if err != nil {
		t.Fatalf("export with data from A: %v", err)
	}
	computedKeys := []string{
		"opp_code", "lead_code", "account_name", "account_industry",
		"total_opp_value", "opportunity_count", "avg_opp_value", "stakeholder_count", "weighted_value",
		"quote_number", "line_total", "total", "contract_number",
	}
	sampleCount := 0
	for _, ps := range pack.Samples {
		for _, rec := range ps.Records {
			sampleCount++
			for _, ck := range computedKeys {
				if _, present := rec.Data[ck]; present {
					t.Errorf("sample %q record carries computed key %q — must be stripped (trap B9/G1)", ps.Entity, ck)
				}
			}
		}
	}
	if sampleCount != 31 {
		t.Errorf("exported sample records = %d, want 31 (4 acct + 4 contact + 5 opp + 4 lead + 4 activity + 2 quote + 5 quote_line + 3 contract)", sampleCount)
	}

	// --- Install the exported (with-data) pack into fresh tenant B. ---
	res, err := svc.Install(ctx, vpTenantB, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("install exported pack into B: %v", err)
	}
	if want := crmEntityCount(t); len(res.EntitiesCreated) != want {
		t.Errorf("B entities created = %v (%d), want %d", res.EntitiesCreated, len(res.EntitiesCreated), want)
	}
	// 5 rollups + the polymorphic related_to (reconciled onto the shared obj_* table — metadata
	// registered, DDL skipped because A already materialized the sub-columns).
	if len(res.DeferredFieldsAdded) != 6 {
		t.Errorf("B deferred fields added = %v, want 6 (5 rollup + related_to)", res.DeferredFieldsAdded)
	}
	if len(res.NNRelationsCreated) != 1 {
		t.Errorf("B N-N created = %v, want 1", res.NNRelationsCreated)
	}
	if res.SamplesInserted != 31 {
		t.Errorf("B samples inserted = %d, want 31", res.SamplesInserted)
	}

	// --- Parity + rollups compute on B (numeric assert, trap B8). ---
	metaRepo := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sdb)
	recordSvc := services.NewRecordService(metaRepo, dyn).
		WithLabelFetcher(dyn).
		WithFieldValueFetcher(dyn).
		WithRollupMeta(metaRepo).
		WithRollupLinked(dyn).
		WithSequenceClaimer(dyn)
	scope := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}

	// Entity parity: B has all 8.
	bEntities, err := metaRepo.ListEntities(vpTenantB)
	if err != nil {
		t.Fatalf("list B entities: %v", err)
	}
	if want := crmEntityCount(t); len(bEntities) != want {
		t.Fatalf("B entities = %d, want %d", len(bEntities), want)
	}
	// Relationship parity: R1–R8 (1-N×6, 1-1×1, N-N×1).
	bRels, err := metaRepo.ListAllRelationships(ctx, vpTenantB)
	if err != nil {
		t.Fatalf("list B relationships: %v", err)
	}
	// 11 = the original R1–R8 plus the three 1.15.0 links.
	if len(bRels) != 11 {
		t.Errorf("B relationships = %d, want 11 (R1–R8 + the 3 convert-chain links)", len(bRels))
	}
	// Layout parity: B received crm_accounts layouts (list + detail), and the detail layout still
	// references a DEFERRED rollup field (total_opp_value in the Pipeline section) — proving the
	// export→install path round-trips layouts AND their references to Pass-2b fields together.
	bLayouts, err := metadata.NewLayoutRepository(gdb).GetByEntity(ctx, vpTenantB, "crm_accounts")
	if err != nil {
		t.Fatalf("list B crm_accounts layouts: %v", err)
	}
	if len(bLayouts) < 2 {
		t.Errorf("B crm_accounts layouts = %d, want >= 2 (list + detail)", len(bLayouts))
	}
	detailFound := false
	for _, l := range bLayouts {
		if l.ViewType == "detail" {
			detailFound = true
			if !strings.Contains(l.LayoutJSON, "total_opp_value") {
				t.Errorf("B crm_accounts detail layout does not reference deferred rollup total_opp_value: %s", l.LayoutJSON)
			}
		}
	}
	if !detailFound {
		t.Errorf("B crm_accounts detail layout missing")
	}

	// B accounts + acme rollups (the headline numeric assert — order-independent by FK).
	accList, err := recordSvc.List(ctx, scope, vpTenantB, "crm_accounts", services.ListQuery{
		Page: 1, Limit: 20, Sort: "created_at", Order: "asc",
	})
	if err != nil {
		t.Fatalf("B list accounts: %v", err)
	}
	if len(accList.Records) != 4 {
		t.Fatalf("B accounts = %d, want 4", len(accList.Records))
	}
	accByName := map[string]string{}
	for _, r := range accList.Records {
		name, _ := r.Data["name"].(string)
		accByName[name] = r.ID
	}
	acme, err := recordSvc.Get(ctx, scope, vpTenantB, "crm_accounts", accByName["Acme Corp"], nil,
		[]string{"total_opp_value", "opportunity_count", "avg_opp_value"})
	if err != nil {
		t.Fatalf("B get acme rollups: %v", err)
	}
	if n := w8AsInt(acme.Data["opportunity_count"]); n != 3 {
		t.Errorf("B acme opportunity_count = %v, want 3 (rollup must compute on B)", acme.Data["opportunity_count"])
	}
	if sum := w8CurrencyAmount(t, "B acme total_opp_value", acme.Data["total_opp_value"]); sum != 235000 {
		t.Errorf("B acme total_opp_value = %v, want 235000 (75k+40k+120k)", sum)
	}

	// Auto-number re-assigned FRESH on B — assert as a SET (created_at ties → order is
	// non-deterministic, so DO NOT pin OPP-00001 to a specific opportunity, trap Blocker-2).
	opps, err := recordSvc.List(ctx, scope, vpTenantB, "crm_opportunities", services.ListQuery{
		Page: 1, Limit: 20, Sort: "created_at", Order: "asc",
	})
	if err != nil {
		t.Fatalf("B list opportunities: %v", err)
	}
	if len(opps.Records) != 5 {
		t.Fatalf("B opportunities = %d, want 5", len(opps.Records))
	}
	gotCodes := map[string]bool{}
	for _, r := range opps.Records {
		if code, _ := r.Data["opp_code"].(string); code != "" {
			gotCodes[code] = true
		}
	}
	for _, want := range []string{"OPP-00001", "OPP-00002", "OPP-00003", "OPP-00004", "OPP-00005"} {
		if !gotCodes[want] {
			t.Errorf("B opp_codes missing %q; got set %v", want, keys(gotCodes))
		}
	}

	// Lookup resolves on B — every contact carries account_name (server-recomputed).
	contacts, err := recordSvc.List(ctx, scope, vpTenantB, "crm_contacts", services.ListQuery{
		Page: 1, Limit: 20, Sort: "created_at", Order: "asc",
	})
	if err != nil {
		t.Fatalf("B list contacts: %v", err)
	}
	if len(contacts.Records) != 4 {
		t.Fatalf("B contacts = %d, want 4", len(contacts.Records))
	}
	for _, r := range contacts.Records {
		if _, present := r.Data["account_name"]; !present {
			first, _ := r.Data["first_name"].(string)
			t.Errorf("B contact %s: lookup account_name absent — lookup must resolve on B", first)
		}
	}
}

// TestVerticalPack_W8_InstallLedger asserts the installer records one ledger row per successful
// install (slug/version from the manifest), tenant-scoped, and appends on re-install (fail-open,
// per-success). Uses the in-memory fake store (the real GORM ledger is covered by
// internal/packinstall's own integration test).
// TestVerticalPack_W8_LedgerFailOpen proves the documented safety property: a failing ledger
// insert (RecordInstall error) is swallowed so the install still succeeds and returns a populated
// InstallResult — the schema change already landed regardless of the ledger note (reviewer must-fix
// #2: the fail-open branch had zero coverage).
func TestVerticalPack_W8_LedgerFailOpen(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, store := vpBuildStore(gdb, sdb)
	store.failRecord = true // every RecordInstall now errors
	ctx := context.Background()

	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm pack not found")
	}
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, crm)
	if err != nil {
		t.Fatalf("install must succeed despite ledger failure (fail-open), got: %v", err)
	}
	if res == nil || len(res.EntitiesCreated) == 0 {
		t.Fatalf("install returned no result / no entities created: %+v", res)
	}
	// The entities really landed (the ledger failure did not roll them back).
	installs, err := svc.ListInstalls(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list installs: %v", err)
	}
	if len(installs) != 0 {
		t.Errorf("ledger has %d rows despite forced insert failure, want 0", len(installs))
	}
}

func TestVerticalPack_W8_InstallLedger(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, _ := vpBuildStore(gdb, sdb)
	ctx := context.Background()

	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm pack not found")
	}

	// Install crm into A → exactly one ledger row {crm, 1.7.0} with the InstallResult captured.
	if _, err := svc.Install(ctx, vpTenantA, vpSeedUserID, crm); err != nil {
		t.Fatalf("install crm into A: %v", err)
	}
	aInstalls, err := svc.ListInstalls(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list A installs: %v", err)
	}
	if len(aInstalls) != 1 {
		t.Fatalf("A ledger = %d rows, want 1", len(aInstalls))
	}
	// Version comes from the CRM pack manifest, so pinning a literal here rots on every bump — this
	// assertion sat red at "want 1.8.0" while the pack was already 1.9.0, and nobody noticed because
	// it only runs under -tags=integration and CI has been off since 2026-07-08. What the ledger
	// contract actually promises is "the version that was INSTALLED is recorded", so read it from
	// the manifest instead of restating it.
	crmPack, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("no builtin pack with slug crm — the ledger assertion below would be vacuous")
	}
	if crmPack.Manifest == nil || crmPack.Manifest.Version == "" {
		t.Fatal("crm pack has no manifest version — the ledger assertion below would be vacuous")
	}
	wantVersion := crmPack.Manifest.Version
	if aInstalls[0].Slug != "crm" || aInstalls[0].Version != wantVersion {
		t.Errorf("A ledger row = {slug:%q version:%q}, want {crm %s}", aInstalls[0].Slug, aInstalls[0].Version, wantVersion)
	}
	if aInstalls[0].Result == nil {
		t.Errorf("A ledger row Result is nil, want the InstallResult map")
	}

	// Tenant isolation: B has no ledger rows.
	bInstalls, err := svc.ListInstalls(ctx, vpTenantB)
	if err != nil {
		t.Fatalf("list B installs: %v", err)
	}
	if len(bInstalls) != 0 {
		t.Errorf("B ledger = %d rows, want 0 (tenant isolation)", len(bInstalls))
	}

	// Append-on-re-install: seedPack has NO samples (no unique-email collision on re-install), so
	// installing it twice into tenant C yields two ledger rows.
	for i := 0; i < 2; i++ {
		if _, err := svc.Install(ctx, vpTenantC, vpSeedUserID, seedPack()); err != nil {
			t.Fatalf("seedPack install #%d into C: %v", i+1, err)
		}
	}
	cInstalls, err := svc.ListInstalls(ctx, vpTenantC)
	if err != nil {
		t.Fatalf("list C installs: %v", err)
	}
	if len(cInstalls) != 2 {
		t.Errorf("C ledger after two installs = %d rows, want 2 (append-only per success)", len(cInstalls))
	}
}

// --- W8-BE-2: dry-run install plan ------------------------------------------------------------

// vpCount runs a scalar COUNT(*) query and fails the test on error.
func vpCount(t *testing.T, db *sqlx.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Get(&n, query, args...); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// vpMetaSnapshot captures the row counts of every table a pack install can touch, for one tenant.
// Two snapshots taken around a dry-run must be identical (the zero-write invariant).
func vpMetaSnapshot(t *testing.T, sdb *sqlx.DB, tenantID string) map[string]int {
	t.Helper()
	return map[string]int{
		"sys_entities":      vpCount(t, sdb, "SELECT COUNT(*) FROM sys_entities WHERE tenant_id=?", tenantID),
		"sys_fields":        vpCount(t, sdb, "SELECT COUNT(*) FROM sys_fields WHERE tenant_id=?", tenantID),
		"acl_roles":         vpCount(t, sdb, "SELECT COUNT(*) FROM acl_roles WHERE tenant_id=?", tenantID),
		"sys_relationships": vpCount(t, sdb, "SELECT COUNT(*) FROM sys_relationships WHERE tenant_id=?", tenantID),
		"sys_dashboards":    vpCount(t, sdb, "SELECT COUNT(*) FROM sys_dashboards WHERE tenant_id=?", tenantID),
		"sys_pack_installs": vpCount(t, sdb, "SELECT COUNT(*) FROM sys_pack_installs WHERE tenant_id=?", tenantID),
		"obj_crm_contacts":  vpCount(t, sdb, "SELECT COUNT(*) FROM obj_crm_contacts WHERE tenant_id=?", tenantID),
		"obj_crm_accounts":  vpCount(t, sdb, "SELECT COUNT(*) FROM obj_crm_accounts WHERE tenant_id=?", tenantID),
	}
}

// vpSameSet asserts two string slices are equal as multisets (order-independent).
func vpSameSet(t *testing.T, name string, got, want []string) {
	t.Helper()
	gs := map[string]int{}
	for _, g := range got {
		gs[g]++
	}
	ws := map[string]int{}
	for _, w := range want {
		ws[w]++
	}
	if len(gs) != len(ws) {
		t.Errorf("%s: got %v, want %v (size mismatch)", name, got, want)
		return
	}
	for k, c := range ws {
		if gs[k] != c {
			t.Errorf("%s: got %v, want %v (mismatch on %q)", name, got, want, k)
			return
		}
	}
}

// TestVerticalPack_W8_DryRun_FreshTenant: a dry-run on a pristine tenant plans EVERY create (nothing
// skipped, no conflicts) and writes NOTHING — no metadata rows, no obj_* tables, no ledger entry.

// TestVerticalPack_W8_DuplicateDashboardName pins the intra-pack dedup restored after the
// W8-BE-2 decide/execute refactor: sys_dashboards has no UNIQUE(tenant_id,name), so a pack that
// declares two dashboards of the SAME name must create ONE and skip the second (not silently
// insert a duplicate row). Regression guard for the reviewer-flagged execute() divergence.
func TestVerticalPack_W8_DuplicateDashboardName(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc := vpBuild(gdb, sdb)
	ctx := context.Background()

	pack := seedPack()
	pack.Dashboards = []PackDashboard{
		{Name: "Tổng quan", Widgets: []PackWidget{{ID: "w1", Title: "Số công ty", Type: "kpi", Entity: "vp_companies", Metric: "count"}}},
		{Name: "Tổng quan", Widgets: []PackWidget{{ID: "w1", Title: "Trùng tên", Type: "kpi", Entity: "vp_companies", Metric: "count"}}},
	}
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(res.DashboardsCreated) != 1 || res.DashboardsCreated[0] != "Tổng quan" {
		t.Errorf("DashboardsCreated = %v, want exactly [Tổng quan]", res.DashboardsCreated)
	}
	if len(res.DashboardsSkipped) != 1 {
		t.Errorf("DashboardsSkipped = %v, want the 2nd same-name dashboard skipped", res.DashboardsSkipped)
	}
	// Physical proof: exactly one row in sys_dashboards for this tenant+name.
	var n int
	if err := sdb.Get(&n, "SELECT COUNT(*) FROM sys_dashboards WHERE tenant_id=? AND name=? AND deleted_at IS NULL", vpTenantA, "Tổng quan"); err != nil {
		t.Fatalf("count dashboards: %v", err)
	}
	if n != 1 {
		t.Errorf("sys_dashboards rows = %d, want 1 (no silent duplicate)", n)
	}
}

func TestVerticalPack_W8_DryRun_FreshTenant(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, store := vpBuildStore(gdb, sdb)
	ctx := context.Background()

	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm pack not found")
	}

	plan, err := svc.DryRunInstall(ctx, vpTenantA, crm)
	if err != nil {
		t.Fatalf("dry-run fresh: %v", err)
	}

	if want := crmEntityCount(t); len(plan.EntitiesToCreate) != want {
		t.Errorf("fresh dry-run: EntitiesToCreate = %v (%d), want %d", plan.EntitiesToCreate, len(plan.EntitiesToCreate), want)
	}
	if len(plan.EntitiesToSkip) != 0 {
		t.Errorf("fresh dry-run: EntitiesToSkip = %v, want 0", plan.EntitiesToSkip)
	}
	var rel, def int
	for _, f := range plan.FieldsToAdd {
		if f.Deferred {
			def++
		} else {
			rel++
		}
	}
	if rel != 10 || def != 5 {
		t.Errorf("fresh dry-run: fieldsToAdd rel=%d def=%d, want 10/5 (%v)", rel, def, plan.FieldsToAdd)
	}
	if len(plan.NNToRegister) != 1 || plan.NNToRegister[0] != "crm_contacts_opportunities" {
		t.Errorf("fresh dry-run: NNToRegister = %v, want [crm_contacts_opportunities]", plan.NNToRegister)
	}
	if len(plan.RolesToCreate) != 2 {
		t.Errorf("fresh dry-run: RolesToCreate = %v, want 2", plan.RolesToCreate)
	}
	if plan.SamplesCount != 31 {
		t.Errorf("fresh dry-run: SamplesCount = %d, want 31", plan.SamplesCount)
	}
	if len(plan.Conflicts) != 0 {
		t.Errorf("fresh dry-run: Conflicts = %v, want 0", plan.Conflicts)
	}

	// ZERO-WRITE PROOF.
	if n := vpCount(t, sdb, "SELECT COUNT(*) FROM sys_entities WHERE tenant_id=?", vpTenantA); n != 0 {
		t.Errorf("fresh dry-run wrote %d sys_entities rows, want 0", n)
	}
	if n := vpCount(t, sdb, "SELECT COUNT(*) FROM acl_roles WHERE tenant_id=?", vpTenantA); n != 0 {
		t.Errorf("fresh dry-run wrote %d acl_roles rows, want 0", n)
	}
	if n := vpCount(t, sdb, "SELECT COUNT(*) FROM sys_pack_installs WHERE tenant_id=?", vpTenantA); n != 0 {
		t.Errorf("fresh dry-run wrote %d ledger rows, want 0", n)
	}
	if got := len(store.recs); got != 0 {
		t.Errorf("fresh dry-run recorded %d install-ledger calls, want 0", got)
	}
	if vpTableExists(t, sdb, "obj_crm_accounts") {
		t.Errorf("fresh dry-run created obj_crm_accounts, want no table")
	}
}

// TestVerticalPack_W8_DryRun_MatchesRealInstall: on a fresh tenant, the plan a dry-run produces
// matches EXACTLY what the subsequent real install actually did (advisor's discriminating check).
func TestVerticalPack_W8_DryRun_MatchesRealInstall(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc := vpBuild(gdb, sdb)
	ctx := context.Background()

	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm pack not found")
	}

	plan, err := svc.DryRunInstall(ctx, vpTenantA, crm)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, crm)
	if err != nil {
		t.Fatalf("install after dry-run: %v", err)
	}

	vpSameSet(t, "entitiesCreated", res.EntitiesCreated, plan.EntitiesToCreate)
	vpSameSet(t, "rolesCreated", res.RolesCreated, plan.RolesToCreate)
	vpSameSet(t, "nnRegistered", res.NNRelationsCreated, plan.NNToRegister)
	if res.SamplesInserted != plan.SamplesCount {
		t.Errorf("samples: install=%d plan=%d", res.SamplesInserted, plan.SamplesCount)
	}
	var relKeys, defKeys []string
	for _, f := range plan.FieldsToAdd {
		if f.Deferred {
			defKeys = append(defKeys, f.Entity+"."+f.Field)
		} else {
			relKeys = append(relKeys, f.Entity+"."+f.Field)
		}
	}
	vpSameSet(t, "relationFields", res.RelationFieldsAdded, relKeys)
	vpSameSet(t, "deferredFields", res.DeferredFieldsAdded, defKeys)
}

// TestVerticalPack_W8_DryRun_NoCrossTenantOracle locks the sharpest security property: the
// unique-slot probe must NEVER read another tenant's rows in the SHARED obj_* table. Tenant B
// installs CRM (its contacts hold the sample emails); a dry-run for tenant A of the SAME pack —
// with A having zero data — must report ZERO unique_slot_held conflicts. If a future change drops
// the probe's tenant pin (reasoning "the physical table is shared, check globally"), this dry-run
// becomes an existence oracle over B's data and THIS test goes red (security audit W1).
func TestVerticalPack_W8_DryRun_NoCrossTenantOracle(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc := vpBuild(gdb, sdb)
	ctx := context.Background()

	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm pack not found")
	}
	// Tenant B holds live rows with the pack's sample emails in the shared obj_crm_contacts.
	if _, err := svc.Install(ctx, vpTenantB, vpSeedUserID, crm); err != nil {
		t.Fatalf("install into B: %v", err)
	}

	// Tenant A (no data) dry-runs the SAME pack. The shared table now exists (TableExists=true),
	// so the probe DOES run — and must find nothing, because every WHERE is tenant_id = A.
	plan, err := svc.DryRunInstall(ctx, vpTenantA, crm)
	if err != nil {
		t.Fatalf("dry-run for A: %v", err)
	}
	for _, cf := range plan.Conflicts {
		if cf.Kind == "unique_slot_held" {
			t.Errorf("cross-tenant oracle leak: A's dry-run reported unique_slot_held %s.%s while only B holds the value", cf.Entity, cf.Field)
		}
	}
	if want := crmEntityCount(t); len(plan.EntitiesToCreate) != want {
		t.Errorf("A dry-run: entitiesToCreate=%v, want 8 (A has no data)", plan.EntitiesToCreate)
	}
}

// TestVerticalPack_W8_DryRun_ConflictAndZeroWrite: a dry-run of a pack ALREADY installed reports
// every skip, flags the unique-slot conflict a re-install would hit (trap B3), and writes nothing.
func TestVerticalPack_W8_DryRun_ConflictAndZeroWrite(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc := vpBuild(gdb, sdb)
	ctx := context.Background()

	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm pack not found")
	}

	// Install once so obj_crm_contacts holds live rows with unique emails + layouts exist.
	if _, err := svc.Install(ctx, vpTenantA, vpSeedUserID, crm); err != nil {
		t.Fatalf("initial install: %v", err)
	}

	before := vpMetaSnapshot(t, sdb, vpTenantA)

	plan, err := svc.DryRunInstall(ctx, vpTenantA, crm)
	if err != nil {
		t.Fatalf("dry-run on installed tenant: %v", err)
	}

	if want := crmEntityCount(t); len(plan.EntitiesToCreate) != 0 || len(plan.EntitiesToSkip) != want {
		t.Errorf("installed dry-run: create=%v skip=%v, want 0 create / 8 skip", plan.EntitiesToCreate, plan.EntitiesToSkip)
	}
	if len(plan.NNToRegister) != 0 || len(plan.NNToSkip) != 1 {
		t.Errorf("installed dry-run: nnRegister=%v nnSkip=%v, want 0/1", plan.NNToRegister, plan.NNToSkip)
	}
	if len(plan.RolesToCreate) != 0 || len(plan.RolesToSkip) != 2 {
		t.Errorf("installed dry-run: roleCreate=%v roleSkip=%v, want 0/2", plan.RolesToCreate, plan.RolesToSkip)
	}

	// HARD conflict: crm_contacts.email unique slot held by the live sample rows.
	foundUnique := false
	for _, cf := range plan.Conflicts {
		if cf.Kind == "unique_slot_held" && cf.Entity == "crm_contacts" && cf.Field == "email" {
			foundUnique = true
		}
	}
	if !foundUnique {
		t.Errorf("installed dry-run: no unique_slot_held conflict for crm_contacts.email (%v)", plan.Conflicts)
	}

	// ZERO-WRITE PROOF: every counted table is unchanged.
	after := vpMetaSnapshot(t, sdb, vpTenantA)
	for k, v := range before {
		if after[k] != v {
			t.Errorf("dry-run mutated %s: before=%d after=%d", k, v, after[k])
		}
	}
}

// TestVerticalPack_W8_DryRun_SoftDeleteFreesSlot pins the HONEST current behavior (no false
// positive): soft-deleting an entity's METADATA also soft-deletes this tenant's rows in the shared
// obj_* table (the RM-03 data-bleed sweep in EntityService.SoftDeleteEntity). A soft-deleted record
// projects NULL through the unique generated column's CASE guard, so it no longer squats the slot —
// a re-install would NOT hit RECORD_DUPLICATE. The probe (deleted_at IS NULL) therefore reports NO
// unique_slot_held conflict, and crm_contacts is planned as a fresh create. This proves the probe
// distinguishes LIVE squatters (a real conflict — see ConflictAndZeroWrite) from freed slots.
func TestVerticalPack_W8_DryRun_SoftDeleteFreesSlot(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc := vpBuild(gdb, sdb)
	ctx := context.Background()

	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm pack not found")
	}
	if _, err := svc.Install(ctx, vpTenantA, vpSeedUserID, crm); err != nil {
		t.Fatalf("initial install: %v", err)
	}

	// Soft-delete the crm_contacts ENTITY metadata (the engine also soft-deletes its obj_* rows).
	metaRepo := metadata.NewRepository(gdb)
	entitySvc := services.NewEntityService(dynrepo.New(sdb), metaRepo)
	if err := entitySvc.SoftDeleteEntity(ctx, vpTenantA, "crm_contacts"); err != nil {
		t.Fatalf("soft-delete crm_contacts: %v", err)
	}

	plan, err := svc.DryRunInstall(ctx, vpTenantA, crm)
	if err != nil {
		t.Fatalf("dry-run after soft-delete: %v", err)
	}

	// crm_contacts is planned as a CREATE again (metadata gone) ...
	inCreate := false
	for _, e := range plan.EntitiesToCreate {
		if e == "crm_contacts" {
			inCreate = true
		}
	}
	if !inCreate {
		t.Errorf("dry-run: crm_contacts not in EntitiesToCreate after soft-delete (%v)", plan.EntitiesToCreate)
	}
	// ... and its slot is FREED (rows soft-deleted) → NO unique_slot_held conflict (no false positive).
	for _, cf := range plan.Conflicts {
		if cf.Kind == "unique_slot_held" && cf.Entity == "crm_contacts" {
			t.Errorf("dry-run: false-positive unique_slot_held on a freed (soft-deleted) slot: %+v", cf)
		}
	}
}

// TestVerticalPack_AppMint (Apps Phase 4, ../specs/010-app-container/spec.md §0.2 "cài một vertical-pack → mint
// một sys_app"): on REAL MySQL, a pack install with the app service wired mints exactly one
// sys_app whose nav rows list the pack's entities in PACK order (not creation/dependency order)
// followed by its dashboards; a re-install leaves the app untouched (no dup, no error, nav
// intact); apps are tenant-scoped.
func TestVerticalPack_AppMint(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc := vpBuild(gdb, sdb)
	metaRepo := metadata.NewRepository(gdb)
	dashboardRepo := dashboard.NewSQLRepository(sdb)
	// nil ACL is fine here: CreateApp/ReplaceNav/GetApp never consult it (only GetNav does).
	appSvc := services.NewAppService(appmeta.NewSQLRepository(sdb), metaRepo, dashboardRepo, nil, zap.NewNop())
	svc.WithAppService(appSvc)
	ctx := context.Background()

	// seedPack has no manifest — give it one with a KEBAB slug so the kebab→snake conversion is
	// proven against the real CreateApp slug validator. Also reverse the entity list: dependency
	// order still creates vp_companies first, so a nav of (vp_people, vp_companies) proves the
	// mint follows the pack's AUTHORED order, not creation order.
	pack := seedPack()
	pack.Manifest = &Manifest{Name: "Test Vertical", Slug: "test-vertical", Version: "1.0.0"}
	pack.Entities = []PackEntity{pack.Entities[1], pack.Entities[0]} // people first, companies second

	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !res.AppMinted || res.AppSlug != "test_vertical" || res.AppError != "" {
		t.Fatalf("result = minted:%v slug:%q appErr:%q; want minted:true slug:test_vertical", res.AppMinted, res.AppSlug, res.AppError)
	}

	// One app row with the manifest label + fallback icon.
	apps, err := appSvc.ListApps(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list apps A: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "test_vertical" || apps[0].Label != "Test Vertical" {
		t.Fatalf("apps A = %+v; want 1 app test_vertical/Test Vertical", apps)
	}
	if apps[0].Icon == nil || *apps[0].Icon != "package" {
		t.Errorf("app icon = %v; want package (non-builtin slug)", apps[0].Icon)
	}

	// Nav rows follow PACK order: vp_people then vp_companies (both entity items).
	ents, err := metaRepo.ListEntities(vpTenantA)
	if err != nil {
		t.Fatalf("list entities A: %v", err)
	}
	idByName := map[string]string{}
	for _, e := range ents {
		idByName[e.Name] = e.ID
	}
	_, items, err := appSvc.GetApp(ctx, vpTenantA, apps[0].ID)
	if err != nil {
		t.Fatalf("get app A: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("nav items = %+v; want 2", items)
	}
	for i, wantName := range []string{"vp_people", "vp_companies"} {
		if items[i].ItemType != appmeta.ItemTypeEntity || items[i].RefID != idByName[wantName] {
			t.Errorf("nav[%d] = %s:%s; want entity:%s (%s)", i, items[i].ItemType, items[i].RefID, idByName[wantName], wantName)
		}
	}

	// Re-install: idempotent — no dup app, no error recorded, nav untouched.
	res2, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("re-install: %v", err)
	}
	if res2.AppMinted || res2.AppSlug != "test_vertical" || res2.AppError != "" {
		t.Errorf("re-install result = minted:%v slug:%q appErr:%q; want minted:false, no error", res2.AppMinted, res2.AppSlug, res2.AppError)
	}
	apps2, err := appSvc.ListApps(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list apps A after re-install: %v", err)
	}
	if len(apps2) != 1 {
		t.Errorf("apps after re-install = %d; want 1 (no dup)", len(apps2))
	}
	_, items2, err := appSvc.GetApp(ctx, vpTenantA, apps[0].ID)
	if err != nil {
		t.Fatalf("get app A after re-install: %v", err)
	}
	if len(items2) != len(items) {
		t.Errorf("nav after re-install = %d items; want %d (untouched)", len(items2), len(items))
	}

	// Builtin CRM into tenant B: dashboards land in the nav AFTER the entities.
	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm pack not found")
	}
	resB, err := svc.Install(ctx, vpTenantB, vpSeedUserID, crm)
	if err != nil {
		t.Fatalf("install crm into B: %v", err)
	}
	if !resB.AppMinted || resB.AppSlug != "crm" {
		t.Fatalf("crm result = minted:%v slug:%q; want minted:true slug:crm", resB.AppMinted, resB.AppSlug)
	}
	appsB, err := appSvc.ListApps(ctx, vpTenantB)
	if err != nil {
		t.Fatalf("list apps B: %v", err)
	}
	if len(appsB) != 1 || appsB[0].Name != "crm" || appsB[0].Icon == nil || *appsB[0].Icon != "briefcase" {
		t.Fatalf("apps B = %+v; want 1 app crm with icon briefcase", appsB)
	}
	_, itemsB, err := appSvc.GetApp(ctx, vpTenantB, appsB[0].ID)
	if err != nil {
		t.Fatalf("get app B: %v", err)
	}
	wantLen := len(crm.Entities) + len(crm.Dashboards)
	if len(itemsB) != wantLen {
		t.Fatalf("crm nav = %d items; want %d (entities + dashboards)", len(itemsB), wantLen)
	}
	for i, it := range itemsB {
		wantType := appmeta.ItemTypeEntity
		if i >= len(crm.Entities) {
			wantType = appmeta.ItemTypeDashboard
		}
		if it.ItemType != wantType {
			t.Errorf("crm nav[%d] type = %s; want %s (dashboards after entities)", i, it.ItemType, wantType)
		}
	}

	// Tenant isolation: tenant C (never installed) sees no app.
	appsC, err := appSvc.ListApps(ctx, vpTenantC)
	if err != nil {
		t.Fatalf("list apps C: %v", err)
	}
	if len(appsC) != 0 {
		t.Errorf("apps C = %+v; want none (tenant-scoped)", appsC)
	}
}

// vpBuildWF mirrors vpBuild but ALSO wires a real WorkflowService as the WorkflowWriter so the
// pack's workflows install through the SAME validated admin path (buildWorkflowRule + WF-01 field
// guard). It returns the vertical-pack service and the workflow service (to query rules back).
// The workflow service is wired WithCache (a nil cache would panic on CreateWorkflow's post-commit
// Invalidate) and WithEntityChecker(metaRepo) — *metadata.Repository satisfies both GetEntityByName
// and ListFields, so the WF-01 field-target guard is ARMED (a field_update on a missing field 422s).
func vpBuildWF(gdb *gorm.DB, sdb *sqlx.DB) (*Service, *services.WorkflowService) {
	return vpBuildWFWith(gdb, sdb, nil, nil)
}

// vpBuildWFWith is vpBuildWF with the workflow admin service optionally wrapped by the draft layer
// (specs/063): gate + drafts non-nil ⇒ the service gets a DraftWorkflowRepo, exactly as main.go wires it.
func vpBuildWFWith(gdb *gorm.DB, sdb *sqlx.DB, gate *services.DraftGate, drafts *metaversion.DraftRepo) (*Service, *services.WorkflowService) {
	metaRepo := metadata.NewRepository(gdb)
	layoutRepo := metadata.NewLayoutRepository(gdb)
	roleRepo := auth.NewRoleRepository(gdb)
	dyn := dynrepo.New(sdb)

	mc := services.NewMetaCache(metaRepo, services.DefaultMetaTTL) // as cmd/server: a field added at (re)install is visible to the validators at once
	entitySvc := services.NewEntityService(dyn, metaRepo).WithMetaCache(mc)
	fieldSvc := services.NewFieldService(dyn, metaRepo).WithMetaCache(mc)
	layoutSvc := services.NewLayoutService(metaRepo, layoutRepo, zap.NewNop())
	roleSvc := services.NewRoleService(roleRepo)
	relationSvc := services.NewRelationService(dyn, metaRepo)
	recordSvc := services.NewRecordService(metaRepo, dyn).
		WithLabelFetcher(dyn).
		WithFieldValueFetcher(dyn).
		WithRollupMeta(metaRepo).
		WithRollupLinked(dyn).
		WithSequenceClaimer(dyn)
	dashboardRepo := dashboard.NewSQLRepository(sdb)
	dashboardSvc := services.NewDashboardService(dashboardRepo, recordSvc, zap.NewNop())

	workflowRepo := workflow.NewSQLRepository(sdb)
	workflowCache := workflow.NewCachedLoader(workflowRepo, workflow.DefaultWorkflowTTL)
	workflowSvc := services.NewWorkflowService(workflowRepo).WithEntityChecker(metaRepo).WithCache(workflowCache)
	if gate != nil {
		workflowSvc = services.NewWorkflowService(services.NewDraftWorkflowRepo(workflowRepo, drafts, gate)).WithEntityChecker(metaRepo).WithCache(workflowCache)
	}

	svc := NewService(Deps{
		EntityReader:    metaRepo,
		LayoutReader:    layoutRepo,
		RoleReader:      roleRepo,
		EntityWriter:    entitySvc,
		EntityExists:    metaRepo,
		FieldWriter:     fieldSvc,
		LayoutWriter:    layoutSvc,
		RoleWriter:      roleSvc,
		RelationWriter:  relationSvc,
		RecordWriter:    recordSvc,
		Linker:          relationSvc,
		RecordExists:    dyn,
		RecordFetcher:   dyn,
		DashboardWriter: dashboardSvc,
		WorkflowWriter:  workflowSvc,
		// PACK-QUEUES: real WorkQueueService so builtin installs seed queues AND the fresh-install
		// provenance gate covers the work_queue item type end-to-end. acl nil is fine here — only
		// ListForUser consults it, and the install/validate path never does.
		WorkQueueWriter:    services.NewWorkQueueService(workqueue.New(gdb), recordSvc, nil, roleRepo, nil),
		TeamWriter:         auth.NewTeamRepository(gdb), // PACK-TEAMS: seed teams + memberships
		RelationshipReader: metaRepo,
		RecordLister:       dyn,
		ObjProber:          dyn,
	})
	return svc, workflowSvc
}

// wfPack is a Ticketing-style pack: one entity + a sample row + two workflows — an after_create
// routing rule (field_update priority=high) and a date_offset SLA timer (created_at + 2 days,
// field_update priority=urgent). The SLA timer proves the primitives compose: a pack can ship an
// event routing rule AND a time-based escalation. The sample is here to pin the install ORDER —
// workflows are the LAST data pass, so the seed row is already in place when the rules are created
// (a rule created before the sample would fire the dispatcher on the seed insert).
func wfPack() *Pack {
	active := true
	return &Pack{
		Manifest: &Manifest{Name: "Ticketing", Slug: "wf-ticketing", Version: "1.0.0"},
		Entities: []PackEntity{
			{Name: "wf_tickets", Label: "Tickets", Fields: []PackField{
				{Name: "subject", Label: "Subject", FieldType: "text", IsRequired: true},
				{Name: "priority", Label: "Priority", FieldType: "text"},
			}},
		},
		Samples: []PackSample{
			{Entity: "wf_tickets", Records: []PackRecord{
				{Ref: "@t1", Data: map[string]any{"subject": "Login broken", "priority": "low"}},
			}},
		},
		Workflows: []PackWorkflow{
			{
				Name:       "Auto-escalate new tickets",
				EntityName: "wf_tickets",
				Trigger:    "after_create",
				Actions: []PackWorkflowAction{
					{Type: "field_update", Field: "priority", Value: "high"},
				},
				IsActive: &active,
			},
			{
				Name:       "SLA breach escalation",
				EntityName: "wf_tickets",
				Trigger:    "date_offset",
				Condition: &PackWorkflowCondition{
					Timer: &PackWorkflowTimer{
						DateField: "created_at", // system anchor — no date field needed on the entity
						Offset:    PackWorkflowTimerOffset{Amount: 2, Unit: "days"},
						Direction: "after",
					},
				},
				Actions: []PackWorkflowAction{
					{Type: "field_update", Field: "priority", Value: "urgent"},
				},
			},
		},
	}
}

// TestVerticalPack_InstallWorkflows installs a pack carrying an entity + sample + two workflows
// against real MySQL and asserts:
//   - both workflows are created and queryable back (config-validated through the real service);
//   - the date_offset SLA timer + after_create routing rule both persist (primitives compose);
//   - re-install is idempotent (both skipped by name, no duplicate rows);
//   - a pack workflow whose field_update targets a MISSING field FAILS install with the WF-01
//     code RECORD_UNKNOWN_FIELD (proving install runs the SAME config-time validation as the API).
func TestVerticalPack_InstallWorkflows(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, workflowSvc := vpBuildWF(gdb, sdb)
	ctx := context.Background()

	// --- Install into tenant A. ---
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, wfPack())
	if err != nil {
		t.Fatalf("install wf pack into A: %v", err)
	}
	// Install ORDER: workflows are the LAST data pass (after Pass 3 samples + Pass 7 dashboards),
	// so the seed row landed first. We assert the seed row is present AND both workflows created.
	if res.SamplesInserted != 1 {
		t.Errorf("SamplesInserted = %d, want 1 (seed row landed before the workflows pass)", res.SamplesInserted)
	}
	wantCreated := map[string]bool{"Auto-escalate new tickets": true, "SLA breach escalation": true}
	if len(res.WorkflowsCreated) != 2 {
		t.Fatalf("WorkflowsCreated = %v, want 2", res.WorkflowsCreated)
	}
	for _, n := range res.WorkflowsCreated {
		if !wantCreated[n] {
			t.Errorf("unexpected workflow created %q", n)
		}
	}
	if len(res.WorkflowsSkipped) != 0 {
		t.Errorf("WorkflowsSkipped = %v, want empty on first install", res.WorkflowsSkipped)
	}

	// --- Query the workflows back (proves they persisted + validated). ---
	rules, err := workflowSvc.ListWorkflows(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list workflows A: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("tenant A has %d workflows, want 2", len(rules))
	}
	byName := map[string]workflow.WorkflowRecord{}
	for _, r := range rules {
		byName[r.Name] = r
	}
	if r, ok := byName["Auto-escalate new tickets"]; !ok || r.Trigger != "after_create" || r.EntityName != "wf_tickets" {
		t.Errorf("Auto-escalate rule = %+v, want trigger after_create on wf_tickets", r)
	}
	sla, ok := byName["SLA breach escalation"]
	if !ok || sla.Trigger != "date_offset" {
		t.Fatalf("SLA rule = %+v, want trigger date_offset", sla)
	}
	// The date_offset timer config persisted inside the condition JSON — decode and assert the
	// actual struct (created_at anchor + 2 days after) rather than a fragile raw-substring check.
	slaWF, err := sla.ToWorkflow()
	if err != nil {
		t.Fatalf("decode SLA condition: %v", err)
	}
	if slaWF.Condition == nil || slaWF.Condition.Timer == nil {
		t.Fatalf("SLA rule has no persisted timer config; condition = %+v", slaWF.Condition)
	}
	tm := slaWF.Condition.Timer
	if tm.DateField != "created_at" || tm.Offset.Amount != 2 || tm.Offset.Unit != "days" || tm.Direction != "after" {
		t.Errorf("SLA timer = %+v, want {created_at, {2 days}, after}", *tm)
	}

	// --- Tenant isolation: tenant B (never installed) sees no workflows. ---
	rulesB, err := workflowSvc.ListWorkflows(ctx, vpTenantB)
	if err != nil {
		t.Fatalf("list workflows B: %v", err)
	}
	if len(rulesB) != 0 {
		t.Errorf("tenant B must see 0 workflows (isolation), got %d", len(rulesB))
	}

	// --- Idempotent re-install into A: both skipped by name, no duplicate rows. ---
	res2, err := svc.Install(ctx, vpTenantA, vpSeedUserID, wfPack())
	if err != nil {
		t.Fatalf("re-install wf pack into A: %v", err)
	}
	if len(res2.WorkflowsCreated) != 0 {
		t.Errorf("re-install WorkflowsCreated = %v, want empty (idempotent)", res2.WorkflowsCreated)
	}
	if len(res2.WorkflowsSkipped) != 2 {
		t.Errorf("re-install WorkflowsSkipped = %v, want 2", res2.WorkflowsSkipped)
	}
	rules2, _ := workflowSvc.ListWorkflows(ctx, vpTenantA)
	if len(rules2) != 2 {
		t.Errorf("after re-install tenant A has %d workflows, want 2 (no dup)", len(rules2))
	}

	// --- WF-01: a pack workflow targeting a MISSING field fails install with RECORD_UNKNOWN_FIELD. ---
	// Installs into a fresh tenant (C): Pass 1 creates the entity, the workflows pass (terminal, like
	// dashboards) then rejects the bad rule and the install returns the error WITHOUT tearing down the
	// committed entity (auto-committed DDL cannot be undone; idempotent re-install completes the rest).
	badPack := &Pack{
		Manifest: &Manifest{Name: "Bad", Slug: "wf-bad", Version: "1.0.0"},
		Entities: []PackEntity{
			{Name: "wf_bad", Label: "Bad", Fields: []PackField{
				{Name: "subject", Label: "Subject", FieldType: "text"},
			}},
		},
		Workflows: []PackWorkflow{
			{
				Name:       "Bad rule",
				EntityName: "wf_bad",
				Trigger:    "after_create",
				Actions: []PackWorkflowAction{
					{Type: "field_update", Field: "nonexistent_field", Value: "x"},
				},
			},
		},
	}
	_, err = svc.Install(ctx, vpTenantC, vpSeedUserID, badPack)
	if err == nil {
		t.Fatal("install bad-workflow pack: want error, got nil")
	}
	var ve *services.ValidationError
	if !errors.As(err, &ve) || ve.Code != "RECORD_UNKNOWN_FIELD" {
		t.Fatalf("install bad-workflow pack error = %v, want RECORD_UNKNOWN_FIELD (WF-01 guard)", err)
	}
	// Terminal-pass semantics (mirrors dashboards): the entity created before the failing workflow
	// pass stays committed (no rollback), and NO workflow was persisted for the bad rule.
	cRules, err := workflowSvc.ListWorkflows(ctx, vpTenantC)
	if err != nil {
		t.Fatalf("list workflows C: %v", err)
	}
	if len(cRules) != 0 {
		t.Errorf("tenant C has %d workflows after a failed bad-workflow install, want 0", len(cRules))
	}
}

// PACK-HOOKS: một pack chở được endpoint nhận tín hiệu, và chở KHÔNG kèm khoá ký (2026-08-02).
//
// Trước lát này, vòng tiền mạnh nhất của engine phải dựng tay cho TỪNG khách: hook + mapping +
// regex + secret. Lời hứa "90% engine" hụt đúng một mắt ở chính thứ nó tự hào nhất.
//
// Tính chất được ghim ở đây, theo thứ tự quan trọng:
//  1. Secret là khoá ký SỐNG. Pack không được mang nó — mang là trao khoá endpoint của khách A cho
//     khách B, và còn nằm lại lần thứ hai trong sys_pack_items (provenance lưu JSON mọi struct
//     pack). Nên PackInboundHook KHÔNG có trường secret, install tự sinh mới mỗi tenant.
//  2. Cài lại phải BỎ QUA theo tên (hook UNIQUE theo (tenant,name) nên tạo mù sẽ 409).
//  3. Dry-run phải khai báo hook — một kế hoạch im lặng về endpoint sắp mọc là một cổng nói dối.
func TestVerticalPack_Hooks_MintedPerTenant_NeverCarried(t *testing.T) {
	// Không cần DB: hai tính chất ở đây là tính chất của SHAPE pack, kiểm được thuần tuý.
	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm missing")
	}
	if len(crm.InboundHooks) == 0 {
		t.Fatal("pack CRM phải chở hook đối soát — nếu bỏ thì vòng tiền lại phải dựng tay từng khách")
	}
	// Bằng chứng cấu trúc: shape của pack KHÔNG có chỗ nào để nhét secret.
	raw, err := json.Marshal(crm.InboundHooks[0])
	if err != nil {
		t.Fatalf("marshal hook: %v", err)
	}
	if strings.Contains(strings.ToLower(string(raw)), "secret") {
		t.Fatalf("pack hook serialize ra có chữ 'secret' — khoá ký không được rời tenant: %s", raw)
	}
}

// TestVerticalPack_Reconcile_UpgradeAddsScalarFields is the upgrade path: a tenant installed on an
// OLD pack version re-installs a NEWER one that added scalar fields to an existing entity. Before
// the reconcile pass, install skipped the existing entity and its new fields never arrived — a
// tenant provisioned on CRM 1.14.0 would never receive amount_due, so the payment QR could not
// render and the money loop silently did not work for every already-delivered customer.
//
// Relation/lookup fields are deliberately NOT reconciled (they create generated columns and
// registry state that collide on an existing entity); this pins the scalar path that the money
// loop depends on, and that a version bump most commonly adds.
func TestVerticalPack_Reconcile_UpgradeAddsScalarFields(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc := vpBuild(gdb, sdb)
	ctx := context.Background()

	// v1: an entity with two fields.
	oldPack := &Pack{
		Manifest: &Manifest{Slug: "recon", Name: "Recon", Version: "1.0.0"},
		Entities: []PackEntity{{
			Name: "rc_orders", Label: "Đơn",
			Fields: []PackField{
				{Name: "code", Label: "Mã", FieldType: "text", IsRequired: true},
				{Name: "status", Label: "TT", FieldType: "picklist", Options: map[string]any{"values": []any{"Mới", "Xong"}}},
			},
		}},
	}
	if _, err := svc.Install(ctx, vpTenantA, vpSeedUserID, oldPack); err != nil {
		t.Fatalf("install v1: %v", err)
	}

	// v2: the SAME entity gains a scalar field (amount) and a relation the reconcile must SKIP.
	newPack := &Pack{
		Manifest: &Manifest{Slug: "recon", Name: "Recon", Version: "2.0.0"},
		Entities: []PackEntity{
			{Name: "rc_customers", Label: "KH", Fields: []PackField{{Name: "name", Label: "Tên", FieldType: "text", IsRequired: true}}},
			{
				Name: "rc_orders", Label: "Đơn",
				Fields: []PackField{
					{Name: "code", Label: "Mã", FieldType: "text", IsRequired: true},
					{Name: "status", Label: "TT", FieldType: "picklist", Options: map[string]any{"values": []any{"Mới", "Xong"}}},
					{Name: "amount", Label: "Tiền", FieldType: "currency", Options: map[string]any{"currencyCode": "VND"}},
					{Name: "customer", Label: "KH", FieldType: "relation", Options: map[string]any{"relatedEntity": "rc_customers", "inverseField": "orders"}},
				},
			},
		},
	}
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, newPack)
	if err != nil {
		t.Fatalf("upgrade install v2: %v", err)
	}

	// The scalar field must have been reconciled onto the existing entity; the relation must not.
	if !containsStr(res.RelationFieldsAdded, "rc_orders.amount") {
		t.Errorf("RelationFieldsAdded = %v, want it to include the reconciled scalar rc_orders.amount", res.RelationFieldsAdded)
	}
	if containsStr(res.RelationFieldsAdded, "rc_orders.customer") {
		t.Errorf("a relation field was reconciled onto an existing entity — that path is deliberately skipped: %v", res.RelationFieldsAdded)
	}

	// Prove it is really there: fields of the live entity include amount, not customer.
	ents, err := metadata.NewRepository(gdb).ListEntities(vpTenantA)
	if err != nil {
		t.Fatalf("list entities: %v", err)
	}
	var oid string
	for _, e := range ents {
		if e.Name == "rc_orders" {
			oid = e.ID
		}
	}
	fields, err := metadata.NewRepository(gdb).ListFields(ctx, vpTenantA, oid)
	if err != nil {
		t.Fatalf("list fields: %v", err)
	}
	have := map[string]bool{}
	for _, f := range fields {
		have[f.Name] = true
	}
	if !have["amount"] {
		t.Error("scalar field 'amount' did NOT arrive on the existing entity after upgrade — reconcile failed")
	}
	if have["customer"] {
		t.Error("relation field 'customer' arrived — the reconcile skip did not hold")
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
