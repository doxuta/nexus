//go:build integration

// DOCTOR-CORE integration tests — real MySQL 8 over the o3a throwaway-DB harness (one
// TEST_DATABASE_URL source for gorm + sqlx + migrate, fully migrated per test). Proofs a
// unit fake cannot give:
//  1. The B1/B2/C1/C2 detect SQL really matches deliberately-seeded drift rows (a phantom
//     sys_relationships row, an orphan 1-N registry row, a layout carrying a dead field ref,
//     a layout of a tombstoned entity, an orphan nav row) — and NOTHING else.
//  2. Fix-all → re-scan returns empty (the QRR idempotent re-run contract) and the DB rows
//     really changed shape (tombstone-renamed + soft-deleted registry rows, pruned
//     layout_json, hard-deleted layout, purged nav rows).
//  3. 2-tenant isolation: tenant B's identical drift is untouched by tenant A's fix.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestDoctorInt -race -v
package services

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/doctor"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

const (
	drTenantA = "d0c70a0a-0000-0000-0000-00000000000a"
	drTenantB = "d0c70b0b-0000-0000-0000-00000000000b"
)

// drSeed is the per-tenant drift fixture: ids of the seeded rows so assertions can target them.
type drSeed struct {
	entityID   string // live entity "dr_accounts"
	relPhantom string // B1: N-N row whose parent entity does not exist
	relOrphan  string // B2: 1-N row whose owning relation field does not exist
	layDirty   string // C1: layout of dr_accounts carrying a dead field ref
	layDead    string // C1: layout of a tombstoned entity
	navOrphan  string // C2: nav row pointing at a dead entity id
	deadRefID  string // the dead entity id the nav row points at
}

// drSeedTenant plants one full drift set for a tenant. All rows are ACTIVE (deleted_at NULL);
// the drift is in what they reference.
func drSeedTenant(t *testing.T, db *sqlx.DB, tenantID string) drSeed {
	t.Helper()
	ctx := context.Background()
	s := drSeed{
		entityID:   uuid.NewString(),
		relPhantom: uuid.NewString(),
		relOrphan:  uuid.NewString(),
		layDirty:   uuid.NewString(),
		layDead:    uuid.NewString(),
		navOrphan:  uuid.NewString(),
		deadRefID:  uuid.NewString(),
	}
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed %s: %v", tenantID, err)
		}
	}

	// Live entity + one live text field (the C1 allowlist). The PHYSICAL obj_dr_accounts
	// table is materialized too (idempotent across the two tenants — shared table), so the
	// DOCTOR-SCHEMA checks (A1/A2a/A5/A6) see a healthy schema and this fixture stays a
	// soft-metadata-only drift set.
	mustExec(`INSERT INTO sys_entities (id, tenant_id, name, label, table_name) VALUES (?, ?, 'dr_accounts', 'DR Accounts', 'obj_dr_accounts')`,
		s.entityID, tenantID)
	mustExec(`INSERT INTO sys_fields (id, entity_id, tenant_id, name, label, field_type) VALUES (?, ?, ?, 'name', 'Name', 'text')`,
		uuid.NewString(), s.entityID, tenantID)
	if err := dynrepo.New(db).CreateTable(ctx, "dr_accounts", []ddl.FieldDef{{Name: "name", FieldType: "text"}}); err != nil {
		t.Fatalf("seed %s: create obj_dr_accounts: %v", tenantID, err)
	}

	// B1: phantom relationship — parent entity "dr_ghost" has no active sys_entities row.
	mustExec(`INSERT INTO sys_relationships (id, tenant_id, name, parent_entity_name, child_entity_name, relationship_type, join_table_name)
		VALUES (?, ?, 'dr_phantom', 'dr_ghost', 'dr_accounts', 'N-N', 'rel_dr_phantom')`,
		s.relPhantom, tenantID)

	// B2: orphan 1-N registry row — both entities alive (self-referential), but no active
	// `relation` field named "boss" exists on dr_accounts (DeleteField step-3 window).
	mustExec(`INSERT INTO sys_relationships (id, tenant_id, name, parent_entity_name, child_entity_name, relationship_type, inverse_field, child_fk_column)
		VALUES (?, ?, 'dr_accounts_boss', 'dr_accounts', 'dr_accounts', '1-N', 'reports', 'boss_id')`,
		s.relOrphan, tenantID)

	// C1: layout referencing a dead field ("ghost") + a layout of a tombstoned entity.
	mustExec(`INSERT INTO sys_layouts (id, tenant_id, entity_name, view_type, layout_json)
		VALUES (?, ?, 'dr_accounts', 'list', '{"columns":[{"field":"name"},{"field":"ghost"}]}')`,
		s.layDirty, tenantID)
	mustExec(`INSERT INTO sys_layouts (id, tenant_id, entity_name, view_type, layout_json)
		VALUES (?, ?, 'dr_gone', 'edit', '{"sections":[{"title":"a","fields":["x"]}]}')`,
		s.layDead, tenantID)

	// C2: app + nav row whose ref_id resolves to no active entity.
	appID := uuid.NewString()
	mustExec(`INSERT INTO sys_apps (id, tenant_id, name, label) VALUES (?, ?, 'dr_app', 'DR App')`, appID, tenantID)
	mustExec(`INSERT INTO sys_app_nav_items (id, tenant_id, app_id, item_type, ref_id, sort_order) VALUES (?, ?, ?, 'entity', ?, 0)`,
		s.navOrphan, tenantID, appID, s.deadRefID)
	// A HEALTHY nav row too — the fix must purge only the orphan ref, never the live one.
	mustExec(`INSERT INTO sys_app_nav_items (id, tenant_id, app_id, item_type, ref_id, sort_order) VALUES (?, ?, ?, 'entity', ?, 1)`,
		uuid.NewString(), tenantID, appID, s.entityID)

	return s
}

// drWire builds the real doctor service over a fresh, fully-migrated throwaway DB.
func drWire(t *testing.T) (*DoctorService, *sqlx.DB) {
	t.Helper()
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	layoutRepo := metadata.NewLayoutRepository(gdb)
	svc := NewDoctorService(doctor.NewSQLStore(sdb), dynrepo.New(sdb), metaRepo, metaRepo, layoutRepo, nil, nil)
	return svc, sdb
}

// drScanCounts scans and indexes count by check id.
func drScanCounts(t *testing.T, svc *DoctorService, tenantID string) map[string]doctor.CheckResult {
	t.Helper()
	results, err := svc.Scan(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("Scan(%s): %v", tenantID, err)
	}
	out := make(map[string]doctor.CheckResult, len(results))
	for _, r := range results {
		out[r.CheckID] = r
	}
	return out
}

var drAllChecks = []string{
	DoctorCheckPhantomRelationships, DoctorCheckOrphanRelationRows,
	DoctorCheckLayoutDeadFields, DoctorCheckOrphanNavItems,
}

func TestDoctorInt_ScanFixRescanAndTenantIsolation(t *testing.T) {
	svc, db := drWire(t)
	ctx := context.Background()
	seedA := drSeedTenant(t, db, drTenantA)
	seedB := drSeedTenant(t, db, drTenantB)

	// --- 1. Scan tenant A finds exactly the seeded drift. ------------------------------------
	scanA := drScanCounts(t, svc, drTenantA)
	if got := scanA[DoctorCheckPhantomRelationships]; got.Count != 1 || got.Items[0].Key != seedA.relPhantom {
		t.Fatalf("B1 = %+v; want 1 finding key %s", got, seedA.relPhantom)
	}
	if got := scanA[DoctorCheckOrphanRelationRows]; got.Count != 1 || got.Items[0].Key != seedA.relOrphan {
		t.Fatalf("B2 = %+v; want 1 finding key %s", got, seedA.relOrphan)
	}
	if got := scanA[DoctorCheckLayoutDeadFields]; got.Count != 2 {
		t.Fatalf("C1 = %+v; want 2 findings (dirty + dead-entity layout)", got)
	}
	if got := scanA[DoctorCheckOrphanNavItems]; got.Count != 1 || got.Items[0].Key != "entity:"+seedA.deadRefID {
		t.Fatalf("C2 = %+v; want 1 finding key entity:%s", got, seedA.deadRefID)
	}

	// --- 2. Fix all four checks for tenant A. -------------------------------------------------
	results, err := svc.Fix(ctx, drTenantA, drAllChecks, false)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	fixedByID := map[string]doctor.FixResult{}
	for _, r := range results {
		fixedByID[r.CheckID] = r
		if r.Failed != 0 {
			t.Fatalf("check %s failed items: %v", r.CheckID, r.Errors)
		}
	}
	if fixedByID[DoctorCheckPhantomRelationships].Fixed != 1 ||
		fixedByID[DoctorCheckOrphanRelationRows].Fixed != 1 ||
		fixedByID[DoctorCheckLayoutDeadFields].Fixed != 2 ||
		fixedByID[DoctorCheckOrphanNavItems].Fixed != 1 {
		t.Fatalf("fixed counts = %+v; want B1=1 B2=1 C1=2 C2=1", results)
	}

	// --- 3. Re-scan tenant A → empty (QRR idempotent contract). -------------------------------
	for id, r := range drScanCounts(t, svc, drTenantA) {
		if r.Count != 0 {
			t.Fatalf("re-scan %s = %+v; want clean", id, r)
		}
	}
	// A second fix run finds nothing to do.
	results, err = svc.Fix(ctx, drTenantA, drAllChecks, false)
	if err != nil {
		t.Fatalf("second Fix: %v", err)
	}
	for _, r := range results {
		if r.Fixed != 0 || r.Failed != 0 {
			t.Fatalf("second fix %s = %+v; want all-zero", r.CheckID, r)
		}
	}

	// --- 4. DB state really changed shape for tenant A. ---------------------------------------
	// B1/B2 rows: soft-deleted AND tombstone-renamed (unique slots freed — the 000038 semantic).
	for _, relID := range []string{seedA.relPhantom, seedA.relOrphan} {
		var row struct {
			Name      string  `db:"name"`
			DeletedAt *string `db:"deleted_at"`
		}
		if err := db.Get(&row, `SELECT name, deleted_at FROM sys_relationships WHERE id = ?`, relID); err != nil {
			t.Fatalf("read rel %s: %v", relID, err)
		}
		if row.DeletedAt == nil || !strings.Contains(row.Name, "__deleted_") {
			t.Fatalf("rel %s = %+v; want soft-deleted + tombstone-renamed", relID, row)
		}
	}
	// C1 dirty layout: persisted JSON no longer mentions the dead field.
	var layoutJSON string
	if err := db.Get(&layoutJSON, `SELECT layout_json FROM sys_layouts WHERE id = ?`, seedA.layDirty); err != nil {
		t.Fatalf("read layout: %v", err)
	}
	if strings.Contains(layoutJSON, "ghost") || !strings.Contains(layoutJSON, "name") {
		t.Fatalf("pruned layout_json = %s; want 'ghost' dropped, 'name' kept", layoutJSON)
	}
	// C1 dead-entity layout: row hard-deleted.
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM sys_layouts WHERE id = ?`, seedA.layDead); err != nil || n != 0 {
		t.Fatalf("dead-entity layout rows = %d (err %v); want 0 (hard delete)", n, err)
	}
	// C2: orphan nav row purged, healthy nav row survives.
	if err := db.Get(&n, `SELECT COUNT(*) FROM sys_app_nav_items WHERE tenant_id = ? AND ref_id = ?`, drTenantA, seedA.deadRefID); err != nil || n != 0 {
		t.Fatalf("orphan nav rows = %d (err %v); want 0", n, err)
	}
	if err := db.Get(&n, `SELECT COUNT(*) FROM sys_app_nav_items WHERE tenant_id = ? AND ref_id = ?`, drTenantA, seedA.entityID); err != nil || n != 1 {
		t.Fatalf("healthy nav rows = %d (err %v); want 1 (fix must not overreach)", n, err)
	}

	// --- 5. Tenant isolation: B's identical drift is COMPLETELY untouched. --------------------
	scanB := drScanCounts(t, svc, drTenantB)
	if scanB[DoctorCheckPhantomRelationships].Count != 1 ||
		scanB[DoctorCheckOrphanRelationRows].Count != 1 ||
		scanB[DoctorCheckLayoutDeadFields].Count != 2 ||
		scanB[DoctorCheckOrphanNavItems].Count != 1 {
		t.Fatalf("tenant B scan = %+v; want B's drift fully intact after A's fix", scanB)
	}
	var bRel struct {
		Name      string  `db:"name"`
		DeletedAt *string `db:"deleted_at"`
	}
	if err := db.Get(&bRel, `SELECT name, deleted_at FROM sys_relationships WHERE id = ?`, seedB.relPhantom); err != nil {
		t.Fatalf("read tenant B rel: %v", err)
	}
	if bRel.DeletedAt != nil || bRel.Name != "dr_phantom" {
		t.Fatalf("tenant B phantom = %+v; want untouched (active, original name)", bRel)
	}
	if err := db.Get(&n, `SELECT COUNT(*) FROM sys_app_nav_items WHERE tenant_id = ? AND ref_id = ?`, drTenantB, seedB.deadRefID); err != nil || n != 1 {
		t.Fatalf("tenant B orphan nav rows = %d (err %v); want 1 (untouched)", n, err)
	}
}

// TestDoctorInt_ScanIsReadOnly proves the scan mutates NOTHING (the rebuildAuditTables
// anti-lesson): checksum the drifted registry/layout/nav rows before and after a scan.
func TestDoctorInt_ScanIsReadOnly(t *testing.T) {
	svc, db := drWire(t)
	drSeedTenant(t, db, drTenantA)

	snapshot := func() (rels, layouts, navs string) {
		t.Helper()
		// GROUP_CONCAT over the ordered row images is a cheap whole-table fingerprint.
		if err := db.Get(&rels, `SELECT COALESCE(GROUP_CONCAT(CONCAT_WS('|', id, name, COALESCE(deleted_at,'-')) ORDER BY id), '') FROM sys_relationships WHERE tenant_id = ?`, drTenantA); err != nil {
			t.Fatalf("snapshot rels: %v", err)
		}
		if err := db.Get(&layouts, `SELECT COALESCE(GROUP_CONCAT(CONCAT_WS('|', id, layout_json, COALESCE(deleted_at,'-')) ORDER BY id), '') FROM sys_layouts WHERE tenant_id = ?`, drTenantA); err != nil {
			t.Fatalf("snapshot layouts: %v", err)
		}
		if err := db.Get(&navs, `SELECT COALESCE(GROUP_CONCAT(id ORDER BY id), '') FROM sys_app_nav_items WHERE tenant_id = ?`, drTenantA); err != nil {
			t.Fatalf("snapshot navs: %v", err)
		}
		return
	}

	r1, l1, n1 := snapshot()
	if _, err := svc.Scan(context.Background(), drTenantA); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	r2, l2, n2 := snapshot()
	if r1 != r2 || l1 != l2 || n1 != n2 {
		t.Fatalf("scan mutated state:\nrels  %q → %q\nlayouts %q → %q\nnavs %q → %q", r1, r2, l1, l2, n1, n2)
	}
}
