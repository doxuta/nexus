//go:build integration

package dynrepo

import (
	"context"
	"errors"
	"testing"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/config"
	"nexus/platform/internal/ddl"
)

// TestSetOwner_ReassignsVisibility is the end-to-end proof of TRỤ 2 mục 2 "Giao việc & đổi chủ"
// against real MySQL 8: after SetOwner reassigns a record, the ORIGINAL owner (an AccessOwner
// scope) no longer sees it, and the NEW owner does — the exact SQL-predicate claim a fake store
// cannot prove (mirrors rls_a5_integration_test.go's harness/seed helpers).
func TestSetOwner_ReassignsVisibility(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_setowner"

	tenantA := newID()
	userA := newID() // original creator/owner
	userB := newID() // reassigned-to owner
	userOther := newID()

	seedTenant(t, db, tenantA, "it-setowner-"+tenantA[:8])
	seedUser(t, db, userA, tenantA)
	seedUser(t, db, userB, tenantA)
	seedUser(t, db, userOther, tenantA)

	t.Cleanup(func() {
		_ = repo.DropTable(ctx, entity)
		cleanupA5(db, tenantA, "", []string{userA, userB, userOther}, nil)
	})

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "subject", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	recID, err := repo.InsertRecord(ctx, entity, tenantA, userA, map[string]any{"subject": "ticket 1"}, nil)
	if err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}

	// Sanity: owner_id starts equal to created_by — userA (AccessOwner) sees it, userOther does not.
	ownerScope := func(uid string) RowScope { return RowScope{UserID: uid, Level: AccessOwner} }
	if _, err := repo.GetRecord(ctx, entity, tenantA, recID, ownerScope(userA)); err != nil {
		t.Fatalf("GetRecord (creator, pre-reassign): %v", err)
	}
	if _, err := repo.GetRecord(ctx, entity, tenantA, recID, ownerScope(userOther)); err == nil {
		t.Fatalf("GetRecord (unrelated user, pre-reassign): expected ErrRecordNotFound, got nil")
	}

	// Reassign userA -> userB.
	if err := repo.SetOwner(ctx, entity, tenantA, recID, userA, AdminScope(), userB); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}

	// The OLD owner (userA) no longer sees it under AccessOwner scope.
	if _, err := repo.GetRecord(ctx, entity, tenantA, recID, ownerScope(userA)); err == nil {
		t.Fatalf("GetRecord (old owner, post-reassign): expected ErrRecordNotFound (chủ cũ hết thấy), got nil")
	}
	// The NEW owner (userB) now sees it under AccessOwner scope.
	rec, err := repo.GetRecord(ctx, entity, tenantA, recID, ownerScope(userB))
	if err != nil {
		t.Fatalf("GetRecord (new owner, post-reassign): expected to see it (chủ mới thấy được), got %v", err)
	}
	if rec.ID != recID {
		t.Fatalf("GetRecord (new owner) returned id %q; want %q", rec.ID, recID)
	}
	// created_by is UNCHANGED (still the original creator) — only owner_id moved.
	if rec.CreatedBy != userA {
		t.Fatalf("CreatedBy = %q; want %q (created_by must stay immutable)", rec.CreatedBy, userA)
	}

	// ListRecords under AccessOwner mirrors the same flip (proves the list path, not just Get).
	listIDs := func(uid string) map[string]bool {
		recs, _, err := repo.ListRecords(ctx, entity, tenantA, ownerScope(uid), ListParams{Page: 1, Limit: 50})
		if err != nil {
			t.Fatalf("ListRecords(%s): %v", uid, err)
		}
		out := make(map[string]bool, len(recs))
		for _, r := range recs {
			out[r.ID] = true
		}
		return out
	}
	if listIDs(userA)[recID] {
		t.Errorf("ListRecords(old owner) still includes %s — chủ cũ chưa hết thấy", recID)
	}
	if !listIDs(userB)[recID] {
		t.Errorf("ListRecords(new owner) missing %s — chủ mới chưa thấy được", recID)
	}
}

// TestSetOwner_UnknownRecordIsNotFound proves SetOwner reports ErrRecordNotFound for an id that
// does not exist (never a generic 500), and never touches an out-of-scope row: an AccessOwner
// scope belonging to someone other than the record's current owner cannot reassign it either
// (defense-in-depth — RecordService.SetOwner is expected to have already required "all"/admin,
// but the repository layer must fail closed on its own too).
func TestSetOwner_UnknownRecordIsNotFound(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_setowner_404"

	tenantA := newID()
	userA := newID()
	seedTenant(t, db, tenantA, "it-setowner-404-"+tenantA[:8])
	seedUser(t, db, userA, tenantA)

	t.Cleanup(func() {
		_ = repo.DropTable(ctx, entity)
		cleanupA5(db, tenantA, "", []string{userA}, nil)
	})

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "subject", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	if err := repo.SetOwner(ctx, entity, tenantA, "no-such-id", userA, AdminScope(), userA); err == nil {
		t.Fatal("SetOwner on unknown id: expected ErrRecordNotFound, got nil")
	} else if err != ErrRecordNotFound {
		t.Fatalf("SetOwner on unknown id: err = %v; want ErrRecordNotFound", err)
	}
}

// TestSetOwner_SameOwnerIsNotNotFound pins DAILY-QUEUE #173: re-installing Ticketing on a clone
// of northwind logged `workflow.runAssign: set owner: … SetOwner: record not found` for tickets that
// existed. runAssign writes the display field first (modified_by = the engine) and then hands the
// record over; when the round-robin picked the member who already owned it, the SetOwner UPDATE
// changed nothing. Under MySQL's default a no-op UPDATE reports 0 rows and requireAffected read
// that as "no such record". The server DSN counts MATCHED rows (config.ApplyDBTimeouts, specs/057),
// so this test opens its pool through that same function — it judges the production DSN, not the
// harness's. On the raw harness DSN (no clientFoundRows) the second call fails exactly as observed.
func TestSetOwner_SameOwnerIsNotNotFound(t *testing.T) {
	db, err := sqlx.Connect("mysql", config.ApplyDBTimeouts(testDSN, 0, 0))
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping integration test", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := New(db)
	ctx := context.Background()
	const entity = "it_setowner_same"

	tenantA := newID()
	userA, userB := newID(), newID()
	seedTenant(t, db, tenantA, "it-setowner-same-"+tenantA[:8])
	seedUser(t, db, userA, tenantA)
	seedUser(t, db, userB, tenantA)
	t.Cleanup(func() {
		_ = repo.DropTable(ctx, entity)
		cleanupA5(db, tenantA, "", []string{userA, userB}, nil)
	})
	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "subject", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	recID, err := repo.InsertRecord(ctx, entity, tenantA, userA, map[string]any{"subject": "ticket"}, nil)
	if err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}

	const engine = "00000000-0000-0000-0000-000000000000"
	if err := repo.SetOwner(ctx, entity, tenantA, recID, engine, AdminScope(), userB); err != nil {
		t.Fatalf("first SetOwner (owner changes): %v", err)
	}
	// Same actor, same owner: the row matches and nothing changes.
	if err := repo.SetOwner(ctx, entity, tenantA, recID, engine, AdminScope(), userB); err != nil {
		t.Fatalf("SetOwner to the owner the record already has = %v; want nil (the record exists)", err)
	}
	// Not-found keeps its meaning: an unknown id and another tenant's id still answer 404.
	if err := repo.SetOwner(ctx, entity, tenantA, newID(), engine, AdminScope(), userB); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("SetOwner on an unknown id = %v; want ErrRecordNotFound", err)
	}
	if err := repo.SetOwner(ctx, entity, newID(), recID, engine, AdminScope(), userB); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("SetOwner through another tenant = %v; want ErrRecordNotFound", err)
	}
}

// TestSetOwner_KeepsUpdatedAt proves a reassignment is not a content edit: `updated_at` (the
// anchor of the "no change for N days" workflow timer, #287) must survive SetOwner.
func TestSetOwner_KeepsUpdatedAt(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_setowner_ts"

	tenantA := newID()
	userA := newID()
	userB := newID()
	seedTenant(t, db, tenantA, "it-setownerts-"+tenantA[:8])
	seedUser(t, db, userA, tenantA)
	seedUser(t, db, userB, tenantA)
	t.Cleanup(func() {
		_ = repo.DropTable(ctx, entity)
		cleanupA5(db, tenantA, "", []string{userA, userB}, nil)
	})
	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "subject", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	recID, err := repo.InsertRecord(ctx, entity, tenantA, userA, map[string]any{"subject": "x"}, nil)
	if err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}
	// Age the row so an auto-stamp (NOW) would be visibly different.
	if _, err := db.ExecContext(ctx, "UPDATE `obj_"+entity+"` SET `updated_at` = '2020-01-02 03:04:05' WHERE `id` = ?", recID); err != nil {
		t.Fatalf("age row: %v", err)
	}
	before, err := repo.GetRecord(ctx, entity, tenantA, recID, AdminScope())
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	if err := repo.SetOwner(ctx, entity, tenantA, recID, userA, AdminScope(), userB); err != nil {
		t.Fatalf("SetOwner: %v", err)
	}
	after, err := repo.GetRecord(ctx, entity, tenantA, recID, AdminScope())
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("updated_at moved on SetOwner: %v -> %v", before.UpdatedAt, after.UpdatedAt)
	}
}
