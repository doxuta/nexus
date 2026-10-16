//go:build integration

// RM-28 Slice A2 (§4.1) — AddField's unique-conversion path is pivot-aware. Uses the SAME
// throwaway-migrated-DB harness (o3aSetup) as the other *_integration_test.go files in this
// package (real GORM metadata.Repository + real dynrepo over a fresh per-test database).
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestAddFieldPivot -race -v
package services

import (
	"context"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// TestAddField_UniqueBackfillsExistingRows: raw-seeded rows (simulating data written before the
// field existed — e.g. a CSV-import-style bypass) with DISTINCT values must ALL end up pivoted
// after AddField, and the pivot value must be byte-identical to the gencol (not re-derived).
func TestAddField_UniqueBackfillsExistingRows(t *testing.T) {
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	entitySvc := NewEntityService(dynRepo, metaRepo)
	fieldSvc := NewFieldService(dynRepo, metaRepo)
	ctx := context.Background()
	const tenant = "af11-tenant-aaaaaaaaaaaaaaaaaaaaaaaa"

	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name: "af_fill", Label: "AF Fill",
		Fields: []FieldInput{{Name: "name", Label: "Name", FieldType: "text"}},
	}); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	// Seed live rows with a "code" JSON key that has NEVER been a registered field — the
	// physical column does not exist yet, so no constraint applies to this raw insert.
	if _, err := sdb.Exec("INSERT INTO `obj_af_fill` (id, tenant_id, created_by, data) VALUES (?, ?, ?, JSON_OBJECT('name','x','code','C1'))",
		"11111111-1111-1111-1111-111111111101", tenant, "u1"); err != nil {
		t.Fatalf("seed row 1: %v", err)
	}
	if _, err := sdb.Exec("INSERT INTO `obj_af_fill` (id, tenant_id, created_by, data) VALUES (?, ?, ?, JSON_OBJECT('name','y','code','C2'))",
		"11111111-1111-1111-1111-111111111102", tenant, "u1"); err != nil {
		t.Fatalf("seed row 2: %v", err)
	}

	if _, err := fieldSvc.AddField(ctx, tenant, AddFieldInput{
		EntityName: "af_fill",
		Field:      FieldInput{Name: "code", Label: "Code", FieldType: "text", IsUnique: true},
	}); err != nil {
		t.Fatalf("AddField code unique: %v", err)
	}

	var vals []string
	if err := sdb.Select(&vals,
		"SELECT `value` FROM `sys_unique_index` WHERE `tenant_id` = ? AND `entity_name` = ? AND `field_name` = ? ORDER BY `value`",
		tenant, "af_fill", "code"); err != nil {
		t.Fatalf("query pivot: %v", err)
	}
	if len(vals) != 2 || vals[0] != "C1" || vals[1] != "C2" {
		t.Fatalf("pivot rows = %v; want [C1 C2] (raw-seeded rows backfilled by AddField)", vals)
	}
}

// TestAddField_UniqueSharedColumnIntraTenantDupe409: tenant A already made `code` unique on the
// shared table (clean, no data). Tenant B shares the physical table, has NOT declared `code` at
// all, but already has its OWN two live rows sharing a value — reachable WITHOUT any drift
// simulation post RM-28 Slice B, since the physical index behind `code` is non-unique from birth
// (BuildCreateTable) and never blocked B's raw inserts to begin with. When tenant B then calls
// AddField for `code` unique, the column already exists (co-tenant reconcile: no ALTER,
// altered=false) so the backfill path (A2) is what discovers tenant B's own duplicate — and must
// reject 409 RECORD_DUPLICATE, not persist a half-covered pivot.
func TestAddField_UniqueSharedColumnIntraTenantDupe409(t *testing.T) {
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	entitySvc := NewEntityService(dynRepo, metaRepo)
	fieldSvc := NewFieldService(dynRepo, metaRepo)
	ctx := context.Background()
	const tenantA = "af12-tenant-aaaaaaaaaaaaaaaaaaaaaaaa"
	const tenantB = "af12-tenant-bbbbbbbbbbbbbbbbbbbbbbbb"

	if _, _, err := entitySvc.CreateEntity(ctx, tenantA, CreateEntityInput{
		Name: "af_xdupe", Label: "AF Xdupe A",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text"},
			{Name: "code", Label: "Code", FieldType: "text", IsUnique: true},
		},
	}); err != nil {
		t.Fatalf("CreateEntity tenant A: %v", err)
	}
	if _, _, err := entitySvc.CreateEntity(ctx, tenantB, CreateEntityInput{
		Name: "af_xdupe", Label: "AF Xdupe B",
		Fields: []FieldInput{{Name: "name", Label: "Name", FieldType: "text"}},
	}); err != nil {
		t.Fatalf("CreateEntity tenant B (shared table, no code field): %v", err)
	}

	// No drift simulation needed post RM-28 Slice B: `code`'s physical index is non-unique from
	// birth, so these two raw inserts holding the SAME value for tenant B succeed unassisted.
	if _, err := sdb.Exec("INSERT INTO `obj_af_xdupe` (id, tenant_id, created_by, data) VALUES (?, ?, ?, JSON_OBJECT('name','p','code','DUPX'))",
		"22222222-2222-2222-2222-222222222201", tenantB, "u1"); err != nil {
		t.Fatalf("seed B row 1: %v", err)
	}
	if _, err := sdb.Exec("INSERT INTO `obj_af_xdupe` (id, tenant_id, created_by, data) VALUES (?, ?, ?, JSON_OBJECT('name','q','code','DUPX'))",
		"22222222-2222-2222-2222-222222222202", tenantB, "u1"); err != nil {
		t.Fatalf("seed B row 2: %v", err)
	}

	_, err := fieldSvc.AddField(ctx, tenantB, AddFieldInput{
		EntityName: "af_xdupe",
		Field:      FieldInput{Name: "code", Label: "Code", FieldType: "text", IsUnique: true},
	})
	if codeOf(err) != "RECORD_DUPLICATE" {
		t.Fatalf("AddField tenant B: code = %q; want RECORD_DUPLICATE (got err=%v)", codeOf(err), err)
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Details["field"] != "code" {
		t.Fatalf("AddField tenant B: err = %+v; want details.field=code", err)
	}

	// Tenant B's field must NOT be registered — the add was rejected before the metadata write.
	if _, ferr := metaRepo.GetEntityByName(ctx, tenantB, "af_xdupe"); ferr != nil {
		t.Fatalf("resolve tenant B entity: %v", ferr)
	}
}

// TestAddField_CrossTenantDupeDoesNotBlockAddField is the RM-28 Slice B retarget of the retired
// CROSS_TENANT_SHARED_UNIQUE regression guard (dispatch §4.1 — "its role is fully replaced by
// the pivot's intra-tenant RECORD_DUPLICATE"): tenant A has TWO live rows sharing a value under
// an ordinary (non-unique) `code` field on the SHARED obj_* table. When tenant B — who has NEVER
// even seen `code` before — calls AddField to make `code` unique, the ALTER now materializes a
// NON-unique index (AddUniqueGencolNonUnique) over the WHOLE shared table (including A's rows),
// which cannot 1062 no matter what data A holds. The subsequent backfill is tenant-SCOPED to B
// (§4.1), and B has zero live `code` values of its own, so AddField succeeds cleanly — proving A's
// pre-existing intra-tenant duplicate does NOT block B's unrelated field registration (the exact
// coupling RM-28 exists to remove; pre-Slice-B this same setup returned 409
// CROSS_TENANT_SHARED_UNIQUE via the old ADD UNIQUE KEY's own 1062).
func TestAddField_CrossTenantDupeDoesNotBlockAddField(t *testing.T) {
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	entitySvc := NewEntityService(dynRepo, metaRepo)
	fieldSvc := NewFieldService(dynRepo, metaRepo)
	ctx := context.Background()
	const tenantA = "af13-tenant-aaaaaaaaaaaaaaaaaaaaaaaa"
	const tenantB = "af13-tenant-bbbbbbbbbbbbbbbbbbbbbbbb"

	if _, _, err := entitySvc.CreateEntity(ctx, tenantA, CreateEntityInput{
		Name: "af_xtenant", Label: "AF Xtenant A",
		Fields: []FieldInput{
			{Name: "title", Label: "Title", FieldType: "text", IsRequired: true},
			{Name: "code", Label: "Code", FieldType: "text"}, // ordinary for A — no physical column
		},
	}); err != nil {
		t.Fatalf("CreateEntity tenant A: %v", err)
	}
	if _, err := dynRepo.InsertRecord(ctx, "af_xtenant", tenantA, "u-a", map[string]any{"title": "r1", "code": "DUP"}, nil); err != nil {
		t.Fatalf("tenant A insert row 1: %v", err)
	}
	if _, err := dynRepo.InsertRecord(ctx, "af_xtenant", tenantA, "u-a", map[string]any{"title": "r2", "code": "DUP"}, nil); err != nil {
		t.Fatalf("tenant A insert row 2: %v", err)
	}
	if _, _, err := entitySvc.CreateEntity(ctx, tenantB, CreateEntityInput{
		Name: "af_xtenant", Label: "AF Xtenant B",
		Fields: []FieldInput{{Name: "title", Label: "Title", FieldType: "text", IsRequired: true}},
	}); err != nil {
		t.Fatalf("CreateEntity tenant B: %v", err)
	}

	if _, err := fieldSvc.AddField(ctx, tenantB, AddFieldInput{
		EntityName: "af_xtenant",
		Field:      FieldInput{Name: "code", Label: "Code", FieldType: "text", IsUnique: true},
	}); err != nil {
		t.Fatalf("AddField tenant B: %v; want success — A's intra-tenant dupe must not block B (RM-28 decoupling)", err)
	}
	// B's own pivot is empty (B has zero live `code` rows) and the physical index is non-unique.
	var vals []string
	if err := sdb.Select(&vals, "SELECT `value` FROM `sys_unique_index` WHERE `tenant_id` = ? AND `entity_name` = ? AND `field_name` = ?",
		tenantB, "af_xtenant", "code"); err != nil {
		t.Fatalf("query tenant B pivot: %v", err)
	}
	if len(vals) != 0 {
		t.Fatalf("tenant B pivot = %v; want [] (B has no live code rows)", vals)
	}
	var nonUnique int
	if err := sdb.Get(&nonUnique, "SELECT NON_UNIQUE FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'obj_af_xtenant' AND INDEX_NAME = 'idx_obj_af_xtenant_code' LIMIT 1"); err != nil {
		t.Fatalf("query idx_obj_af_xtenant_code: %v (expected a non-unique index to exist)", err)
	}
	if nonUnique != 1 {
		t.Fatalf("idx_obj_af_xtenant_code NON_UNIQUE = %d; want 1 (non-unique)", nonUnique)
	}
}
