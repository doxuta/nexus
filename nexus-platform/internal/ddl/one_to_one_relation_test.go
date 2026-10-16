package ddl

import (
	"strings"
	"testing"
)

// ENG-REL2 #5 (1-1 relationship, FK-on-child + tenant-scoped UNIQUE). A 1-1 `relation`
// field differs from an ordinary 1-N relation field in two ways: (1) the generated FK
// column is CASE-guarded against deleted_at (W5-E1-style, so a soft-deleted row's slot is
// released instead of squatted forever), and (2) the index is a tenant-scoped UNIQUE KEY
// (not a plain index) — enforcing at most one LIVE child row per (tenant, parent).

// TestBuildCreateTable_OneToOneRelationField pins the CASE-guarded, UNIQUE shape emitted
// at entity-create time for a relation field with cardinality 1-1.
func TestBuildCreateTable_OneToOneRelationField(t *testing.T) {
	sql, err := BuildCreateTable("profiles", []FieldDef{
		{Name: "name", FieldType: "text"},
		{Name: "user", FieldType: "relation", RelatedEntity: "users", OneToOne: true},
	})
	if err != nil {
		t.Fatalf("BuildCreateTable returned error: %v", err)
	}
	mustContain := []string{
		"`user_id` CHAR(36) GENERATED ALWAYS AS (CASE WHEN `deleted_at` IS NULL THEN JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.user')) ELSE NULL END) STORED",
		"UNIQUE KEY `uq_obj_profiles_user_id` (`tenant_id`, `user_id`)",
	}
	for _, frag := range mustContain {
		if !strings.Contains(sql, frag) {
			t.Errorf("CREATE TABLE missing %q\n--- got ---\n%s", frag, sql)
		}
	}
	// Must NOT also get the plain (non-unique) 1-N relation index.
	if strings.Contains(sql, "INDEX `idx_obj_profiles_user_id`") {
		t.Errorf("1-1 relation field must not ALSO get the plain 1-N index\n--- got ---\n%s", sql)
	}
}

// TestBuildCreateTable_RelationField_OneToOneFalseUnchanged re-pins that a plain 1-N
// relation field (OneToOne left at its zero value, false) produces the EXACT same DDL as
// before this slice — byte-identical, no CASE guard, no UNIQUE.
func TestBuildCreateTable_RelationField_OneToOneFalseUnchanged(t *testing.T) {
	sql, err := BuildCreateTable("tickets", []FieldDef{
		{Name: "title", FieldType: "text"},
		{Name: "account", FieldType: "relation", RelatedEntity: "accounts"},
	})
	if err != nil {
		t.Fatalf("BuildCreateTable returned error: %v", err)
	}
	for _, frag := range []string{
		"`account_id` CHAR(36) GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.account'))) STORED",
		"INDEX `idx_obj_tickets_account_id` (`tenant_id`, `account_id`)",
	} {
		if !strings.Contains(sql, frag) {
			t.Errorf("CREATE TABLE missing %q\n--- got ---\n%s", frag, sql)
		}
	}
	if strings.Contains(sql, "CASE WHEN `deleted_at`") {
		t.Errorf("plain 1-N relation column must not be CASE-guarded\n--- got ---\n%s", sql)
	}
	if strings.Contains(sql, "UNIQUE KEY `uq_obj_tickets_account_id`") {
		t.Errorf("plain 1-N relation must not get a UNIQUE key\n--- got ---\n%s", sql)
	}
}

// TestBuildAddOneToOneRelationColumn pins the ALTER used when a 1-1 `relation` field is
// added to an already-registered entity: CASE-guarded FK column + tenant-scoped UNIQUE key
// in ONE statement (mirrors BuildAddRelationColumn's shape but guarded+unique).
func TestBuildAddOneToOneRelationColumn(t *testing.T) {
	sql, err := BuildAddOneToOneRelationColumn("profiles", "user")
	if err != nil {
		t.Fatalf("BuildAddOneToOneRelationColumn returned error: %v", err)
	}
	for _, frag := range []string{
		"ALTER TABLE `obj_profiles`",
		"ADD COLUMN `user_id` CHAR(36) GENERATED ALWAYS AS (CASE WHEN `deleted_at` IS NULL THEN JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.user')) ELSE NULL END) STORED",
		"ADD UNIQUE KEY `uq_obj_profiles_user_id` (`tenant_id`, `user_id`)",
	} {
		if !strings.Contains(sql, frag) {
			t.Errorf("ALTER missing %q\n--- got ---\n%s", frag, sql)
		}
	}
}

func TestBuildAddOneToOneRelationColumn_RejectsBadNames(t *testing.T) {
	if _, err := BuildAddOneToOneRelationColumn("select", "user"); err == nil {
		t.Error("reserved entity name should error")
	}
	if _, err := BuildAddOneToOneRelationColumn("profiles", "user; drop"); err == nil {
		t.Error("injection field name should error")
	}
}

// TestBuildDropOneToOneRelationColumn pins the compensation ALTER that reverses
// BuildAddOneToOneRelationColumn: drop the UNIQUE index then the generated column.
func TestBuildDropOneToOneRelationColumn(t *testing.T) {
	sql, err := BuildDropOneToOneRelationColumn("profiles", "user")
	if err != nil {
		t.Fatalf("BuildDropOneToOneRelationColumn returned error: %v", err)
	}
	for _, frag := range []string{
		"ALTER TABLE `obj_profiles`",
		"DROP INDEX `uq_obj_profiles_user_id`",
		"DROP COLUMN `user_id`",
	} {
		if !strings.Contains(sql, frag) {
			t.Errorf("ALTER missing %q\n--- got ---\n%s", frag, sql)
		}
	}
}

func TestBuildDropOneToOneRelationColumn_RejectsBadNames(t *testing.T) {
	if _, err := BuildDropOneToOneRelationColumn("select", "user"); err == nil {
		t.Error("reserved entity name should error")
	}
	if _, err := BuildDropOneToOneRelationColumn("profiles", "user; drop"); err == nil {
		t.Error("injection field name should error")
	}
}
