//go:build integration

// Integration test for bug3: a SHARED obj_<name> table reconcile. obj_* tables are keyed by
// entity NAME, not by tenant, so two tenants creating an entity with the same name share one
// physical table. The second creator's `CREATE TABLE IF NOT EXISTS` is a no-op — so a
// unique/relation field it declares that the first creator lacked would silently never get
// its physical column (uniqueness never enforced / FK column missing). EntityService.CreateEntity
// must reconcile by ALTERing the missing columns in.
//
// Run with: go test -tags=integration ./internal/services/
// Requires a reachable MySQL 8 (docker compose `mysql` service / nexus-mysql).
package services

import (
	"context"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// reconcileTestDB used to connect to the raw TEST_DATABASE_URL as-is (harness bug #84: broke
// against a db-less DSN with "Error 1046 (3D000): No database selected"). It now reuses the
// package's own self-provisioning gold helper (o3aSetup, metadata_cache_wired_integration_test.go)
// so this suite gets a throwaway migrated DB like every other services integration test.
func reconcileTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	_, sdb := o3aSetup(t)
	return sdb
}

func columnExistsIT(t *testing.T, db *sqlx.DB, table, column string) bool {
	t.Helper()
	var n int
	if err := db.Get(&n,
		`SELECT COUNT(*) FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		table, column); err != nil {
		t.Fatalf("query column existence: %v", err)
	}
	return n > 0
}

// memMeta is an in-memory MetadataWriter so the integration test exercises the REAL DDL path
// (dynrepo against MySQL) without depending on the sys_* schema being migrated. It records
// per-tenant entity names so EntityExists is tenant-scoped, matching production semantics.
type memMeta struct {
	// names[tenantID][entityName] = true
	names map[string]map[string]bool
}

func newMemMeta() *memMeta { return &memMeta{names: map[string]map[string]bool{}} }

func (m *memMeta) EntityExists(_ context.Context, tenantID, name string) (bool, error) {
	return m.names[tenantID][name], nil
}

// NameClaimedByAnyTenant is the UNSCOPED question the CreateEntity compensation path asks before
// dropping a shared obj_* table: does anyone own this name right now?
func (m *memMeta) NameClaimedByAnyTenant(_ context.Context, name string) (bool, error) {
	for _, byName := range m.names {
		if byName[name] {
			return true, nil
		}
	}
	return false, nil
}

// GetEntityByName resolves a previously-created entity for the tenant (tenant-scoped), or
// metadata.ErrEntityNotFound. Backs CreateEntity's relation-target existence check.
func (m *memMeta) GetEntityByName(_ context.Context, tenantID, name string) (*metadata.EntityDefinition, error) {
	if m.names[tenantID][name] {
		return &metadata.EntityDefinition{TenantID: tenantID, Name: name}, nil
	}
	return nil, metadata.ErrEntityNotFound
}

func (m *memMeta) CreateEntityWithFields(_ context.Context, e *metadata.EntityDefinition, _ []metadata.FieldDefinition, _ []metadata.RelationshipDefinition) error {
	if m.names[e.TenantID] == nil {
		m.names[e.TenantID] = map[string]bool{}
	}
	m.names[e.TenantID][e.Name] = true
	return nil
}

// ListFields / GetRelationshipByName satisfy the extended MetadataWriter interface
// (ENG-COMPUTED #3); this test never declares a rollup field, so both are inert here.
func (m *memMeta) ListFields(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	return nil, nil
}

func (m *memMeta) GetRelationshipByName(_ context.Context, _, _ string) (*metadata.RelationshipDefinition, error) {
	return nil, metadata.ErrRelationshipNotFound
}

// GetRelationshipByInverseField satisfies the extended MetadataWriter interface (ENG-REL2
// #4); this test never declares a relation field, so it is inert here (always "available").
func (m *memMeta) GetRelationshipByInverseField(_ context.Context, _, _, _ string) (*metadata.RelationshipDefinition, error) {
	return nil, metadata.ErrRelationshipNotFound
}

func TestCreateEntity_SharedTableReconcile_Integration(t *testing.T) {
	db := reconcileTestDB(t)
	dyn := dynrepo.New(db)
	ctx := context.Background()

	const entity = "it_shared_x"
	const table = "obj_it_shared_x"

	_ = dyn.DropTable(ctx, entity) // clean slate from any prior failed run
	t.Cleanup(func() { _ = dyn.DropTable(ctx, entity) })

	meta := newMemMeta()
	svc := NewEntityService(dyn, meta)

	// Tenant A creates entity "it_shared_x" with a unique field `e` and a relation field `m`.
	// First, register a relation target so the relation field's options.relatedEntity passes
	// validation. (validateFieldConfig only checks identifier shape, not existence here at the
	// orchestration layer for CreateEntity, but we keep the target name well-formed.)
	inA := CreateEntityInput{
		Name:  entity,
		Label: "Shared X (tenant A)",
		Fields: []FieldInput{
			{Name: "title", Label: "Title", FieldType: "text", IsRequired: true},
			{Name: "e", Label: "Email", FieldType: "text", IsUnique: true},
		},
	}
	if _, _, err := svc.CreateEntity(ctx, "tenant-A", inA); err != nil {
		t.Fatalf("tenant A CreateEntity: %v", err)
	}
	if !columnExistsIT(t, db, table, "e") {
		t.Fatalf("after tenant A: unique column %q missing on %s", "e", table)
	}
	if columnExistsIT(t, db, table, "c") {
		t.Fatalf("pre-condition: column %q must NOT exist yet on %s", "c", table)
	}

	// Tenant B creates an entity with the SAME name but a DIFFERENT unique field `c`.
	// CREATE TABLE IF NOT EXISTS is a no-op on the shared table; reconcile must ADD `c`.
	inB := CreateEntityInput{
		Name:  entity,
		Label: "Shared X (tenant B)",
		Fields: []FieldInput{
			{Name: "title", Label: "Title", FieldType: "text", IsRequired: true},
			{Name: "c", Label: "Code", FieldType: "text", IsUnique: true},
		},
	}
	if _, _, err := svc.CreateEntity(ctx, "tenant-B", inB); err != nil {
		t.Fatalf("tenant B CreateEntity: %v", err)
	}

	// THE BUG3 ASSERTION: the unique column `c` from tenant B MUST have been ALTERed in.
	if !columnExistsIT(t, db, table, "c") {
		t.Errorf("bug3: tenant B's unique column %q was NOT reconciled onto shared table %s", "c", table)
	}
	// Tenant A's column survives untouched.
	if !columnExistsIT(t, db, table, "e") {
		t.Errorf("tenant A's unique column %q disappeared from %s", "e", table)
	}

	// Re-running tenant B (column now present) must NOT error trying to re-ADD `c`.
	meta2 := newMemMeta()
	svc2 := NewEntityService(dyn, meta2)
	if _, _, err := svc2.CreateEntity(ctx, "tenant-C", inB); err != nil {
		t.Fatalf("tenant C CreateEntity (column c already present, must be skipped): %v", err)
	}
}

// TestCreateEntity_SharedUniqueDuplicate_DoesNotBlock_Integration is the RM-28 Slice B retarget of
// the retired CROSS_TENANT_SHARED_UNIQUE classification (bug #1/#8/#10; dispatch §4.1 — "its
// role is fully replaced by the pivot's intra-tenant RECORD_DUPLICATE"): on a SHARED obj_* table,
// tenant A declares `code` as an ORDINARY field and writes two rows with the SAME code value;
// then tenant B (same entity name) declares `code` as UNIQUE. Reconcile's ALTER now materializes
// a NON-unique index (AddUniqueGencolNonUnique) over the whole shared table — it cannot 1062 no
// matter what data A holds, so CreateEntity for tenant B must SUCCEED cleanly (pre-Slice-B this
// exact setup returned 409 CROSS_TENANT_SHARED_UNIQUE via the old ADD UNIQUE KEY's own 1062).
// Tenant B's own pivot is empty (B has zero live `code` rows of its own) and the physical index
// is non-unique — proving A's pre-existing intra-tenant duplicate neither blocks nor corrupts B's
// unrelated field registration; the shared table is left intact either way.
func TestCreateEntity_SharedUniqueDuplicate_DoesNotBlock_Integration(t *testing.T) {
	db := reconcileTestDB(t)
	dyn := dynrepo.New(db)
	ctx := context.Background()

	const entity = "it_shared_dup"
	const table = "obj_it_shared_dup"

	_ = dyn.DropTable(ctx, entity) // clean slate
	t.Cleanup(func() { _ = dyn.DropTable(ctx, entity) })

	meta := newMemMeta()
	svc := NewEntityService(dyn, meta)

	// Tenant A: entity with `code` as an ORDINARY (non-unique) field.
	inA := CreateEntityInput{
		Name:  entity,
		Label: "Shared dup (tenant A)",
		Fields: []FieldInput{
			{Name: "title", Label: "Title", FieldType: "text", IsRequired: true},
			{Name: "code", Label: "Code", FieldType: "text"}, // NOT unique for A
		},
	}
	if _, _, err := svc.CreateEntity(ctx, "tenant-A", inA); err != nil {
		t.Fatalf("tenant A CreateEntity: %v", err)
	}
	// Tenant A writes two rows with the SAME `code` — legal for A (no unique constraint).
	if _, err := dyn.InsertRecord(ctx, entity, "tenant-A", "user-A", map[string]any{"title": "r1", "code": "DUP"}, nil); err != nil {
		t.Fatalf("tenant A insert row 1: %v", err)
	}
	if _, err := dyn.InsertRecord(ctx, entity, "tenant-A", "user-A", map[string]any{"title": "r2", "code": "DUP"}, nil); err != nil {
		t.Fatalf("tenant A insert row 2: %v", err)
	}

	// Tenant B: SAME entity name, but declares `code` as UNIQUE. Reconcile now runs
	// AddUniqueGencolNonUnique — a non-unique ADD INDEX — which cannot 1062 regardless of tenant
	// A's data, so CreateEntity must succeed cleanly.
	inB := CreateEntityInput{
		Name:  entity,
		Label: "Shared dup (tenant B)",
		Fields: []FieldInput{
			{Name: "title", Label: "Title", FieldType: "text", IsRequired: true},
			{Name: "code", Label: "Code", FieldType: "text", IsUnique: true},
		},
	}
	_, _, err := svc.CreateEntity(ctx, "tenant-B", inB)
	if err != nil {
		t.Fatalf("tenant B CreateEntity: %v; want success — A's intra-tenant dupe must not block B (RM-28 decoupling)", err)
	}
	// The shared table is intact and `code`'s generated column now exists (reconciled in for B).
	if !columnExistsIT(t, db, table, "data") {
		t.Errorf("shared table %s appears damaged (data column missing)", table)
	}
	if !columnExistsIT(t, db, table, "code") {
		t.Errorf("reconciled unique column %q must exist on %s after a successful CreateEntity", "code", table)
	}
	// The physical index is non-unique and tenant B's own pivot is empty (B has zero live `code`
	// rows — only tenant A's pre-existing rows share the "DUP" value, and A never declared the
	// field unique, so nothing of A's was ever eligible for B's pivot).
	var nonUnique int
	if err := db.Get(&nonUnique, `SELECT NON_UNIQUE FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ? LIMIT 1`,
		table, "idx_"+table+"_code"); err != nil {
		t.Fatalf("query idx_%s_code: %v (expected a non-unique index to exist)", table, err)
	}
	if nonUnique != 1 {
		t.Fatalf("idx_%s_code NON_UNIQUE = %d; want 1 (non-unique)", table, nonUnique)
	}
	var pivotCount int
	if err := db.Get(&pivotCount, "SELECT COUNT(*) FROM `sys_unique_index` WHERE `tenant_id` = ? AND `entity_name` = ? AND `field_name` = ?",
		"tenant-B", entity, "code"); err != nil {
		t.Fatalf("query tenant B pivot: %v", err)
	}
	if pivotCount != 0 {
		t.Fatalf("tenant B pivot count = %d; want 0 (B has no live code rows)", pivotCount)
	}
}
