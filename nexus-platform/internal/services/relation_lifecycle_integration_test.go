//go:build integration

// W2 relationship lifecycle (impact / delete "gỡ nối") integration tests — exercised
// end-to-end through EntityService/FieldService/RecordService/RelationService +
// RelationLifecycleService over a freshly-migrated throwaway MySQL DB (o3aSetup harness).
// Covers everything fakes cannot prove: the reused DeleteField flow really soft-deletes
// BOTH the sys_fields row and the registry row, child FK VALUES survive (documented
// choice), N-N tombstones really free BOTH unique slots (re-register works), the join
// table survives the delete (grace window), and FK/join-row counts run against real data.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestRelLifecycle -race -v
package services

import (
	"context"
	"errors"
	"testing"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// lcWire builds the full production-shaped composition over a throwaway DB, returning the
// raw sqlx handle too for direct schema/value assertions.
func lcWire(t *testing.T) (*EntityService, *FieldService, *RecordService, *RelationService, *RelationLifecycleService, *metadata.Repository, *dynrepo.Repository, *sqlx.DB) {
	t.Helper()
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	entitySvc := NewEntityService(dynRepo, metaRepo)
	fieldSvc := NewFieldService(dynRepo, metaRepo)
	recordSvc := NewRecordService(metaRepo, dynRepo)
	relationSvc := NewRelationService(dynRepo, metaRepo)
	lifecycleSvc := NewRelationLifecycleService(metaRepo, dynRepo, fieldSvc)
	return entitySvc, fieldSvc, recordSvc, relationSvc, lifecycleSvc, metaRepo, dynRepo, sdb
}

// lcSetupOneToN registers accounts + contacts (with the 1-N relation field "account") and
// returns the registry row id.
func lcSetupOneToN(t *testing.T, entitySvc *EntityService, fieldSvc *FieldService, metaRepo *metadata.Repository, tenant string) string {
	t.Helper()
	ctx := context.Background()
	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name: "accounts", Label: "Accounts",
		Fields: []FieldInput{{Name: "name", Label: "Name", FieldType: "text", IsRequired: true}},
	}); err != nil {
		t.Fatalf("CreateEntity accounts: %v", err)
	}
	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name: "contacts", Label: "Contacts",
		Fields: []FieldInput{{Name: "name", Label: "Name", FieldType: "text", IsRequired: true}},
	}); err != nil {
		t.Fatalf("CreateEntity contacts: %v", err)
	}
	if _, err := fieldSvc.AddField(ctx, tenant, AddFieldInput{
		EntityName: "contacts",
		Field: FieldInput{Name: "account", Label: "Account", FieldType: "relation",
			Options: map[string]any{"relatedEntity": "accounts"}},
	}); err != nil {
		t.Fatalf("AddField(relation): %v", err)
	}
	rel, err := metaRepo.GetRelationshipByName(ctx, tenant, "contacts_account")
	if err != nil {
		t.Fatalf("GetRelationshipByName(contacts_account): %v", err)
	}
	return rel.ID
}

func TestRelLifecycle_DeleteOneToN_Integration(t *testing.T) {
	entitySvc, fieldSvc, recordSvc, _, lifecycleSvc, metaRepo, _, sdb := lcWire(t)
	ctx := context.Background()
	const user = "11111111-1111-1111-1111-111111111101"

	relID := lcSetupOneToN(t, entitySvc, fieldSvc, metaRepo, engTenantA)

	// Populate: 1 account, 2 linked contacts, 1 unlinked contact.
	accID, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "accounts", map[string]any{"name": "Acme"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	for _, name := range []string{"Jane", "John"} {
		if _, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "contacts",
			map[string]any{"name": name, "account": accID}); err != nil {
			t.Fatalf("create linked contact %s: %v", name, err)
		}
	}
	if _, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "contacts",
		map[string]any{"name": "Loner"}); err != nil {
		t.Fatalf("create unlinked contact: %v", err)
	}

	// Impact pre-check: exactly the 2 linked contacts count; deletable.
	imp, err := lifecycleSvc.Impact(ctx, engTenantA, relID)
	if err != nil {
		t.Fatalf("Impact: %v", err)
	}
	if imp.AffectedRecords != 2 {
		t.Errorf("AffectedRecords = %d; want 2 (non-null FK holders only)", imp.AffectedRecords)
	}
	if !imp.Deletable || len(imp.DependentRollups) != 0 {
		t.Errorf("Deletable/DependentRollups = %v/%v; want true/empty", imp.Deletable, imp.DependentRollups)
	}

	// Cross-tenant delete must be impossible: tenant B addressing tenant A's row → not-found,
	// and the row (plus the field) stays fully intact for tenant A.
	err = lifecycleSvc.Delete(ctx, engTenantB, relID)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "RELATIONSHIP_NOT_FOUND" {
		t.Fatalf("cross-tenant Delete: err = %v; want RELATIONSHIP_NOT_FOUND", err)
	}
	if _, err := metaRepo.GetRelationshipByName(ctx, engTenantA, "contacts_account"); err != nil {
		t.Fatalf("tenant A's registry row must survive a cross-tenant delete attempt: %v", err)
	}

	// The real delete.
	if err := lifecycleSvc.Delete(ctx, engTenantA, relID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Registry row gone from every active lookup (soft-deleted).
	if _, err := metaRepo.GetRelationshipByName(ctx, engTenantA, "contacts_account"); !errors.Is(err, metadata.ErrRelationshipNotFound) {
		t.Errorf("registry row still resolves after delete: %v", err)
	}
	var relDeleted int
	if err := sdb.Get(&relDeleted, `SELECT COUNT(*) FROM sys_relationships
		WHERE id = ? AND tenant_id = ? AND deleted_at IS NOT NULL`, relID, engTenantA); err != nil {
		t.Fatalf("check registry soft-delete: %v", err)
	}
	if relDeleted != 1 {
		t.Errorf("registry row must be SOFT-deleted (deleted_at set), not hard-deleted/live")
	}

	// The owning relation field is soft-deleted (same convention as DELETE /admin/fields/:id).
	var fieldDeleted int
	if err := sdb.Get(&fieldDeleted, `SELECT COUNT(*) FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		WHERE e.tenant_id = ? AND e.name = 'contacts' AND f.name = 'account' AND f.deleted_at IS NOT NULL`,
		engTenantA); err != nil {
		t.Fatalf("check field soft-delete: %v", err)
	}
	if fieldDeleted != 1 {
		t.Errorf("relation field must be soft-deleted by the relationship delete")
	}

	// DOCUMENTED CHOICE: child records keep their FK VALUES (no DDL, §4.4) — the column and
	// both stored ids survive; they are merely inert now that the field left the metadata.
	var fkKept int
	if err := sdb.Get(&fkKept, "SELECT COUNT(*) FROM `obj_contacts` WHERE `tenant_id` = ? AND `account_id` IS NOT NULL",
		engTenantA); err != nil {
		t.Fatalf("check FK values preserved: %v", err)
	}
	if fkKept != 2 {
		t.Errorf("child FK values = %d rows; want 2 preserved (soft delete never wipes obj_* data)", fkKept)
	}

	// Re-delete → not-found (already soft-deleted, indistinguishable from never-existed).
	err = lifecycleSvc.Delete(ctx, engTenantA, relID)
	if !errors.As(err, &ve) || ve.Code != "RELATIONSHIP_NOT_FOUND" {
		t.Errorf("re-delete: err = %v; want RELATIONSHIP_NOT_FOUND", err)
	}
}

// TestRelLifecycle_DeleteOneToN_ThenRecreateEntity_DoesNotBrick_Integration is the W2 review
// MED regression proof: SoftDeleteRelationshipByChildFK (called from inside the reused
// DeleteField flow) must tombstone the registry row's `name` — not just soft-delete it —
// or the (tenant_id, name) UNIQUE slot (uq_sysrel_tenant_name, migration 000025, which spans
// soft-deleted rows) stays permanently claimed. The exact brick scenario the reviewer found:
//
//  1. register a 1-N relation (row named "contacts_account")
//  2. delete the relation via the W2 lifecycle DELETE (drives DeleteField)
//  3. delete the CHILD entity — SoftDeleteEntity's own tombstone loop only lists still-LIVE
//     owned rows, so it does NOT touch the row already soft-deleted by step 2 (whether or
//     not THAT soft-delete tombstoned the name — this is what makes the bug reachable via
//     the entity-delete path even though SoftDeleteEntity itself is correct)
//  4. recreate the child entity + the SAME-named relation field — the re-registration
//     INSERTs a NEW row also named "contacts_account", colliding with the untombstoned
//     leftover from step 2 -> permanent 409 RELATIONSHIP_ALREADY_EXISTS before the fix.
func TestRelLifecycle_DeleteOneToN_ThenRecreateEntity_DoesNotBrick_Integration(t *testing.T) {
	entitySvc, fieldSvc, _, _, lifecycleSvc, metaRepo, _, sdb := lcWire(t)
	ctx := context.Background()

	relID := lcSetupOneToN(t, entitySvc, fieldSvc, metaRepo, engTenantA)

	// Step 2: delete the relation via the lifecycle DELETE (reuses DeleteField).
	if err := lifecycleSvc.Delete(ctx, engTenantA, relID); err != nil {
		t.Fatalf("Delete relation: %v", err)
	}

	// Prove the fix directly: the soft-deleted row's `name` must NOT still be the live
	// value (that is exactly the stuck-slot condition the brick scenario exploits).
	var storedName string
	if err := sdb.Get(&storedName, `SELECT name FROM sys_relationships WHERE id = ? AND tenant_id = ?`,
		relID, engTenantA); err != nil {
		t.Fatalf("load soft-deleted registry row: %v", err)
	}
	if storedName == "contacts_account" {
		t.Fatalf("registry row name NOT tombstoned after 1-N delete (name = %q) — the (tenant_id,name) UNIQUE slot is still claimed; recreate will brick", storedName)
	}

	// Step 3: delete the child entity. Mirrors the brick scenario exactly: SoftDeleteEntity's
	// OWN cleanup loop only lists LIVE owned rows (GORM's default soft-delete scope), so it
	// does not re-touch the row step 2 already soft-deleted.
	if err := entitySvc.SoftDeleteEntity(ctx, engTenantA, "contacts"); err != nil {
		t.Fatalf("SoftDeleteEntity(contacts): %v", err)
	}

	// Step 4: recreate "contacts" WITH the SAME "account" relation field declared INLINE
	// (this is the reachable shape of the brick: obj_contacts is a SHARED-by-name physical
	// table that CreateEntity never drops, so the recreate hits the "existedBefore" RECONCILE
	// path in entity_orchestration.go — it SILENTLY SKIPS re-adding the still-present
	// `account_id` column (no conflict, unlike a separate AddField call) and proceeds straight
	// to inserting the sys_relationships registry row named "contacts_account". Before the fix
	// THAT INSERT trips uq_sysrel_tenant_name on the stale, untombstoned name -> 409
	// RELATIONSHIP_ALREADY_EXISTS, no in-product recovery.
	if _, _, err := entitySvc.CreateEntity(ctx, engTenantA, CreateEntityInput{
		Name: "contacts", Label: "Contacts",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
			{Name: "account", Label: "Account", FieldType: "relation",
				Options: map[string]any{"relatedEntity": "accounts"}},
		},
	}); err != nil {
		t.Fatalf("recreate contacts entity + relation field: %v — BRICK REGRESSION (MED fix reverted?)", err)
	}

	// The re-registration produced a fresh, resolvable row.
	rel, err := metaRepo.GetRelationshipByName(ctx, engTenantA, "contacts_account")
	if err != nil {
		t.Fatalf("GetRelationshipByName after recreate: %v", err)
	}
	if rel.ID == relID {
		t.Errorf("recreated registry row should be a NEW row (different id); got the SAME id %s", relID)
	}
	if rel.RelationshipType != "1-N" || rel.ChildFKColumn != "account_id" {
		t.Errorf("recreated row = %+v; want fresh 1-N contacts_account/account_id", rel)
	}
}

// TestRelLifecycle_DeleteFieldDirect_ThenReRegisterViaEntityRecreate_Integration confirms the
// SAME fix (SoftDeleteRelationshipByChildFK tombstoning) also unbricks the DIRECT
// DELETE-/admin/fields/:id path (FieldService.DeleteField called WITHOUT going through the
// W2 lifecycle wrapper) — the two entry points share the exact same registry-cleanup call,
// so this pins that the shared code path benefits identically.
func TestRelLifecycle_DeleteFieldDirect_ThenReRegisterViaEntityRecreate_Integration(t *testing.T) {
	entitySvc, fieldSvc, _, _, _, metaRepo, _, _ := lcWire(t)
	ctx := context.Background()
	lcSetupOneToN(t, entitySvc, fieldSvc, metaRepo, engTenantA)

	fields, err := metaRepo.ListFields(ctx, engTenantA, mustEntityID(t, ctx, metaRepo, engTenantA, "contacts"))
	if err != nil {
		t.Fatalf("ListFields contacts: %v", err)
	}
	var accountFieldID string
	for _, f := range fields {
		if f.Name == "account" {
			accountFieldID = f.ID
		}
	}
	if accountFieldID == "" {
		t.Fatal("could not resolve contacts.account field id")
	}

	// Direct DeleteField (the DELETE /admin/fields/:id path), NOT the lifecycle wrapper.
	if err := fieldSvc.DeleteField(ctx, engTenantA, DeleteFieldInput{FieldID: accountFieldID}); err != nil {
		t.Fatalf("DeleteField(contacts.account): %v", err)
	}

	if err := entitySvc.SoftDeleteEntity(ctx, engTenantA, "contacts"); err != nil {
		t.Fatalf("SoftDeleteEntity(contacts): %v", err)
	}
	// Relation field declared INLINE (see the sibling lifecycle-path test's comment for why
	// this — not a separate AddField call — is the shape that actually reaches the
	// sys_relationships INSERT instead of tripping the unrelated physical-column-conflict
	// guard on a re-add via AddField).
	if _, _, err := entitySvc.CreateEntity(ctx, engTenantA, CreateEntityInput{
		Name: "contacts", Label: "Contacts",
		Fields: []FieldInput{
			{Name: "name", Label: "Name", FieldType: "text", IsRequired: true},
			{Name: "account", Label: "Account", FieldType: "relation",
				Options: map[string]any{"relatedEntity": "accounts"}},
		},
	}); err != nil {
		t.Fatalf("recreate contacts entity + relation field (direct DeleteField path): %v — BRICK REGRESSION", err)
	}
}

// mustEntityID resolves an entity's id or fails the test — a tiny local helper so the direct
// DeleteField test above can locate the "account" field without going through GetEntityByName
// boilerplate inline.
func mustEntityID(t *testing.T, ctx context.Context, metaRepo *metadata.Repository, tenantID, name string) string {
	t.Helper()
	e, err := metaRepo.GetEntityByName(ctx, tenantID, name)
	if err != nil {
		t.Fatalf("GetEntityByName(%s): %v", name, err)
	}
	return e.ID
}

func TestRelLifecycle_DeleteNN_Integration(t *testing.T) {
	entitySvc, _, recordSvc, relationSvc, lifecycleSvc, metaRepo, dynRepo, sdb := lcWire(t)
	ctx := context.Background()
	const user = "11111111-1111-1111-1111-111111111102"

	for _, name := range []string{"accounts", "contacts"} {
		if _, _, err := entitySvc.CreateEntity(ctx, engTenantA, CreateEntityInput{
			Name: name, Label: name,
			Fields: []FieldInput{{Name: "name", Label: "Name", FieldType: "text", IsRequired: true}},
		}); err != nil {
			t.Fatalf("CreateEntity %s: %v", name, err)
		}
	}
	rel, err := relationSvc.RegisterRelationship(ctx, engTenantA, RegisterRelationshipInput{
		Name: "accounts_contacts", EntityA: "accounts", EntityB: "contacts",
	})
	if err != nil {
		t.Fatalf("RegisterRelationship: %v", err)
	}

	accID, err := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "accounts", map[string]any{"name": "Acme"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	for _, name := range []string{"Jane", "John"} {
		cID, cerr := recordSvc.Create(ctx, engAdmin(user), engTenantA, user, "contacts", map[string]any{"name": name})
		if cerr != nil {
			t.Fatalf("create contact %s: %v", name, cerr)
		}
		if _, lerr := dynRepo.LinkRecords(ctx, engTenantA, user, "accounts", accID, "contacts", cID); lerr != nil {
			t.Fatalf("link %s: %v", name, lerr)
		}
	}

	// Impact: both link rows counted.
	imp, err := lifecycleSvc.Impact(ctx, engTenantA, rel.ID)
	if err != nil {
		t.Fatalf("Impact: %v", err)
	}
	if imp.AffectedRecords != 2 {
		t.Errorf("AffectedRecords = %d; want 2 join rows", imp.AffectedRecords)
	}

	if err := lifecycleSvc.Delete(ctx, engTenantA, rel.ID); err != nil {
		t.Fatalf("Delete N-N: %v", err)
	}

	// Registry row: soft-deleted AND both unique columns tombstoned.
	if _, err := metaRepo.GetRelationshipByName(ctx, engTenantA, "accounts_contacts"); !errors.Is(err, metadata.ErrRelationshipNotFound) {
		t.Errorf("N-N registry row still resolves after delete: %v", err)
	}
	var row struct {
		Name string  `db:"name"`
		Join *string `db:"join_table_name"`
	}
	if err := sdb.Get(&row, `SELECT name, join_table_name FROM sys_relationships
		WHERE id = ? AND tenant_id = ? AND deleted_at IS NOT NULL`, rel.ID, engTenantA); err != nil {
		t.Fatalf("load soft-deleted N-N row: %v", err)
	}
	if row.Name == "accounts_contacts" {
		t.Errorf("name = %q; want tombstoned (unique slot must be freed)", row.Name)
	}
	if row.Join == nil || *row.Join == "rel_accounts_contacts" {
		t.Errorf("join_table_name = %v; want tombstoned (unique slot must be freed)", row.Join)
	}

	// Grace window: the physical join table (and its link rows) survive the delete.
	exists, err := dynRepo.JoinTableExists(ctx, "accounts", "contacts")
	if err != nil {
		t.Fatalf("JoinTableExists: %v", err)
	}
	if !exists {
		t.Errorf("join table must NOT be dropped on delete (grace window)")
	}
	var links int
	if err := sdb.Get(&links, "SELECT COUNT(*) FROM `rel_accounts_contacts` WHERE `tenant_id` = ?", engTenantA); err != nil {
		t.Fatalf("count surviving links: %v", err)
	}
	if links != 2 {
		t.Errorf("surviving link rows = %d; want 2 (grace window preserves data)", links)
	}

	// Freed slots: re-registering the SAME pair with the SAME name must succeed (this is
	// what the tombstones buy — without them both unique indexes would 409 forever).
	if _, err := relationSvc.RegisterRelationship(ctx, engTenantA, RegisterRelationshipInput{
		Name: "accounts_contacts", EntityA: "accounts", EntityB: "contacts",
	}); err != nil {
		t.Fatalf("re-register same pair after delete: %v", err)
	}
}

func TestRelLifecycle_RollupDependentBlocksDelete_Integration(t *testing.T) {
	entitySvc, fieldSvc, _, _, lifecycleSvc, metaRepo, _, _ := lcWire(t)
	ctx := context.Background()

	relID := lcSetupOneToN(t, entitySvc, fieldSvc, metaRepo, engTenantA)

	// A rollup on the parent counts children through the relation.
	rollup, err := fieldSvc.AddField(ctx, engTenantA, AddFieldInput{
		EntityName: "accounts",
		Field: FieldInput{Name: "contact_count", Label: "Contacts", FieldType: "rollup",
			Options: map[string]any{"childEntity": "contacts", "viaRelation": "account", "metric": "count"}},
	})
	if err != nil {
		t.Fatalf("AddField(rollup): %v", err)
	}

	// Impact reports the dependent and flips deletable off.
	imp, err := lifecycleSvc.Impact(ctx, engTenantA, relID)
	if err != nil {
		t.Fatalf("Impact: %v", err)
	}
	if len(imp.DependentRollups) != 1 || imp.DependentRollups[0].FieldName != "contact_count" || imp.Deletable {
		t.Errorf("impact = %+v; want dependentRollups=[accounts.contact_count], deletable=false", imp)
	}

	// Delete refused 409 while the rollup exists; the relation field stays live.
	err = lifecycleSvc.Delete(ctx, engTenantA, relID)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "RELATIONSHIP_HAS_DEPENDENTS" {
		t.Fatalf("Delete with dependent rollup: err = %v; want RELATIONSHIP_HAS_DEPENDENTS", err)
	}
	if _, err := metaRepo.GetRelationshipByName(ctx, engTenantA, "contacts_account"); err != nil {
		t.Fatalf("registry row must survive a refused delete: %v", err)
	}

	// Remove the dependent, then the delete goes through.
	if err := fieldSvc.DeleteField(ctx, engTenantA, DeleteFieldInput{FieldID: rollup.ID}); err != nil {
		t.Fatalf("DeleteField(rollup): %v", err)
	}
	if err := lifecycleSvc.Delete(ctx, engTenantA, relID); err != nil {
		t.Fatalf("Delete after removing dependent: %v", err)
	}
}
