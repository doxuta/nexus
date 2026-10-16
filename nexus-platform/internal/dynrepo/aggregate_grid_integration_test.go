//go:build integration

// Integration tests for the report grid engine (specs/049-report-grouping-pivot, slice S1
// report-grid-engine): ONE GROUP BY … WITH ROLLUP computes every leaf, sub-total and the grand
// total on MySQL itself, and must agree to the digit with the single-axis engine the grid never
// touches (AggregateGrouped / AggregateScalar), inside the SAME tenant + row scope.
//
// Run with:  go test -tags=integration -run 'TestAggregateGrid' ./internal/dynrepo/
package dynrepo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/ddl"
)

const (
	gridEntity   = "it_grid_opps"
	gridAccounts = "it_grid_accts"
)

// gridKey renders one ROLLUP row as a readable path: a rolled-up level prints "*", a NULL data
// key prints "∅" — the two NULLs the engine must keep apart.
func gridKey(r GridRow) string {
	parts := make([]string, len(r.Keys))
	for i := range r.Keys {
		switch {
		case r.Rolled[i]:
			parts[i] = "*"
		case r.Keys[i] == nil:
			parts[i] = "∅"
		default:
			parts[i] = *r.Keys[i]
		}
	}
	return strings.Join(parts, "|")
}

func gridIndex(t *testing.T, rows []GridRow) map[string]GridRow {
	t.Helper()
	m := make(map[string]GridRow, len(rows))
	for _, r := range rows {
		k := gridKey(r)
		if _, dup := m[k]; dup {
			t.Fatalf("duplicate grid row %q", k)
		}
		m[k] = r
	}
	return m
}

func valEq(got *string, want *string) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return decEq(*got, *want)
}

func seedGridTable(t *testing.T, repo *Repository, ctx context.Context) {
	t.Helper()
	_ = repo.DropTable(ctx, gridEntity)
	if err := repo.CreateTable(ctx, gridEntity, []ddl.FieldDef{
		{Name: "stage", FieldType: "picklist", Indexed: true},
		{Name: "close_date", FieldType: "date", Indexed: true},
		{Name: "amount", FieldType: "number"},
		{Name: "code", FieldType: "text", Indexed: true},
		{Name: "grp", FieldType: "text", Indexed: true},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, gridEntity) })
}

// TestAggregateGrid_RollupMatchesSingleAxis is T017: for every metric, the level-1 sub-totals of
// a 2-level grid equal AggregateGrouped on level 1 alone, the grand total equals AggregateScalar,
// and the column totals equal AggregateGrouped on the column axis — AVG/MIN/MAX computed over the
// base rows (not averaged averages), DECIMAL exact. A NULL data key stays a real group, distinct
// from a rolled-up row. An empty set returns NO row at all (MySQL emits no grand total for
// WITH ROLLUP over nothing) — the service builds `total` itself (plan §4.2).
func TestAggregateGrid_RollupMatchesSingleAxis(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	seedGridTable(t, repo, ctx)
	const tenantA = "11111111-1111-1111-1111-11111111a049"
	const tenantB = "22222222-2222-2222-2222-22222222b049"
	const user = "00000000-0000-0000-0000-0000000u0049"

	ins := func(tenant string, data map[string]any) {
		t.Helper()
		if _, err := repo.InsertRecord(ctx, gridEntity, tenant, user, data, nil); err != nil {
			t.Fatalf("InsertRecord: %v", err)
		}
	}
	ins(tenantA, map[string]any{"stage": "Proposal", "close_date": "2026-01-05", "amount": 100.25})
	ins(tenantA, map[string]any{"stage": "Proposal", "close_date": "2026-01-20", "amount": 200.5})
	ins(tenantA, map[string]any{"stage": "Proposal", "close_date": "2026-02-03", "amount": 300})
	ins(tenantA, map[string]any{"stage": "Won", "close_date": "2026-01-09", "amount": 50})
	ins(tenantA, map[string]any{"stage": "Won", "amount": 70.75}) // NULL close_date → a real ∅ group
	ins(tenantA, map[string]any{"close_date": "2026-03-01", "amount": 11})
	ins(tenantB, map[string]any{"stage": "Proposal", "close_date": "2026-01-05", "amount": 99999}) // other tenant

	axes := []GridAxis{{Physical: "stage"}, {Physical: "close_date", Bucket: BucketMonth}}
	for _, m := range []AggregateMetric{MetricCount, MetricSum, MetricAvg, MetricMin, MetricMax} {
		t.Run(string(m), func(t *testing.T) {
			ap := AggregateParams{Metric: m}
			if m != MetricCount {
				ap.FieldColumn = "amount"
			}
			rows, cols, err := repo.AggregateGrid(ctx, gridEntity, tenantA, AdminScope(), GridParams{
				AggregateParams: ap, Axes: axes, CrossTotals: true,
			})
			if err != nil {
				t.Fatalf("AggregateGrid: %v", err)
			}
			idx := gridIndex(t, rows)

			// Leaves: 6 rows → 5 leaf groups (∅ stage, ∅ date are groups of their own).
			for _, k := range []string{"Proposal|2026-01", "Proposal|2026-02", "Won|2026-01", "Won|∅", "∅|2026-03"} {
				if _, ok := idx[k]; !ok {
					t.Fatalf("missing leaf %q in %v", k, keysOf(idx))
				}
			}
			if leaf := idx["Proposal|2026-01"]; leaf.Count != 2 {
				t.Fatalf("leaf Proposal|2026-01 count = %d, want 2", leaf.Count)
			}
			// A NULL data key and a rolled-up level are different rows.
			if _, ok := idx["Won|*"]; !ok {
				t.Fatalf("missing subtotal Won|* next to data group Won|∅: %v", keysOf(idx))
			}

			// Level-1 sub-totals == the single-axis engine on the same axis.
			single, err := repo.AggregateGrouped(ctx, gridEntity, tenantA, AdminScope(), AggregateParams{
				Metric: ap.Metric, FieldColumn: ap.FieldColumn, GroupByPhysical: "stage",
			})
			if err != nil {
				t.Fatalf("AggregateGrouped: %v", err)
			}
			if len(single) != 3 {
				t.Fatalf("single-axis groups = %d, want 3", len(single))
			}
			for _, g := range single {
				k := "∅|*"
				if g.Key != nil {
					k = *g.Key + "|*"
				}
				sub, ok := idx[k]
				if !ok || sub.Count != g.Count || !valEq(sub.Value, g.Value) {
					t.Fatalf("subtotal %q = %v/%d; single-axis %v/%d", k, strOrNil(sub.Value), sub.Count, strOrNil(g.Value), g.Count)
				}
			}

			// Grand total == AggregateScalar; tenant B's 99999 never counted.
			sc, err := repo.AggregateScalar(ctx, gridEntity, tenantA, AdminScope(), ap)
			if err != nil {
				t.Fatalf("AggregateScalar: %v", err)
			}
			total, ok := idx["*|*"]
			if !ok || total.Count != 6 || total.Count != sc.Count || !valEq(total.Value, sc.Value) {
				t.Fatalf("grand total = %v/%d; scalar %v/%d", strOrNil(total.Value), total.Count, strOrNil(sc.Value), sc.Count)
			}

			// Column totals == single-axis grouped on the column axis (bucketed).
			colSingle, err := repo.AggregateGrouped(ctx, gridEntity, tenantA, AdminScope(), AggregateParams{
				Metric: ap.Metric, FieldColumn: ap.FieldColumn, GroupByPhysical: "close_date", GroupByBucket: BucketMonth,
			})
			if err != nil {
				t.Fatalf("AggregateGrouped column axis: %v", err)
			}
			if len(cols) != len(colSingle) {
				t.Fatalf("column totals = %d, single-axis = %d", len(cols), len(colSingle))
			}
			colIdx := map[string]GridRow{}
			for _, c := range cols {
				if len(c.Keys) != 1 || len(c.Rolled) != 1 || c.Rolled[0] {
					t.Fatalf("column total row shape %+v", c)
				}
				colIdx[gridKey(c)] = c
			}
			for _, g := range colSingle {
				k := "∅"
				if g.Key != nil {
					k = *g.Key
				}
				c, ok := colIdx[k]
				if !ok || c.Count != g.Count || !valEq(c.Value, g.Value) {
					t.Fatalf("column total %q = %v/%d; single-axis %v/%d", k, strOrNil(c.Value), c.Count, strOrNil(g.Value), g.Count)
				}
			}
		})
	}

	// AVG sub-total is the average of the base rows, not of the leaf averages:
	// Proposal = (100.25 + 200.5 + 300) / 3 = 200.25 (leaf avgs 150.375 and 300 would give 225.1875).
	rows, _, err := repo.AggregateGrid(ctx, gridEntity, tenantA, AdminScope(), GridParams{
		AggregateParams: AggregateParams{Metric: MetricAvg, FieldColumn: "amount"}, Axes: axes,
	})
	if err != nil {
		t.Fatalf("AggregateGrid avg: %v", err)
	}
	if sub := gridIndex(t, rows)["Proposal|*"]; sub.Value == nil || !decEq(*sub.Value, "200.25") {
		t.Fatalf("avg subtotal Proposal = %v, want 200.25", strOrNil(sub.Value))
	}

	t.Run("empty set returns no row, not even a grand total", func(t *testing.T) {
		rows, cols, err := repo.AggregateGrid(ctx, gridEntity, tenantA, AdminScope(), GridParams{
			AggregateParams: AggregateParams{Metric: MetricSum, FieldColumn: "amount",
				Filters: []Filter{{Column: "stage", Operator: FilterEq, Value: "no-such-stage", PhysicalColumn: "stage"}}},
			Axes: axes, CrossTotals: true,
		})
		if err != nil {
			t.Fatalf("AggregateGrid empty: %v", err)
		}
		if len(rows) != 0 || len(cols) != 0 {
			t.Fatalf("empty set: rows=%v cols=%v, want none", rows, cols)
		}
	})

	t.Run("having keeps leaves over the threshold; totals only add the kept leaves", func(t *testing.T) {
		// Leaves by SUM: Proposal|01=300.75 Proposal|02=300 Won|01=50 Won|∅=70.75 ∅|03=11.
		// SUM > 60 keeps Proposal|01, Proposal|02, Won|∅ → Proposal=600.75, Won=70.75, total 671.5.
		rows, cols, err := repo.AggregateGrid(ctx, gridEntity, tenantA, AdminScope(), GridParams{
			AggregateParams: AggregateParams{Metric: MetricSum, FieldColumn: "amount"},
			Axes:            axes, CrossTotals: true,
			Having: &GridHaving{Op: "gt", Values: []string{"60"}},
		})
		if err != nil {
			t.Fatalf("AggregateGrid having: %v", err)
		}
		idx := gridIndex(t, rows)
		want := map[string]string{"Proposal|2026-01": "300.75", "Proposal|2026-02": "300", "Won|∅": "70.75",
			"Proposal|*": "600.75", "Won|*": "70.75", "*|*": "671.5"}
		if len(idx) != len(want) {
			t.Fatalf("having rows = %v, want %v", keysOf(idx), want)
		}
		for k, v := range want {
			if r, ok := idx[k]; !ok || r.Value == nil || !decEq(*r.Value, v) {
				t.Fatalf("having %q = %v, want %s", k, strOrNil(r.Value), v)
			}
		}
		// Column totals over kept leaves only: 2026-01=300.75, 2026-02=300, ∅=70.75.
		colIdx := map[string]string{}
		for _, c := range cols {
			colIdx[gridKey(c)] = strOrNil(c.Value)
		}
		for k, v := range map[string]string{"2026-01": "300.75", "2026-02": "300", "∅": "70.75"} {
			if !decEq(colIdx[k], v) {
				t.Fatalf("having column total %q = %s, want %s (all: %v)", k, colIdx[k], v, colIdx)
			}
		}
		if len(colIdx) != 3 {
			t.Fatalf("having column totals = %v, want 3", colIdx)
		}
	})
}

func keysOf(m map[string]GridRow) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// bulkInsertGrid writes n rows (code c<start>..c<start+n-1>, grp=grp) in ONE statement — a digit
// cross join instead of 1000 round-trips or a recursive CTE capped by cte_max_recursion_depth.
func bulkInsertGrid(t *testing.T, db *sqlx.DB, tenant, user, grp string, start, n int) {
	t.Helper()
	table, err := TableFor(gridEntity)
	if err != nil {
		t.Fatalf("TableFor: %v", err)
	}
	stmt := fmt.Sprintf("INSERT INTO `%s` (`id`, `tenant_id`, `created_by`, `owner_id`, `data`) "+
		"SELECT UUID(), ?, ?, ?, JSON_OBJECT('code', CONCAT('c', LPAD(s.n, 5, '0')), 'grp', ?) FROM ("+
		"SELECT a.d + 10*b.d + 100*c.d + 1000*e.d AS n FROM "+
		"(SELECT 0 d UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9) a, "+
		"(SELECT 0 d UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9) b, "+
		"(SELECT 0 d UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9) c, "+
		"(SELECT 0 d UNION ALL SELECT 1) e) s WHERE s.n >= ? AND s.n < ?", table)
	if _, err := db.Exec(stmt, tenant, user, user, grp, start, start+n); err != nil {
		t.Fatalf("bulkInsertGrid: %v", err)
	}
}

// TestAggregateGrid_LimitBoundRejects is T018 — the Q3 cap (1000 leaves, reject, never truncate)
// with no counting pre-query: LIMIT N·1000+2 on the ROLLUP statement. Each case names its N.
func TestAggregateGrid_LimitBoundRejects(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	seedGridTable(t, repo, ctx)
	const tenant = "33333333-3333-3333-3333-33333333c049"
	const user = "00000000-0000-0000-0000-0000000u0149"

	count := AggregateParams{Metric: MetricCount}
	two := []GridAxis{{Physical: "grp"}, {Physical: "code"}}
	one := []GridAxis{{Physical: "code"}}

	// 1000 leaves under one parent.
	bulkInsertGrid(t, db, tenant, user, "g1", 0, 1000)

	t.Run("N=2, exactly 1000 leaves passes", func(t *testing.T) {
		rows, _, err := repo.AggregateGrid(ctx, gridEntity, tenant, AdminScope(), GridParams{AggregateParams: count, Axes: two})
		if err != nil {
			t.Fatalf("1000 leaves must pass: %v", err)
		}
		if len(rows) != 1000+1+1 {
			t.Fatalf("rows = %d, want 1002 (1000 leaves + 1 subtotal + grand total)", len(rows))
		}
	})
	t.Run("N=1, exactly 1000 leaves passes", func(t *testing.T) {
		if _, _, err := repo.AggregateGrid(ctx, gridEntity, tenant, AdminScope(), GridParams{AggregateParams: count, Axes: one}); err != nil {
			t.Fatalf("1000 leaves must pass: %v", err)
		}
	})

	// One more leaf under a second parent → 1001 leaves.
	bulkInsertGrid(t, db, tenant, user, "g2", 1000, 1)

	t.Run("N=2, 1001 leaves on 2 parents: exact count, LIMIT not reached", func(t *testing.T) {
		_, _, err := repo.AggregateGrid(ctx, gridEntity, tenant, AdminScope(), GridParams{AggregateParams: count, Axes: two})
		var tl *ErrGridTooLarge
		if !errors.As(err, &tl) {
			t.Fatalf("err = %v, want ErrGridTooLarge", err)
		}
		if tl.Leaves != 1001 || tl.LowerBound {
			t.Fatalf("got %+v, want {Leaves:1001 LowerBound:false}", *tl)
		}
	})
	t.Run("N=1, 1001 leaves + total hits LIMIT 1002: lower bound", func(t *testing.T) {
		_, _, err := repo.AggregateGrid(ctx, gridEntity, tenant, AdminScope(), GridParams{AggregateParams: count, Axes: one})
		var tl *ErrGridTooLarge
		if !errors.As(err, &tl) {
			t.Fatalf("err = %v, want ErrGridTooLarge", err)
		}
		if tl.Leaves != 1001 || !tl.LowerBound {
			t.Fatalf("got %+v, want {Leaves:1001 LowerBound:true}", *tl)
		}
	})
	t.Run("the cap is judged after having", func(t *testing.T) {
		// Every leaf has COUNT 1; COUNT > 1 keeps none → 0 rows, no error.
		rows, _, err := repo.AggregateGrid(ctx, gridEntity, tenant, AdminScope(), GridParams{
			AggregateParams: count, Axes: one, Having: &GridHaving{OnCount: true, Op: "gt", Values: []string{"1"}},
		})
		if err != nil || len(rows) != 0 {
			t.Fatalf("having that removes every leaf: rows=%d err=%v", len(rows), err)
		}
	})
}

// TestAggregateGrid_JoinedAxis is T019: a grid axis on the related entity (REPORT-JOIN hop) is
// qualified by the hop alias and the hop's own tenant + row-scope predicate still applies — an
// owner-scoped hop drops rows whose account another user owns, in every leaf and total.
func TestAggregateGrid_JoinedAxis(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const tenant = "44444444-4444-4444-4444-44444444d049"
	const tenantX = "55555555-5555-5555-5555-55555555e049"
	const u1 = "00000000-0000-0000-0000-00000000a049"
	const u2 = "00000000-0000-0000-0000-00000000b049"

	_ = repo.DropTable(ctx, gridEntity)
	_ = repo.DropTable(ctx, gridAccounts)
	if err := repo.CreateTable(ctx, gridAccounts, []ddl.FieldDef{{Name: "industry", FieldType: "picklist", Indexed: true}}); err != nil {
		t.Fatalf("CreateTable accounts: %v", err)
	}
	if err := repo.CreateTable(ctx, gridEntity, []ddl.FieldDef{
		{Name: "stage", FieldType: "picklist", Indexed: true},
		{Name: "amount", FieldType: "number"},
		{Name: "account", FieldType: "relation", RelatedEntity: gridAccounts},
	}); err != nil {
		t.Fatalf("CreateTable opps: %v", err)
	}
	t.Cleanup(func() {
		_ = repo.DropTable(ctx, gridEntity)
		_ = repo.DropTable(ctx, gridAccounts)
	})

	accTech, _ := repo.InsertRecord(ctx, gridAccounts, tenant, u1, map[string]any{"industry": "tech"}, nil)
	accBank, _ := repo.InsertRecord(ctx, gridAccounts, tenant, u2, map[string]any{"industry": "bank"}, nil)
	accOther, _ := repo.InsertRecord(ctx, gridAccounts, tenantX, u1, map[string]any{"industry": "leak"}, nil)
	for _, d := range []map[string]any{
		{"stage": "Proposal", "amount": 10, "account": accTech},
		{"stage": "Proposal", "amount": 20, "account": accBank},
		{"stage": "Won", "amount": 40, "account": accTech},
		{"stage": "Won", "amount": 80, "account": accOther}, // points at another tenant's account id
	} {
		if _, err := repo.InsertRecord(ctx, gridEntity, tenant, u1, d, nil); err != nil {
			t.Fatalf("insert opp: %v", err)
		}
	}
	relTable, _ := TableFor(gridAccounts)
	run := func(hopScope RowScope) map[string]GridRow {
		t.Helper()
		rows, cols, err := repo.AggregateGrid(ctx, gridEntity, tenant, AdminScope(), GridParams{
			AggregateParams: AggregateParams{Metric: MetricSum, FieldColumn: "amount", Joins: []JoinSpec{{
				Alias: "j0", RelatedTable: relTable, Scope: hopScope, LeftCol: "account_id", RightCol: "id",
			}}},
			Axes:        []GridAxis{{Physical: "stage"}, {Physical: "industry", Alias: "j0"}},
			CrossTotals: true,
		})
		if err != nil {
			t.Fatalf("AggregateGrid joined: %v", err)
		}
		idx := gridIndex(t, rows)
		for _, c := range cols {
			idx["col:"+gridKey(c)] = c
		}
		return idx
	}

	all := run(AdminScope())
	for k, v := range map[string]string{"Proposal|tech": "10", "Proposal|bank": "20", "Won|tech": "40",
		"Proposal|*": "30", "Won|*": "40", "*|*": "70", "col:tech": "50", "col:bank": "20"} {
		if r, ok := all[k]; !ok || r.Value == nil || !decEq(*r.Value, v) {
			t.Fatalf("admin hop %q = %v, want %s (all %v)", k, strOrNil(r.Value), v, keysOf(all))
		}
	}
	for k := range all {
		if strings.Contains(k, "leak") {
			t.Fatalf("CROSS-TENANT LEAK through the joined axis: %q", k)
		}
	}

	owner := run(RowScope{Level: AccessOwner, UserID: u1}) // u1 owns accTech only
	if _, leaked := owner["Proposal|bank"]; leaked {
		t.Fatalf("ROW-SCOPE LEAK: owner-scoped hop surfaced u2's account: %v", keysOf(owner))
	}
	for k, v := range map[string]string{"Proposal|*": "10", "Won|*": "40", "*|*": "50", "col:tech": "50"} {
		if r, ok := owner[k]; !ok || r.Value == nil || !decEq(*r.Value, v) {
			t.Fatalf("owner hop %q = %v, want %s", k, strOrNil(r.Value), v)
		}
	}
}

// TestAggregateGrid_RowScopeOwnerAndTeam is T020: a non-admin principal's grid adds up ONLY the
// rows it may read — owner scope counts its own rows in every leaf, sub-total and grand total;
// admin counts all; a team scope sees its member team's rows plus every descendant team's rows
// (teams.parent_id closure resolved before the statement is built).
func TestAggregateGrid_RowScopeOwnerAndTeam(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	seedGridTable(t, repo, ctx)

	tenant := newID()
	seedTenant(t, db, tenant, "grid-"+tenant[:8])
	region, office := newID(), newID()
	seedTeamUnder(t, db, region, tenant, "Vùng Bắc", "")
	seedTeamUnder(t, db, office, tenant, "PGD Cầu Giấy", region)
	uA, uB, uRegion := newID(), newID(), newID()
	for _, u := range []string{uA, uB, uRegion} {
		seedUser(t, db, u, tenant)
	}
	seedMembership(t, db, uB, office)
	seedMembership(t, db, uRegion, region)
	t.Cleanup(func() { cleanupA5(db, tenant, "", []string{uA, uB, uRegion}, []string{region, office}) })

	ins := func(user string, data map[string]any) {
		t.Helper()
		if _, err := repo.InsertRecord(ctx, gridEntity, tenant, user, data, nil); err != nil {
			t.Fatalf("InsertRecord: %v", err)
		}
	}
	// 5 rows: B (office team) owns 2, A (no team) owns 3.
	ins(uB, map[string]any{"stage": "Proposal", "close_date": "2026-01-05", "amount": 100})
	ins(uB, map[string]any{"stage": "Won", "close_date": "2026-02-05", "amount": 200})
	ins(uA, map[string]any{"stage": "Proposal", "close_date": "2026-01-06", "amount": 1000})
	ins(uA, map[string]any{"stage": "Proposal", "close_date": "2026-02-06", "amount": 2000})
	ins(uA, map[string]any{"stage": "Lost", "close_date": "2026-03-06", "amount": 4000})

	axes := []GridAxis{{Physical: "stage"}, {Physical: "close_date", Bucket: BucketMonth}}
	grid := func(scope RowScope) (map[string]GridRow, map[string]GridRow) {
		t.Helper()
		rows, cols, err := repo.AggregateGrid(ctx, gridEntity, tenant, scope, GridParams{
			AggregateParams: AggregateParams{Metric: MetricSum, FieldColumn: "amount"}, Axes: axes, CrossTotals: true,
		})
		if err != nil {
			t.Fatalf("AggregateGrid: %v", err)
		}
		colIdx := map[string]GridRow{}
		for _, c := range cols {
			colIdx[gridKey(c)] = c
		}
		return gridIndex(t, rows), colIdx
	}
	check := func(label string, idx map[string]GridRow, want map[string]string) {
		t.Helper()
		if len(idx) != len(want) {
			t.Fatalf("%s: rows %v, want exactly %v", label, keysOf(idx), want)
		}
		for k, v := range want {
			if r, ok := idx[k]; !ok || r.Value == nil || !decEq(*r.Value, v) {
				t.Fatalf("%s: %q = %v, want %s", label, k, strOrNil(r.Value), v)
			}
		}
	}
	ownerWant := map[string]string{"Proposal|2026-01": "100", "Won|2026-02": "200",
		"Proposal|*": "100", "Won|*": "200", "*|*": "300"}

	t.Run("owner scope: only B's 2 rows in every leaf and total", func(t *testing.T) {
		rows, cols := grid(RowScope{Level: AccessOwner, UserID: uB})
		check("owner rows", rows, ownerWant)
		check("owner cols", cols, map[string]string{"2026-01": "100", "2026-02": "200"})
		if rows["*|*"].Count != 2 {
			t.Fatalf("owner grand total count = %d, want 2", rows["*|*"].Count)
		}
	})
	t.Run("admin scope: all 5 rows", func(t *testing.T) {
		rows, cols := grid(AdminScope())
		if tot := rows["*|*"]; tot.Count != 5 || tot.Value == nil || !decEq(*tot.Value, "7300") {
			t.Fatalf("admin grand total = %v/%d, want 7300/5", strOrNil(tot.Value), tot.Count)
		}
		check("admin cols", cols, map[string]string{"2026-01": "1100", "2026-02": "2200", "2026-03": "4000"})
	})
	t.Run("team scope: the region member sees the office (descendant) rows, not A's", func(t *testing.T) {
		rows, _ := grid(RowScope{Level: AccessTeam, UserID: uRegion})
		check("region team rows", rows, ownerWant)
	})
	t.Run("none scope: nothing", func(t *testing.T) {
		rows, cols := grid(RowScope{Level: AccessNone, UserID: uB})
		if len(rows) != 0 || len(cols) != 0 {
			t.Fatalf("none scope rows=%v cols=%v", keysOf(rows), keysOf(cols))
		}
	})
}
