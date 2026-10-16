package ddl

import (
	"strings"
	"testing"
)

// USER-FIELD slice: a `user` field (points at the FIXED `users` system table, never a
// sys_entity) reuses the plain 1-N `relation` DDL shape as-is — a CHAR(36) `<f>_id` STORED
// generated column + a NON-unique tenant-scoped index. These tests pin that a `user` field
// gets the byte-identical treatment `relation` already gets (relation_test.go), proving the
// reuse rather than re-deriving the shape.

// TestBuildCreateTable_UserField asserts a `user` field declared at create time materializes
// the same FK-shaped column + non-unique tenant-scoped index inline in CREATE TABLE that a
// plain 1-N `relation` field gets (relation_test.go's TestBuildCreateTable_RelationField).
func TestBuildCreateTable_UserField(t *testing.T) {
	sql, err := BuildCreateTable("tickets", []FieldDef{
		{Name: "subject", FieldType: "text"},
		{Name: "assigned_to", FieldType: "user"},
	})
	if err != nil {
		t.Fatalf("BuildCreateTable returned error: %v", err)
	}
	for _, frag := range []string{
		"`assigned_to_id` CHAR(36) GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.assigned_to'))) STORED",
		"INDEX `idx_obj_tickets_assigned_to_id` (`tenant_id`, `assigned_to_id`)",
	} {
		if !strings.Contains(sql, frag) {
			t.Errorf("CREATE TABLE missing %q\n--- got ---\n%s", frag, sql)
		}
	}
	// A user field's index must NOT be unique — many records may be assigned to one user.
	if strings.Contains(sql, "UNIQUE KEY `uq_obj_tickets_assigned_to") {
		t.Errorf("user field must not produce a UNIQUE index\n--- got ---\n%s", sql)
	}
	// Plain (no CASE guard) — the 1-1 exclusivity shape is not offered for `user` fields.
	if strings.Contains(sql, "CASE WHEN `deleted_at`") {
		t.Errorf("user field must not be CASE-guarded (no 1-1 cardinality for user fields)\n--- got ---\n%s", sql)
	}
}

// TestBuildAddRelationColumn_ServesUserField proves the SAME ALTER builder relation's
// AddField path uses (BuildAddRelationColumn) is reused byte-for-byte for a `user` field —
// the services layer calls it directly (no separate BuildAddUserColumn exists, by design).
func TestBuildAddRelationColumn_ServesUserField(t *testing.T) {
	sql, err := BuildAddRelationColumn("tickets", "assigned_to")
	if err != nil {
		t.Fatalf("BuildAddRelationColumn returned error: %v", err)
	}
	for _, frag := range []string{
		"ALTER TABLE `obj_tickets`",
		"ADD COLUMN `assigned_to_id` CHAR(36) GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.assigned_to'))) STORED",
		"ADD INDEX `idx_obj_tickets_assigned_to_id` (`tenant_id`, `assigned_to_id`)",
	} {
		if !strings.Contains(sql, frag) {
			t.Errorf("ALTER missing %q\n--- got ---\n%s", frag, sql)
		}
	}
}
