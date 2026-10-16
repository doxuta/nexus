package dynrepo

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// TestDescendantClosure proves the in-Go walk behind team-hierarchy row scope (queue #20): from
// the (id, parent_id, member) rows of ONE tenant it returns every team the principal is a member
// of PLUS every team below those — with a visited set, so a cycle written straight into the data
// (pre-guard rows, no hard FK on parent_id) terminates instead of hanging.
func TestDescendantClosure(t *testing.T) {
	row := func(id, parent string, member bool) teamTreeRow {
		r := teamTreeRow{ID: id, Member: member}
		if parent != "" {
			p := parent
			r.ParentID = &p
		}
		return r
	}
	deep := make([]teamTreeRow, 0, 40)
	deepWant := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("d%02d", i)
		parent := ""
		if i > 0 {
			parent = fmt.Sprintf("d%02d", i-1)
		}
		deep = append(deep, row(id, parent, i == 0))
		deepWant = append(deepWant, id)
	}

	cases := []struct {
		name string
		rows []teamTreeRow
		want []string
	}{
		{"no membership → empty (owner rows only)",
			[]teamTreeRow{row("a", "", false), row("b", "a", false)}, []string{}},
		{"3-level tree: region member sees branch and office, not a sibling root",
			[]teamTreeRow{row("region", "", true), row("branch", "region", false),
				row("office", "branch", false), row("other", "", false)},
			[]string{"branch", "office", "region"}},
		{"leaf member sees only its own team (no inheritance upward)",
			[]teamTreeRow{row("region", "", false), row("branch", "region", false),
				row("office", "branch", true)},
			[]string{"office"}},
		{"member of two teams on one chain dedupes",
			[]teamTreeRow{row("region", "", true), row("branch", "region", true)},
			[]string{"branch", "region"}},
		{"cycle a→b→a already in data terminates with both",
			[]teamTreeRow{row("a", "b", true), row("b", "a", false)}, []string{"a", "b"}},
		{"self-parent terminates",
			[]teamTreeRow{row("a", "a", true)}, []string{"a"}},
		{"deep chain of 40 reaches the leaf", deep, deepWant},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := descendantClosure(tc.rows)
			if got == nil {
				t.Fatalf("closure returned nil; want a non-nil (possibly empty) slice — nil means UNRESOLVED")
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("closure = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestRowFilter_TeamIDs proves the SQL shape of the team disjunct once the closure is resolved:
// a plain `st.team_id IN (?,…)` over team_sets_teams (no membership join — membership was already
// consulted in Go), owner-only when the closure is empty, and the legacy membership subquery when
// the scope was never resolved (nil), so an un-resolved caller degrades to direct-membership
// visibility rather than widening or failing.
func TestRowFilter_TeamIDs(t *testing.T) {
	const uid = "user-1"

	t.Run("resolved ids → IN list over team_sets_teams", func(t *testing.T) {
		sql, args := RowScope{UserID: uid, Level: AccessTeam, TeamIDs: []string{"t-1", "t-2"}}.rowFilter()
		want := " AND (COALESCE(`owner_id`, `created_by`) = ? OR `team_set_id` IN (" +
			"SELECT st.team_set_id FROM team_sets_teams st WHERE st.team_id IN (?,?)))"
		if sql != want {
			t.Fatalf("sql = %q\nwant %q", sql, want)
		}
		if !reflect.DeepEqual(args, []any{uid, "t-1", "t-2"}) {
			t.Fatalf("args = %v; want [%s t-1 t-2]", args, uid)
		}
	})

	t.Run("resolved ids are alias-qualified on the outer columns only", func(t *testing.T) {
		sql, args := RowScope{UserID: uid, Level: AccessTeam, TeamIDs: []string{"t-1"}}.rowFilterFor("j0")
		want := " AND (COALESCE(`j0`.`owner_id`, `j0`.`created_by`) = ? OR `j0`.`team_set_id` IN (" +
			"SELECT st.team_set_id FROM team_sets_teams st WHERE st.team_id IN (?)))"
		if sql != want {
			t.Fatalf("sql = %q\nwant %q", sql, want)
		}
		if !reflect.DeepEqual(args, []any{uid, "t-1"}) {
			t.Fatalf("args = %v", args)
		}
	})

	t.Run("resolved empty → owner predicate only, one arg", func(t *testing.T) {
		sql, args := RowScope{UserID: uid, Level: AccessTeam, TeamIDs: []string{}}.rowFilter()
		if sql != " AND COALESCE(`owner_id`, `created_by`) = ?" {
			t.Fatalf("sql = %q", sql)
		}
		if !reflect.DeepEqual(args, []any{uid}) {
			t.Fatalf("args = %v; want [%s]", args, uid)
		}
	})

	t.Run("nil (unresolved) keeps the legacy direct-membership subquery", func(t *testing.T) {
		sql, args := RowScope{UserID: uid, Level: AccessTeam}.rowFilter()
		if !strings.Contains(sql, "team_memberships tm") || len(args) != 2 {
			t.Fatalf("unresolved scope must emit the membership subquery with 2 args: %q %v", sql, args)
		}
	})

	t.Run("TeamIDs never widen owner/none/all", func(t *testing.T) {
		ids := []string{"t-1"}
		if sql, args := (RowScope{UserID: uid, Level: AccessOwner, TeamIDs: ids}).rowFilter(); strings.Contains(sql, "team_set_id") || len(args) != 1 {
			t.Fatalf("owner: %q %v", sql, args)
		}
		if sql, _ := (RowScope{UserID: uid, Level: AccessNone, TeamIDs: ids}).rowFilter(); !strings.Contains(sql, "1=0") {
			t.Fatalf("none: %q", sql)
		}
		if sql, _ := (RowScope{UserID: uid, Level: AccessAll, TeamIDs: ids}).rowFilter(); sql != "" {
			t.Fatalf("all: %q", sql)
		}
	})
}
