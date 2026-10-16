//go:build integration

// Repository-level tenant re-pin suite (A2 defense-in-depth, card t_b2895c63).
//
// Run with:
//
//	go test -tags=integration ./internal/metadata/
//
// Requires a reachable MySQL 8 with the sys_* tables migrated INCLUDING migration 000004
// (sys_fields.tenant_id). docker-compose dev DB.
//
// The handler/forge integration suite already proves that a cross-tenant field id 404s via
// the GetFieldByID JOIN (defense-by-convention: the caller resolves tenant-scoped first).
// This suite drops one layer BELOW that JOIN and proves defense-IN-depth: even if a forged
// or stale field id (a real id, but owned by ANOTHER tenant) is handed straight to the
// mutating repository methods, the `AND tenant_id = ?` re-pin makes them touch 0 rows and
// return ErrFieldNotFound — they can never mutate or soft-delete another tenant's field row.
package metadata

import (
	"context"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

const (
	repinTenantA = "11111111-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	repinTenantB = "22222222-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

// repinDSN returns the DSN of the throwaway migrated database provisioned by TestMain (see
// main_test.go). Before #84 this read TEST_DATABASE_URL directly, which broke against a db-less
// DSN ("No database selected"); TestMain now creates+migrates a throwaway DB and points testDSN
// at it. invRegDSN (repository_inverse_registry_integration_test.go) reuses this same helper, so
// fixing it here also fixes that suite (and repository_rm21_rel_sweep_integration_test.go, which
// reuses newInvRegRepo). rm18's own suite self-provisions independently and is unaffected.
func repinDSN() string {
	return testDSN
}

// newRepinRepo opens a GORM repository against the dev DB, skipping the suite when MySQL is
// unreachable. It returns the repo plus the raw *gorm.DB for fixture seeding/inspection.
func newRepinRepo(t *testing.T) (*Repository, *gorm.DB) {
	t.Helper()
	gdb, err := gorm.Open(gormmysql.Open(repinDSN()), &gorm.Config{})
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil || sqlDB.Ping() != nil {
		t.Skipf("MySQL not pingable — skipping")
	}
	return NewRepository(gdb), gdb
}

// seedRepinFixture creates, for EACH of tenant A and tenant B, an entity + one mutable text
// field, all sharing a deterministic name. It returns the two tenants' field ids and
// registers cleanup. Both entities carry their tenant_id; both fields are backfilled with
// the owning tenant's id (mirroring migration 000004 + the CreateField tenant set).
func seedRepinFixture(t *testing.T, gdb *gorm.DB) (fieldA, fieldB string) {
	t.Helper()
	const entName = "it_repin_widgets"

	cleanup := func() {
		gdb.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		          WHERE e.name = ? AND e.tenant_id IN (?, ?)`, entName, repinTenantA, repinTenantB)
		gdb.Exec(`DELETE FROM sys_entities WHERE name = ? AND tenant_id IN (?, ?)`,
			entName, repinTenantA, repinTenantB)
	}
	cleanup()
	t.Cleanup(cleanup)

	now := time.Now()
	// IDs must fit varchar(36); use short deterministic ids keyed by a tenant tag.
	seed := func(tag, tenant string) (entID, fldID string) {
		entID = "repin-ent-" + tag
		fldID = "repin-fld-" + tag
		ent := &EntityDefinition{
			ID: entID, TenantID: tenant, Name: entName, Label: "Repin Widget",
			DataTable: "obj_" + entName, CreatedAt: now, UpdatedAt: now,
		}
		fld := &FieldDefinition{
			ID: fldID, EntityID: entID, TenantID: tenant, Name: "status",
			Label: "Trạng thái", FieldType: "text", DefaultVal: "orig-" + tag,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := gdb.Create(ent).Error; err != nil {
			t.Fatalf("seed entity for %s: %v", tenant, err)
		}
		if err := gdb.Create(fld).Error; err != nil {
			t.Fatalf("seed field for %s: %v", tenant, err)
		}
		return entID, fldID
	}
	_, fieldA = seed("a", repinTenantA)
	_, fieldB = seed("b", repinTenantB)
	return fieldA, fieldB
}

// TestRepo_UpdateFieldMutable_TenantRepinBlocksForgedID proves UpdateFieldMutable re-pins
// tenant directly: with a REAL field id that belongs to tenant A, but the WRONG tenant (B)
// passed in, the WHERE `tenant_id = ?` matches 0 rows → ErrFieldNotFound, and tenant A's row
// is NOT mutated. The right-tenant call still updates exactly that row.
func TestRepo_UpdateFieldMutable_TenantRepinBlocksForgedID(t *testing.T) {
	repo, gdb := newRepinRepo(t)
	fieldA, _ := seedRepinFixture(t, gdb)
	ctx := context.Background()

	// Forged: A's id but B's tenant. The re-pin must reject it (0 rows → ErrFieldNotFound).
	forged := &FieldDefinition{ID: fieldA, Label: "HACKED", DefaultVal: "hacked"}
	if err := repo.UpdateFieldMutable(ctx, repinTenantB, forged); err != ErrFieldNotFound {
		t.Fatalf("cross-tenant UpdateFieldMutable: err = %v; want ErrFieldNotFound", err)
	}

	// A's row must be untouched by the forged update.
	var afterForge FieldDefinition
	if err := gdb.First(&afterForge, "id = ?", fieldA).Error; err != nil {
		t.Fatalf("reload A field: %v", err)
	}
	if afterForge.Label == "HACKED" || afterForge.DefaultVal == "hacked" {
		t.Fatalf("forged cross-tenant update LANDED on tenant A's field: label=%q default=%q",
			afterForge.Label, afterForge.DefaultVal)
	}

	// Right tenant updates exactly that row.
	ok := &FieldDefinition{ID: fieldA, Label: "Updated", DefaultVal: "new"}
	if err := repo.UpdateFieldMutable(ctx, repinTenantA, ok); err != nil {
		t.Fatalf("same-tenant UpdateFieldMutable: %v", err)
	}
	var afterOK FieldDefinition
	if err := gdb.First(&afterOK, "id = ?", fieldA).Error; err != nil {
		t.Fatalf("reload A field after ok update: %v", err)
	}
	if afterOK.Label != "Updated" || afterOK.DefaultVal != "new" {
		t.Fatalf("same-tenant update did not land: label=%q default=%q", afterOK.Label, afterOK.DefaultVal)
	}
}

// TestRepo_SoftDeleteField_TenantRepinBlocksForgedID proves SoftDeleteField re-pins tenant
// directly: A's real field id with the WRONG tenant (B) deletes 0 rows → ErrFieldNotFound,
// and A's field stays active. The right-tenant call soft-deletes exactly that row.
func TestRepo_SoftDeleteField_TenantRepinBlocksForgedID(t *testing.T) {
	repo, gdb := newRepinRepo(t)
	fieldA, _ := seedRepinFixture(t, gdb)
	ctx := context.Background()

	// Forged: A's id, B's tenant → 0 rows → ErrFieldNotFound.
	if err := repo.SoftDeleteField(ctx, repinTenantB, fieldA); err != ErrFieldNotFound {
		t.Fatalf("cross-tenant SoftDeleteField: err = %v; want ErrFieldNotFound", err)
	}

	// A's field must still be active (deleted_at IS NULL).
	var stillActive int64
	gdb.Model(&FieldDefinition{}).Where("id = ?", fieldA).Count(&stillActive)
	if stillActive != 1 {
		t.Fatalf("forged cross-tenant delete affected tenant A's field: active count = %d; want 1", stillActive)
	}

	// Right tenant soft-deletes exactly that row.
	if err := repo.SoftDeleteField(ctx, repinTenantA, fieldA); err != nil {
		t.Fatalf("same-tenant SoftDeleteField: %v", err)
	}
	var afterDelete int64
	gdb.Model(&FieldDefinition{}).Where("id = ?", fieldA).Count(&afterDelete)
	if afterDelete != 0 {
		t.Fatalf("same-tenant soft delete did not hide the row: active count = %d; want 0", afterDelete)
	}
	// And the row physically survives (soft delete, not hard delete).
	var raw int64
	gdb.Unscoped().Model(&FieldDefinition{}).Where("id = ?", fieldA).Count(&raw)
	if raw != 1 {
		t.Fatalf("soft delete removed the physical row: raw count = %d; want 1", raw)
	}
}

// TestRepo_CreateField_SetsTenantID proves CreateField persists tenant_id on the new row, so
// every new sys_fields row carries the owning tenant directly (not only via the JOIN).
func TestRepo_CreateField_SetsTenantID(t *testing.T) {
	repo, gdb := newRepinRepo(t)
	fieldA, _ := seedRepinFixture(t, gdb)
	_ = fieldA
	ctx := context.Background()

	const newID = "repin-fld-new"
	gdb.Exec("DELETE FROM sys_fields WHERE id = ?", newID)
	t.Cleanup(func() { gdb.Exec("DELETE FROM sys_fields WHERE id = ?", newID) })

	fd := &FieldDefinition{
		ID: newID, EntityID: "repin-ent-a", TenantID: repinTenantA,
		Name: "extra", Label: "Extra", FieldType: "text",
	}
	if err := repo.CreateField(ctx, fd); err != nil {
		t.Fatalf("CreateField: %v", err)
	}
	var got string
	if err := gdb.Raw("SELECT tenant_id FROM sys_fields WHERE id = ?", newID).Scan(&got).Error; err != nil {
		t.Fatalf("read back tenant_id: %v", err)
	}
	if got != repinTenantA {
		t.Fatalf("CreateField stored tenant_id = %q; want %q", got, repinTenantA)
	}
}
