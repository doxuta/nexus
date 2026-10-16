//go:build integration

// RM-28 Slice B — migration 000051 (demote_obj_unique_index) integration tests. Uses a CUSTOM
// throwaway-DB staging (not o3aSetup, which migrates ALL the way to head): these tests need to
// seed PRE-Slice-B physical state (a real `uq_obj_*` UNIQUE index) BEFORE stepping the demote
// migration forward one version, which requires stopping the initial migrate at v50.
//
// The Go DDL code itself has moved on unconditionally (BuildCreateTable/AddUniqueGencolNonUnique
// always emit non-unique idx_ now) — so a real `uq_obj_*` index can only be materialized here via
// the intentionally-kept dynrepo.AddUniqueColumn primitive (see its doc comment), which is
// EXACTLY what the up migration's own discovery cursor expects to find pre-existing.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestRM28SliceB_Migration -race -v
package services

import (
	"context"
	"os/exec"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/dynrepo"
)

// rm28bCreateThrowawayDB creates a fresh, empty throwaway database (mirrors o3aSetup's DB
// lifecycle, but WITHOUT migrating — the caller controls exactly which migrations run).
func rm28bCreateThrowawayDB(t *testing.T) string {
	t.Helper()
	dbName := o3aUniqueDBName()
	admin, err := sqlx.Connect("mysql", o3aAdminDSN(t))
	if err != nil {
		t.Skipf("rm28b: MySQL not reachable (%v) — skipping", err)
	}
	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`"); err != nil {
		_ = admin.Close()
		t.Fatalf("rm28b: drop throwaway DB: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = admin.Close()
		t.Fatalf("rm28b: create throwaway DB: %v", err)
	}
	_ = admin.Close()
	t.Cleanup(func() {
		if a2, err := sqlx.Connect("mysql", o3aAdminDSN(t)); err == nil {
			_, _ = a2.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
			_ = a2.Close()
		}
	})
	return dbName
}

// rm28bMigrate runs the golang-migrate CLI against dbName with the given args (e.g. "up","50" or
// "down","1"). Skips (not fails) when the migrate binary is unavailable.
func rm28bMigrate(t *testing.T, dbName string, args ...string) {
	t.Helper()
	migrateBin := o3aMigrateBin(t)
	if migrateBin == "" {
		t.Skip("rm28b: migrate binary not found — skipping")
	}
	fullArgs := append([]string{"-path", o3aMigrationsDir, "-database", o3aMigrateDSN(t, dbName)}, args...)
	cmd := exec.Command(migrateBin, fullArgs...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rm28b: migrate %v: %v\n%s", args, err, out)
	}
}

// rm28bIndexUnique reports (exists, isUnique) for `index` on `table`.
func rm28bIndexUnique(t *testing.T, db *sqlx.DB, table, index string) (exists, unique bool) {
	t.Helper()
	var nonUnique int
	err := db.Get(&nonUnique, "SELECT NON_UNIQUE FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ? LIMIT 1", table, index)
	if err != nil {
		return false, false
	}
	return true, nonUnique == 0
}

// rm28bSeedField inserts one active (sys_entities, sys_fields) pair for tenantID naming a plain
// `is_unique=1` text field, and returns the entity id.
func rm28bSeedField(t *testing.T, db *sqlx.DB, tenantID, entityName, fieldName string) string {
	t.Helper()
	entID := uuid.NewString()
	if _, err := db.Exec(`INSERT INTO sys_entities (id, tenant_id, name, label, table_name) VALUES (?, ?, ?, ?, ?)`,
		entID, tenantID, entityName, entityName, "obj_"+entityName); err != nil {
		t.Fatalf("seed entity %s: %v", entityName, err)
	}
	if _, err := db.Exec(`INSERT INTO sys_fields (id, entity_id, tenant_id, name, label, field_type, is_unique) VALUES (?, ?, ?, ?, ?, 'text', 1)`,
		uuid.NewString(), entID, tenantID, fieldName, fieldName); err != nil {
		t.Fatalf("seed field %s.%s: %v", entityName, fieldName, err)
	}
	return entID
}

// TestRM28SliceB_MigrationDemote_CoverageGuard is the migration 000051 up test (dispatch's
// "migration demote up/down" requirement): on a throwaway DB migrated only through v50, seed TWO
// pre-existing unique fields shaped exactly like Slice-A-era data (real uq_obj_* index via the
// kept AddUniqueColumn primitive):
//   - "rm28_covered": pivot fully backfilled (every live row has a matching pivot row) → the up
//     migration's coverage guard must demote it (uq_ gone, idx_ present).
//   - "rm28_uncovered": ONE live row deliberately left WITHOUT a pivot row → the coverage guard
//     must NOT demote it (stays uq_) and must log the skip with the correct missing_count.
//   - "rm28_shared": a THIRD, multi-tenant case that discriminates correct per-declaring-tenant
//     coverage scoping from a naive per-table scoping (see its own comment below) — only tenant A
//     declares the field unique and is fully covered; tenant B shares the table, never declared
//     it, and is left fully UNCOVERED on purpose. Must still demote.
func TestRM28SliceB_MigrationDemote_CoverageGuard(t *testing.T) {
	dbName := rm28bCreateThrowawayDB(t)
	// 49 migration STEPS lands at numeric version 50 (000018 was retired, so there are only 50
	// migration files numbered 1-51 with a gap at 18 — "up 50" would apply ALL 50 files,
	// including 000051). One step short stops right before the demote migration.
	rm28bMigrate(t, dbName, "up", "49")

	sdb, err := sqlx.Connect("mysql", o3aDSN(t, dbName))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	dyn := dynrepo.New(sdb)
	ctx := context.Background()

	const tenantCovered = "cccccccc-0000-0000-0000-000000000001"
	const tenantUncovered = "dddddddd-0000-0000-0000-000000000002"

	// --- "rm28_covered": pre-Slice-B unique field, pivot fully caught up. ----------------------
	if err := dyn.CreateTable(ctx, "rm28_covered", []ddl.FieldDef{{Name: "name", FieldType: "text"}}); err != nil {
		t.Fatalf("create rm28_covered: %v", err)
	}
	if err := dyn.AddUniqueColumn(ctx, "rm28_covered", "email"); err != nil {
		t.Fatalf("simulate pre-Slice-B uq_ index on rm28_covered.email: %v", err)
	}
	rm28bSeedField(t, sdb, tenantCovered, "rm28_covered", "email")
	if _, err := dyn.InsertRecord(ctx, "rm28_covered", tenantCovered, "u1", map[string]any{"email": "a@x.com"}, nil); err != nil {
		t.Fatalf("insert covered row1: %v", err)
	}
	if _, err := dyn.InsertRecord(ctx, "rm28_covered", tenantCovered, "u1", map[string]any{"email": "b@x.com"}, nil); err != nil {
		t.Fatalf("insert covered row2: %v", err)
	}
	bf, err := dyn.BackfillUniqueField(ctx, "rm28_covered", tenantCovered, "email")
	if err != nil {
		t.Fatalf("backfill rm28_covered: %v", err)
	}
	if bf.Inserted != 2 || len(bf.Conflicts) != 0 {
		t.Fatalf("backfill rm28_covered = %+v; want Inserted=2, no conflicts", bf)
	}

	// --- "rm28_uncovered": pre-Slice-B unique field, ONE live row missing a pivot row. ----------
	if err := dyn.CreateTable(ctx, "rm28_uncovered", []ddl.FieldDef{{Name: "name", FieldType: "text"}}); err != nil {
		t.Fatalf("create rm28_uncovered: %v", err)
	}
	if err := dyn.AddUniqueColumn(ctx, "rm28_uncovered", "email"); err != nil {
		t.Fatalf("simulate pre-Slice-B uq_ index on rm28_uncovered.email: %v", err)
	}
	rm28bSeedField(t, sdb, tenantUncovered, "rm28_uncovered", "email")
	if _, err := dyn.InsertRecord(ctx, "rm28_uncovered", tenantUncovered, "u1", map[string]any{"email": "c@x.com"}, nil); err != nil {
		t.Fatalf("insert uncovered row1: %v", err)
	}
	if _, err := dyn.InsertRecord(ctx, "rm28_uncovered", tenantUncovered, "u1", map[string]any{"email": "d@x.com"}, nil); err != nil {
		t.Fatalf("insert uncovered row2: %v", err)
	}
	// Deliberately do NOT backfill — both live rows have zero pivot coverage.

	// --- "rm28_shared": THE SCOPING DISCRIMINATOR — a field on ONE physical table shared by two
	// tenants, where ONLY tenant A declares it unique (is_unique=1 in ITS sys_fields row). Tenant
	// A's rows are fully backfilled; tenant B never declared the field unique (no sys_fields row
	// at all for B) and holds two LIVE rows with ZERO pivot coverage. The correct coverage guard
	// scopes to DECLARING tenants only (o.tenant_id IN <tenants with is_unique=1 for this field>
	// — migrations/000051_demote_obj_unique_index.up.sql), so tenant B's uncovered rows must be
	// irrelevant to the decision and the field must still be demoted. The "rm28_covered" and
	// "rm28_uncovered" cases above are each single-tenant, so a regression to a NAIVE scoping
	// ("any live row on the shared table missing a pivot row, from ANY tenant") would pass both of
	// them too — only this multi-tenant case can tell the two implementations apart.
	const tenantSharedDeclaring = "aaaaaaaa-0000-0000-0000-000000000005"
	const tenantSharedSilent = "bbbbbbbb-0000-0000-0000-000000000006"
	if err := dyn.CreateTable(ctx, "rm28_shared", []ddl.FieldDef{{Name: "name", FieldType: "text"}}); err != nil {
		t.Fatalf("create rm28_shared: %v", err)
	}
	if err := dyn.AddUniqueColumn(ctx, "rm28_shared", "email"); err != nil {
		t.Fatalf("simulate pre-Slice-B uq_ index on rm28_shared.email: %v", err)
	}
	rm28bSeedField(t, sdb, tenantSharedDeclaring, "rm28_shared", "email") // ONLY tenant A declares.
	if _, err := dyn.InsertRecord(ctx, "rm28_shared", tenantSharedDeclaring, "u1", map[string]any{"email": "shared-a@x.com"}, nil); err != nil {
		t.Fatalf("insert shared declaring row: %v", err)
	}
	if bf, err := dyn.BackfillUniqueField(ctx, "rm28_shared", tenantSharedDeclaring, "email"); err != nil || bf.Inserted != 1 {
		t.Fatalf("backfill rm28_shared (declaring tenant): bf=%+v err=%v", bf, err)
	}
	// Tenant B never declared "email" unique on this entity (no sys_fields row for B at all) —
	// its values are irrelevant to the coverage decision, and it is left with ZERO pivot rows on
	// purpose (matches production: RecordService only threads UniqueFieldSpec when the CALLER's
	// own sys_fields says is_unique — a non-declaring tenant's writes never touch the pivot).
	if _, err := dyn.InsertRecord(ctx, "rm28_shared", tenantSharedSilent, "u1", map[string]any{"email": "shared-b1@x.com"}, nil); err != nil {
		t.Fatalf("insert shared silent row1: %v", err)
	}
	if _, err := dyn.InsertRecord(ctx, "rm28_shared", tenantSharedSilent, "u1", map[string]any{"email": "shared-b2@x.com"}, nil); err != nil {
		t.Fatalf("insert shared silent row2: %v", err)
	}

	// --- Step the demote migration forward exactly one version (000051). -----------------------
	rm28bMigrate(t, dbName, "up", "1")

	// "rm28_covered.email" must be demoted: uq_ gone, idx_ present and non-unique.
	if exists, _ := rm28bIndexUnique(t, sdb, "obj_rm28_covered", "uq_obj_rm28_covered_email"); exists {
		t.Errorf("uq_obj_rm28_covered_email still exists — covered field should have been demoted")
	}
	if exists, unique := rm28bIndexUnique(t, sdb, "obj_rm28_covered", "idx_obj_rm28_covered_email"); !exists || unique {
		t.Errorf("idx_obj_rm28_covered_email exists=%v unique=%v; want exists=true unique=false", exists, unique)
	}

	// "rm28_uncovered.email" must NOT be demoted: uq_ still present, untouched.
	if exists, unique := rm28bIndexUnique(t, sdb, "obj_rm28_uncovered", "uq_obj_rm28_uncovered_email"); !exists || !unique {
		t.Errorf("uq_obj_rm28_uncovered_email exists=%v unique=%v; want exists=true unique=true (coverage guard must skip an uncovered field)", exists, unique)
	}
	if exists, _ := rm28bIndexUnique(t, sdb, "obj_rm28_uncovered", "idx_obj_rm28_uncovered_email"); exists {
		t.Errorf("idx_obj_rm28_uncovered_email exists — uncovered field must not be demoted at all")
	}

	// The log must record both decisions.
	var demotedCount int
	if err := sdb.Get(&demotedCount, "SELECT COUNT(*) FROM sys_unique_demote_log WHERE table_name = 'obj_rm28_covered' AND field_name = 'email' AND action = 'demoted'"); err != nil {
		t.Fatalf("query demote log (covered): %v", err)
	}
	if demotedCount != 1 {
		t.Errorf("sys_unique_demote_log 'demoted' rows for rm28_covered.email = %d; want 1", demotedCount)
	}
	var skippedCount, missing int
	if err := sdb.Get(&skippedCount, "SELECT COUNT(*) FROM sys_unique_demote_log WHERE table_name = 'obj_rm28_uncovered' AND field_name = 'email' AND action = 'skipped_uncovered'"); err != nil {
		t.Fatalf("query demote log (uncovered): %v", err)
	}
	if skippedCount != 1 {
		t.Errorf("sys_unique_demote_log 'skipped_uncovered' rows for rm28_uncovered.email = %d; want 1", skippedCount)
	}
	if err := sdb.Get(&missing, "SELECT missing_count FROM sys_unique_demote_log WHERE table_name = 'obj_rm28_uncovered' AND field_name = 'email' AND action = 'skipped_uncovered' LIMIT 1"); err != nil {
		t.Fatalf("query missing_count: %v", err)
	}
	if missing != 2 {
		t.Errorf("skipped_uncovered missing_count = %d; want 2 (both live rows lack a pivot row)", missing)
	}

	// "rm28_shared.email" must be demoted DESPITE tenant B's two fully-uncovered live rows: they
	// sit outside the coverage set entirely (B never declared the field unique). Proves the guard
	// scopes coverage per DECLARING tenant, not "any live row on the shared physical table".
	if exists, _ := rm28bIndexUnique(t, sdb, "obj_rm28_shared", "uq_obj_rm28_shared_email"); exists {
		t.Errorf("uq_obj_rm28_shared_email still exists — a non-declaring co-tenant's uncovered rows must NOT block demote of the declaring tenant's fully-covered field")
	}
	if exists, unique := rm28bIndexUnique(t, sdb, "obj_rm28_shared", "idx_obj_rm28_shared_email"); !exists || unique {
		t.Errorf("idx_obj_rm28_shared_email exists=%v unique=%v; want exists=true unique=false", exists, unique)
	}
	var sharedDemoted int
	if err := sdb.Get(&sharedDemoted, "SELECT COUNT(*) FROM sys_unique_demote_log WHERE table_name = 'obj_rm28_shared' AND field_name = 'email' AND action = 'demoted'"); err != nil {
		t.Fatalf("query demote log (shared): %v", err)
	}
	if sharedDemoted != 1 {
		t.Errorf("sys_unique_demote_log 'demoted' rows for rm28_shared.email = %d; want 1 (coverage scoping must be per-declaring-tenant, not per-table)", sharedDemoted)
	}

	// Re-run idempotency is by construction, not re-exercised here via the migrate CLI (`up`
	// with no further migration file to apply is itself an error, unrelated to this migration's
	// own idempotency): the demote candidate cursor is information_schema-driven off uq_
	// indexes, so a field already demoted to idx_ simply never surfaces again on any future run.
}

// TestRM28SliceB_MigrationRevert_GuardedPerField is the migration 000051 down test: a cleanly
// demoted field (full pivot coverage, no live data conflicts) must revert to a real uq_ index; a
// field whose LIVE data would violate a single shared UNIQUE(tenant_id, value) index post-demote
// (the exact decoupling scenario RM-28 enables — a non-declaring tenant free to hold duplicates)
// must be SKIPPED with a 1062 guarded per-field, logged, and the loop must continue to revert
// every other eligible field in the SAME down run (never abort the whole batch on one conflict).
func TestRM28SliceB_MigrationRevert_GuardedPerField(t *testing.T) {
	dbName := rm28bCreateThrowawayDB(t)
	// Pin to EXACTLY v51 — not head. This test's teardown step is "revert migration 000051",
	// and `migrate down 1` reverts whatever HEAD happens to be: written when head WAS 51, it
	// silently started reverting 000052, then 53… as each later migration landed, and the
	// assertions have been red ever since (unnoticed — integration-only, CI off since
	// 2026-07-08). The sibling demote test already pins with "up 49"/"up 1"; this one forgot.
	rm28bMigrate(t, dbName, "goto", "51")

	sdb, err := sqlx.Connect("mysql", o3aDSN(t, dbName))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	dyn := dynrepo.New(sdb)
	ctx := context.Background()

	const tenantClean = "eeeeeeee-0000-0000-0000-000000000003"
	const tenantConflict = "ffffffff-0000-0000-0000-000000000004"

	// --- "rm28_revert_clean": normal post-Slice-B unique field (CreateTable's own unique branch
	// already materializes the idx_ non-unique shape directly), no conflicting data. ------------
	if err := dyn.CreateTable(ctx, "rm28_revert_clean", []ddl.FieldDef{{Name: "email", FieldType: "text", IsUnique: true}}); err != nil {
		t.Fatalf("create rm28_revert_clean: %v", err)
	}
	rm28bSeedField(t, sdb, tenantClean, "rm28_revert_clean", "email")
	uf := []dynrepo.UniqueFieldSpec{{FieldName: "email", GenCol: "email"}}
	if _, err := dyn.InsertRecord(ctx, "rm28_revert_clean", tenantClean, "u1", map[string]any{"email": "clean@x.com"}, uf); err != nil {
		t.Fatalf("insert clean row: %v", err)
	}

	// --- "rm28_revert_conflict": a tenant that never declared the field unique holds TWO live
	// rows with the SAME value under the shared column — legal post-Slice-B (decoupled), but
	// re-imposing a single shared UNIQUE(tenant_id,value) index on DOWN cannot represent it.
	if err := dyn.CreateTable(ctx, "rm28_revert_conflict", []ddl.FieldDef{{Name: "email", FieldType: "text", IsUnique: true}}); err != nil {
		t.Fatalf("create rm28_revert_conflict: %v", err)
	}
	rm28bSeedField(t, sdb, tenantClean, "rm28_revert_conflict", "email")
	// tenantConflict shares the physical table but is seeded RAW (bypassing the pivot-maintaining
	// mutator) with two live rows of the SAME value — simulating a tenant who never declared the
	// field unique in ITS OWN metadata still ending up with duplicate data under the shared JSON
	// key (the down migration's candidate set is is_unique=1-driven off the field NAME globally,
	// not per-declaring-tenant, so it still attempts this table — the 1062 must be caught here).
	if _, err := sdb.Exec("INSERT INTO `obj_rm28_revert_conflict` (id, tenant_id, created_by, data) VALUES (?, ?, ?, JSON_OBJECT('email','dup@x.com'))",
		uuid.NewString(), tenantConflict, "u1"); err != nil {
		t.Fatalf("seed conflict row1: %v", err)
	}
	if _, err := sdb.Exec("INSERT INTO `obj_rm28_revert_conflict` (id, tenant_id, created_by, data) VALUES (?, ?, ?, JSON_OBJECT('email','dup@x.com'))",
		uuid.NewString(), tenantConflict, "u1"); err != nil {
		t.Fatalf("seed conflict row2: %v", err)
	}

	// --- Revert (000051 down). --------------------------------------------------------------
	rm28bMigrate(t, dbName, "down", "1")

	// Clean field reverted to a real UNIQUE index.
	if exists, unique := rm28bIndexUnique(t, sdb, "obj_rm28_revert_clean", "uq_obj_rm28_revert_clean_email"); !exists || !unique {
		t.Errorf("uq_obj_rm28_revert_clean_email exists=%v unique=%v; want exists=true unique=true (clean field must revert)", exists, unique)
	}
	var revertedCount int
	if err := sdb.Get(&revertedCount, "SELECT COUNT(*) FROM sys_unique_demote_log WHERE table_name = 'obj_rm28_revert_clean' AND field_name = 'email' AND action = 'reverted'"); err != nil {
		t.Fatalf("query revert log (clean): %v", err)
	}
	if revertedCount != 1 {
		t.Errorf("sys_unique_demote_log 'reverted' rows for rm28_revert_clean.email = %d; want 1", revertedCount)
	}

	// Conflicting field STAYS non-unique (guarded 1062 — never partially applied) and is logged.
	if exists, unique := rm28bIndexUnique(t, sdb, "obj_rm28_revert_conflict", "idx_obj_rm28_revert_conflict_email"); !exists || unique {
		t.Errorf("idx_obj_rm28_revert_conflict_email exists=%v unique=%v; want exists=true unique=false (revert must be guarded off on 1062)", exists, unique)
	}
	if exists, _ := rm28bIndexUnique(t, sdb, "obj_rm28_revert_conflict", "uq_obj_rm28_revert_conflict_email"); exists {
		t.Errorf("uq_obj_rm28_revert_conflict_email exists — a guarded 1062 must never leave a dirty partial UNIQUE index")
	}
	var failedCount int
	if err := sdb.Get(&failedCount, "SELECT COUNT(*) FROM sys_unique_demote_log WHERE table_name = 'obj_rm28_revert_conflict' AND field_name = 'email' AND action = 'revert_failed_duplicate'"); err != nil {
		t.Fatalf("query revert log (conflict): %v", err)
	}
	if failedCount != 1 {
		t.Errorf("sys_unique_demote_log 'revert_failed_duplicate' rows for rm28_revert_conflict.email = %d; want 1 (the OTHER field's successful revert proves the loop continued past this failure)", failedCount)
	}
}
