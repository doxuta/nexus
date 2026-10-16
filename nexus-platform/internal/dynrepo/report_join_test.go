package dynrepo

import (
	"strings"
	"testing"
)

// These are pure SQL-shape/arg-order tests for the REPORT-JOIN builders — no DB. They lock the
// two things that make a JOIN safe: (1) every JOINed table carries its OWN `tenant_id = ?` bound
// param + its own row-level ACL predicate, and (2) placeholder args come out in statement order
// (JOIN args before WHERE args). The cross-tenant / field-ACL BEHAVIOUR is proven separately by
// integration tests against MySQL.

func TestRowFilterFor_AliasQualifies(t *testing.T) {
	tests := []struct {
		name    string
		scope   RowScope
		alias   string
		wantSQL string
		wantN   int // number of placeholder args
	}{
		{"all-no-alias", RowScope{Level: AccessAll}, "", "", 0},
		{"all-aliased", RowScope{Level: AccessAll}, "j0", "", 0},
		{"owner-no-alias", RowScope{Level: AccessOwner, UserID: "u1"}, "", " AND COALESCE(`owner_id`, `created_by`) = ?", 1},
		{"owner-aliased", RowScope{Level: AccessOwner, UserID: "u1"}, "j0", " AND COALESCE(`j0`.`owner_id`, `j0`.`created_by`) = ?", 1},
		{"team-aliased", RowScope{Level: AccessTeam, UserID: "u1"}, "j0",
			" AND (COALESCE(`j0`.`owner_id`, `j0`.`created_by`) = ? OR `j0`.`team_set_id` IN (", 2},
		{"none-aliased", RowScope{Level: AccessNone}, "j0", " AND 1=0", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql, args := tt.scope.rowFilterFor(tt.alias)
			if tt.name == "team-aliased" {
				if !strings.HasPrefix(sql, tt.wantSQL) {
					t.Fatalf("sql = %q, want prefix %q", sql, tt.wantSQL)
				}
				// The team subquery's own aliases (st, tm) must NOT be rewritten by the outer alias.
				if !strings.Contains(sql, "team_sets_teams st") || !strings.Contains(sql, "team_memberships tm") {
					t.Fatalf("team subquery aliases rewritten: %q", sql)
				}
			} else if sql != tt.wantSQL {
				t.Fatalf("sql = %q, want %q", sql, tt.wantSQL)
			}
			if len(args) != tt.wantN {
				t.Fatalf("args = %d, want %d", len(args), tt.wantN)
			}
		})
	}
	// rowFilter() must stay identical to rowFilterFor("").
	s := RowScope{Level: AccessOwner, UserID: "u1"}
	a, _ := s.rowFilter()
	b, _ := s.rowFilterFor("")
	if a != b {
		t.Fatalf("rowFilter()=%q diverged from rowFilterFor(\"\")=%q", a, b)
	}
}

func TestJoinSQL_FKChild_TenantAndScopePerHop(t *testing.T) {
	// base is CHILD (report on contacts), hop to parent accounts: j0.id = base.account_id.
	js := JoinSpec{
		Alias: "j0", RelatedTable: "obj_accounts", Optional: false,
		Scope:   RowScope{Level: AccessOwner, UserID: "u1"},
		LeftCol: "account_id", RightCol: "id",
	}
	sql, args, err := js.joinSQL("base", "tenant-A")
	if err != nil {
		t.Fatalf("joinSQL: %v", err)
	}
	// INNER JOIN with the join key, the per-hop tenant filter, soft-delete, and the row-level scope.
	for _, want := range []string{
		"INNER JOIN `obj_accounts` `j0`",
		"ON `j0`.`id` = `base`.`account_id`",
		"AND `j0`.`tenant_id` = ?",
		"AND `j0`.`deleted_at` IS NULL",
		"COALESCE(`j0`.`owner_id`, `j0`.`created_by`) = ?",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("sql missing %q\n got: %s", want, sql)
		}
	}
	// Args: [tenant, scope-user] in that order (tenant precedes the ON-clause scope arg).
	if len(args) != 2 || args[0] != "tenant-A" || args[1] != "u1" {
		t.Fatalf("args = %v, want [tenant-A u1]", args)
	}
}

func TestJoinSQL_LeftJoinKeepsPredicatesInOn(t *testing.T) {
	js := JoinSpec{
		Alias: "j0", RelatedTable: "obj_contacts", Optional: true,
		Scope:   RowScope{Level: AccessAll},
		LeftCol: "id", RightCol: "account_id", // base is PARENT (accounts), fan-out to contacts
	}
	sql, _, err := js.joinSQL("base", "tenant-A")
	if err != nil {
		t.Fatalf("joinSQL: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(sql), "LEFT JOIN") {
		t.Fatalf("want LEFT JOIN, got: %s", sql)
	}
	// A LEFT join must keep the tenant filter in the ON clause (there is no WHERE here), else it
	// collapses to an inner join.
	if !strings.Contains(sql, "ON `j0`.`account_id` = `base`.`id`") ||
		!strings.Contains(sql, "AND `j0`.`tenant_id` = ?") {
		t.Fatalf("LEFT join predicates not in ON: %s", sql)
	}
}

func TestJoinSQL_RejectsBadIdentifiers(t *testing.T) {
	bad := JoinSpec{Alias: "j0;DROP", RelatedTable: "obj_x", LeftCol: "id", RightCol: "id"}
	if _, _, err := bad.joinSQL("base", "t"); err == nil {
		t.Fatal("want error on injected alias")
	}
	bad2 := JoinSpec{Alias: "j0", RelatedTable: "obj_x", LeftCol: "a`b", RightCol: "id"}
	if _, _, err := bad2.joinSQL("base", "t"); err == nil {
		t.Fatal("want error on injected column")
	}
}

func TestAggregateFromWhere_NoJoin_UnqualifiedZeroRegression(t *testing.T) {
	p := AggregateParams{Metric: MetricCount}
	from, where, args, err := aggregateFromWhere("obj_deals", "tenant-A", RowScope{Level: AccessAll}, p)
	if err != nil {
		t.Fatalf("aggregateFromWhere: %v", err)
	}
	if from != "`obj_deals`" {
		t.Fatalf("from = %q, want `obj_deals` (unqualified)", from)
	}
	if where != "`tenant_id` = ? AND `deleted_at` IS NULL" {
		t.Fatalf("where = %q, want unqualified tenant/deleted", where)
	}
	if len(args) != 1 || args[0] != "tenant-A" {
		t.Fatalf("args = %v, want [tenant-A]", args)
	}
}

func TestAggregateFromWhere_Join_ArgOrderAndAliasing(t *testing.T) {
	// Report on contacts, joined to accounts (owner scope on both), grouped by account.industry,
	// filtered by a base field. Verifies: base aliased, JOIN present, and arg order =
	// [join tenant, join scope] ++ [base tenant] ++ [base scope] ++ [base filter].
	p := AggregateParams{
		Metric:  MetricCount,
		Filters: []Filter{{Column: "stage", Operator: FilterEq, Value: "won"}},
		Joins: []JoinSpec{{
			Alias: "j0", RelatedTable: "obj_accounts",
			Scope:   RowScope{Level: AccessOwner, UserID: "hop-user"},
			LeftCol: "account_id", RightCol: "id",
		}},
	}
	baseScope := RowScope{Level: AccessOwner, UserID: "base-user"}
	from, where, args, err := aggregateFromWhere("obj_contacts", "tenant-A", baseScope, p)
	if err != nil {
		t.Fatalf("aggregateFromWhere: %v", err)
	}
	if !strings.HasPrefix(from, "`obj_contacts` `base`") || !strings.Contains(from, "INNER JOIN `obj_accounts` `j0`") {
		t.Fatalf("from = %q, want aliased base + join", from)
	}
	// Base WHERE columns must be qualified with the base alias (else ambiguous under the join).
	for _, want := range []string{"`base`.`tenant_id` = ?", "`base`.`deleted_at` IS NULL", "COALESCE(`base`.`owner_id`, `base`.`created_by`) = ?"} {
		if !strings.Contains(where, want) {
			t.Fatalf("where missing %q\n got: %s", want, where)
		}
	}
	// The base filter must also be base-qualified.
	if !strings.Contains(where, "`base`.`data`") {
		t.Fatalf("base filter not base-qualified: %s", where)
	}
	// Arg order: join(tenant, hop-user) → base tenant → base scope → base filter value.
	want := []any{"tenant-A", "hop-user", "tenant-A", "base-user", "won"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %v, want %v (full %v)", i, args[i], want[i], args)
		}
	}
}

func TestAliasFor_DefaultsBaseWhenJoined(t *testing.T) {
	joined := AggregateParams{Joins: []JoinSpec{{Alias: "j0"}}}
	if got := joined.aliasFor(""); got != "base" {
		t.Fatalf("aliasFor(\"\") with join = %q, want base", got)
	}
	if got := joined.aliasFor("j0"); got != "j0" {
		t.Fatalf("aliasFor(j0) = %q, want j0", got)
	}
	single := AggregateParams{}
	if got := single.aliasFor("anything"); got != "" {
		t.Fatalf("aliasFor with no join = %q, want empty (unqualified)", got)
	}
}

// TestJoinSQL_ComposedTableNameIsNotHeldToTheColumnLengthRule pins the bug that broke every N-N
// report join on the SHIPPED CRM pack.
//
// ddl.SanitizeIdentifier caps at 26 characters, and it is right to: that budget exists so the index
// names DERIVED from a column/entity name stay inside MySQL's 64-char limit. But JoinSpec.
// RelatedTable is not a component identifier — it is an already-COMPOSED physical table name built
// from parts that were each sanitized on the way in (ddl.JoinParts / dynrepo.TableFor). Applying
// the component rule to the composed result rejects names MySQL is perfectly happy with.
//
// The stock CRM pack's only N-N link is crm_contacts × crm_opportunities, whose join table is
// "rel_crm_contacts_crm_opportunities" — 34 characters. So the moment the UI let a user pick an
// N-N relationship for a report, EVERY such report failed on the pack we ship. The N-N path that
// already worked (dynrepo/relation.go) never re-sanitized the composed name; only this one did.
func TestJoinSQL_ComposedTableNameIsNotHeldToTheColumnLengthRule(t *testing.T) {
	// Exactly what the shipped CRM pack produces.
	const crmLink = "rel_crm_contacts_crm_opportunities"
	if len(crmLink) <= 26 {
		t.Fatalf("fixture is no longer representative: %q is %d chars", crmLink, len(crmLink))
	}

	js := JoinSpec{
		Alias:        "j1",
		RelatedTable: crmLink,
		LeftCol:      "id",
		RightCol:     "crm_contacts_id",
	}
	sql, args, err := js.joinSQL("base", "tenant-1")
	if err != nil {
		t.Fatalf("joinSQL rejected the shipped CRM pack's own N-N link table: %v", err)
	}
	if !strings.Contains(sql, "`"+crmLink+"`") {
		t.Errorf("composed table name missing from SQL: %s", sql)
	}
	// Defense-in-depth must NOT be traded away for the fix: the tenant still binds as a parameter.
	if len(args) == 0 {
		t.Error("tenant id is no longer bound as a placeholder")
	}

	// And the charset rule must still hold — a longer name does not mean an unchecked one.
	bad := JoinSpec{Alias: "j1", RelatedTable: "rel_x`; DROP TABLE users; --", LeftCol: "id", RightCol: "x_id"}
	if _, _, err := bad.joinSQL("base", "tenant-1"); err == nil {
		t.Error("a composed table name containing backticks/semicolons was accepted — the length fix removed the charset check")
	}
}

// Queue #293: hop 2 chains off hop 1's alias, never the base — FK hop then an N-N link + far table.
func TestJoinSQL_SecondHopChainsOffFirstAlias(t *testing.T) {
	specs := []JoinSpec{
		{Alias: "j0", RelatedTable: "obj_employees", Scope: RowScope{Level: AccessAll}, LeftCol: "assignee_id", RightCol: "id"},
		{Alias: "lt1", RelatedTable: "rel_employees_tags", FromAlias: "j0", LeftCol: "id", RightCol: "employees_id", NoSoftDelete: true, NoRowScope: true},
		{Alias: "j1", RelatedTable: "obj_tags", FromAlias: "lt1", Scope: RowScope{Level: AccessAll}, LeftCol: "tags_id", RightCol: "id"},
	}
	var all string
	for _, js := range specs {
		sql, args, err := js.joinSQL("base", "tenant-A")
		if err != nil {
			t.Fatalf("joinSQL(%s): %v", js.Alias, err)
		}
		if len(args) == 0 || args[0] != "tenant-A" {
			t.Fatalf("hop %s must bind the tenant first, args=%v", js.Alias, args)
		}
		all += sql + "\n"
	}
	for _, want := range []string{
		"ON `j0`.`id` = `base`.`assignee_id`",
		"ON `lt1`.`employees_id` = `j0`.`id`",
		"ON `j1`.`id` = `lt1`.`tags_id`",
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("sql missing %q\n got: %s", want, all)
		}
	}
	if strings.Contains(all, "`lt1`.`employees_id` = `base`") {
		t.Fatalf("link table joined off the base, not hop 1: %s", all)
	}
}
