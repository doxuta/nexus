//go:build integration

package dynrepo

import (
	"context"
	"errors"
	"testing"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/ddl"
)

// seedShare writes one live sys_record_shares row (specs/051-record-manual-share) straight into the
// table — the dynrepo suite proves the row filter, not the share service.
func seedShare(t *testing.T, db *sqlx.DB, tenantID, entity, recordID, userID, sharedBy string) string {
	t.Helper()
	id := newID()
	if _, err := db.Exec(
		`INSERT INTO sys_record_shares (id, tenant_id, entity_name, record_id, user_id, shared_by, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`, id, tenantID, entity, recordID, userID, sharedBy, sharedBy); err != nil {
		t.Fatalf("seedShare: %v", err)
	}
	return id
}

// TestRowLevelSecurity_ManualShare is the MySQL proof of DAILY-QUEUE #34 (record-manual-share-a):
// a record shared with B widens B's owner/team READ scope — List, Get, cursor, aggregate — and
// nothing else. Update and soft-delete stay on B's own scope (Q4: shares are read-only), AccessNone
// still sees nothing, and a share row from another tenant, for another entity, revoked, or
// consulted through a scope that did not opt in, matches nothing.
func TestRowLevelSecurity_ManualShare(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity, other = "it_share_a", "it_share_b"

	tenant, tenant2 := newID(), newID()
	seedTenant(t, db, tenant, "share-"+tenant[:8])
	seedTenant(t, db, tenant2, "share2-"+tenant2[:8])
	uA, uB, uX := newID(), newID(), newID()
	seedUser(t, db, uA, tenant)
	seedUser(t, db, uB, tenant)
	seedUser(t, db, uX, tenant2)

	t.Cleanup(func() {
		_ = repo.DropTable(ctx, entity)
		_ = repo.DropTable(ctx, other)
		db.Exec("DELETE FROM sys_record_shares WHERE tenant_id IN (?, ?)", tenant, tenant2)
		cleanupA5(db, tenant, tenant2, []string{uA, uB, uX}, nil)
	})
	for _, e := range []string{entity, other} {
		_ = repo.DropTable(ctx, e)
		if err := repo.CreateTable(ctx, e, []ddl.FieldDef{{Name: "name", FieldType: "text"}}); err != nil {
			t.Fatalf("CreateTable %s: %v", e, err)
		}
	}
	insert := func(e, tenantID, user, name string) string {
		t.Helper()
		id, err := repo.InsertRecord(ctx, e, tenantID, user, map[string]any{"name": name}, nil)
		if err != nil {
			t.Fatalf("InsertRecord %s: %v", name, err)
		}
		return id
	}
	recShared := insert(entity, tenant, uA, "shared")
	recPrivate := insert(entity, tenant, uA, "private")
	recOther := insert(other, tenant, uA, "other-entity")
	recX := insert(entity, tenant2, uX, "other-tenant")

	seedShare(t, db, tenant, entity, recShared, uB, uA)
	// Decoys that must NEVER match for B in tenant: right record id under the wrong entity name, a
	// share written in another tenant, and a revoked share.
	seedShare(t, db, tenant, entity, recOther, uB, uA)    // recOther lives in `other`, not `entity`
	seedShare(t, db, tenant2, entity, recPrivate, uB, uX) // tenant2 row naming a tenant-1 record
	revoked := seedShare(t, db, tenant, other, recOther, uB, uA)
	if _, err := db.Exec("UPDATE sys_record_shares SET deleted_at = NOW() WHERE id = ?", revoked); err != nil {
		t.Fatalf("revoke decoy: %v", err)
	}

	read := RowScope{UserID: uB, Level: AccessOwner, IncludeShares: true}
	noOptIn := RowScope{UserID: uB, Level: AccessOwner}

	listIDs := func(e string, s RowScope) map[string]bool {
		t.Helper()
		recs, total, err := repo.ListRecords(ctx, e, tenant, s, ListParams{Page: 1, Limit: 50})
		if err != nil {
			t.Fatalf("ListRecords: %v", err)
		}
		got := make(map[string]bool, len(recs))
		for _, r := range recs {
			got[r.ID] = true
		}
		if len(got) != total {
			t.Fatalf("total %d != rows %d", total, len(got))
		}
		return got
	}

	t.Run("LIST: grantee sees exactly the shared row", func(t *testing.T) {
		got := listIDs(entity, read)
		if !got[recShared] || got[recPrivate] || got[recX] || len(got) != 1 {
			t.Fatalf("list = %v; want only %s", got, recShared)
		}
		if got := listIDs(other, read); len(got) != 0 {
			t.Fatalf("other entity list = %v; want empty (wrong-entity share + revoked share)", got)
		}
	})

	t.Run("GET: shared row visible, private row still 404", func(t *testing.T) {
		if _, err := repo.GetRecord(ctx, entity, tenant, recShared, read); err != nil {
			t.Fatalf("GetRecord shared: %v", err)
		}
		if _, err := repo.GetRecord(ctx, entity, tenant, recPrivate, read); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("GetRecord private err = %v; want ErrRecordNotFound", err)
		}
		if _, err := repo.GetRecord(ctx, other, tenant, recOther, read); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("GetRecord wrong-entity share err = %v; want ErrRecordNotFound", err)
		}
	})

	// DAILY-QUEUE #223: GetRecord says, per record, whether the caller sees it ONLY through a share —
	// the FE hides "Sửa" on open instead of waiting for the 403 on save.
	t.Run("GET marks a share-only row SharedWithMe, and only that", func(t *testing.T) {
		get := func(name, id string, s RowScope) bool {
			t.Helper()
			rec, err := repo.GetRecord(ctx, entity, tenant, id, s)
			if err != nil {
				t.Fatalf("%s: GetRecord: %v", name, err)
			}
			return rec.SharedWithMe
		}
		if !get("B through the share", recShared, read) {
			t.Fatal("B reads a share-only row with SharedWithMe = false; want true")
		}
		if !get("B team-level through the share", recShared, RowScope{UserID: uB, Level: AccessTeam, IncludeShares: true}) {
			t.Fatal("team-level B (no team) reads a share-only row with SharedWithMe = false; want true")
		}
		if get("A (owner) with shares on", recShared, RowScope{UserID: uA, Level: AccessOwner, IncludeShares: true}) {
			t.Fatal("the owner reads their own row as SharedWithMe")
		}
		if get("admin", recShared, AdminScope()) {
			t.Fatal("admin reads the row as SharedWithMe")
		}
		// B's OWN row that is also shared with B: own predicate wins — not read-only.
		recB := insert(entity, tenant, uB, "b-own")
		seedShare(t, db, tenant, entity, recB, uB, uA)
		if get("B own + shared", recB, read) {
			t.Fatal("B's own row reads as SharedWithMe because it is also shared")
		}
		// Hard-delete: a soft-deleted B-owned row would sit in B's recycle bin and skew the later subtest.
		if _, err := db.Exec("DELETE FROM `obj_"+entity+"` WHERE id = ?", recB); err != nil {
			t.Fatalf("cleanup recB: %v", err)
		}
		if _, err := db.Exec("DELETE FROM sys_record_shares WHERE tenant_id = ? AND record_id = ?", tenant, recB); err != nil {
			t.Fatalf("cleanup recB share: %v", err)
		}
	})

	t.Run("scope that did not opt in sees nothing through a share", func(t *testing.T) {
		if got := listIDs(entity, noOptIn); len(got) != 0 {
			t.Fatalf("list without IncludeShares = %v; want empty", got)
		}
		if _, err := repo.GetRecord(ctx, entity, tenant, recShared, noOptIn); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("GetRecord without IncludeShares err = %v; want ErrRecordNotFound", err)
		}
	})

	t.Run("AccessNone stays 1=0 with a share", func(t *testing.T) {
		none := RowScope{UserID: uB, Level: AccessNone, IncludeShares: true}
		if got := listIDs(entity, none); len(got) != 0 {
			t.Fatalf("none list = %v; want empty", got)
		}
	})

	t.Run("cursor + aggregate count the same rows as the list", func(t *testing.T) {
		recs, _, err := repo.ListRecordsCursor(ctx, entity, tenant, read, CursorParams{Limit: 50})
		if err != nil {
			t.Fatalf("ListRecordsCursor: %v", err)
		}
		if len(recs) != 1 || recs[0].ID != recShared {
			t.Fatalf("cursor = %v; want only %s", recs, recShared)
		}
		agg, err := repo.AggregateScalar(ctx, entity, tenant, read, AggregateParams{Metric: MetricCount})
		if err != nil {
			t.Fatalf("AggregateScalar: %v", err)
		}
		if agg.Count != 1 {
			t.Fatalf("aggregate count = %d; want 1", agg.Count)
		}
	})

	t.Run("UPDATE: a share never widens a write (read-only)", func(t *testing.T) {
		err := repo.UpdateRecord(ctx, entity, tenant, recShared, uB, read, map[string]any{"name": "hacked"}, nil, nil)
		if !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("UpdateRecord through a share err = %v; want ErrRecordNotFound", err)
		}
		v := 0
		if err := repo.UpdateRecord(ctx, entity, tenant, recShared, uB, read, map[string]any{"name": "hacked"}, &v, nil); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("version-guarded UpdateRecord through a share err = %v; want ErrRecordNotFound (no version leak)", err)
		}
		rec, err := repo.GetRecord(ctx, entity, tenant, recShared, AdminScope())
		if err != nil || rec.Data["name"] != "shared" {
			t.Fatalf("shared row changed: %v %v", rec, err)
		}
	})

	t.Run("SOFT DELETE: a share never widens a delete", func(t *testing.T) {
		if err := repo.SoftDeleteRecord(ctx, entity, tenant, recShared, read); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("SoftDeleteRecord through a share err = %v; want ErrRecordNotFound", err)
		}
		if _, err := repo.GetRecord(ctx, entity, tenant, recShared, AdminScope()); err != nil {
			t.Fatalf("shared row was deleted: %v", err)
		}
		if err := repo.SetOwner(ctx, entity, tenant, recShared, uB, read, uB); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("SetOwner through a share err = %v; want ErrRecordNotFound", err)
		}
	})

	t.Run("recycle bin does not add shares", func(t *testing.T) {
		if err := repo.SoftDeleteRecord(ctx, entity, tenant, recShared, AdminScope()); err != nil {
			t.Fatalf("admin soft-delete: %v", err)
		}
		recs, _, err := repo.ListRecords(ctx, entity, tenant, read, ListParams{Page: 1, Limit: 50, OnlyDeleted: true})
		if err != nil {
			t.Fatalf("ListRecords deleted: %v", err)
		}
		if len(recs) != 0 {
			t.Fatalf("recycle bin through a share = %d rows; want 0", len(recs))
		}
		if _, err := repo.GetRecord(ctx, entity, tenant, recShared, read); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("soft-deleted shared row still readable: %v", err)
		}
	})

	t.Run("revoking the share removes it on the next read", func(t *testing.T) {
		if err := repo.RestoreRecord(ctx, entity, tenant, recShared, uA, AdminScope(), nil); err != nil {
			t.Fatalf("restore: %v", err)
		}
		if _, err := repo.GetRecord(ctx, entity, tenant, recShared, read); err != nil {
			t.Fatalf("restored shared row not visible: %v", err)
		}
		if _, err := db.Exec("UPDATE sys_record_shares SET deleted_at = NOW() WHERE tenant_id = ? AND entity_name = ? AND record_id = ? AND user_id = ?",
			tenant, entity, recShared, uB); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if _, err := repo.GetRecord(ctx, entity, tenant, recShared, read); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("revoked share still readable: %v", err)
		}
	})
}
