package dynrepo

import (
	"strings"
	"testing"
)

// TestRowFilter_AccessLevels proves the row-level predicate fragments + args for each access
// level, including the fail-closed default for unknown levels and the admin short-circuit.
func TestRowFilter_AccessLevels(t *testing.T) {
	const uid = "user-1"

	t.Run("all → no narrowing", func(t *testing.T) {
		sql, args := RowScope{UserID: uid, Level: AccessAll}.rowFilter()
		if sql != "" || len(args) != 0 {
			t.Fatalf("all: sql=%q args=%v; want empty", sql, args)
		}
	})

	t.Run("owner → COALESCE(owner_id, created_by) = ?", func(t *testing.T) {
		sql, args := RowScope{UserID: uid, Level: AccessOwner}.rowFilter()
		if !strings.Contains(sql, "COALESCE(`owner_id`, `created_by`) = ?") {
			t.Fatalf("owner sql missing COALESCE(owner_id, created_by) predicate: %q", sql)
		}
		if len(args) != 1 || args[0] != uid {
			t.Fatalf("owner args = %v; want [%s]", args, uid)
		}
	})

	t.Run("team → owner OR team subquery, two ? for user", func(t *testing.T) {
		sql, args := RowScope{UserID: uid, Level: AccessTeam}.rowFilter()
		if !strings.Contains(sql, "COALESCE(`owner_id`, `created_by`) = ?") || !strings.Contains(sql, "team_sets_teams") ||
			!strings.Contains(sql, "team_memberships") || !strings.Contains(sql, "team_set_id` IN") {
			t.Fatalf("team sql missing owner/team parts: %q", sql)
		}
		if len(args) != 2 || args[0] != uid || args[1] != uid {
			t.Fatalf("team args = %v; want [%s %s]", args, uid, uid)
		}
	})

	t.Run("none → match nothing", func(t *testing.T) {
		sql, args := RowScope{UserID: uid, Level: AccessNone}.rowFilter()
		if !strings.Contains(sql, "1=0") || len(args) != 0 {
			t.Fatalf("none: sql=%q args=%v; want 1=0 / no args", sql, args)
		}
	})

	t.Run("unknown level fails closed to none", func(t *testing.T) {
		sql, _ := RowScope{UserID: uid, Level: AccessLevel("bogus")}.rowFilter()
		if !strings.Contains(sql, "1=0") {
			t.Fatalf("unknown level did not fail closed: %q", sql)
		}
	})

	t.Run("empty level fails closed to none", func(t *testing.T) {
		sql, _ := RowScope{UserID: uid, Level: AccessLevel("")}.rowFilter()
		if !strings.Contains(sql, "1=0") {
			t.Fatalf("empty level did not fail closed: %q", sql)
		}
	})

	t.Run("admin short-circuits to all even with a narrow level", func(t *testing.T) {
		sql, args := RowScope{UserID: uid, Level: AccessOwner, IsAdmin: true}.rowFilter()
		if sql != "" || len(args) != 0 {
			t.Fatalf("admin: sql=%q args=%v; want all (empty)", sql, args)
		}
	})

	t.Run("AdminScope helper sees all", func(t *testing.T) {
		sql, args := AdminScope().rowFilter()
		if sql != "" || len(args) != 0 {
			t.Fatalf("AdminScope: sql=%q args=%v; want empty", sql, args)
		}
	})
}

// TestRowFilterFor_OwnerColumn_COALESCE proves the owner predicate — for BOTH AccessOwner and
// the owner-disjunct of AccessTeam, unaliased AND alias-qualified — is the COALESCE(owner_id,
// created_by) form (TRỤ 2 mục 2 "Giao việc & đổi chủ"), not a bare `created_by` reference. This
// guards the exact regression a half-applied edit to rowFilterFor's owner/team branches would
// produce (one branch updated, the other left on the old column) — report_join_test.go's
// TestRowFilterFor_AliasQualifies exercises the SAME primitive at the JOIN-builder call site;
// this is the dedicated unit-level lock on rowFilterFor itself.
func TestRowFilterFor_OwnerColumn_COALESCE(t *testing.T) {
	const uid = "user-1"

	t.Run("owner unaliased", func(t *testing.T) {
		sql, args := RowScope{UserID: uid, Level: AccessOwner}.rowFilterFor("")
		if sql != " AND COALESCE(`owner_id`, `created_by`) = ?" {
			t.Fatalf("sql = %q", sql)
		}
		if len(args) != 1 || args[0] != uid {
			t.Fatalf("args = %v; want [%s]", args, uid)
		}
	})

	t.Run("owner aliased base", func(t *testing.T) {
		sql, args := RowScope{UserID: uid, Level: AccessOwner}.rowFilterFor("base")
		if sql != " AND COALESCE(`base`.`owner_id`, `base`.`created_by`) = ?" {
			t.Fatalf("sql = %q", sql)
		}
		if len(args) != 1 || args[0] != uid {
			t.Fatalf("args = %v; want [%s]", args, uid)
		}
	})

	t.Run("team unaliased owner-disjunct", func(t *testing.T) {
		sql, args := RowScope{UserID: uid, Level: AccessTeam}.rowFilterFor("")
		want := " AND (COALESCE(`owner_id`, `created_by`) = ? OR `team_set_id` IN ("
		if !strings.HasPrefix(sql, want) {
			t.Fatalf("sql = %q, want prefix %q", sql, want)
		}
		if len(args) != 2 || args[0] != uid || args[1] != uid {
			t.Fatalf("args = %v; want [%s %s]", args, uid, uid)
		}
	})

	t.Run("team aliased base owner-disjunct", func(t *testing.T) {
		sql, args := RowScope{UserID: uid, Level: AccessTeam}.rowFilterFor("base")
		want := " AND (COALESCE(`base`.`owner_id`, `base`.`created_by`) = ? OR `base`.`team_set_id` IN ("
		if !strings.HasPrefix(sql, want) {
			t.Fatalf("sql = %q, want prefix %q", sql, want)
		}
		if len(args) != 2 || args[0] != uid || args[1] != uid {
			t.Fatalf("args = %v; want [%s %s]", args, uid, uid)
		}
	})
}

// TestTeamMD5_OrderIndependent proves the team-set dedupe key is independent of input order
// (so the same set of teams always resolves to the same team_sets row) and differs for a
// different set.
func TestTeamMD5_OrderIndependent(t *testing.T) {
	a := teamMD5([]string{"t-3", "t-1", "t-2"})
	b := teamMD5([]string{"t-1", "t-2", "t-3"})
	if a != b {
		t.Fatalf("md5 not order-independent: %q vs %q", a, b)
	}
	c := teamMD5([]string{"t-1", "t-2"})
	if a == c {
		t.Fatalf("different team sets hashed equal: %q", a)
	}
	if len(a) != 32 {
		t.Fatalf("md5 hex length = %d; want 32", len(a))
	}
}
