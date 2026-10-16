//go:build integration

// Migration 000012 (sys_fields unique (tenant_id, entity_id, name)) up/down + orphan-guard
// acceptance. Proves:
//
//  1. ORPHAN GUARD fail-loud: on a DB that already holds a duplicate (tenant_id, entity_id,
//     name) group (incl. soft-deleted rows), the 000012 up migration ABORTs with the readable
//     SIGNAL message and NEVER silently drops/dedups data.
//  2. SOFT-DELETED field migrates OK: a DB whose sys_fields has a soft-deleted row (no dup)
//     applies 000012 cleanly — the unique key spans soft-deleted rows without spurious abort.
//  3. up → down → up is clean: after applying 012, the down drops the key, and a re-up
//     re-creates it (idempotent across the cycle).
//
// Run with: go test -tags=integration ./internal/handlers/ -race -run TestMig000012
// Requires a reachable MySQL 8. Uses the throwaway-DB helpers from the RM-25/27 test file.
package handlers

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

const mig012Up = "000012_sys_fields_unique_tenant_entity_name.up.sql"
const mig012Down = "000012_sys_fields_unique_tenant_entity_name.down.sql"

// applyMigrationsBelow applies every migrations/*.up.sql whose filename sorts strictly BEFORE
// `stopExclusive` (so we can land a DB at v11 and then exercise v12 by hand). Each file is a
// single multi-statement Exec so stored-procedure orphan guards run intact. (migrationsDir(t)
// is defined in a4_rbac_harness_integration_test.go.)
func applyMigrationsBelow(t *testing.T, db *sqlx.DB, stopExclusive string) {
	t.Helper()
	entries, err := os.ReadDir(migrationsDir(t))
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var ups []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			ups = append(ups, e.Name())
		}
	}
	sort.Strings(ups)
	for _, name := range ups {
		if name >= stopExclusive {
			break
		}
		b, err := os.ReadFile(filepath.Join(migrationsDir(t), name))
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("apply migration %s: %v", name, err)
		}
	}
}

// execMigrationFile runs one named migration file (up or down) and returns the error verbatim
// so the test can assert on the orphan-guard SIGNAL message.
func execMigrationFile(t *testing.T, db *sqlx.DB, name string) error {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(migrationsDir(t), name))
	if err != nil {
		t.Fatalf("read migration %s: %v", name, err)
	}
	_, err = db.Exec(string(b))
	return err
}

// uq012Cols returns the number of columns in the uq_sys_fields_tenant_entity_name index (0 if
// the index is absent, 3 when present).
func uq012Cols(t *testing.T, db *sqlx.DB) int {
	t.Helper()
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='sys_fields' AND INDEX_NAME='uq_sys_fields_tenant_entity_name'`); err != nil {
		t.Fatalf("count uq index cols: %v", err)
	}
	return n
}

// setupDBAtV11 provisions a throwaway DB and applies migrations 000001..000011 (everything
// below 000012), leaving sys_fields WITHOUT the v12 unique key so the test can drive 012 by hand.
func setupDBAtV11(t *testing.T) *sqlx.DB {
	t.Helper()
	db, _ := setupMigratedDBNoOp(t) // create DB only
	applyMigrationsBelow(t, db, mig012Up)
	if uq012Cols(t, db) != 0 {
		t.Fatalf("precondition: uq_sys_fields_tenant_entity_name must be absent at v11")
	}
	return db
}

// setupMigratedDBNoOp creates a throwaway DB (with cleanup) but applies NO migrations, so the
// caller controls exactly which migrations land. Mirrors setupMigratedDB's provisioning half.
func setupMigratedDBNoOp(t *testing.T) (*sqlx.DB, string) {
	t.Helper()
	server, err := sqlx.Connect("mysql", baseDSNNoDB(t))
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping", err)
	}
	defer server.Close()
	dbName := "nexus_mig012_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := server.Exec("CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("create throwaway db: %v", err)
	}
	t.Cleanup(func() {
		if s2, e := sqlx.Connect("mysql", baseDSNNoDB(t)); e == nil {
			_, _ = s2.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
			_ = s2.Close()
		}
	})
	db, err := sqlx.Connect("mysql", throwawayDSN(t, dbName))
	if err != nil {
		t.Fatalf("connect throwaway db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, dbName
}

// seedEntityWithField inserts a sys_entities row and a sys_fields row directly (bypassing the
// service) so a migration test can craft exact pre-states (incl. soft-deleted / duplicate rows).
func seedEntityWithField(t *testing.T, db *sqlx.DB, tenant, entityID, entityName, fieldID, fieldName string, softDeleted bool) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO sys_entities (id, tenant_id, name, label, table_name)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE name=name`,
		entityID, tenant, entityName, entityName, "obj_"+entityName); err != nil {
		// Entity may already exist for this (tenant,name); ignore the unique clash.
		if !strings.Contains(err.Error(), "1062") {
			t.Fatalf("seed entity: %v", err)
		}
	}
	del := "NULL"
	if softDeleted {
		del = "NOW()"
	}
	if _, err := db.Exec(`INSERT INTO sys_fields (id, entity_id, tenant_id, name, label, field_type, deleted_at)
		VALUES (?, ?, ?, ?, ?, 'text', `+del+`)`,
		fieldID, entityID, tenant, fieldName, fieldName); err != nil {
		t.Fatalf("seed field: %v", err)
	}
}

// TestMig000012_OrphanGuard_FailsLoudOnDuplicate seeds a DUPLICATE (tenant_id, entity_id, name)
// pair (one active + one soft-deleted, same name) at v11, then runs 000012 up and asserts it
// ABORTs with the readable orphan-guard SIGNAL — never silently dropping a row.
func TestMig000012_OrphanGuard_FailsLoudOnDuplicate(t *testing.T) {
	db := setupDBAtV11(t)

	tenant := uuid.NewString()
	entityID := uuid.NewString()
	// Two rows, SAME (tenant, entity, name) — one active, one soft-deleted: a duplicate group.
	seedEntityWithField(t, db, tenant, entityID, "dupent", uuid.NewString(), "code", false)
	seedEntityWithField(t, db, tenant, entityID, "dupent", uuid.NewString(), "code", true)

	err := execMigrationFile(t, db, mig012Up)
	if err == nil {
		t.Fatalf("000012 up MUST fail loud on a duplicate (tenant_id, entity_id, name) group, but succeeded")
	}
	if !strings.Contains(err.Error(), "RM-25 ABORT") {
		t.Fatalf("000012 up failed but NOT with the readable orphan-guard SIGNAL; got: %v", err)
	}
	// The unique key must NOT have been created (the ALTER never ran), and BOTH rows survive.
	if uq012Cols(t, db) != 0 {
		t.Errorf("uq index must be absent after a guarded abort")
	}
	var rows int
	if err := db.Get(&rows, `SELECT COUNT(*) FROM sys_fields WHERE tenant_id=? AND entity_id=? AND name='code'`, tenant, entityID); err != nil {
		t.Fatalf("count code rows: %v", err)
	}
	if rows != 2 {
		t.Errorf("orphan guard must NOT delete rows: have %d 'code' rows, want 2 (both preserved)", rows)
	}
}

// TestMig000012_SoftDeletedField_MigratesClean seeds a soft-deleted field (NO duplicate) at v11,
// then applies 000012 and asserts it lands cleanly — the unique key spans soft-deleted rows
// without a spurious abort (the soft-delete semantics are intentional, see migration comment).
func TestMig000012_SoftDeletedField_MigratesClean(t *testing.T) {
	db := setupDBAtV11(t)

	tenant := uuid.NewString()
	entityID := uuid.NewString()
	// One active field + one soft-deleted field of a DIFFERENT name: no duplicate group.
	seedEntityWithField(t, db, tenant, entityID, "softent", uuid.NewString(), "active_f", false)
	seedEntityWithField(t, db, tenant, entityID, "softent", uuid.NewString(), "gone_f", true)

	if err := execMigrationFile(t, db, mig012Up); err != nil {
		t.Fatalf("000012 up must apply cleanly with a soft-deleted (non-dup) field present; got: %v", err)
	}
	if uq012Cols(t, db) != 3 {
		t.Errorf("uq_sys_fields_tenant_entity_name must have 3 columns after a clean up; got %d", uq012Cols(t, db))
	}
	// Both rows survive untouched.
	var rows int
	if err := db.Get(&rows, `SELECT COUNT(*) FROM sys_fields WHERE tenant_id=? AND entity_id=?`, tenant, entityID); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 2 {
		t.Errorf("soft-deleted migration must preserve all rows: have %d, want 2", rows)
	}
}

// TestMig000012_UpDownUp_Clean applies 000012 on a clean v11 DB, drops it via the down, and
// re-applies the up — proving the cycle is idempotent (no leftover index, re-up succeeds).
func TestMig000012_UpDownUp_Clean(t *testing.T) {
	db := setupDBAtV11(t)

	if err := execMigrationFile(t, db, mig012Up); err != nil {
		t.Fatalf("first up: %v", err)
	}
	if uq012Cols(t, db) != 3 {
		t.Fatalf("after first up: uq index must have 3 cols, got %d", uq012Cols(t, db))
	}
	if err := execMigrationFile(t, db, mig012Down); err != nil {
		t.Fatalf("down: %v", err)
	}
	if uq012Cols(t, db) != 0 {
		t.Fatalf("after down: uq index must be absent, got %d cols", uq012Cols(t, db))
	}
	if err := execMigrationFile(t, db, mig012Up); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if uq012Cols(t, db) != 3 {
		t.Fatalf("after re-up: uq index must have 3 cols, got %d", uq012Cols(t, db))
	}
}
