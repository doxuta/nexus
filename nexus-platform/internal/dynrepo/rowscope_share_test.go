package dynrepo

import (
	"strings"
	"testing"
)

// shareSubquery is the exact manual-share disjunct (specs/051-record-manual-share) for an unaliased
// table — every value is a placeholder; tenant, entity and user travel as args.
const shareSubquery = "`id` IN (SELECT rs.record_id FROM sys_record_shares rs " +
	"WHERE rs.tenant_id = ? AND rs.entity_name = ? AND rs.user_id = ? AND rs.deleted_at IS NULL)"

// TestRowFilter_ManualShare pins the two-key design of the share clause: it is emitted only for a
// scope that opted in (IncludeShares) AND was pinned by a read method (forRead), it is one more OR
// term next to the owner/team predicate, and it never widens AccessNone or touches AccessAll.
func TestRowFilter_ManualShare(t *testing.T) {
	const uid, tenant, entity = "user-1", "tenant-1", "crm_contracts"

	read := func(t *testing.T, s RowScope) RowScope {
		t.Helper()
		out, err := s.forRead(tenant, entity)
		if err != nil {
			t.Fatalf("forRead: %v", err)
		}
		return out
	}

	t.Run("owner + shares → owner OR shared, args owner then tenant/entity/user", func(t *testing.T) {
		sql, args := read(t, RowScope{UserID: uid, Level: AccessOwner, IncludeShares: true}).rowFilter()
		want := " AND (COALESCE(`owner_id`, `created_by`) = ? OR " + shareSubquery + ")"
		if sql != want {
			t.Fatalf("sql = %q\nwant  %q", sql, want)
		}
		if len(args) != 4 || args[0] != uid || args[1] != tenant || args[2] != entity || args[3] != uid {
			t.Fatalf("args = %v; want [%s %s %s %s]", args, uid, tenant, entity, uid)
		}
	})

	t.Run("team (resolved, member of teams) + shares → three disjuncts", func(t *testing.T) {
		sql, args := read(t, RowScope{UserID: uid, Level: AccessTeam, TeamIDs: []string{"t1"}, IncludeShares: true}).rowFilter()
		if !strings.HasPrefix(sql, " AND (COALESCE(`owner_id`, `created_by`) = ? OR `team_set_id` IN (") ||
			!strings.HasSuffix(sql, " OR "+shareSubquery+")") {
			t.Fatalf("sql = %q", sql)
		}
		if len(args) != 5 || args[0] != uid || args[1] != "t1" || args[2] != tenant || args[3] != entity || args[4] != uid {
			t.Fatalf("args = %v", args)
		}
	})

	t.Run("team (resolved, no team) + shares → owner OR shared", func(t *testing.T) {
		sql, _ := read(t, RowScope{UserID: uid, Level: AccessTeam, TeamIDs: []string{}, IncludeShares: true}).rowFilter()
		if sql != " AND (COALESCE(`owner_id`, `created_by`) = ? OR "+shareSubquery+")" {
			t.Fatalf("sql = %q", sql)
		}
	})

	t.Run("team (unresolved) + shares keeps the membership subquery and adds shares", func(t *testing.T) {
		sql, args := read(t, RowScope{UserID: uid, Level: AccessTeam, IncludeShares: true}).rowFilter()
		if !strings.Contains(sql, "team_memberships") || !strings.HasSuffix(sql, " OR "+shareSubquery+")") {
			t.Fatalf("sql = %q", sql)
		}
		if len(args) != 5 {
			t.Fatalf("args = %v; want 5", args)
		}
	})

	t.Run("aliased read qualifies the id column", func(t *testing.T) {
		sql, _ := read(t, RowScope{UserID: uid, Level: AccessOwner, IncludeShares: true}).rowFilterFor("o")
		if !strings.Contains(sql, " OR `o`.`id` IN (SELECT rs.record_id FROM sys_record_shares rs ") {
			t.Fatalf("sql = %q", sql)
		}
	})

	t.Run("AccessNone stays 1=0 even with shares", func(t *testing.T) {
		sql, args := read(t, RowScope{UserID: uid, Level: AccessNone, IncludeShares: true}).rowFilter()
		if sql != " AND 1=0" || len(args) != 0 {
			t.Fatalf("none: sql=%q args=%v; want 1=0", sql, args)
		}
	})

	t.Run("AccessAll and admin stay unnarrowed", func(t *testing.T) {
		for _, s := range []RowScope{
			{UserID: uid, Level: AccessAll, IncludeShares: true},
			{UserID: uid, Level: AccessOwner, IsAdmin: true, IncludeShares: true},
		} {
			if sql, args := read(t, s).rowFilter(); sql != "" || len(args) != 0 {
				t.Fatalf("%+v: sql=%q args=%v; want empty", s, sql, args)
			}
		}
	})

	t.Run("opted in but never pinned by a read method → no share clause (write paths)", func(t *testing.T) {
		sql, _ := RowScope{UserID: uid, Level: AccessOwner, IncludeShares: true}.rowFilter()
		if strings.Contains(sql, "sys_record_shares") {
			t.Fatalf("unpinned scope emitted the share clause: %q", sql)
		}
	})

	t.Run("pinned by a read but not opted in → no share clause", func(t *testing.T) {
		sql, _ := read(t, RowScope{UserID: uid, Level: AccessOwner}).rowFilter()
		if strings.Contains(sql, "sys_record_shares") {
			t.Fatalf("scope without IncludeShares emitted the share clause: %q", sql)
		}
	})

	t.Run("forRead rejects an unsafe entity name when opted in", func(t *testing.T) {
		if _, err := (RowScope{UserID: uid, Level: AccessOwner, IncludeShares: true}).forRead(tenant, "x`; DROP"); err == nil {
			t.Fatal("forRead accepted an unsanitizable entity name")
		}
	})
}

// TestRowFilter_ManualShareTeams pins the team branch of the share clause (DAILY-QUEUE #224): a row
// shared with a TEAM is one more OR term, `team_id IN (the viewer's team closure)`, armed only by a
// read (forRead) with a resolved closure — and, like the user term, never on a write, AccessNone or
// AccessAll.
func TestRowFilter_ManualShareTeams(t *testing.T) {
	const uid, tenant, entity = "user-1", "tenant-1", "crm_contracts"
	teamSubquery := "`id` IN (SELECT rs.record_id FROM sys_record_shares rs " +
		"WHERE rs.tenant_id = ? AND rs.entity_name = ? AND rs.team_id IN (?,?) AND rs.deleted_at IS NULL)"
	armed := func(t *testing.T, s RowScope) RowScope {
		t.Helper()
		out, err := s.forRead(tenant, entity)
		if err != nil {
			t.Fatalf("forRead: %v", err)
		}
		out.shareTeamIDs = []string{"ta", "tb"} // what resolveReadScope fills
		return out
	}

	t.Run("owner + shares + share teams → owner OR user-shared OR team-shared", func(t *testing.T) {
		sql, args := armed(t, RowScope{UserID: uid, Level: AccessOwner, IncludeShares: true}).rowFilter()
		want := " AND (COALESCE(`owner_id`, `created_by`) = ? OR " + shareSubquery + " OR " + teamSubquery + ")"
		if sql != want {
			t.Fatalf("sql = %q\nwant  %q", sql, want)
		}
		wantArgs := []any{uid, tenant, entity, uid, tenant, entity, "ta", "tb"}
		if len(args) != len(wantArgs) {
			t.Fatalf("args = %v; want %v", args, wantArgs)
		}
		for i := range wantArgs {
			if args[i] != wantArgs[i] {
				t.Fatalf("args = %v; want %v", args, wantArgs)
			}
		}
	})

	t.Run("no share teams → no team term", func(t *testing.T) {
		s := armed(t, RowScope{UserID: uid, Level: AccessOwner, IncludeShares: true})
		s.shareTeamIDs = []string{}
		if sql, _ := s.rowFilter(); strings.Contains(sql, "team_id IN") {
			t.Fatalf("empty closure emitted a team term: %q", sql)
		}
	})

	t.Run("unpinned scope (write path) ignores share teams", func(t *testing.T) {
		s := RowScope{UserID: uid, Level: AccessOwner, IncludeShares: true, shareTeamIDs: []string{"ta"}}
		if sql, _ := s.rowFilter(); strings.Contains(sql, "sys_record_shares") {
			t.Fatalf("write-path scope emitted a share term: %q", sql)
		}
	})

	t.Run("AccessNone / AccessAll untouched", func(t *testing.T) {
		if sql, _ := armed(t, RowScope{UserID: uid, Level: AccessNone, IncludeShares: true}).rowFilter(); sql != " AND 1=0" {
			t.Fatalf("none: %q", sql)
		}
		if sql, _ := armed(t, RowScope{UserID: uid, Level: AccessAll, IncludeShares: true}).rowFilter(); sql != "" {
			t.Fatalf("all: %q", sql)
		}
	})

	t.Run("the read-only marker's own predicate drops the team term too", func(t *testing.T) {
		expr, _ := armed(t, RowScope{UserID: uid, Level: AccessOwner, IncludeShares: true}).sharedOnlyExpr()
		if expr != "IF(COALESCE(`owner_id`, `created_by`) = ?, 0, 1)" {
			t.Fatalf("expr = %q", expr)
		}
	})
}

// TestSharedOnlyExpr pins the per-record read-only marker (DAILY-QUEUE #223): on a share-armed read the
// SELECT carries IF(<the caller's OWN owner/team predicate>, 0, 1) — 1 exactly when the row is visible
// ONLY through the share clause. No marker when the share clause is not armed (write paths, scopes that
// did not opt in) or when the level does not narrow at all (AccessAll / admin see the row on their own).
func TestSharedOnlyExpr(t *testing.T) {
	const uid, tenant, entity = "user-1", "tenant-1", "crm_contracts"
	read := func(t *testing.T, s RowScope) RowScope {
		t.Helper()
		out, err := s.forRead(tenant, entity)
		if err != nil {
			t.Fatalf("forRead: %v", err)
		}
		return out
	}

	t.Run("owner + shares → IF(own predicate, 0, 1), own args only", func(t *testing.T) {
		expr, args := read(t, RowScope{UserID: uid, Level: AccessOwner, IncludeShares: true}).sharedOnlyExpr()
		if expr != "IF(COALESCE(`owner_id`, `created_by`) = ?, 0, 1)" {
			t.Fatalf("expr = %q", expr)
		}
		if len(args) != 1 || args[0] != uid {
			t.Fatalf("args = %v; want [%s]", args, uid)
		}
	})

	t.Run("team + shares → own predicate keeps the team term, drops the share term", func(t *testing.T) {
		expr, args := read(t, RowScope{UserID: uid, Level: AccessTeam, TeamIDs: []string{"t1"}, IncludeShares: true}).sharedOnlyExpr()
		if !strings.HasPrefix(expr, "IF((COALESCE(`owner_id`, `created_by`) = ? OR `team_set_id` IN (") ||
			!strings.HasSuffix(expr, "), 0, 1)") || strings.Contains(expr, "sys_record_shares") {
			t.Fatalf("expr = %q", expr)
		}
		if len(args) != 2 || args[0] != uid || args[1] != "t1" {
			t.Fatalf("args = %v", args)
		}
	})

	t.Run("share clause not armed → no marker", func(t *testing.T) {
		for name, s := range map[string]RowScope{
			"unpinned (write path)": {UserID: uid, Level: AccessOwner, IncludeShares: true},
			"not opted in":          read(t, RowScope{UserID: uid, Level: AccessOwner}),
		} {
			if expr, args := s.sharedOnlyExpr(); expr != "" || len(args) != 0 {
				t.Fatalf("%s: expr=%q args=%v; want none", name, expr, args)
			}
		}
	})

	t.Run("AccessAll and admin → no marker (never share-only)", func(t *testing.T) {
		for _, s := range []RowScope{
			{UserID: uid, Level: AccessAll, IncludeShares: true},
			{UserID: uid, Level: AccessOwner, IsAdmin: true, IncludeShares: true},
		} {
			if expr, args := read(t, s).sharedOnlyExpr(); expr != "" || len(args) != 0 {
				t.Fatalf("%+v: expr=%q args=%v; want none", s, expr, args)
			}
		}
	})
}
