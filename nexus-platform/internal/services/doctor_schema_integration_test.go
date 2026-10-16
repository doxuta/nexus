//go:build integration

// DOCTOR-SCHEMA integration tests — real MySQL 8 over the o3a throwaway-DB harness. Proofs a
// unit fake cannot give:
//  1. Real drift (hand-DROPped generated columns, un-guarded unique column, dropped/mis-shaped
//     mandatory indexes, DROPped obj_/rel_ tables) is detected by the real information_schema
//     queries — each in ITS class and nothing else.
//  2. Auto-fix (A1/A2a/A5) → re-scan clean; A6/B4 refuse without confirm=true then recreate
//     with it; record/link INSERTs work again on the recreated tables.
//  3. RM-47 closure: the mis-shaped idx_created_at really becomes the composite
//     (tenant_id, deleted_at, created_at, id) — asserted against information_schema.STATISTICS.
//  4. SHARED-TABLE cross-tenant benefit: tenant A's fix heals tenant B's identical findings
//     (B's re-scan goes clean) and does NOT corrupt B's data (B's row survives, its unique
//     generated column recomputes from the untouched JSON).
//  5. The 1062 backfill dead-end is a per-item FAILED verdict carrying a stable public
//     message (sanitized — no driver internals), never a retry loop. Post RM-28 Slice B a
//     plain unique field's A1 repair can no longer 1062 (its index is non-unique) — the one
//     remaining path is a 1-1 relation's cardinality index (FIELD_RELATION_CARDINALITY_CONFLICT).
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestDoctorSchemaInt -race -v
package services

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/dynrepo"
)

const (
	dsTenantA = "d5c70a0a-0000-0000-0000-00000000000a"
	dsTenantB = "d5c70b0b-0000-0000-0000-00000000000b"
)

// dsExec is a t.Fatalf-on-error exec helper.
func dsExec(t *testing.T, db *sqlx.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("exec %s: %v", q, err)
	}
}

// dsSeedField inserts one sys_fields row (options may be "" for NULL).
func dsSeedField(t *testing.T, db *sqlx.DB, tenantID, entityID, name, fieldType string, isUnique bool, options string) string {
	t.Helper()
	id := uuid.NewString()
	var opts any
	if options != "" {
		opts = options
	}
	dsExec(t, db, `INSERT INTO sys_fields (id, entity_id, tenant_id, name, label, field_type, is_unique, options)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, entityID, tenantID, name, name, fieldType, isUnique, opts)
	return id
}

// dsSeedEntity inserts one active sys_entities row and returns its id.
func dsSeedEntity(t *testing.T, db *sqlx.DB, tenantID, name string) string {
	t.Helper()
	id := uuid.NewString()
	dsExec(t, db, `INSERT INTO sys_entities (id, tenant_id, name, label, table_name) VALUES (?, ?, ?, ?, ?)`,
		id, tenantID, name, name, "obj_"+name)
	return id
}

// dsTableExists probes information_schema for a physical table.
func dsTableExists(t *testing.T, db *sqlx.DB, table string) bool {
	t.Helper()
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table); err != nil {
		t.Fatalf("dsTableExists(%s): %v", table, err)
	}
	return n > 0
}

func TestDoctorSchemaInt_DriftDetectFixRescanCrossTenant(t *testing.T) {
	svc, db := drWire(t)
	dyn := dynrepo.New(db)
	ctx := context.Background()

	// --- Seed: shared entity ds_accounts (tenant A full field set; tenant B email only). ------
	entA := dsSeedEntity(t, db, dsTenantA, "ds_accounts")
	dsSeedField(t, db, dsTenantA, entA, "name", "text", false, "")
	fEmailA := dsSeedField(t, db, dsTenantA, entA, "email", "text", true, "")
	fParentA := dsSeedField(t, db, dsTenantA, entA, "parent", "relation", false, `{"relatedEntity":"ds_accounts"}`)
	dsSeedField(t, db, dsTenantA, entA, "partner", "relation", false, `{"relatedEntity":"ds_accounts","cardinality":"1-1"}`)
	fScoreA := dsSeedField(t, db, dsTenantA, entA, "score", "number", false, `{"indexed":true}`)
	dsSeedField(t, db, dsTenantA, entA, "sku", "text", true, "")
	if err := dyn.CreateTable(ctx, "ds_accounts", []ddl.FieldDef{
		{Name: "name", FieldType: "text"},
		{Name: "email", FieldType: "text", IsUnique: true},
		{Name: "parent", FieldType: "relation", RelatedEntity: "ds_accounts"},
		{Name: "partner", FieldType: "relation", RelatedEntity: "ds_accounts", OneToOne: true},
		{Name: "score", FieldType: "number", Indexed: true},
		{Name: "sku", FieldType: "text", IsUnique: true},
	}); err != nil {
		t.Fatalf("create obj_ds_accounts: %v", err)
	}
	entB := dsSeedEntity(t, db, dsTenantB, "ds_accounts")
	fEmailB := dsSeedField(t, db, dsTenantB, entB, "email", "text", true, "")
	// Tenant B's live row — inserted while the schema is healthy; must survive A's repairs.
	bRowID := uuid.NewString()
	dsExec(t, db, `INSERT INTO obj_ds_accounts (id, tenant_id, created_by, data) VALUES (?, ?, ?, ?)`,
		bRowID, dsTenantB, "u-b", `{"email":"b@x.vn"}`)

	// A6 fixture: tenant-A entity whose table gets hand-DROPped.
	entTickets := dsSeedEntity(t, db, dsTenantA, "ds_tickets")
	dsSeedField(t, db, dsTenantA, entTickets, "code", "text", true, "")
	dsSeedField(t, db, dsTenantA, entTickets, "subject", "text", false, "")
	if err := dyn.CreateTable(ctx, "ds_tickets", []ddl.FieldDef{
		{Name: "code", FieldType: "text", IsUnique: true},
		{Name: "subject", FieldType: "text"},
	}); err != nil {
		t.Fatalf("create obj_ds_tickets: %v", err)
	}

	// B4 fixture: N-N ds_accounts ↔ ds_tags with its canonical join table.
	entTags := dsSeedEntity(t, db, dsTenantA, "ds_tags")
	dsSeedField(t, db, dsTenantA, entTags, "label", "text", false, "")
	if err := dyn.CreateTable(ctx, "ds_tags", []ddl.FieldDef{{Name: "label", FieldType: "text"}}); err != nil {
		t.Fatalf("create obj_ds_tags: %v", err)
	}
	joinTable, _, _, err := ddl.JoinParts("ds_accounts", "ds_tags")
	if err != nil {
		t.Fatalf("JoinParts: %v", err)
	}
	relNN := uuid.NewString()
	dsExec(t, db, `INSERT INTO sys_relationships (id, tenant_id, name, parent_entity_name, child_entity_name, relationship_type, join_table_name)
		VALUES (?, ?, 'ds_accounts_tags', 'ds_accounts', 'ds_tags', 'N-N', ?)`, relNN, dsTenantA, joinTable)
	if err := dyn.CreateJoinTable(ctx, "ds_accounts", "ds_tags"); err != nil {
		t.Fatalf("create join table: %v", err)
	}

	// --- Inject real drift with hand-DDL (the exact ops the Doctor exists to survive). --------
	// A1: three generated columns vanish (unique / 1-N relation / indexed).
	dsExec(t, db, "ALTER TABLE `obj_ds_accounts` DROP INDEX `idx_obj_ds_accounts_email`, DROP COLUMN `email`")
	dsExec(t, db, "ALTER TABLE `obj_ds_accounts` DROP INDEX `idx_obj_ds_accounts_parent_id`, DROP COLUMN `parent_id`")
	dsExec(t, db, "ALTER TABLE `obj_ds_accounts` DROP INDEX `idx_obj_ds_accounts_score`, DROP COLUMN `score`")
	// A2a: sku loses its soft-delete guard (the pre-000015 shape).
	dsExec(t, db, "ALTER TABLE `obj_ds_accounts` MODIFY COLUMN `sku` VARCHAR(255) COLLATE utf8mb4_bin GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.sku'))) STORED")
	// A5: version column gone (pre-000013), idx_updated_at gone, idx_created_at mis-shaped (RM-47).
	dsExec(t, db, "ALTER TABLE `obj_ds_accounts` DROP COLUMN `version`")
	dsExec(t, db, "ALTER TABLE `obj_ds_accounts` DROP INDEX `idx_updated_at`")
	dsExec(t, db, "ALTER TABLE `obj_ds_accounts` DROP INDEX `idx_created_at`, ADD INDEX `idx_created_at` (`created_at`)")
	// A6 + B4: whole tables gone.
	dsExec(t, db, "DROP TABLE `obj_ds_tickets`")
	dsExec(t, db, "DROP TABLE `"+joinTable+"`")

	// --- 1. Scan tenant A: every class detected, keys stable, SQL preview embedded. -----------
	scanA := drScanCounts(t, svc, dsTenantA)
	a1 := scanA[DoctorCheckMissingGenColumn]
	if a1.Count != 3 {
		t.Fatalf("A1 = %+v; want 3 (email + parent_id + score)", a1)
	}
	gotKeys := map[string]bool{}
	for _, it := range a1.Items {
		gotKeys[it.Key] = true
		if !strings.Contains(it.Detail, "— SQL: ALTER TABLE `obj_ds_accounts`") {
			t.Fatalf("A1 finding thiếu SQL preview: %s", it.Detail)
		}
	}
	for _, k := range []string{fEmailA, fParentA, fScoreA} {
		if !gotKeys[k] {
			t.Fatalf("A1 thiếu finding key %s trong %v", k, gotKeys)
		}
	}
	if got := scanA[DoctorCheckUnguardedUnique]; got.Count != 1 || !strings.Contains(got.Items[0].Detail, "MODIFY COLUMN") {
		t.Fatalf("A2a = %+v; want 1 finding (sku) kèm SQL MODIFY", got)
	}
	if got := scanA[DoctorCheckMandatoryDDL]; got.Count != 3 {
		t.Fatalf("A5 = %+v; want 3 (version + idx_updated_at + idx_created_at reshape)", got)
	}
	a6 := scanA[DoctorCheckMissingObjTable]
	if a6.Count != 1 || a6.Items[0].Key != "ds_tickets" || a6.AutoFixSafe {
		t.Fatalf("A6 = %+v; want 1 finding ds_tickets, autoFixSafe=false", a6)
	}
	if !strings.Contains(a6.Items[0].Detail, "KHÔNG quay lại") {
		t.Fatalf("A6 detail phải nêu hệ quả mất dữ liệu: %s", a6.Items[0].Detail)
	}
	b4 := scanA[DoctorCheckMissingJoinTable]
	if b4.Count != 1 || b4.Items[0].Key != relNN || b4.AutoFixSafe {
		t.Fatalf("B4 = %+v; want 1 finding %s, autoFixSafe=false", b4, relNN)
	}
	// Scan is READ-ONLY: the dropped tables must still be gone after scanning.
	if dsTableExists(t, db, "obj_ds_tickets") || dsTableExists(t, db, joinTable) {
		t.Fatalf("scan đã mutate schema — bảng bị recreate ngoài fix path")
	}

	// --- 2. Tenant B sees the SHARED drift through its own metadata. --------------------------
	scanB := drScanCounts(t, svc, dsTenantB)
	if got := scanB[DoctorCheckMissingGenColumn]; got.Count != 1 || got.Items[0].Key != fEmailB {
		t.Fatalf("tenant B A1 = %+v; want 1 finding (B's email field id %s)", got, fEmailB)
	}
	if got := scanB[DoctorCheckMandatoryDDL]; got.Count != 3 {
		t.Fatalf("tenant B A5 = %+v; want 3 (shared-table drift hiện với mọi tenant)", got)
	}
	if scanB[DoctorCheckMissingObjTable].Count != 0 || scanB[DoctorCheckMissingJoinTable].Count != 0 {
		t.Fatalf("tenant B A6/B4 phải sạch (entity/rel là của tenant A): %+v", scanB)
	}

	// --- 3. Auto-fix the safe classes as tenant A. ---------------------------------------------
	results, err := svc.Fix(ctx, dsTenantA, []string{DoctorCheckMissingGenColumn, DoctorCheckUnguardedUnique, DoctorCheckMandatoryDDL}, false)
	if err != nil {
		t.Fatalf("Fix safe: %v", err)
	}
	for _, r := range results {
		if r.Failed != 0 {
			t.Fatalf("check %s failed: %v", r.CheckID, r.Errors)
		}
	}

	// --- 4. Confirm gate: A6/B4 refuse without confirm, then recreate with it. ----------------
	_, err = svc.Fix(ctx, dsTenantA, []string{DoctorCheckMissingObjTable, DoctorCheckMissingJoinTable}, false)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "DOCTOR_CONFIRM_REQUIRED" {
		t.Fatalf("err = %v; want DOCTOR_CONFIRM_REQUIRED (whole request refused)", err)
	}
	if dsTableExists(t, db, "obj_ds_tickets") {
		t.Fatalf("bảng recreate dù request bị reject — mutation trước validation")
	}
	results, err = svc.Fix(ctx, dsTenantA, []string{DoctorCheckMissingObjTable, DoctorCheckMissingJoinTable}, true)
	if err != nil {
		t.Fatalf("Fix confirm: %v", err)
	}
	for _, r := range results {
		if r.Fixed != 1 || r.Failed != 0 {
			t.Fatalf("confirm fix %s = %+v; want 1 fixed", r.CheckID, r)
		}
	}

	// --- 5. Re-scan tenant A → everything clean (QRR idempotent contract). --------------------
	for id, r := range drScanCounts(t, svc, dsTenantA) {
		if r.Count != 0 {
			t.Fatalf("re-scan %s = %+v; want clean", id, r)
		}
	}

	// --- 6. Physical truth. --------------------------------------------------------------------
	// A2a: both unique columns carry the deleted_at guard again.
	for _, col := range []string{"email", "sku"} {
		var expr string
		if err := db.Get(&expr, `SELECT GENERATION_EXPRESSION FROM information_schema.COLUMNS
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'obj_ds_accounts' AND COLUMN_NAME = ?`, col); err != nil {
			t.Fatalf("read expr %s: %v", col, err)
		}
		if !strings.Contains(expr, "deleted_at") {
			t.Fatalf("cột %s chưa guarded: %s", col, expr)
		}
	}
	// A5 / RM-47 closure: idx_created_at is the 4-column composite again.
	var idxCols []string
	if err := db.Select(&idxCols, `SELECT COLUMN_NAME FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'obj_ds_accounts' AND INDEX_NAME = 'idx_created_at'
		ORDER BY SEQ_IN_INDEX`); err != nil {
		t.Fatalf("read idx_created_at: %v", err)
	}
	if strings.Join(idxCols, ",") != "tenant_id,deleted_at,created_at,id" {
		t.Fatalf("idx_created_at = %v; want composite O1∪O6 (RM-47)", idxCols)
	}
	// A6/B4: record + link ops work again on the recreated tables.
	dsExec(t, db, `INSERT INTO obj_ds_tickets (id, tenant_id, created_by, data) VALUES (?, ?, ?, ?)`,
		uuid.NewString(), dsTenantA, "u-a", `{"code":"T-1","subject":"hồi sinh"}`)
	dsExec(t, db, `INSERT INTO `+"`"+joinTable+"`"+` (id, tenant_id, ds_accounts_id, ds_tags_id, created_by) VALUES (?, ?, ?, ?, ?)`,
		uuid.NewString(), dsTenantA, uuid.NewString(), uuid.NewString(), "u-a")

	// --- 7. Cross-tenant benefit + no corruption: B re-scans clean, B's row survived. ----------
	// D1_PIVOT_RECONCILE is NOT a shared-schema check like A1/A2a/A5 — it tracks PER-TENANT pivot
	// DATA, not the physical column/index every tenant on the table shares. bRowID (seeded above
	// via a raw INSERT that bypasses the pivot-maintaining mutator) genuinely never had a pivot
	// row, independent of anything tenant A's fixes just repaired — so tenant A's fix batch does
	// NOT clear it "for free" the way A1/A5 do; fix it explicitly here (a real Doctor lifecycle
	// step) before asserting the blanket-clean loop below.
	if got := drScanCounts(t, svc, dsTenantB)[DoctorCheckPivotReconcile]; got.Count != 1 || got.Items[0].Key != pivotReconcileFillPrefix+"ds_accounts:email" {
		t.Fatalf("tenant B D1 pre-fix = %+v; want 1 fill finding for ds_accounts:email (bRowID never had a pivot row)", got)
	}
	if _, err := svc.Fix(ctx, dsTenantB, []string{DoctorCheckPivotReconcile}, false); err != nil {
		t.Fatalf("Fix D1 tenant B: %v", err)
	}
	for id, r := range drScanCounts(t, svc, dsTenantB) {
		if r.Count != 0 {
			t.Fatalf("tenant B re-scan %s = %+v; want clean (shared fix benefit)", id, r)
		}
	}
	var bEmail string
	if err := db.Get(&bEmail, `SELECT email FROM obj_ds_accounts WHERE id = ? AND tenant_id = ?`, bRowID, dsTenantB); err != nil {
		t.Fatalf("read tenant B row: %v", err)
	}
	if bEmail != "b@x.vn" {
		t.Fatalf("tenant B email gen-col = %q; want recomputed 'b@x.vn' từ data JSON nguyên vẹn", bEmail)
	}
}

// TestDoctorSchemaInt_A1BackfillBlockedByDuplicates pins the 1062 dead-end contract for the ONE
// A1 kind that can still hit it post RM-28 Slice B: a 1-1 relation's cardinality index
// (AddOneToOneRelationColumn) is a SEPARATE physical UNIQUE index, NOT pivot-backed, NOT
// retired. When the shared table already holds LIVE rows violating 1-1 exclusivity, the A1
// repair fails LOUD as a per-item verdict carrying FIELD_RELATION_CARDINALITY_CONFLICT (no
// driver internals after the W-1 sanitizer) and stays failed on re-run (no blind retry). A
// PLAIN unique field can no longer reach this: its A1 repair (AddUniqueGencolNonUnique)
// materializes a non-unique idx_ index that cannot 1062, whatever the live data looks like.
func TestDoctorSchemaInt_A1BackfillBlockedByDuplicates(t *testing.T) {
	svc, db := drWire(t)
	dyn := dynrepo.New(db)
	ctx := context.Background()

	entID := dsSeedEntity(t, db, dsTenantA, "ds_dup")
	dsSeedField(t, db, dsTenantA, entID, "partner", "relation", false, `{"relatedEntity":"ds_dup","cardinality":"1-1"}`)
	if err := dyn.CreateTable(ctx, "ds_dup", []ddl.FieldDef{
		{Name: "partner", FieldType: "relation", RelatedEntity: "ds_dup", OneToOne: true},
	}); err != nil {
		t.Fatalf("create obj_ds_dup: %v", err)
	}
	dsExec(t, db, "ALTER TABLE `obj_ds_dup` DROP INDEX `uq_obj_ds_dup_partner_id`, DROP COLUMN `partner_id`")
	// With the 1-1 unique column gone, nothing stops two live rows pointing at the same parent.
	sameParent := uuid.NewString()
	for i := 0; i < 2; i++ {
		dsExec(t, db, `INSERT INTO obj_ds_dup (id, tenant_id, created_by, data) VALUES (?, ?, ?, ?)`,
			uuid.NewString(), dsTenantA, "u-a", `{"partner":"`+sameParent+`"}`)
	}

	if got := drScanCounts(t, svc, dsTenantA)[DoctorCheckMissingGenColumn]; got.Count != 1 {
		t.Fatalf("A1 = %+v; want 1 finding", got)
	}
	for run := 1; run <= 2; run++ { // second run proves no retry-away: still an honest failure
		results, err := svc.Fix(ctx, dsTenantA, []string{DoctorCheckMissingGenColumn}, false)
		if err != nil {
			t.Fatalf("Fix run %d: %v", run, err)
		}
		r := results[0]
		if r.Fixed != 0 || r.Failed != 1 || len(r.Errors) != 1 {
			t.Fatalf("run %d fix = %+v; want 1 failed item", run, r)
		}
		if !strings.Contains(r.Errors[0], "FIELD_RELATION_CARDINALITY_CONFLICT") {
			t.Fatalf("run %d error = %q; want verdict FIELD_RELATION_CARDINALITY_CONFLICT", run, r.Errors[0])
		}
		if strings.Contains(r.Errors[0], "1062") || strings.Contains(r.Errors[0], "dynrepo.") {
			t.Fatalf("run %d error = %q; driver internals lộ ra client", run, r.Errors[0])
		}
	}
}

// TestDoctorSchemaInt_A3FlagsManuallyReaddedUniqueIndex pins the RM-28 Slice B A3 repurpose
// (spec §6): a plain `is_unique` field's index is non-unique (idx_) from birth post Slice B, so
// it NEVER legitimately owns a `uq_` name any more. If an operator (or a half-run demote
// migration) manually re-adds a physical UNIQUE index for such a field, A3 must flag it as an
// anomaly — it is no longer expected/owned, unlike a 1-1 relation's own uq_ index which stays
// legitimate and unflagged.
func TestDoctorSchemaInt_A3FlagsManuallyReaddedUniqueIndex(t *testing.T) {
	svc, db := drWire(t)
	dyn := dynrepo.New(db)
	ctx := context.Background()
	const tenant = "d3a3-tenant-aaaaaaaaaaaaaaaaaaaaaaaa"

	entID := dsSeedEntity(t, db, tenant, "ds_a3")
	dsSeedField(t, db, tenant, entID, "email", "text", true, "")
	dsSeedField(t, db, tenant, entID, "partner", "relation", false, `{"relatedEntity":"ds_a3","cardinality":"1-1"}`)
	if err := dyn.CreateTable(ctx, "ds_a3", []ddl.FieldDef{
		{Name: "email", FieldType: "text", IsUnique: true},
		{Name: "partner", FieldType: "relation", RelatedEntity: "ds_a3", OneToOne: true},
	}); err != nil {
		t.Fatalf("create obj_ds_a3: %v", err)
	}

	// Sanity (healthy steady state): NO A3 finding yet — email is idx_ (non-unique, unowned-uq_
	// is moot since there IS no uq_), partner's uq_ is legitimately owned.
	if got := drScanCounts(t, svc, tenant)[DoctorCheckOrphanUniqueIndex]; got.Count != 0 {
		t.Fatalf("A3 pre-tamper = %+v; want 0 findings (healthy post-Slice-B shape)", got)
	}

	// Operator (or half-run demote) manually re-adds a physical UNIQUE index for `email`.
	dsExec(t, db, "ALTER TABLE `obj_ds_a3` DROP INDEX `idx_obj_ds_a3_email`, ADD UNIQUE KEY `uq_obj_ds_a3_email` (`tenant_id`, `email`)")

	got := drScanCounts(t, svc, tenant)[DoctorCheckOrphanUniqueIndex]
	if got.Count != 1 || got.Items[0].Key != "ds_a3:uq_obj_ds_a3_email" {
		t.Fatalf("A3 post-tamper = %+v; want exactly 1 finding (ds_a3:uq_obj_ds_a3_email)", got)
	}
	if !got.ReportOnly || got.AutoFixSafe {
		t.Fatalf("A3 = %+v; want reportOnly=true, autoFixSafe=false", got)
	}
	// The 1-1 relation's own uq_ index must NOT be flagged (still legitimately owned).
	for _, it := range got.Items {
		if strings.Contains(it.Key, "partner") {
			t.Fatalf("A3 incorrectly flagged the 1-1 relation's own index: %+v", it)
		}
	}
}
