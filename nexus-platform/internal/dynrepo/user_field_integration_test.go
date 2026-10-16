//go:build integration

// Integration tests for the USER-FIELD slice at the dynrepo layer. Run with:
//
//	go test -tags=integration ./internal/dynrepo/
//
// They require a reachable MySQL 8; the harness (testDB, columnSet, indexSet, tableExists,
// isUniqueIndex) lives in dynrepo_integration_test.go / relation_integration_test.go. seedTenant
// / seedUser (ACTIVE-user seed) live in rls_a5_integration_test.go — reused here as-is.
package dynrepo

import (
	"context"
	"testing"

	"nexus/platform/internal/ddl"
)

// TestCreateTable_UserFieldInline proves a `user` field declared at CREATE TABLE time
// materializes the SAME FK-shaped column + non-unique tenant-scoped index a plain 1-N
// `relation` field gets (relation_integration_test.go's TestCreateTable_RelationFieldInline) —
// against REAL MySQL, not just the ddl package's string-shape unit tests.
func TestCreateTable_UserFieldInline(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_user_inline"

	_ = repo.DropTable(ctx, entity)
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "subject", FieldType: "text"},
		{Name: "assigned_to", FieldType: "user"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	const table = "obj_it_user_inline"
	if _, ok := columnSet(t, db, table)["assigned_to_id"]; !ok {
		t.Error("create-time user field did not produce the `assigned_to_id` column")
	}
	if _, ok := indexSet(t, db, table)["idx_obj_it_user_inline_assigned_to_id"]; !ok {
		t.Error("create-time user field did not produce the FK index")
	}
	if isUniqueIndex(t, db, table, "idx_obj_it_user_inline_assigned_to_id") {
		t.Error("user field index must be non-unique (many records may share one assignee)")
	}
}

// TestAddAndDropUserColumn proves the AddField path (AddRelationColumn — REUSED as-is for a
// `user` field, see field_orchestration.go's "case in.Field.FieldType == \"user\"") materializes
// and reverses the column identically to a plain relation field.
func TestAddAndDropUserColumn(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_user_addfield"

	_ = repo.DropTable(ctx, entity)
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "title", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	const table = "obj_it_user_addfield"

	if err := repo.AddRelationColumn(ctx, entity, "assignee"); err != nil {
		t.Fatalf("AddRelationColumn (user field reuse): %v", err)
	}
	if _, ok := columnSet(t, db, table)["assignee_id"]; !ok {
		t.Error("AddRelationColumn did not create the `assignee_id` FK column")
	}

	if err := repo.DropRelationColumn(ctx, entity, "assignee"); err != nil {
		t.Fatalf("DropRelationColumn: %v", err)
	}
	if _, ok := columnSet(t, db, table)["assignee_id"]; ok {
		t.Error("DropRelationColumn did not remove the `assignee_id` column")
	}
}

// TestBatchActiveUserExists proves the write-time existence probe (services.checkUserTargets'
// backing query) resolves ONLY a live, ACTIVE, same-tenant user — a cross-tenant user, a
// disabled user, a soft-deleted user, and a garbage id all report false (collapsed into ONE
// "doesn't count" outcome by the caller; this test proves EACH SQL predicate actually excludes
// its case).
func TestBatchActiveUserExists(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()

	tenantA := newID()
	tenantB := newID()
	seedTenant(t, db, tenantA, "it-uf-tenant-a-"+tenantA[:8])
	seedTenant(t, db, tenantB, "it-uf-tenant-b-"+tenantB[:8])

	activeID := newID()
	seedUser(t, db, activeID, tenantA) // seedUser sets status='active'

	crossTenantID := newID()
	seedUser(t, db, crossTenantID, tenantB) // active, but WRONG tenant

	disabledID := newID()
	if _, err := db.Exec(
		`INSERT INTO users (id, tenant_id, email, password_hash, status, is_admin, created_at, updated_at) VALUES (?, ?, ?, 'x', 'disabled', 0, NOW(3), NOW(3))`,
		disabledID, tenantA, disabledID+"@example.test"); err != nil {
		t.Fatalf("seed disabled user: %v", err)
	}

	deletedID := newID()
	seedUser(t, db, deletedID, tenantA)
	if _, err := db.Exec(`UPDATE users SET deleted_at = NOW(3) WHERE id = ?`, deletedID); err != nil {
		t.Fatalf("soft-delete seeded user: %v", err)
	}

	garbageID := "deadbeef-0000-0000-0000-000000000000"

	found, err := repo.BatchActiveUserExists(ctx, tenantA, []string{activeID, crossTenantID, disabledID, deletedID, garbageID})
	if err != nil {
		t.Fatalf("BatchActiveUserExists: %v", err)
	}
	if !found[activeID] {
		t.Errorf("active same-tenant user %q should resolve true", activeID)
	}
	for name, id := range map[string]string{
		"cross-tenant": crossTenantID, "disabled": disabledID, "soft-deleted": deletedID, "garbage": garbageID,
	} {
		if found[id] {
			t.Errorf("%s id %q should NOT resolve true", name, id)
		}
	}
}

// TestBatchFetchUserLabels proves the ?expand= primitive resolves name+email, INCLUDING for a
// disabled user (history must still show who a record was assigned to — see
// BatchFetchUserLabels' doc for why status is NOT filtered), but NOT for a soft-deleted
// (permanently removed) user or a garbage id.
func TestBatchFetchUserLabels(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()

	tenantA := newID()
	seedTenant(t, db, tenantA, "it-uf-labels-"+tenantA[:8])

	activeID := newID()
	if _, err := db.Exec(
		`INSERT INTO users (id, tenant_id, email, password_hash, full_name, status, is_admin, created_at, updated_at) VALUES (?, ?, ?, 'x', ?, 'active', 0, NOW(3), NOW(3))`,
		activeID, tenantA, "active@example.test", "Nguyễn Văn A"); err != nil {
		t.Fatalf("seed active user: %v", err)
	}

	disabledID := newID()
	if _, err := db.Exec(
		`INSERT INTO users (id, tenant_id, email, password_hash, full_name, status, is_admin, created_at, updated_at) VALUES (?, ?, ?, 'x', ?, 'disabled', 0, NOW(3), NOW(3))`,
		disabledID, tenantA, "disabled@example.test", "Trần Thị B"); err != nil {
		t.Fatalf("seed disabled user: %v", err)
	}

	deletedID := newID()
	seedUser(t, db, deletedID, tenantA)
	if _, err := db.Exec(`UPDATE users SET deleted_at = NOW(3) WHERE id = ?`, deletedID); err != nil {
		t.Fatalf("soft-delete seeded user: %v", err)
	}

	garbageID := "deadbeef-0000-0000-0000-000000000000"

	results, err := repo.BatchFetchUserLabels(ctx, tenantA, []string{activeID, disabledID, deletedID, garbageID})
	if err != nil {
		t.Fatalf("BatchFetchUserLabels: %v", err)
	}
	byID := make(map[string]UserLabelResult, len(results))
	for _, r := range results {
		byID[r.ID] = r
	}
	active, ok := byID[activeID]
	if !ok || active.Name != "Nguyễn Văn A" || active.Email != "active@example.test" {
		t.Errorf("active user result = %+v, ok=%v; want {Nguyễn Văn A active@example.test}", active, ok)
	}
	// Disabled user MUST still resolve — history should not go blank.
	disabled, ok := byID[disabledID]
	if !ok || disabled.Name != "Trần Thị B" {
		t.Errorf("disabled user result = %+v, ok=%v; want to still resolve (Trần Thị B)", disabled, ok)
	}
	if _, ok := byID[deletedID]; ok {
		t.Errorf("soft-deleted user %q must NOT resolve", deletedID)
	}
	if _, ok := byID[garbageID]; ok {
		t.Errorf("garbage id must NOT resolve")
	}
}

// TestBatchActiveUsersByLogin (queue #357) proves the CSV import's e-mail / full-name lookup is
// tenant-scoped, ACTIVE-only, case-insensitive and ACCENT-SENSITIVE ("nguyen" must not find "Nguyễn"),
// and returns every user of a shared name so the caller can call it ambiguous.
func TestBatchActiveUsersByLogin(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()

	tenantA, tenantB := newID(), newID()
	seedTenant(t, db, tenantA, "it-ul-tenant-a-"+tenantA[:8])
	seedTenant(t, db, tenantB, "it-ul-tenant-b-"+tenantB[:8])

	seed := func(tenant, email, name, status string, deleted bool) string {
		id := newID()
		if _, err := db.Exec(
			`INSERT INTO users (id, tenant_id, email, password_hash, full_name, status, is_admin, created_at, updated_at) VALUES (?, ?, ?, 'x', ?, ?, 0, NOW(3), NOW(3))`,
			id, tenant, email, name, status); err != nil {
			t.Fatalf("seed user %s: %v", email, err)
		}
		if deleted {
			if _, err := db.Exec(`UPDATE users SET deleted_at = NOW(3) WHERE id = ?`, id); err != nil {
				t.Fatalf("soft-delete %s: %v", email, err)
			}
		}
		return id
	}
	an := seed(tenantA, "An.Nguyen@Example.test", "Nguyễn Văn An", "active", false)
	lan1 := seed(tenantA, "lan1@example.test", "Lê Lan", "active", false)
	lan2 := seed(tenantA, "lan2@example.test", "Lê Lan", "active", false)
	seed(tenantA, "off@example.test", "Người Nghỉ", "disabled", false)
	seed(tenantA, "gone@example.test", "Người Xoá", "active", true)
	seed(tenantB, "other@example.test", "Khách Khác", "active", false)

	got, err := repo.BatchActiveUsersByLogin(ctx, tenantA, []string{
		"an.nguyen@example.test", "lê lan", "nguyen văn an", "người nghỉ", "off@example.test",
		"gone@example.test", "người xoá", "other@example.test", "khách khác",
	})
	if err != nil {
		t.Fatalf("BatchActiveUsersByLogin: %v", err)
	}
	ids := map[string]UserLoginMatch{}
	for _, m := range got {
		ids[m.ID] = m
	}
	if len(got) != 3 || ids[an].Email != "an.nguyen@example.test" || ids[an].Name != "nguyễn văn an" || ids[lan1].Name != "lê lan" || ids[lan2].Name != "lê lan" {
		t.Fatalf("matches = %+v; want An (by e-mail, any case), Lan1 and Lan2 (shared name) only — no accent-folded, disabled, deleted or other-tenant user", got)
	}
}
