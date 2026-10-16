//go:build integration

package dynrepo

import (
	"testing"
)

// TestReportJoin_ManualShareOnJoinedEntity is the MySQL proof of the join half of DAILY-QUEUE #225:
// a JOIN hop's related rows honour the manual-share clause (person AND team) once the JoinSpec names
// its entity (JoinSpec.Entity pins the share clause to that table) and the hop's scope opted in. The
// base table here is read at AccessAll so only the hop's row filter is in play.
func TestReportJoin_ManualShareOnJoinedEntity(t *testing.T) {
	repo, ctx, cleanup := seedJoinFixture(t)
	defer cleanup()
	db := repo.db

	tenant := newID()
	seedTenant(t, db, tenant, "rjshare-"+tenant[:8])
	uOwner, uViewer, uTeammate := newID(), newID(), newID()
	for _, u := range []string{uOwner, uViewer, uTeammate} {
		seedUser(t, db, u, tenant)
	}
	team := newID()
	seedTeam(t, db, team, tenant, "rj-share-team")
	seedMembership(t, db, uTeammate, team)
	t.Cleanup(func() {
		db.Exec("DELETE FROM sys_record_shares WHERE tenant_id = ?", tenant)
		cleanupA5(db, tenant, "", []string{uOwner, uViewer, uTeammate}, []string{team})
	})

	insertAccount := func(industry string) string {
		t.Helper()
		id, err := repo.InsertRecord(ctx, "it_rj_accounts", tenant, uOwner, map[string]any{"industry": industry}, nil)
		if err != nil {
			t.Fatalf("insert account %s: %v", industry, err)
		}
		if _, err := repo.InsertRecord(ctx, "it_rj_contacts", tenant, uOwner, map[string]any{"account": id}, nil); err != nil {
			t.Fatalf("insert contact for %s: %v", industry, err)
		}
		return id
	}
	accPerson := insertAccount("shared-with-person")
	accTeam := insertAccount("shared-with-team")
	insertAccount("private")
	seedShare(t, db, tenant, "it_rj_accounts", accPerson, uViewer, uOwner)
	seedTeamShare(t, db, tenant, "it_rj_accounts", accTeam, team, uOwner)

	relTable, _ := TableFor("it_rj_accounts")
	groups := func(user string, entity string, includeShares bool) map[string]int64 {
		t.Helper()
		out, err := repo.AggregateGrouped(ctx, "it_rj_contacts", tenant, AdminScope(), AggregateParams{
			Metric: MetricCount, GroupByPhysical: "industry", GroupByAlias: "j0",
			Joins: []JoinSpec{{
				Alias: "j0", RelatedTable: relTable, Entity: entity,
				Scope:   RowScope{UserID: user, Level: AccessOwner, IncludeShares: includeShares},
				LeftCol: "account_id", RightCol: "id",
			}},
		})
		if err != nil {
			t.Fatalf("AggregateGrouped: %v", err)
		}
		return groupsByKey(out)
	}

	t.Run("a person share reaches the joined hop", func(t *testing.T) {
		got := groups(uViewer, "it_rj_accounts", true)
		if got["shared-with-person"] != 1 || len(got) != 1 {
			t.Fatalf("groups = %v; want only {shared-with-person:1}", got)
		}
	})
	t.Run("a team share reaches the joined hop", func(t *testing.T) {
		got := groups(uTeammate, "it_rj_accounts", true)
		if got["shared-with-team"] != 1 || len(got) != 1 {
			t.Fatalf("groups = %v; want only {shared-with-team:1}", got)
		}
	})
	t.Run("a hop that does not name its entity sees no share (fail closed)", func(t *testing.T) {
		if got := groups(uViewer, "", true); len(got) != 0 {
			t.Fatalf("unnamed hop leaked shared rows: %v", got)
		}
	})
	t.Run("a hop whose scope did not opt in sees no share", func(t *testing.T) {
		if got := groups(uViewer, "it_rj_accounts", false); len(got) != 0 {
			t.Fatalf("scope without IncludeShares reached shared rows: %v", got)
		}
	})
	t.Run("another entity's name never matches (entity_name stays in the predicate)", func(t *testing.T) {
		if got := groups(uViewer, "it_rj_contacts", true); len(got) != 0 {
			t.Fatalf("share of an account matched under the contacts entity name: %v", got)
		}
	})
}
