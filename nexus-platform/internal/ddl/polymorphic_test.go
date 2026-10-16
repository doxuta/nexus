package ddl

import (
	"strings"
	"testing"
)

// TestBuildAddPolymorphicColumns pins the ALTER used when a `polymorphic_relation` field is added
// to an existing entity (ACTIVITY-CORE #3a). Unlike a relation's single `<f>_id` FK column, it
// materializes TWO STORED generated columns (`<f>_type` VARCHAR(100), `<f>_id` CHAR(36))
// projecting the JSON {type,id} object, plus a NON-unique composite tenant-scoped index over
// (tenant_id, <f>_type, <f>_id) named `idx_obj_<entity>_<f>`.
func TestBuildAddPolymorphicColumns(t *testing.T) {
	sql, err := BuildAddPolymorphicColumns("activities", "related_to")
	if err != nil {
		t.Fatalf("BuildAddPolymorphicColumns returned error: %v", err)
	}
	for _, frag := range []string{
		"ALTER TABLE `obj_activities`",
		"ADD COLUMN `related_to_type` VARCHAR(100) GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.related_to.type'))) STORED",
		"ADD COLUMN `related_to_id` CHAR(36) GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.related_to.id'))) STORED",
		"ADD INDEX `idx_obj_activities_related_to` (`tenant_id`, `related_to_type`, `related_to_id`)",
	} {
		if !strings.Contains(sql, frag) {
			t.Errorf("ALTER missing %q\n--- got ---\n%s", frag, sql)
		}
	}
	// The composite index must NOT be unique (many activities may point at one parent).
	if strings.Contains(sql, "UNIQUE") {
		t.Errorf("polymorphic composite index must not be UNIQUE\n--- got ---\n%s", sql)
	}
}

func TestBuildAddPolymorphicColumns_RejectsBadNames(t *testing.T) {
	if _, err := BuildAddPolymorphicColumns("select", "related_to"); err == nil {
		t.Error("reserved entity name should error")
	}
	if _, err := BuildAddPolymorphicColumns("activities", "related; drop"); err == nil {
		t.Error("injection field name should error")
	}
}

// TestBuildDropPolymorphicColumns pins the compensation ALTER: drop the composite index then both
// generated columns, restoring the pre-ALTER shape.
func TestBuildDropPolymorphicColumns(t *testing.T) {
	sql, err := BuildDropPolymorphicColumns("activities", "related_to")
	if err != nil {
		t.Fatalf("BuildDropPolymorphicColumns returned error: %v", err)
	}
	for _, frag := range []string{
		"ALTER TABLE `obj_activities`",
		"DROP INDEX `idx_obj_activities_related_to`",
		"DROP COLUMN `related_to_type`",
		"DROP COLUMN `related_to_id`",
	} {
		if !strings.Contains(sql, frag) {
			t.Errorf("ALTER missing %q\n--- got ---\n%s", frag, sql)
		}
	}
}

// TestBuildCreateTable_PolymorphicField asserts a polymorphic_relation field declared at create
// time materializes the same two gen-columns + composite index inline in CREATE TABLE — parity
// with the add-field path, so create-time and add-field-time schema can never drift.
func TestBuildCreateTable_PolymorphicField(t *testing.T) {
	sql, err := BuildCreateTable("activities", []FieldDef{
		{Name: "subject", FieldType: "text"},
		{Name: "related_to", FieldType: "polymorphic_relation", Polymorphic: true},
	})
	if err != nil {
		t.Fatalf("BuildCreateTable returned error: %v", err)
	}
	for _, frag := range []string{
		"`related_to_type` VARCHAR(100) GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.related_to.type'))) STORED",
		"`related_to_id` CHAR(36) GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.related_to.id'))) STORED",
		"INDEX `idx_obj_activities_related_to` (`tenant_id`, `related_to_type`, `related_to_id`)",
	} {
		if !strings.Contains(sql, frag) {
			t.Errorf("CREATE TABLE missing %q\n--- got ---\n%s", frag, sql)
		}
	}
	// The composite index must not be a UNIQUE KEY.
	if strings.Contains(sql, "UNIQUE KEY `uq_obj_activities_related_to") {
		t.Errorf("polymorphic field must not produce a UNIQUE index\n--- got ---\n%s", sql)
	}
}

// TestPolymorphicIndexNameWithinLimit verifies the composite index name stays within MySQL's
// 64-char identifier limit even when BOTH entity and field are at SanitizeIdentifier's 26-char
// cap — idx_obj_<26> _ <26> = 9 + 26 + 26 = 61 chars, shorter than the relation FK index (12 +
// entity + field) that binds the cap, so no clamping is ever required.
func TestPolymorphicIndexNameWithinLimit(t *testing.T) {
	ent := strings.Repeat("e", 26)
	fld := strings.Repeat("f", 26)
	sql, err := BuildAddPolymorphicColumns(ent, fld)
	if err != nil {
		t.Fatalf("BuildAddPolymorphicColumns at cap returned error: %v", err)
	}
	idxName := "idx_obj_" + ent + "_" + fld
	if len(idxName) > 64 {
		t.Fatalf("index name %q is %d chars — exceeds MySQL's 64-char limit", idxName, len(idxName))
	}
	if !strings.Contains(sql, "`"+idxName+"`") {
		t.Errorf("ALTER missing index name %q\n--- got ---\n%s", idxName, sql)
	}
}
