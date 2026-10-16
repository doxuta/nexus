package ddl

import (
	"strings"
	"testing"
)

// TestBuildAddUniqueColumn pins the ALTER used when a UNIQUE field is added to an existing
// entity: a STORED generated column projecting the JSON value + a tenant-scoped unique index,
// matching exactly what BuildCreateTable would have produced at create time.
func TestBuildAddUniqueColumn(t *testing.T) {
	sql, err := BuildAddUniqueColumn("courses", "code")
	if err != nil {
		t.Fatalf("BuildAddUniqueColumn returned error: %v", err)
	}
	for _, frag := range []string{
		"ALTER TABLE `obj_courses`",
		"ADD COLUMN `code` VARCHAR(255) COLLATE utf8mb4_bin GENERATED ALWAYS AS (CASE WHEN `deleted_at` IS NULL THEN JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.code')) ELSE NULL END) STORED",
		"ADD UNIQUE KEY `uq_obj_courses_code` (`tenant_id`, `code`)",
	} {
		if !strings.Contains(sql, frag) {
			t.Errorf("ALTER missing %q\n--- got ---\n%s", frag, sql)
		}
	}
}

func TestBuildAddUniqueColumn_RejectsBadNames(t *testing.T) {
	if _, err := BuildAddUniqueColumn("select", "code"); err == nil {
		t.Error("reserved entity name should error")
	}
	if _, err := BuildAddUniqueColumn("courses", "code; drop"); err == nil {
		t.Error("injection field name should error")
	}
}

// TestBuildDropUniqueColumn pins the compensation ALTER that reverses BuildAddUniqueColumn:
// it drops both the tenant-scoped unique index and the generated column, restoring the table
// to its pre-ALTER shape.
func TestBuildDropUniqueColumn(t *testing.T) {
	sql, err := BuildDropUniqueColumn("courses", "code")
	if err != nil {
		t.Fatalf("BuildDropUniqueColumn returned error: %v", err)
	}
	for _, frag := range []string{
		"ALTER TABLE `obj_courses`",
		"DROP INDEX `uq_obj_courses_code`",
		"DROP COLUMN `code`",
	} {
		if !strings.Contains(sql, frag) {
			t.Errorf("ALTER missing %q\n--- got ---\n%s", frag, sql)
		}
	}
}

func TestBuildDropUniqueColumn_RejectsBadNames(t *testing.T) {
	if _, err := BuildDropUniqueColumn("select", "code"); err == nil {
		t.Error("reserved entity name should error")
	}
	if _, err := BuildDropUniqueColumn("courses", "code; drop"); err == nil {
		t.Error("injection field name should error")
	}
}

// TestBuildAddUniqueGencolNonUnique pins the RM-28 Slice B ALTER used when a NEW `is_unique`
// field is materialized: the byte-identical gencol BuildAddUniqueColumn emits, but a NON-unique
// tenant-scoped index (uniqueness now lives solely in the sys_unique_index pivot).
func TestBuildAddUniqueGencolNonUnique(t *testing.T) {
	sql, err := BuildAddUniqueGencolNonUnique("courses", "code")
	if err != nil {
		t.Fatalf("BuildAddUniqueGencolNonUnique returned error: %v", err)
	}
	for _, frag := range []string{
		"ALTER TABLE `obj_courses`",
		"ADD COLUMN `code` VARCHAR(255) COLLATE utf8mb4_bin GENERATED ALWAYS AS (CASE WHEN `deleted_at` IS NULL THEN JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.code')) ELSE NULL END) STORED",
		"ADD INDEX `idx_obj_courses_code` (`tenant_id`, `code`)",
	} {
		if !strings.Contains(sql, frag) {
			t.Errorf("ALTER missing %q\n--- got ---\n%s", frag, sql)
		}
	}
	if strings.Contains(sql, "UNIQUE KEY") {
		t.Errorf("BuildAddUniqueGencolNonUnique must NOT emit a UNIQUE KEY, got: %s", sql)
	}
	// Must be byte-identical to BuildAddUniqueColumn's gencol (only the index differs).
	uq, err := BuildAddUniqueColumn("courses", "code")
	if err != nil {
		t.Fatalf("BuildAddUniqueColumn: %v", err)
	}
	uqColDef := extractColDef(t, uq, "ADD")
	nonUqColDef := extractColDef(t, sql, "ADD")
	if uqColDef != nonUqColDef {
		t.Errorf("gencol coldef diverged from BuildAddUniqueColumn:\nuq    = %s\nnonUq = %s", uqColDef, nonUqColDef)
	}
}

func TestBuildAddUniqueGencolNonUnique_RejectsBadNames(t *testing.T) {
	if _, err := BuildAddUniqueGencolNonUnique("select", "code"); err == nil {
		t.Error("reserved entity name should error")
	}
	if _, err := BuildAddUniqueGencolNonUnique("courses", "code; drop"); err == nil {
		t.Error("injection field name should error")
	}
}

// TestBuildDropUniqueGencolNonUnique pins the compensation ALTER that reverses
// BuildAddUniqueGencolNonUnique: drops the non-unique idx_ index then the generated column.
func TestBuildDropUniqueGencolNonUnique(t *testing.T) {
	sql, err := BuildDropUniqueGencolNonUnique("courses", "code")
	if err != nil {
		t.Fatalf("BuildDropUniqueGencolNonUnique returned error: %v", err)
	}
	for _, frag := range []string{
		"ALTER TABLE `obj_courses`",
		"DROP INDEX `idx_obj_courses_code`",
		"DROP COLUMN `code`",
	} {
		if !strings.Contains(sql, frag) {
			t.Errorf("ALTER missing %q\n--- got ---\n%s", frag, sql)
		}
	}
}

func TestBuildDropUniqueGencolNonUnique_RejectsBadNames(t *testing.T) {
	if _, err := BuildDropUniqueGencolNonUnique("select", "code"); err == nil {
		t.Error("reserved entity name should error")
	}
	if _, err := BuildDropUniqueGencolNonUnique("courses", "code; drop"); err == nil {
		t.Error("injection field name should error")
	}
}

// TestBuildAddIndexedColumn_Types verifies that each supported field_type emits the correct
// typed generated column and a non-unique tenant-scoped index (D1 design table).
func TestBuildAddIndexedColumn_Types(t *testing.T) {
	cases := []struct {
		fieldType   string
		wantColFrag string
		wantIdxFrag string
		notWantFrag string // must NOT appear (e.g. UNIQUE KEY)
	}{
		{
			fieldType:   "text",
			wantColFrag: "ADD COLUMN `status` VARCHAR(255) COLLATE utf8mb4_unicode_ci GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.status'))) VIRTUAL",
			wantIdxFrag: "ADD INDEX `idx_obj_tickets_status` (`tenant_id`, `status`)",
			notWantFrag: "UNIQUE KEY",
		},
		{
			fieldType:   "email",
			wantColFrag: "ADD COLUMN `contact` VARCHAR(255) COLLATE utf8mb4_unicode_ci GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.contact'))) VIRTUAL",
			wantIdxFrag: "ADD INDEX `idx_obj_tickets_contact` (`tenant_id`, `contact`)",
			notWantFrag: "UNIQUE KEY",
		},
		{
			fieldType:   "picklist",
			wantColFrag: "ADD COLUMN `priority` VARCHAR(255) COLLATE utf8mb4_unicode_ci GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.priority'))) VIRTUAL",
			wantIdxFrag: "ADD INDEX `idx_obj_tickets_priority` (`tenant_id`, `priority`)",
			notWantFrag: "UNIQUE KEY",
		},
		{
			fieldType:   "number",
			wantColFrag: "ADD COLUMN `age` DECIMAL(38,10) GENERATED ALWAYS AS (CAST(JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.age')) AS DECIMAL(38,10))) VIRTUAL",
			wantIdxFrag: "ADD INDEX `idx_obj_students_age` (`tenant_id`, `age`)",
			notWantFrag: "UNIQUE KEY",
		},
		{
			fieldType:   "rating",
			wantColFrag: "ADD COLUMN `score` DECIMAL(38,10) GENERATED ALWAYS AS (CAST(JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.score')) AS DECIMAL(38,10))) VIRTUAL",
			wantIdxFrag: "ADD INDEX `idx_obj_courses_score` (`tenant_id`, `score`)",
			notWantFrag: "UNIQUE KEY",
		},
		{
			fieldType:   "currency",
			wantColFrag: "ADD COLUMN `price` DECIMAL(38,10) GENERATED ALWAYS AS (CAST(JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.price.amount')) AS DECIMAL(38,10))) VIRTUAL",
			wantIdxFrag: "ADD INDEX `idx_obj_products_price` (`tenant_id`, `price`)",
			notWantFrag: "UNIQUE KEY",
		},
		{
			fieldType:   "date",
			wantColFrag: "ADD COLUMN `due_date` DATETIME GENERATED ALWAYS AS (CAST(JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.due_date')) AS DATETIME)) VIRTUAL",
			wantIdxFrag: "ADD INDEX `idx_obj_tasks_due_date` (`tenant_id`, `due_date`)",
			notWantFrag: "UNIQUE KEY",
		},
		{
			// ENG #6: auto_number's per-record value is a formatted STRING (e.g. "INV-00042"),
			// so it maps to the same VARCHAR(255) VIRTUAL projection as text/email/picklist.
			fieldType:   "auto_number",
			wantColFrag: "ADD COLUMN `invoice_no` VARCHAR(255) COLLATE utf8mb4_unicode_ci GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.invoice_no'))) VIRTUAL",
			wantIdxFrag: "ADD INDEX `idx_obj_invoices_invoice_no` (`tenant_id`, `invoice_no`)",
			notWantFrag: "UNIQUE KEY",
		},
	}

	// field name and entity vary per case to make each assertion independent.
	entities := map[string]string{
		"text": "tickets", "email": "tickets", "picklist": "tickets",
		"number": "students", "rating": "courses", "currency": "products",
		"date": "tasks", "auto_number": "invoices",
	}
	fields := map[string]string{
		"text": "status", "email": "contact", "picklist": "priority",
		"number": "age", "rating": "score", "currency": "price",
		"date": "due_date", "auto_number": "invoice_no",
	}

	for _, tc := range cases {
		t.Run(tc.fieldType, func(t *testing.T) {
			entity := entities[tc.fieldType]
			field := fields[tc.fieldType]
			sql, err := BuildAddIndexedColumn(entity, field, tc.fieldType)
			if err != nil {
				t.Fatalf("BuildAddIndexedColumn(%q,%q,%q) returned error: %v", entity, field, tc.fieldType, err)
			}
			if !strings.Contains(sql, tc.wantColFrag) {
				t.Errorf("missing column fragment:\n  want: %q\n  got:  %s", tc.wantColFrag, sql)
			}
			if !strings.Contains(sql, tc.wantIdxFrag) {
				t.Errorf("missing index fragment:\n  want: %q\n  got:  %s", tc.wantIdxFrag, sql)
			}
			if strings.Contains(sql, tc.notWantFrag) {
				t.Errorf("unexpected fragment %q in DDL:\n  %s", tc.notWantFrag, sql)
			}
		})
	}
}

// TestBuildAddIndexedColumn_DatetimeNowErrors confirms that after the D1 blocker fix,
// BuildAddIndexedColumn rejects "datetime" at the DDL layer (secondary guard — the primary
// guard is validateFieldConfig in the service layer). This test documents that the datetime
// path is intentionally removed from indexedColExpr.
func TestBuildAddIndexedColumn_DatetimeNowErrors(t *testing.T) {
	_, err := BuildAddIndexedColumn("tickets", "closed_at", "datetime")
	if err == nil {
		t.Error("BuildAddIndexedColumn with datetime should return an error after D1 blocker fix")
	}
}

// TestBuildAddIndexedColumn_RejectsBadNames ensures that both entity and field names are
// sanitized and injection attempts are rejected before any DDL is emitted.
func TestBuildAddIndexedColumn_RejectsBadNames(t *testing.T) {
	if _, err := BuildAddIndexedColumn("select", "status", "text"); err == nil {
		t.Error("reserved entity name should error")
	}
	if _, err := BuildAddIndexedColumn("tickets", "status; drop table", "text"); err == nil {
		t.Error("injection field name should error")
	}
	if _, err := BuildAddIndexedColumn("tickets", "status", "boolean"); err == nil {
		t.Error("unsupported field type should error")
	}
}

// TestBuildAddIndexedColumn_SkipDoubleCreate documents the guard that prevents creating a
// second generated column when a field is BOTH indexed AND unique: the caller (entity
// orchestration / field orchestration) must detect IsUnique and skip AddIndexedColumn, since
// the unique generated column already provides a physical B-tree column. This test verifies
// BuildAddIndexedColumn itself does NOT silently reuse the unique-column naming convention
// (utf8mb4_bin) — the two are intentionally different column types.
func TestBuildAddIndexedColumn_UniqueVsIndexedAreDifferent(t *testing.T) {
	uniqueSQL, err := BuildAddUniqueColumn("accounts", "code")
	if err != nil {
		t.Fatalf("BuildAddUniqueColumn: %v", err)
	}
	indexedSQL, err := BuildAddIndexedColumn("accounts", "code", "text")
	if err != nil {
		t.Fatalf("BuildAddIndexedColumn: %v", err)
	}
	// Unique uses utf8mb4_bin; indexed uses utf8mb4_unicode_ci.
	if !strings.Contains(uniqueSQL, "utf8mb4_bin") {
		t.Errorf("unique column should use utf8mb4_bin, got: %s", uniqueSQL)
	}
	if !strings.Contains(indexedSQL, "utf8mb4_unicode_ci") {
		t.Errorf("indexed column should use utf8mb4_unicode_ci, got: %s", indexedSQL)
	}
	// Unique uses UNIQUE KEY; indexed uses non-unique INDEX.
	if !strings.Contains(uniqueSQL, "UNIQUE KEY") {
		t.Errorf("unique ALTER should have UNIQUE KEY, got: %s", uniqueSQL)
	}
	if strings.Contains(indexedSQL, "UNIQUE KEY") {
		t.Errorf("indexed ALTER must NOT have UNIQUE KEY, got: %s", indexedSQL)
	}
}

// TestBuildDropIndexedColumn verifies the DROP reverses the ADD: drops the named index then
// the generated column (same order as BuildDropUniqueColumn / BuildDropRelationColumn).
func TestBuildDropIndexedColumn(t *testing.T) {
	sql, err := BuildDropIndexedColumn("tickets", "status")
	if err != nil {
		t.Fatalf("BuildDropIndexedColumn returned error: %v", err)
	}
	for _, frag := range []string{
		"ALTER TABLE `obj_tickets`",
		"DROP INDEX `idx_obj_tickets_status`",
		"DROP COLUMN `status`",
	} {
		if !strings.Contains(sql, frag) {
			t.Errorf("DROP ALTER missing %q\n--- got ---\n%s", frag, sql)
		}
	}
}

func TestBuildDropIndexedColumn_RejectsBadNames(t *testing.T) {
	if _, err := BuildDropIndexedColumn("select", "status"); err == nil {
		t.Error("reserved entity name should error")
	}
	if _, err := BuildDropIndexedColumn("tickets", "status; drop"); err == nil {
		t.Error("injection field name should error")
	}
}
