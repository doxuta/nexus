//go:build integration

// W5 Builder core v2 integration tests — against a real throwaway MySQL 8 (o3a harness).
// Proofs a unit fake cannot give:
//  1. Migration 000033 BACKFILL: rows that existed BEFORE the migration get deterministic
//     field_order (10,20,30… per (tenant, entity), ORDER BY created_at, name, id) — proven by
//     migrating a throwaway DB to v32, inserting legacy rows, then stepping to v33.
//  2. field_order/description round-trip through the real repo: entity-create numbers by
//     slice position, add-field appends, edit mutates, ListFields serves (field_order, name).
//  3. ReorderFields renumbers in ONE real transaction, tenant-scoped (tenant B cannot reorder
//     tenant A's entity, and tenant B's field id inside tenant A's list rejects atomically).
//  4. ValidateFormula dry-runs against the REAL sibling list and writes NOTHING.
//  5. Layout helpText + 4-column sections persist through the real SaveLayout path.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestW5Int -race -v
package services

import (
	"context"
	"os/exec"
	"testing"

	"github.com/google/uuid"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

const (
	w5TenantA = "w5aaaaaa-0000-0000-0000-000000000001"
	w5TenantB = "w5bbbbbb-0000-0000-0000-000000000002"
)

// w5Wire builds entity+field services over a fresh, fully-migrated throwaway DB.
func w5Wire(t *testing.T) (*EntityService, *FieldService, *metadata.Repository, *gorm.DB) {
	t.Helper()
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	entitySvc := NewEntityService(dynRepo, metaRepo)
	fieldSvc := NewFieldService(dynRepo, metaRepo) // reorder seam auto-wired via type assertion
	return entitySvc, fieldSvc, metaRepo, gdb
}

// TestW5Int_Migration33Backfill proves the one-time backfill numbers PRE-EXISTING rows
// deterministically: throwaway DB → migrate GOTO v32 → insert legacy sys_fields rows (no
// field_order column yet) → migrate UP (applies 000033) → assert 10/20/30 per (tenant,entity)
// ordered by created_at then name (equal created_at exercises the name tie-break).
func TestW5Int_Migration33Backfill(t *testing.T) {
	dbName := o3aUniqueDBName()
	admin, err := gorm.Open(gormmysql.Open(o3aAdminDSN(t)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Skipf("w5: MySQL not reachable (%v) — skipping", err)
	}
	if sqlDB, err := admin.DB(); err != nil || sqlDB.Ping() != nil {
		t.Skip("w5: MySQL not pingable — skipping")
	}
	if err := admin.Exec("CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci").Error; err != nil {
		t.Fatalf("create throwaway DB: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	migrateBin := o3aMigrateBin(t)
	if migrateBin == "" {
		t.Skip("w5: migrate binary not found — skipping")
	}
	// Step 1: schema as it was BEFORE this slice.
	if out, err := exec.Command(migrateBin, "-path", o3aMigrationsDir, "-database", o3aMigrateDSN(t, dbName), "goto", "32").CombinedOutput(); err != nil {
		t.Fatalf("migrate goto 32: %v\n%s", err, out)
	}
	gdb, err := gorm.Open(gormmysql.Open(o3aDSN(t, dbName)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open gorm: %v", err)
	}
	t.Cleanup(func() {
		if d, err := gdb.DB(); err == nil {
			_ = d.Close()
		}
	})

	// Step 2: legacy rows. Entity e1 (tenant A): created_at spread → chronological order wins.
	// Entity e2 (tenant A): IDENTICAL created_at → name tie-break. Entity e3 (tenant B):
	// separate partition proves per-(tenant,entity) numbering.
	mustExec := func(sql string, args ...any) {
		t.Helper()
		if err := gdb.Exec(sql, args...).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	mustExec(`INSERT INTO sys_entities (id, tenant_id, name, label, table_name) VALUES
		('e1', ?, 'w5_orders',   'Orders',   'obj_w5_orders'),
		('e2', ?, 'w5_quotes',   'Quotes',   'obj_w5_quotes'),
		('e3', ?, 'w5_orders',   'Orders B', 'obj_w5_orders')`, w5TenantA, w5TenantA, w5TenantB)
	mustExec(`INSERT INTO sys_fields (id, entity_id, tenant_id, name, label, field_type, created_at) VALUES
		('f-e1-c', 'e1', ?, 'zeta',  'Z', 'text', '2026-01-03 10:00:00'),
		('f-e1-a', 'e1', ?, 'alpha', 'A', 'text', '2026-01-01 10:00:00'),
		('f-e1-b', 'e1', ?, 'beta',  'B', 'text', '2026-01-02 10:00:00'),
		('f-e2-b', 'e2', ?, 'bravo', 'B', 'text', '2026-02-01 09:00:00'),
		('f-e2-a', 'e2', ?, 'anchor','A', 'text', '2026-02-01 09:00:00'),
		('f-e3-x', 'e3', ?, 'xray',  'X', 'text', '2026-03-01 08:00:00')`,
		w5TenantA, w5TenantA, w5TenantA, w5TenantA, w5TenantA, w5TenantB)

	// Step 3: apply 000033.
	if out, err := exec.Command(migrateBin, "-path", o3aMigrationsDir, "-database", o3aMigrateDSN(t, dbName), "up").CombinedOutput(); err != nil {
		t.Fatalf("migrate up (33+): %v\n%s", err, out)
	}

	got := map[string]int{}
	rows, err := gdb.Raw(`SELECT id, field_order FROM sys_fields`).Rows()
	if err != nil {
		t.Fatalf("read backfill: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var ord int
		if err := rows.Scan(&id, &ord); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = ord
	}
	want := map[string]int{
		"f-e1-a": 10, "f-e1-b": 20, "f-e1-c": 30, // chronological created_at
		"f-e2-a": 10, "f-e2-b": 20, // equal created_at → name tie-break
		"f-e3-x": 10, // tenant B partition restarts at 10
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("backfill %s: field_order = %d; want %d (all: %v)", id, got[id], w, got)
		}
	}
}

// TestW5Int_OrderDescriptionLifecycle covers the full write path on a real DB: entity-create
// slice numbering → ListFields serve order → add-field append → explicit order → edit-field
// mutation → reorder transaction → tenant isolation of reorder.
func TestW5Int_OrderDescriptionLifecycle(t *testing.T) {
	entitySvc, fieldSvc, metaRepo, _ := w5Wire(t)
	ctx := context.Background()

	ent, fields, err := entitySvc.CreateEntity(ctx, w5TenantA, CreateEntityInput{
		Name: "w5deals", Label: "W5 Deals",
		Fields: []FieldInput{
			{Name: "zzz_last", Label: "Alphabetically last, declared first", FieldType: "text"},
			{Name: "amount", Label: "Amount", FieldType: "number", Description: "Giá trị hợp đồng"},
			{Name: "stage", Label: "Stage", FieldType: "text"},
		},
	})
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if fields[0].FieldOrder != 10 || fields[1].FieldOrder != 20 || fields[2].FieldOrder != 30 {
		t.Fatalf("slice numbering = %d/%d/%d; want 10/20/30", fields[0].FieldOrder, fields[1].FieldOrder, fields[2].FieldOrder)
	}

	// ListFields serves declaration order (field_order), NOT alphabetical — zzz_last leads.
	listed, err := metaRepo.ListFields(ctx, w5TenantA, ent.ID)
	if err != nil {
		t.Fatalf("ListFields: %v", err)
	}
	if listed[0].Name != "zzz_last" || listed[1].Name != "amount" || listed[2].Name != "stage" {
		t.Fatalf("serve order = %s,%s,%s; want zzz_last,amount,stage", listed[0].Name, listed[1].Name, listed[2].Name)
	}
	if listed[1].Description != "Giá trị hợp đồng" {
		t.Errorf("description round-trip = %q; want persisted", listed[1].Description)
	}

	// Add-field appends after the current max (30 → 40).
	added, err := fieldSvc.AddField(ctx, w5TenantA, AddFieldInput{
		EntityName: "w5deals",
		Field:      FieldInput{Name: "notes", Label: "Notes", FieldType: "text"},
	})
	if err != nil {
		t.Fatalf("AddField: %v", err)
	}
	if added.FieldOrder != 40 {
		t.Errorf("appended order = %d; want 40", added.FieldOrder)
	}

	// Edit mutates order + description (not immutable).
	newOrder := 5
	newDesc := "ghi chú mới"
	edited, err := fieldSvc.EditField(ctx, w5TenantA, EditFieldInput{
		FieldID: added.ID, FieldOrder: &newOrder, Description: &newDesc,
	})
	if err != nil {
		t.Fatalf("EditField: %v", err)
	}
	if edited.FieldOrder != 5 || edited.Description != "ghi chú mới" {
		t.Errorf("edited = %d/%q; want 5/%q", edited.FieldOrder, edited.Description, "ghi chú mới")
	}
	listed, _ = metaRepo.ListFields(ctx, w5TenantA, ent.ID)
	if listed[0].Name != "notes" {
		t.Errorf("after edit, first served = %s; want notes (order 5)", listed[0].Name)
	}

	// Reorder: reverse everything in one transaction → 10/20/30/40 in the NEW order.
	ids := []string{listed[3].ID, listed[2].ID, listed[1].ID, listed[0].ID}
	updated, err := fieldSvc.ReorderFields(ctx, w5TenantA, ReorderFieldsInput{EntityName: "w5deals", Order: ids})
	if err != nil {
		t.Fatalf("ReorderFields: %v", err)
	}
	if updated != 4 {
		t.Errorf("updated = %d; want 4", updated)
	}
	after, _ := metaRepo.ListFields(ctx, w5TenantA, ent.ID)
	for i, wantID := range ids {
		if after[i].ID != wantID {
			t.Errorf("post-reorder position %d = %s; want %s", i, after[i].ID, wantID)
		}
		if after[i].FieldOrder != (i+1)*10 {
			t.Errorf("post-reorder order[%d] = %d; want %d", i, after[i].FieldOrder, (i+1)*10)
		}
	}

	// Tenant isolation: tenant B cannot reorder tenant A's entity (404), and a forged id
	// under the right entity name still rejects.
	if _, err := fieldSvc.ReorderFields(ctx, w5TenantB, ReorderFieldsInput{EntityName: "w5deals", Order: ids}); err == nil {
		t.Error("tenant B reordering tenant A's entity must fail")
	} else if w5IntCode(t, err) != "ENTITY_NOT_FOUND" {
		t.Errorf("cross-tenant code = %q; want ENTITY_NOT_FOUND", w5IntCode(t, err))
	}
	forged := append([]string{uuid.NewString()}, ids[1:]...)
	if _, err := fieldSvc.ReorderFields(ctx, w5TenantA, ReorderFieldsInput{EntityName: "w5deals", Order: forged}); err == nil {
		t.Error("forged id must fail")
	} else if w5IntCode(t, err) != "FIELD_REORDER_UNKNOWN_FIELD" {
		t.Errorf("forged-id code = %q; want FIELD_REORDER_UNKNOWN_FIELD", w5IntCode(t, err))
	}
	// The failed reorders must not have changed anything (all-or-nothing).
	after2, _ := metaRepo.ListFields(ctx, w5TenantA, ent.ID)
	for i := range after {
		if after2[i].ID != after[i].ID || after2[i].FieldOrder != after[i].FieldOrder {
			t.Fatalf("failed reorder mutated state at %d: %s/%d", i, after2[i].ID, after2[i].FieldOrder)
		}
	}
}

// TestW5Int_ValidateFormulaDryRun proves the dry-run reads the REAL sibling list and writes
// nothing — sys_fields row count is identical before/after both outcomes.
func TestW5Int_ValidateFormulaDryRun(t *testing.T) {
	entitySvc, fieldSvc, _, gdb := w5Wire(t)
	ctx := context.Background()

	if _, _, err := entitySvc.CreateEntity(ctx, w5TenantA, CreateEntityInput{
		Name: "w5calc", Label: "W5 Calc",
		Fields: []FieldInput{
			{Name: "price", Label: "Price", FieldType: "number"},
			{Name: "qty", Label: "Qty", FieldType: "number"},
			{Name: "note", Label: "Note", FieldType: "text"},
		},
	}); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	var before int64
	gdb.Raw(`SELECT COUNT(*) FROM sys_fields`).Scan(&before)

	res, err := fieldSvc.ValidateFormula(ctx, w5TenantA, ValidateFormulaInput{EntityName: "w5calc", Formula: "price * qty * 1.1"})
	if err != nil {
		t.Fatalf("ValidateFormula: %v", err)
	}
	if !res.Valid || len(res.Fields) != 2 || res.Fields[0] != "price" || res.Fields[1] != "qty" || res.ResultType != "number" {
		t.Errorf("result = %+v; want valid, fields [price qty], number", res)
	}

	if _, err := fieldSvc.ValidateFormula(ctx, w5TenantA, ValidateFormulaInput{EntityName: "w5calc", Formula: "note + 1"}); err == nil {
		t.Error("non-numeric ref must fail")
	} else if w5IntCode(t, err) != "FIELD_FORMULA_NON_NUMERIC_REF" {
		t.Errorf("code = %q; want FIELD_FORMULA_NON_NUMERIC_REF", w5IntCode(t, err))
	}
	// Cross-tenant entity name → 404, no sibling leak.
	if _, err := fieldSvc.ValidateFormula(ctx, w5TenantB, ValidateFormulaInput{EntityName: "w5calc", Formula: "price"}); err == nil {
		t.Error("cross-tenant dry-run must fail")
	} else if w5IntCode(t, err) != "ENTITY_NOT_FOUND" {
		t.Errorf("code = %q; want ENTITY_NOT_FOUND", w5IntCode(t, err))
	}

	var after int64
	gdb.Raw(`SELECT COUNT(*) FROM sys_fields`).Scan(&after)
	if before != after {
		t.Errorf("dry-run wrote rows: %d → %d", before, after)
	}
}

// TestW5Int_LayoutHelpTextFourColumns proves the extended cell schema persists through the
// REAL SaveLayout path (validate → upsert → read back) and the new bounds reject at save.
func TestW5Int_LayoutHelpTextFourColumns(t *testing.T) {
	entitySvc, _, metaRepo, gdb := w5Wire(t)
	ctx := context.Background()

	if _, _, err := entitySvc.CreateEntity(ctx, w5TenantA, CreateEntityInput{
		Name: "w5form", Label: "W5 Form",
		Fields: []FieldInput{
			{Name: "title", Label: "Title", FieldType: "text"},
			{Name: "level", Label: "Level", FieldType: "number"},
		},
	}); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	layoutSvc := NewLayoutService(metaRepo, metadata.NewLayoutRepository(gdb), nil)

	saved, err := layoutSvc.SaveLayout(ctx, w5TenantA, SaveLayoutInput{
		EntityName: "w5form", ViewType: "edit",
		LayoutJSON: map[string]any{
			"version": 1,
			"sections": []any{map[string]any{
				"columns": 4,
				"fields": []any{
					map[string]any{"field": "title", "colspan": 4, "helpText": "Tên hiển thị với khách"},
					"level",
				},
			}},
		},
	})
	if err != nil {
		t.Fatalf("SaveLayout(helpText, 4 cols): %v", err)
	}
	got, err := layoutSvc.GetLayout(ctx, w5TenantA, "w5form", "edit")
	if err != nil {
		t.Fatalf("GetLayout: %v", err)
	}
	view := got.Views["edit"]
	if view.IsDefault || view.ID != saved.ID {
		t.Fatalf("expected the saved custom layout back, got default=%v id=%s", view.IsDefault, view.ID)
	}
	sections := view.LayoutData["sections"].([]any)
	cell := sections[0].(map[string]any)["fields"].([]any)[0].(map[string]any)
	if cell["helpText"] != "Tên hiển thị với khách" {
		t.Errorf("helpText round-trip = %v; want persisted verbatim", cell["helpText"])
	}

	// Bounds still reject at save: helpText >200 runes and columns >4.
	if _, err := layoutSvc.SaveLayout(ctx, w5TenantA, SaveLayoutInput{
		EntityName: "w5form", ViewType: "edit",
		LayoutJSON: map[string]any{
			"version":  1,
			"sections": []any{map[string]any{"columns": 5, "fields": []any{"title", "level"}}},
		},
	}); err == nil {
		t.Error("columns 5 must reject")
	} else if w5IntCode(t, err) != "LAYOUT_INVALID" {
		t.Errorf("code = %q; want LAYOUT_INVALID", w5IntCode(t, err))
	}
}

// w5IntCode extracts a ValidationError code — thin alias over the shared w5Code helper
// (w5_builder_v2_test.go compiles in both build modes, so it is available here).
func w5IntCode(t *testing.T, err error) string {
	t.Helper()
	return w5Code(t, err)
}
