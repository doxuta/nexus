//go:build integration

// Integration tests for N-N join-table DDL and DML (M4). Run with:
//
//	go test -tags=integration ./internal/dynrepo/
//
// They require a reachable MySQL 8 (docker-compose `mysql` service); the harness
// (testDB, columnSet, indexSet, tableExists) lives in dynrepo_integration_test.go.
package dynrepo

import (
	"context"
	"errors"
	"testing"
)

// TestCreateAndDropJoinTable verifies that CreateJoinTable materializes a rel_<a>_<b>
// table with the correct schema, and DropJoinTable removes it cleanly.
func TestCreateAndDropJoinTable(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()

	const entityA = "it_jt_accounts"
	const entityB = "it_jt_contacts"
	const joinTable = "rel_it_jt_accounts_it_jt_contacts"

	// Clean up regardless of outcome.
	_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS `"+joinTable+"`")
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS `"+joinTable+"`")
	})

	if err := repo.CreateJoinTable(ctx, entityA, entityB); err != nil {
		t.Fatalf("CreateJoinTable: %v", err)
	}
	if !tableExists(t, db, joinTable) {
		t.Fatalf("join table %q should exist after CreateJoinTable", joinTable)
	}

	// Verify mandatory columns.
	cols := columnSet(t, db, joinTable)
	for _, col := range []string{"id", "tenant_id", "it_jt_accounts_id", "it_jt_contacts_id", "created_by", "created_at"} {
		if _, ok := cols[col]; !ok {
			t.Errorf("join table missing column %q", col)
		}
	}
	// Must NOT have obj_*-style columns.
	for _, col := range []string{"team_set_id", "modified_by", "updated_at", "deleted_at", "data"} {
		if _, ok := cols[col]; ok {
			t.Errorf("join table should not have column %q", col)
		}
	}

	// Verify unique index exists on (tenant_id, a_id, b_id).
	idxs := indexSet(t, db, joinTable)
	if _, ok := idxs["uq_it_jt_accounts_it_jt_contacts"]; !ok {
		t.Error("join table missing unique index uq_it_jt_accounts_it_jt_contacts")
	}

	// Idempotent: second call should not fail.
	if err := repo.CreateJoinTable(ctx, entityA, entityB); err != nil {
		t.Errorf("second CreateJoinTable (idempotent) returned error: %v", err)
	}

	// JoinTableExists.
	exists, err := repo.JoinTableExists(ctx, entityA, entityB)
	if err != nil || !exists {
		t.Errorf("JoinTableExists: got %v,%v; want true,nil", exists, err)
	}

	// DropJoinTable removes the table.
	if err := repo.DropJoinTable(ctx, entityA, entityB); err != nil {
		t.Fatalf("DropJoinTable: %v", err)
	}
	if tableExists(t, db, joinTable) {
		t.Error("join table should not exist after DropJoinTable")
	}
	exists, err = repo.JoinTableExists(ctx, entityA, entityB)
	if err != nil || exists {
		t.Errorf("JoinTableExists after drop: got %v,%v; want false,nil", exists, err)
	}
}

// TestLinkUnlinkListLinked is the end-to-end N-N DML test: link → list → unlink → list.
// It also verifies tenant isolation (tenant B cannot see tenant A's links) and link
// idempotency (second Link returns created=false, no error).
func TestLinkUnlinkListLinked(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()

	const eA = "it_nn_accts"
	const eB = "it_nn_conts"
	const joinTable = "rel_it_nn_accts_it_nn_conts"
	const tenantA = "aaaa1111-1111-1111-1111-111111111111"
	const tenantB = "bbbb2222-2222-2222-2222-222222222222"
	const userID = "user0000-0000-0000-0000-000000000001"

	// Set up both obj_* tables so ListLinked can JOIN them.
	_ = repo.DropTable(ctx, eA)
	_ = repo.DropTable(ctx, eB)
	_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS `"+joinTable+"`")
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS `"+joinTable+"`")
		_ = repo.DropTable(ctx, eA)
		_ = repo.DropTable(ctx, eB)
	})

	if err := repo.CreateTable(ctx, eA, nil); err != nil {
		t.Fatalf("CreateTable %s: %v", eA, err)
	}
	if err := repo.CreateTable(ctx, eB, nil); err != nil {
		t.Fatalf("CreateTable %s: %v", eB, err)
	}
	if err := repo.CreateJoinTable(ctx, eA, eB); err != nil {
		t.Fatalf("CreateJoinTable: %v", err)
	}

	// Insert some records in both entities for tenant A.
	acctID, err := repo.InsertRecord(ctx, eA, tenantA, userID, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("InsertRecord acct: %v", err)
	}
	contID1, err := repo.InsertRecord(ctx, eB, tenantA, userID, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("InsertRecord cont1: %v", err)
	}
	contID2, err := repo.InsertRecord(ctx, eB, tenantA, userID, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("InsertRecord cont2: %v", err)
	}

	// --- Link ---
	created, err := repo.LinkRecords(ctx, tenantA, userID, eA, acctID, eB, contID1)
	if err != nil {
		t.Fatalf("LinkRecords 1: %v", err)
	}
	if !created {
		t.Error("first link: expected created=true")
	}

	// Idempotent: second link returns created=false, no error.
	created, err = repo.LinkRecords(ctx, tenantA, userID, eA, acctID, eB, contID1)
	if err != nil {
		t.Fatalf("LinkRecords idempotent: %v", err)
	}
	if created {
		t.Error("idempotent link: expected created=false")
	}

	created, err = repo.LinkRecords(ctx, tenantA, userID, eA, acctID, eB, contID2)
	if err != nil {
		t.Fatalf("LinkRecords 2: %v", err)
	}
	if !created {
		t.Error("second distinct link: expected created=true")
	}

	// --- ListLinked ---
	ids, total, err := repo.ListLinked(ctx, tenantA, eA, acctID, eB, RowScope{Level: AccessAll}, 1, 20)
	if err != nil {
		t.Fatalf("ListLinked: %v", err)
	}
	if total != 2 {
		t.Errorf("ListLinked total = %d; want 2", total)
	}
	if len(ids) != 2 {
		t.Errorf("ListLinked ids len = %d; want 2", len(ids))
	}

	// --- Tenant isolation: tenant B sees zero links even though same obj_* table ---
	ids, total, err = repo.ListLinked(ctx, tenantB, eA, acctID, eB, RowScope{Level: AccessAll}, 1, 20)
	if err != nil {
		t.Fatalf("ListLinked tenantB: %v", err)
	}
	if total != 0 || len(ids) != 0 {
		t.Errorf("cross-tenant ListLinked: got total=%d len=%d; want 0/0", total, len(ids))
	}

	// --- Unlink ---
	if err := repo.UnlinkRecords(ctx, tenantA, eA, acctID, eB, contID1); err != nil {
		t.Fatalf("UnlinkRecords: %v", err)
	}

	ids, total, err = repo.ListLinked(ctx, tenantA, eA, acctID, eB, RowScope{Level: AccessAll}, 1, 20)
	if err != nil {
		t.Fatalf("ListLinked after unlink: %v", err)
	}
	if total != 1 {
		t.Errorf("after unlink total = %d; want 1", total)
	}

	// Unlink a non-existent link returns ErrRecordNotFound.
	err = repo.UnlinkRecords(ctx, tenantA, eA, acctID, eB, contID1)
	if !errors.Is(err, ErrRecordNotFound) {
		t.Errorf("unlink non-existent: got %v; want ErrRecordNotFound", err)
	}
}

// TestLinkRecords_CrossTenantTargetBlocked verifies that a link where sourceID belongs to
// tenant A but the link row uses tenant A — the join-table row is tenant-scoped. If a
// caller tries to link using tenant B's token but tenant A's sourceID, the join row
// has tenant B's tenant_id and therefore ListLinked from tenant A sees no link.
// (The actual cross-tenant guard is at the service layer via RecordExists check; this
// test just confirms the physical isolation at the repo level.)
func TestLinkRecords_TenantIsolation(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()

	const eA = "it_nn_iso_a"
	const eB = "it_nn_iso_b"
	const joinTable = "rel_it_nn_iso_a_it_nn_iso_b"
	const tenantA = "iso1aaaa-1111-1111-1111-111111111111"
	const tenantB = "iso1bbbb-2222-2222-2222-222222222222"
	const userID = "user0000-0000-0000-0000-00000000000b"

	_ = repo.DropTable(ctx, eA)
	_ = repo.DropTable(ctx, eB)
	_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS `"+joinTable+"`")
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS `"+joinTable+"`")
		_ = repo.DropTable(ctx, eA)
		_ = repo.DropTable(ctx, eB)
	})

	if err := repo.CreateTable(ctx, eA, nil); err != nil {
		t.Fatalf("CreateTable %s: %v", eA, err)
	}
	if err := repo.CreateTable(ctx, eB, nil); err != nil {
		t.Fatalf("CreateTable %s: %v", eB, err)
	}
	if err := repo.CreateJoinTable(ctx, eA, eB); err != nil {
		t.Fatalf("CreateJoinTable: %v", err)
	}

	// Insert record under tenant A.
	srcID, _ := repo.InsertRecord(ctx, eA, tenantA, userID, nil, nil)
	tgtID, _ := repo.InsertRecord(ctx, eB, tenantA, userID, nil, nil)

	// Link under tenant A — creates a row with tenant_id = tenantA.
	if _, err := repo.LinkRecords(ctx, tenantA, userID, eA, srcID, eB, tgtID); err != nil {
		t.Fatalf("LinkRecords A: %v", err)
	}

	// Tenant B querying the same src/tgt should see 0 links.
	ids, total, err := repo.ListLinked(ctx, tenantB, eA, srcID, eB, RowScope{Level: AccessAll}, 1, 20)
	if err != nil {
		t.Fatalf("ListLinked as tenantB: %v", err)
	}
	if total != 0 || len(ids) != 0 {
		t.Errorf("cross-tenant link visible to tenant B: total=%d ids=%v", total, ids)
	}
}
