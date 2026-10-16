package ddl

import (
	"strings"
	"testing"
)

// TestBuildCreateTable pins the shape of the obj_* table the engine generates:
// every mandatory column from rules/database.md, a JSON `data` column for dynamic
// values, and — only for unique fields — a STORED generated column + unique index
// scoped by tenant_id. Ordinary fields must NOT get their own column (they live in data).
func TestBuildCreateTable(t *testing.T) {
	sql, err := BuildCreateTable("courses", []FieldDef{
		{Name: "title", FieldType: "text"},
		{Name: "code", FieldType: "text", IsUnique: true},
	})
	if err != nil {
		t.Fatalf("BuildCreateTable returned error: %v", err)
	}

	mustContain := []string{
		"CREATE TABLE IF NOT EXISTS `obj_courses`",
		"`id` CHAR(36) NOT NULL",
		"`tenant_id` CHAR(36) NOT NULL",
		"`team_set_id` CHAR(36)",
		"`created_by` CHAR(36) NOT NULL",
		"`modified_by` CHAR(36)",
		// RM-24 optimistic-lock version column — every new obj_* table carries it from birth.
		"`version` INT NOT NULL DEFAULT 0",
		"`created_at` TIMESTAMP",
		"`updated_at` TIMESTAMP",
		"`deleted_at` TIMESTAMP",
		"`data` JSON",
		// O1∪O6 composite timeline indexes (tenant_id, deleted_at, col, id); idx_tenant + idx_deleted_at dropped (redundant leftmost prefixes)
		"INDEX `idx_created_at` (`tenant_id`, `deleted_at`, `created_at`, `id`)",
		"INDEX `idx_updated_at` (`tenant_id`, `deleted_at`, `updated_at`, `id`)",
		// unique field -> byte-distinct (utf8mb4_bin) generated column + tenant-scoped unique index;
		// W5-E1 conditional-NULL so soft-deleted rows project NULL and don't squat the unique slot
		"`code` VARCHAR(255) COLLATE utf8mb4_bin GENERATED ALWAYS AS (CASE WHEN `deleted_at` IS NULL THEN JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.code')) ELSE NULL END) STORED",
		// RM-28 Slice B: a unique field's physical index is now NON-unique (uniqueness lives
		// in the sys_unique_index pivot, not a shared-table UNIQUE index — ../specs/019-rm28-pivot-uniqueness/spec.md §1.2).
		"INDEX `idx_obj_courses_code` (`tenant_id`, `code`)",
		"ENGINE=InnoDB",
		"utf8mb4",
	}
	for _, frag := range mustContain {
		if !strings.Contains(sql, frag) {
			t.Errorf("CREATE TABLE missing %q\n--- got ---\n%s", frag, sql)
		}
	}

	// Ordinary (non-unique) field must not become its own column.
	if strings.Contains(sql, "`title`") {
		t.Errorf("ordinary field `title` should live in data JSON, not as a column:\n%s", sql)
	}

	// RM-28 Slice B: a plain unique field must NEVER materialize a physical UNIQUE KEY on the
	// shared table (that would re-couple every tenant sharing this entity name).
	if strings.Contains(sql, "UNIQUE KEY") {
		t.Errorf("plain unique field must not produce a physical UNIQUE KEY post RM-28 Slice B:\n%s", sql)
	}
}

func TestBuildCreateTable_RejectsBadNames(t *testing.T) {
	if _, err := BuildCreateTable("select", nil); err == nil {
		t.Error("BuildCreateTable(entity=reserved) should error")
	}
	if _, err := BuildCreateTable("courses", []FieldDef{{Name: "code; drop", FieldType: "text"}}); err == nil {
		t.Error("BuildCreateTable(field=injection) should error")
	}
}
