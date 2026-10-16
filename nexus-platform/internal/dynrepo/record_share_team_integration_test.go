//go:build integration

package dynrepo

import (
	"context"
	"errors"
	"testing"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/ddl"
)

// seedTeamShare writes one live team share (sys_record_shares.team_id, migration 000090).
func seedTeamShare(t *testing.T, db *sqlx.DB, tenantID, entity, recordID, teamID, sharedBy string) string {
	t.Helper()
	id := newID()
	if _, err := db.Exec(
		`INSERT INTO sys_record_shares (id, tenant_id, entity_name, record_id, team_id, shared_by, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`, id, tenantID, entity, recordID, teamID, sharedBy, sharedBy); err != nil {
		t.Fatalf("seedTeamShare: %v", err)
	}
	return id
}

// TestRowLevelSecurity_ManualShareTeam is the MySQL proof of DAILY-QUEUE #224: a record shared with a
// TEAM is readable by that team's members and by members of a team ABOVE it (the closure the team
// access level already uses), under the owner AND team levels; not by the team's children, strangers,
// other tenants, after the share is revoked, on a write, or without the read opt-in.
func TestRowLevelSecurity_ManualShareTeam(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_share_team"

	tenant, tenant2 := newID(), newID()
	seedTenant(t, db, tenant, "shteam-"+tenant[:8])
	seedTenant(t, db, tenant2, "shteam2-"+tenant2[:8])
	uOwner, uMember, uParent, uChild, uStranger, uForeign := newID(), newID(), newID(), newID(), newID(), newID()
	for _, u := range []string{uOwner, uMember, uParent, uChild, uStranger} {
		seedUser(t, db, u, tenant)
	}
	seedUser(t, db, uForeign, tenant2)
	teamParent, teamX, teamChild, teamForeign := newID(), newID(), newID(), newID()
	seedTeamUnder(t, db, teamParent, tenant, "parent", "")
	seedTeamUnder(t, db, teamX, tenant, "x", teamParent)
	seedTeamUnder(t, db, teamChild, tenant, "child", teamX)
	seedTeamUnder(t, db, teamForeign, tenant2, "foreign", "")
	seedMembership(t, db, uMember, teamX)
	seedMembership(t, db, uParent, teamParent)
	seedMembership(t, db, uChild, teamChild)
	seedMembership(t, db, uForeign, teamForeign)

	t.Cleanup(func() {
		_ = repo.DropTable(ctx, entity)
		db.Exec("DELETE FROM sys_record_shares WHERE tenant_id IN (?, ?)", tenant, tenant2)
		cleanupA5(db, tenant, tenant2, []string{uOwner, uMember, uParent, uChild, uStranger, uForeign},
			[]string{teamParent, teamX, teamChild, teamForeign})
	})
	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "name", FieldType: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	recShared, err := repo.InsertRecord(ctx, entity, tenant, uOwner, map[string]any{"name": "shared-with-x"}, nil)
	if err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}
	recPrivate, err := repo.InsertRecord(ctx, entity, tenant, uOwner, map[string]any{"name": "private"}, nil)
	if err != nil {
		t.Fatalf("InsertRecord: %v", err)
	}

	teamShare := seedTeamShare(t, db, tenant, entity, recShared, teamX, uOwner)
	// Decoy: a share in ANOTHER tenant naming a tenant-1 team and record must never match.
	seedTeamShare(t, db, tenant2, entity, recPrivate, teamX, uForeign)

	visible := func(user string, level AccessLevel, optIn bool, id string) bool {
		t.Helper()
		_, err := repo.GetRecord(ctx, entity, tenant, id, RowScope{UserID: user, Level: level, IncludeShares: optIn})
		if err != nil && !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("GetRecord: %v", err)
		}
		return err == nil
	}
	listCount := func(user string, level AccessLevel) int {
		t.Helper()
		_, total, err := repo.ListRecords(ctx, entity, tenant, RowScope{UserID: user, Level: level, IncludeShares: true}, ListParams{Page: 1, Limit: 50})
		if err != nil {
			t.Fatalf("ListRecords: %v", err)
		}
		return total
	}

	for _, level := range []AccessLevel{AccessOwner, AccessTeam} {
		t.Run("member of the team sees the shared record ("+string(level)+")", func(t *testing.T) {
			if !visible(uMember, level, true, recShared) {
				t.Fatal("team member cannot GET the record shared with their team")
			}
			if visible(uMember, level, true, recPrivate) {
				t.Fatal("team member sees a record that was not shared (tenant2 decoy leaked?)")
			}
			if got := listCount(uMember, level); got != 1 {
				t.Fatalf("list total = %d; want 1", got)
			}
		})
		t.Run("member of a team ABOVE sees it ("+string(level)+")", func(t *testing.T) {
			if !visible(uParent, level, true, recShared) {
				t.Fatal("parent-team member cannot see a record shared with a team below")
			}
		})
		t.Run("child team, stranger and other-tenant user do not ("+string(level)+")", func(t *testing.T) {
			for name, u := range map[string]string{"child": uChild, "stranger": uStranger} {
				if visible(u, level, true, recShared) {
					t.Fatalf("%s sees a record shared with a team they are not in or above", name)
				}
			}
			if _, err := repo.GetRecord(ctx, entity, tenant2, recShared, RowScope{UserID: uForeign, Level: level, IncludeShares: true}); !errors.Is(err, ErrRecordNotFound) {
				t.Fatalf("cross-tenant GET err = %v; want ErrRecordNotFound", err)
			}
		})
	}

	t.Run("no read opt-in: no team reach", func(t *testing.T) {
		if visible(uMember, AccessOwner, false, recShared) {
			t.Fatal("a scope without IncludeShares reached a shared record")
		}
	})

	t.Run("write paths never honour a team share", func(t *testing.T) {
		scope := RowScope{UserID: uMember, Level: AccessOwner, IncludeShares: true}
		if err := repo.UpdateRecord(ctx, entity, tenant, recShared, uMember, scope, map[string]any{"name": "hacked"}, nil, nil); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("UpdateRecord through a team share err = %v; want ErrRecordNotFound", err)
		}
		if err := repo.SoftDeleteRecord(ctx, entity, tenant, recShared, scope); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("SoftDeleteRecord through a team share err = %v; want ErrRecordNotFound", err)
		}
	})

	t.Run("AccessNone stays blind", func(t *testing.T) {
		if visible(uMember, AccessNone, true, recShared) {
			t.Fatal("none-level user reached a shared record")
		}
	})

	t.Run("revoked team share: the whole team loses it on the next read", func(t *testing.T) {
		if _, err := db.Exec("UPDATE sys_record_shares SET deleted_at = NOW() WHERE id = ?", teamShare); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		for name, u := range map[string]string{"member": uMember, "parent": uParent} {
			if visible(u, AccessOwner, true, recShared) {
				t.Fatalf("%s still sees the record after the team share was revoked", name)
			}
		}
	})

	t.Run("CHECK: a share names exactly one grantee", func(t *testing.T) {
		_, errBoth := db.Exec(`INSERT INTO sys_record_shares (id, tenant_id, entity_name, record_id, user_id, team_id, shared_by, created_by)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, newID(), tenant, entity, recShared, uStranger, teamX, uOwner, uOwner)
		_, errNone := db.Exec(`INSERT INTO sys_record_shares (id, tenant_id, entity_name, record_id, shared_by, created_by)
			VALUES (?, ?, ?, ?, ?, ?)`, newID(), tenant, entity, recShared, uOwner, uOwner)
		if errBoth == nil || errNone == nil {
			t.Fatalf("schema accepted a share with both or neither grantee: both=%v none=%v", errBoth, errNone)
		}
	})
}
