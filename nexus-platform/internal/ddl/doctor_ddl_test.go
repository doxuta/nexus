package ddl

// Tests for the DOCTOR-SCHEMA builders: the A2a guard MODIFYs must carry the EXACT column
// definition the Add* builders emit (one body — uniqueGenColDef/oneToOneRelGenColDef — so a
// repaired column is byte-identical to a freshly added one), and the A5 mandatory builders
// must re-issue the EXACT defs BuildCreateTable renders (MandatoryColumns/MandatoryIndexes
// are the single source both sides read).

import (
	"strings"
	"testing"
)

// extractColDef pulls the column definition out of an "ALTER TABLE `t` ADD COLUMN <def>, ..."
// or "ALTER TABLE `t` MODIFY COLUMN <def>;" statement.
func extractColDef(t *testing.T, stmt, verb string) string {
	t.Helper()
	_, after, ok := strings.Cut(stmt, verb+" COLUMN ")
	if !ok {
		t.Fatalf("statement %q has no %s COLUMN clause", stmt, verb)
	}
	if def, _, ok := strings.Cut(after, ", ADD "); ok {
		return def
	}
	return strings.TrimSuffix(after, ";")
}

func TestBuildGuardUniqueColumn_SameColDefAsAdd(t *testing.T) {
	add, err := BuildAddUniqueColumn("accounts", "email")
	if err != nil {
		t.Fatalf("BuildAddUniqueColumn: %v", err)
	}
	guard, err := BuildGuardUniqueColumn("accounts", "email")
	if err != nil {
		t.Fatalf("BuildGuardUniqueColumn: %v", err)
	}
	addDef := extractColDef(t, add, "ADD")
	guardDef := extractColDef(t, guard, "MODIFY")
	if addDef != guardDef {
		t.Fatalf("guard MODIFY coldef != add coldef (one-body contract broken):\nadd   = %s\nguard = %s", addDef, guardDef)
	}
	// The 000015 canonical shape: conditional-NULL guard + binary collation, STORED.
	for _, frag := range []string{
		"ALTER TABLE `obj_accounts` MODIFY COLUMN `email`",
		"CASE WHEN `deleted_at` IS NULL",
		"COLLATE utf8mb4_bin",
		") ELSE NULL END) STORED",
	} {
		if !strings.Contains(guard, frag) {
			t.Fatalf("guard statement missing %q:\n%s", frag, guard)
		}
	}
}

func TestBuildGuardOneToOneRelationColumn_SameColDefAsAdd(t *testing.T) {
	add, err := BuildAddOneToOneRelationColumn("contacts", "partner")
	if err != nil {
		t.Fatalf("BuildAddOneToOneRelationColumn: %v", err)
	}
	guard, err := BuildGuardOneToOneRelationColumn("contacts", "partner")
	if err != nil {
		t.Fatalf("BuildGuardOneToOneRelationColumn: %v", err)
	}
	addDef := extractColDef(t, add, "ADD")
	guardDef := extractColDef(t, guard, "MODIFY")
	if addDef != guardDef {
		t.Fatalf("guard MODIFY coldef != add coldef:\nadd   = %s\nguard = %s", addDef, guardDef)
	}
	if !strings.Contains(guard, "`partner_id` CHAR(36)") || !strings.Contains(guard, "CASE WHEN `deleted_at` IS NULL") {
		t.Fatalf("guard statement shape sai:\n%s", guard)
	}
}

func TestGuardBuilders_RejectBadNames(t *testing.T) {
	if _, err := BuildGuardUniqueColumn("select", "email"); err == nil {
		t.Error("BuildGuardUniqueColumn(entity=reserved) should error")
	}
	if _, err := BuildGuardOneToOneRelationColumn("accounts", "x; drop"); err == nil {
		t.Error("BuildGuardOneToOneRelationColumn(field=injection) should error")
	}
}

func TestBuildAddMandatoryColumn(t *testing.T) {
	stmt, err := BuildAddMandatoryColumn("accounts", "version")
	if err != nil {
		t.Fatalf("BuildAddMandatoryColumn: %v", err)
	}
	// Must be the EXACT def BuildCreateTable renders (000013 semantics for old tables).
	want := "ALTER TABLE `obj_accounts` ADD COLUMN `version` INT NOT NULL DEFAULT 0;"
	if stmt != want {
		t.Fatalf("stmt = %q; want %q", stmt, want)
	}
	if _, err := BuildAddMandatoryColumn("accounts", "not_a_system_col"); err == nil {
		t.Error("non-mandatory column name should error before any DDL")
	}
	if _, err := BuildAddMandatoryColumn("select", "version"); err == nil {
		t.Error("reserved entity name should error")
	}
}

func TestBuildMandatoryIndexDDL(t *testing.T) {
	add, err := BuildMandatoryIndexDDL("accounts", "idx_updated_at", false)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	wantAdd := "ALTER TABLE `obj_accounts` ADD INDEX `idx_updated_at` (`tenant_id`, `deleted_at`, `updated_at`, `id`);"
	if add != wantAdd {
		t.Fatalf("add = %q; want %q", add, wantAdd)
	}
	rebuild, err := BuildMandatoryIndexDDL("accounts", "idx_created_at", true)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	wantRebuild := "ALTER TABLE `obj_accounts` DROP INDEX `idx_created_at`, ADD INDEX `idx_created_at` (`tenant_id`, `deleted_at`, `created_at`, `id`);"
	if rebuild != wantRebuild {
		t.Fatalf("rebuild = %q; want %q (RM-47 composite)", rebuild, wantRebuild)
	}
	if _, err := BuildMandatoryIndexDDL("accounts", "PRIMARY", true); err == nil {
		t.Error("rebuild PRIMARY phải bị từ chối (DROP PK trên bảng dùng chung là destructive)")
	}
	if pk, err := BuildMandatoryIndexDDL("accounts", "PRIMARY", false); err != nil || !strings.Contains(pk, "ADD PRIMARY KEY (`id`)") {
		t.Errorf("add PRIMARY = %q (err %v); want ADD PRIMARY KEY", pk, err)
	}
	if _, err := BuildMandatoryIndexDDL("accounts", "idx_nope", false); err == nil {
		t.Error("non-mandatory index name should error")
	}
}

// TestMandatoryDefsRenderIntoCreateTable pins the single-source contract: every structured
// MandatoryColumns/MandatoryIndexes def appears VERBATIM in BuildCreateTable's output, so the
// Doctor A5 diff (structured) and the physical table (rendered) can never disagree.
func TestMandatoryDefsRenderIntoCreateTable(t *testing.T) {
	sql, err := BuildCreateTable("things", nil)
	if err != nil {
		t.Fatalf("BuildCreateTable: %v", err)
	}
	for _, c := range MandatoryColumns {
		if !strings.Contains(sql, c.Def) {
			t.Errorf("CREATE TABLE missing mandatory column def %q", c.Def)
		}
	}
	for _, ix := range MandatoryIndexes {
		if !strings.Contains(sql, ix.Def) {
			t.Errorf("CREATE TABLE missing mandatory index def %q", ix.Def)
		}
		if IsReservedColumn(ix.Name) {
			t.Errorf("index name %q collides with a reserved column name", ix.Name)
		}
	}
}
