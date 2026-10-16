package ddl

import (
	"strings"
	"testing"
)

// TestBuildAddRelationColumn pins the ALTER used when a 1-N `relation` field is added to an
// existing entity (M3-T1, FK-on-child). It mirrors the unique-field pattern but materializes a
// CHAR(36) FK column `<f>_id` projecting the JSON value, plus a NON-unique tenant-scoped index
// named `idx_obj_<entity>_<f>_id` (FK columns must be indexed; many child rows may share one
// parent id, so the index is NOT unique). The column name carries the `_id` suffix while the
// JSON key is the bare field name, matching the `reports_to` -> `reports_to_id` convention.
func TestBuildAddRelationColumn(t *testing.T) {
	sql, err := BuildAddRelationColumn("tickets", "account")
	if err != nil {
		t.Fatalf("BuildAddRelationColumn returned error: %v", err)
	}
	for _, frag := range []string{
		"ALTER TABLE `obj_tickets`",
		"ADD COLUMN `account_id` CHAR(36) GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.account'))) STORED",
		"ADD INDEX `idx_obj_tickets_account_id` (`tenant_id`, `account_id`)",
	} {
		if !strings.Contains(sql, frag) {
			t.Errorf("ALTER missing %q\n--- got ---\n%s", frag, sql)
		}
	}
	// A relation index must NOT be unique (1-N: many children -> one parent).
	if strings.Contains(sql, "UNIQUE") {
		t.Errorf("relation FK index must not be UNIQUE\n--- got ---\n%s", sql)
	}
}

func TestBuildAddRelationColumn_RejectsBadNames(t *testing.T) {
	if _, err := BuildAddRelationColumn("select", "account"); err == nil {
		t.Error("reserved entity name should error")
	}
	if _, err := BuildAddRelationColumn("tickets", "account; drop"); err == nil {
		t.Error("injection field name should error")
	}
}

// TestBuildDropRelationColumn pins the compensation ALTER that reverses
// BuildAddRelationColumn: drop the index then the FK column, restoring the pre-ALTER shape.
func TestBuildDropRelationColumn(t *testing.T) {
	sql, err := BuildDropRelationColumn("tickets", "account")
	if err != nil {
		t.Fatalf("BuildDropRelationColumn returned error: %v", err)
	}
	for _, frag := range []string{
		"ALTER TABLE `obj_tickets`",
		"DROP INDEX `idx_obj_tickets_account_id`",
		"DROP COLUMN `account_id`",
	} {
		if !strings.Contains(sql, frag) {
			t.Errorf("ALTER missing %q\n--- got ---\n%s", frag, sql)
		}
	}
}

func TestBuildDropRelationColumn_RejectsBadNames(t *testing.T) {
	if _, err := BuildDropRelationColumn("select", "account"); err == nil {
		t.Error("reserved entity name should error")
	}
	if _, err := BuildDropRelationColumn("tickets", "account; drop"); err == nil {
		t.Error("injection field name should error")
	}
}

// TestBuildCreateTable_RelationField asserts a relation field declared at create time
// materializes the same FK column + non-unique tenant-scoped index inline in CREATE TABLE.
func TestBuildCreateTable_RelationField(t *testing.T) {
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
	// The relation index must not be a UNIQUE KEY.
	if strings.Contains(sql, "UNIQUE KEY `uq_obj_tickets_account") {
		t.Errorf("relation field must not produce a UNIQUE index\n--- got ---\n%s", sql)
	}
}
