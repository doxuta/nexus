package dynrepo

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// gridFixture is the report the unit tests build SQL for: contacts joined to accounts (owner
// scope on both sides), filtered by a base field, so the FROM/WHERE fragment carries join, scope
// and filter placeholders — every class of arg the grid statements must keep in order.
func gridFixture(axes []GridAxis, having *GridHaving, cross bool) GridParams {
	return GridParams{
		AggregateParams: AggregateParams{
			Metric: MetricSum, FieldColumn: "amount",
			Filters: []Filter{{Column: "stage", Operator: FilterEq, Value: "won"}},
			Joins: []JoinSpec{{
				Alias: "j0", RelatedTable: "obj_accounts",
				Scope:   RowScope{Level: AccessOwner, UserID: "hop-user"},
				LeftCol: "account_id", RightCol: "id",
			}},
		},
		Axes: axes, Having: having, CrossTotals: cross,
	}
}

func buildGrid(t *testing.T, p GridParams) (rollup, cols string, rollupArgs, colArgs, fwArgs []any) {
	t.Helper()
	from, where, fw, err := aggregateFromWhere("obj_contacts", "tenant-A", RowScope{Level: AccessOwner, UserID: "base-user"}, p.AggregateParams)
	if err != nil {
		t.Fatalf("aggregateFromWhere: %v", err)
	}
	rollup, rollupArgs, cols, colArgs, err = New(nil).buildGridSQL(from, where, fw, p)
	if err != nil {
		t.Fatalf("buildGridSQL: %v", err)
	}
	return rollup, cols, rollupArgs, colArgs, fw
}

func cat(parts ...[]any) []any {
	var out []any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// TestAggregateGrid_ArgOrder pins the placeholder order of both grid statements (plan §3):
// [FROM/WHERE args of the CTE] → [having values] → [FROM/WHERE args of the main query] → [limit].
// A shuffled order would bind the tenant id where a threshold is expected — silently wrong
// numbers, or another tenant's rows — so the order is asserted literally, and the placeholder
// count must equal the arg count for every statement.
func TestAggregateGrid_ArgOrder(t *testing.T) {
	axes := []GridAxis{{Physical: "stage"}, {Physical: "industry", Alias: "j0"}}

	t.Run("having + cross totals", func(t *testing.T) {
		rollup, cols, rollupArgs, colArgs, fw := buildGrid(t,
			gridFixture(axes, &GridHaving{Op: "between", Values: []string{"10", "500.5"}}, true))
		wantRollup := cat(fw, []any{"10", "500.5"}, fw, []any{2*MaxGridCells + 2})
		if !reflect.DeepEqual(rollupArgs, wantRollup) {
			t.Fatalf("rollup args = %v\n want %v", rollupArgs, wantRollup)
		}
		wantCols := cat(fw, []any{"10", "500.5"}, fw, []any{MaxGridCells + 1})
		if !reflect.DeepEqual(colArgs, wantCols) {
			t.Fatalf("column-total args = %v\n want %v", colArgs, wantCols)
		}
		assertPlaceholders(t, "rollup", rollup, rollupArgs)
		assertPlaceholders(t, "column totals", cols, colArgs)
	})

	t.Run("no having: no CTE, fw args then limit", func(t *testing.T) {
		rollup, cols, rollupArgs, colArgs, fw := buildGrid(t, gridFixture(axes, nil, true))
		if want := cat(fw, []any{2*MaxGridCells + 2}); !reflect.DeepEqual(rollupArgs, want) {
			t.Fatalf("rollup args = %v, want %v", rollupArgs, want)
		}
		if want := cat(fw, []any{MaxGridCells + 1}); !reflect.DeepEqual(colArgs, want) {
			t.Fatalf("column-total args = %v, want %v", colArgs, want)
		}
		assertPlaceholders(t, "rollup", rollup, rollupArgs)
		assertPlaceholders(t, "column totals", cols, colArgs)
	})

	t.Run("tree (no cross totals): no column statement", func(t *testing.T) {
		_, cols, _, colArgs, _ := buildGrid(t, gridFixture(axes, &GridHaving{OnCount: true, Op: "gt", Values: []string{"3"}}, false))
		if cols != "" || colArgs != nil {
			t.Fatalf("tree must not build a column-total statement: %q %v", cols, colArgs)
		}
	})
}

func assertPlaceholders(t *testing.T, label, stmt string, args []any) {
	t.Helper()
	if n := strings.Count(stmt, "?"); n != len(args) {
		t.Fatalf("%s: %d placeholders vs %d args\n%s", label, n, len(args), stmt)
	}
}

// TestAggregateGrid_SQLShape pins the statement shape the MySQL 8.0.46 spike (plan §3, T014)
// proved: one GROUP BY … WITH ROLLUP over N=1..3 axes, GROUPING() over the exact GROUP BY
// expression (bucket expressions included), alias-qualified joined axes, the HAVING CTE joined
// back with NULL-safe <=> on every axis, operators only from the lookup table, and the column
// totals grouped by the LAST axis only.
func TestAggregateGrid_SQLShape(t *testing.T) {
	t.Run("N=1..3 with a bucket per axis", func(t *testing.T) {
		all := []GridAxis{
			{Physical: "stage"},
			{Physical: "close_date", Bucket: BucketMonth},
			{Physical: "created_at", Bucket: BucketQuarter},
		}
		for n := 1; n <= 3; n++ {
			p := gridFixture(all[:n], nil, false)
			rollup, _, _, _, _ := buildGrid(t, p)
			if !strings.Contains(rollup, " WITH ROLLUP LIMIT ?") {
				t.Fatalf("N=%d: missing WITH ROLLUP LIMIT ?\n%s", n, rollup)
			}
			if got := strings.Count(rollup, "GROUPING("); got != n {
				t.Fatalf("N=%d: %d GROUPING() columns\n%s", n, got, rollup)
			}
			for i := 1; i <= n; i++ {
				for _, want := range []string{fmt.Sprintf("AS `k_%d`", i), fmt.Sprintf("AS `id_%d`", i), fmt.Sprintf("AS `r_%d`", i)} {
					if !strings.Contains(rollup, want) {
						t.Fatalf("N=%d: missing %q\n%s", n, want, rollup)
					}
				}
			}
			if strings.Contains(rollup, "WITH `_kept`") {
				t.Fatalf("N=%d: CTE without having\n%s", n, rollup)
			}
		}
		rollup, _, _, _, _ := buildGrid(t, gridFixture(all, nil, false))
		// Base axes are base-qualified under the join; buckets group by their exact key expression.
		for _, want := range []string{
			"CAST(`base`.`stage` AS CHAR) AS `k_1`",
			// Identity = collation weight of the trimmed value (GridRow.IDs); a bucket key is its own.
			"HEX(WEIGHT_STRING(RTRIM(`base`.`stage`))) AS `id_1`",
			"DATE_FORMAT(`base`.`close_date`, '%Y-%m') AS `id_2`",
			"GROUPING(`base`.`stage`) AS `r_1`",
			"DATE_FORMAT(`base`.`close_date`, '%Y-%m') AS `k_2`",
			"GROUPING(DATE_FORMAT(`base`.`close_date`, '%Y-%m')) AS `r_2`",
			"GROUPING(CONCAT(YEAR(`base`.`created_at`), '-Q', QUARTER(`base`.`created_at`))) AS `r_3`",
			"GROUP BY `base`.`stage`, DATE_FORMAT(`base`.`close_date`, '%Y-%m'), CONCAT(YEAR(`base`.`created_at`), '-Q', QUARTER(`base`.`created_at`)) WITH ROLLUP",
			"SUM(CAST(NULLIF(JSON_UNQUOTE(JSON_EXTRACT(`base`.`data`, '$.amount')), 'null') AS DECIMAL(38,10))) AS `agg_value`",
		} {
			if !strings.Contains(rollup, want) {
				t.Fatalf("missing %q\n%s", want, rollup)
			}
		}
	})

	t.Run("joined axis is qualified by its hop alias", func(t *testing.T) {
		rollup, cols, _, _, _ := buildGrid(t, gridFixture([]GridAxis{{Physical: "stage"}, {Physical: "industry", Alias: "j0"}}, nil, true))
		if !strings.Contains(rollup, "GROUPING(`j0`.`industry`) AS `r_2`") {
			t.Fatalf("joined axis not j0-qualified\n%s", rollup)
		}
		if !strings.Contains(cols, "SELECT CAST(`j0`.`industry` AS CHAR) AS `k_1`, HEX(WEIGHT_STRING(RTRIM(`j0`.`industry`))) AS `id_1`, ") {
			t.Fatalf("column totals must carry the column axis identity\n%s", cols)
		}
		if !strings.Contains(cols, "GROUP BY `j0`.`industry` LIMIT ?") || strings.Contains(cols, "`base`.`stage` AS") {
			t.Fatalf("column totals must group by the last axis only\n%s", cols)
		}
		if strings.Contains(cols, "ROLLUP") {
			t.Fatalf("column totals must not roll up\n%s", cols)
		}
	})

	t.Run("having: CTE joined back NULL-safely, operator from the lookup table", func(t *testing.T) {
		for token, sqlOp := range map[string]string{"gt": ">", "gte": ">=", "lt": "<", "lte": "<=", "eq": "=", "neq": "<>"} {
			rollup, cols, _, _, _ := buildGrid(t, gridFixture(
				[]GridAxis{{Physical: "stage"}, {Physical: "close_date", Bucket: BucketMonth}},
				&GridHaving{Op: token, Values: []string{"1"}}, true))
			for _, stmt := range []string{rollup, cols} {
				for _, want := range []string{
					"WITH `_kept` AS (SELECT `base`.`stage` AS `_k1`, DATE_FORMAT(`base`.`close_date`, '%Y-%m') AS `_k2` FROM ",
					"GROUP BY `base`.`stage`, DATE_FORMAT(`base`.`close_date`, '%Y-%m') HAVING SUM(CAST(NULLIF(JSON_UNQUOTE(JSON_EXTRACT(`base`.`data`, '$.amount')), 'null') AS DECIMAL(38,10))) " + sqlOp + " CAST(? AS DECIMAL(38,10)))",
					"INNER JOIN `_kept` ON `base`.`stage` <=> `_kept`.`_k1` AND DATE_FORMAT(`base`.`close_date`, '%Y-%m') <=> `_kept`.`_k2`",
				} {
					if !strings.Contains(stmt, want) {
						t.Fatalf("op %s: missing %q\n%s", token, want, stmt)
					}
				}
			}
		}
		rollup, _, _, _, _ := buildGrid(t, gridFixture([]GridAxis{{Physical: "stage"}},
			&GridHaving{OnCount: true, Op: "between", Values: []string{"2", "5"}}, false))
		if !strings.Contains(rollup, "HAVING COUNT(*) BETWEEN CAST(? AS DECIMAL(38,10)) AND CAST(? AS DECIMAL(38,10)))") {
			t.Fatalf("count between\n%s", rollup)
		}
	})

	t.Run("rejects what the service must never send", func(t *testing.T) {
		from, where, fw, err := aggregateFromWhere("obj_contacts", "tenant-A", AdminScope(), AggregateParams{Metric: MetricCount})
		if err != nil {
			t.Fatalf("aggregateFromWhere: %v", err)
		}
		r := New(nil)
		cases := map[string]GridParams{
			"no axis":          {AggregateParams: AggregateParams{Metric: MetricCount}},
			"four axes":        {AggregateParams: AggregateParams{Metric: MetricCount}, Axes: []GridAxis{{Physical: "a"}, {Physical: "b"}, {Physical: "c"}, {Physical: "d"}}},
			"duplicate axis":   {AggregateParams: AggregateParams{Metric: MetricCount}, Axes: []GridAxis{{Physical: "stage"}, {Physical: "stage"}}},
			"cross on 1 axis":  {AggregateParams: AggregateParams{Metric: MetricCount}, Axes: []GridAxis{{Physical: "stage"}}, CrossTotals: true},
			"bad identifier":   {AggregateParams: AggregateParams{Metric: MetricCount}, Axes: []GridAxis{{Physical: "stage`; DROP"}}},
			"bad bucket":       {AggregateParams: AggregateParams{Metric: MetricCount}, Axes: []GridAxis{{Physical: "close_date", Bucket: "hour"}}},
			"raw SQL operator": {AggregateParams: AggregateParams{Metric: MetricCount}, Axes: []GridAxis{{Physical: "stage"}}, Having: &GridHaving{Op: "> 0 OR 1=1 --", Values: []string{"1"}}},
			"between arity":    {AggregateParams: AggregateParams{Metric: MetricCount}, Axes: []GridAxis{{Physical: "stage"}}, Having: &GridHaving{Op: "between", Values: []string{"1"}}},
			"gt arity":         {AggregateParams: AggregateParams{Metric: MetricCount}, Axes: []GridAxis{{Physical: "stage"}}, Having: &GridHaving{Op: "gt", Values: []string{"1", "2"}}},
		}
		for name, p := range cases {
			if _, _, _, _, err := r.buildGridSQL(from, where, fw, p); err == nil {
				t.Errorf("%s: want an error", name)
			}
		}
	})
}
