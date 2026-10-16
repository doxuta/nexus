package dynrepo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/ddl"
)

// Report grid engine (specs/049-report-grouping-pivot, slice S1 report-grid-engine): multi-level
// grouping with sub-totals and the two-axis matrix, computed by MySQL in ONE
// `GROUP BY g1..gN WITH ROLLUP` statement — every sub-total and the grand total is aggregated
// over the BASE rows (AVG/MIN/MAX mean what they say, DECIMAL stays exact), never summed up in
// Go. A matrix adds one `GROUP BY <column axis>` statement for the column totals (MySQL 8.0 has
// no GROUPING SETS/CUBE), run in the same read-only REPEATABLE READ transaction so the column
// totals always match the cells.
//
// This file only CALLS the shared single-axis helpers (aggregateFromWhereScoped,
// bucketKeyExprAliased, aggregateValueExprSanitized, qualCol) — aggregate.go and teamtree.go
// stay byte-identical, so the dashboard / work queue / assistant paths cannot move (plan D-2).
// The spike that proved the statement shape on MySQL 8.0.46 is recorded in plan §3 (T014).

// MaxGridCells is the Q3 cap (2026-09-05): a grid/tree with more than 1000 leaf groups is
// REJECTED (never truncated), with no counting pre-query. Same number as maxAggregateGroups.
// Exported so the service's dense-cell check (matrix rows × columns) reads the one constant.
// ponytail: hard cap by decision; raising it needs a virtualized table on the FE + a measured
// query budget (spec "Nợ còn mở").
const MaxGridCells = 1000

// maxGridAxes bounds the GROUP BY depth: a 3-level tree (groupBy + 2 subGroups) or a 2-axis
// matrix (spec FR-001/FR-002).
const maxGridAxes = 3

// GridAxis is one GROUP BY level: a sanitized, service-resolved PHYSICAL (indexed) column, the
// table alias it lives on ("" / "base" = base entity, a JoinSpec alias = the joined entity), and
// an optional calendar bucket over a DATETIME column.
type GridAxis struct {
	Physical string
	Alias    string
	Bucket   AggregateBucket
}

// GridHaving is the single threshold a report may put on its LEAF groups (spec FR-012): on the
// metric value (OnCount=false) or the row count, with an operator TOKEN resolved through
// gridHavingOps (never SQL text from a client) and 1 value (2 for "between"). Values are decimal
// text, validated by the service and bound as CAST(? AS DECIMAL(38,10)).
type GridHaving struct {
	OnCount bool
	Op      string
	Values  []string
}

// gridHavingOps is the ONLY source of operator SQL for a HAVING threshold.
var gridHavingOps = map[string]string{
	"gt": ">", "gte": ">=", "lt": "<", "lte": "<=", "eq": "=", "neq": "<>", "between": "BETWEEN",
}

// GridParams is the resolved input of a grid query. The embedded AggregateParams contributes the
// metric (Metric/FieldColumn/FieldSubPath/FieldAlias), the base-only Filters/Search and the Joins
// — exactly as the single-axis engine reads them; its GroupBy*/GroupLimit fields are ignored.
type GridParams struct {
	AggregateParams
	// Axes are the GROUP BY levels, outermost first (1..maxGridAxes).
	Axes []GridAxis
	// CrossTotals adds the matrix column totals: one GROUP BY on the LAST axis.
	CrossTotals bool
	// Having keeps only the leaf groups passing the threshold; every total then adds up the kept
	// leaves only (a CTE joined back with NULL-safe <=> on every axis).
	Having *GridHaving
}

// GridRow is one ROLLUP output row. Keys[i] is level i's key text (nil = NULL data key); Rolled[i]
// is MySQL GROUPING() for level i — true means "all values of this level" (a sub-total or the
// grand total), which is how a rolled-up NULL is told apart from a NULL data key. ROLLUP only
// rolls up a SUFFIX of the levels, so a row's depth is the number of leading Rolled=false.
//
// IDs[i] is level i's GROUP IDENTITY (nil = NULL data key): two rows — of either statement — are
// in the same level-i group iff their IDs[i] are equal. Keys[i] is NOT that: MySQL groups a text
// column by its collation (utf8mb4_unicode_ci / utf8mb4_bin are PAD SPACE, the former also case-
// and accent-insensitive) and returns ONE member's text per row, not always the same member — a
// leaf, its sub-total and a column total can spell "Proposal", "PROPOSAL", "proposal " for one
// group. Keys are for display; match rows on IDs only.
type GridRow struct {
	Keys   []*string
	IDs    []*string
	Rolled []bool
	Value  *string
	Count  int64
}

// ErrGridTooLarge reports more than MaxGridCells leaf groups. LowerBound means the ROLLUP
// statement hit its LIMIT, so Leaves (= MaxGridCells+1) is only "at least". The service maps it
// to 422 REPORT_TOO_MANY_GROUPS / REPORT_MATRIX_TOO_MANY_CELLS.
type ErrGridTooLarge struct {
	Leaves     int
	LowerBound bool
}

func (e *ErrGridTooLarge) Error() string {
	if e.LowerBound {
		return fmt.Sprintf("grid has at least %d leaf groups (cap %d)", e.Leaves, MaxGridCells)
	}
	return fmt.Sprintf("grid has %d leaf groups (cap %d)", e.Leaves, MaxGridCells)
}

// AggregateGrid runs the grid statements over the tenant + row-scoped (base AND every join hop),
// filtered set and returns the ROLLUP rows (leaves, sub-totals, grand total — in MySQL's order;
// the service sorts) plus, when p.CrossTotals, the column totals (one row per last-axis key).
//
// The cap is judged on the ROLLUP statement itself: it returns ≤ N·leaves+1 rows, so with
// LIMIT N·MaxGridCells+2 a full page means leaves > MaxGridCells (LowerBound); otherwise the
// leaves are counted exactly. An empty set returns no row at all (MySQL emits no grand total for
// WITH ROLLUP over nothing).
func (r *Repository) AggregateGrid(ctx context.Context, entity, tenantID string, scope RowScope, p GridParams) (rows, colTotals []GridRow, err error) {
	table, err := tableFor(entity)
	if err != nil {
		return nil, nil, fmt.Errorf("dynrepo.AggregateGrid: %w", err)
	}
	// Row scopes (base + every hop, incl. the team closure) resolve BEFORE any transaction.
	fromJoin, where, fwArgs, err := r.aggregateFromWhereScoped(ctx, entity, table, tenantID, scope, p.AggregateParams)
	if err != nil {
		return nil, nil, fmt.Errorf("dynrepo.AggregateGrid: %w", err)
	}
	rollupSQL, rollupArgs, colSQL, colArgs, err := r.buildGridSQL(fromJoin, where, fwArgs, p)
	if err != nil {
		return nil, nil, fmt.Errorf("dynrepo.AggregateGrid: %w", err)
	}
	n := len(p.Axes)

	if !p.CrossTotals {
		// One statement = one consistent read; no transaction needed.
		if rows, err = scanGridRows(ctx, r.db, rollupSQL, rollupArgs, n, true); err != nil {
			return nil, nil, fmt.Errorf("dynrepo.AggregateGrid: %w", err)
		}
		if err := checkGridCap(rows, n); err != nil {
			return nil, nil, fmt.Errorf("dynrepo.AggregateGrid: %w", err)
		}
		return rows, nil, nil
	}

	// Matrix: cells and column totals must come from ONE snapshot (spec FR-011). The isolation
	// level is pinned explicitly: under READ COMMITTED (a DSN or server default away) every
	// statement would take its own snapshot and column totals could disagree with the cells.
	tx, err := r.db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, nil, fmt.Errorf("dynrepo.AggregateGrid: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if rows, err = scanGridRows(ctx, tx, rollupSQL, rollupArgs, n, true); err != nil {
		return nil, nil, fmt.Errorf("dynrepo.AggregateGrid: %w", err)
	}
	if err := checkGridCap(rows, n); err != nil {
		return nil, nil, fmt.Errorf("dynrepo.AggregateGrid: %w", err)
	}
	if colTotals, err = scanGridRows(ctx, tx, colSQL, colArgs, 1, false); err != nil {
		return nil, nil, fmt.Errorf("dynrepo.AggregateGrid: column totals: %w", err)
	}
	// Columns ≤ leaves ≤ MaxGridCells under the same snapshot, so this LIMIT is only a backstop —
	// and a backstop that truncates silently would break the Q3 rule; reject instead.
	if len(colTotals) > MaxGridCells {
		return nil, nil, fmt.Errorf("dynrepo.AggregateGrid: %w", &ErrGridTooLarge{Leaves: MaxGridCells + 1, LowerBound: true})
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("dynrepo.AggregateGrid: commit: %w", err)
	}
	return rows, colTotals, nil
}

// checkGridCap applies the Q3 cap to the ROLLUP rows of an n-axis grid (see AggregateGrid).
func checkGridCap(rows []GridRow, n int) error {
	if len(rows) >= n*MaxGridCells+2 {
		return &ErrGridTooLarge{Leaves: MaxGridCells + 1, LowerBound: true}
	}
	leaves := 0
	for _, r := range rows {
		if !r.Rolled[n-1] { // ROLLUP rolls a suffix: the last level un-rolled ⇒ a leaf
			leaves++
		}
	}
	if leaves > MaxGridCells {
		return &ErrGridTooLarge{Leaves: leaves}
	}
	return nil
}

// buildGridSQL composes the ROLLUP statement and (CrossTotals) the column-total statement over a
// FROM/WHERE fragment built by aggregateFromWhere(Scoped). Pure — no I/O — so the statement shape
// and the placeholder order are unit-tested. Arg order of each statement:
//
//	[fwArgs of the HAVING CTE] [having values] [fwArgs of the main query] [limit]
//
// (the CTE block and its args are absent without Having). Every identifier is re-sanitized here
// (defense-in-depth — the service resolved them against sys_fields); operator SQL comes only from
// gridHavingOps; every value is a `?`.
func (r *Repository) buildGridSQL(fromJoin, where string, fwArgs []any, p GridParams) (rollupSQL string, rollupArgs []any, colSQL string, colArgs []any, err error) {
	n := len(p.Axes)
	if n == 0 || n > maxGridAxes {
		return "", nil, "", nil, fmt.Errorf("grid needs 1..%d axes, got %d", maxGridAxes, n)
	}
	if p.CrossTotals && n < 2 {
		return "", nil, "", nil, fmt.Errorf("column totals need a row and a column axis, got %d axis", n)
	}
	groupExprs := make([]string, n)
	keyExprs := make([]string, n)
	idExprs := make([]string, n)
	seen := make(map[string]bool, n)
	for i, ax := range p.Axes {
		col, cerr := ddl.SanitizeIdentifier(ax.Physical)
		if cerr != nil {
			return "", nil, "", nil, fmt.Errorf("axis %d column %q: %w", i+1, ax.Physical, cerr)
		}
		alias, aerr := sanitizeAlias(p.aliasFor(ax.Alias))
		if aerr != nil {
			return "", nil, "", nil, fmt.Errorf("axis %d alias %q: %w", i+1, ax.Alias, aerr)
		}
		// Same key/group expressions as AggregateGrouped: CAST(col AS CHAR) grouped by the column,
		// or the bucket key text grouped by itself (GROUPING() must see the exact GROUP BY text).
		groupExprs[i] = qualCol(alias, col)
		keyExprs[i] = fmt.Sprintf("CAST(%s AS CHAR)", groupExprs[i])
		// Group identity (see GridRow.IDs): the collation weight of the value with its trailing
		// spaces cut — every obj_* text column is PAD SPACE, so members of one group differ only in
		// what the weight ignores (case, accents) or in trailing spaces. Non-text columns (DECIMAL,
		// DATETIME, CHAR ids) become their one canonical text first, so the weight is never NULL.
		idExprs[i] = fmt.Sprintf("HEX(WEIGHT_STRING(RTRIM(%s)))", groupExprs[i])
		if ax.Bucket != "" {
			bk, berr := bucketKeyExprAliased(ax.Bucket, alias, col)
			if berr != nil {
				return "", nil, "", nil, fmt.Errorf("axis %d: %w", i+1, berr)
			}
			// A bucket key is computed from the value, so its text is already one per group.
			groupExprs[i], keyExprs[i], idExprs[i] = bk, bk, bk
		}
		// A repeated level makes GROUPING() ambiguous; the service rejects duplicate axes, and so
		// does this layer.
		if seen[groupExprs[i]] {
			return "", nil, "", nil, fmt.Errorf("axis %d repeats %s", i+1, groupExprs[i])
		}
		seen[groupExprs[i]] = true
	}
	valExpr, err := r.aggregateValueExprSanitized(p.AggregateParams)
	if err != nil {
		return "", nil, "", nil, err
	}

	// HAVING threshold on the LEAF groups: a CTE of the kept leaf keys, joined back NULL-safely so
	// every sub-total/total re-aggregates only the base rows of kept leaves (Excel-pivot "value
	// filter"). CTE columns start with "_" — an obj_* column never can (^[a-z]…), so the join can
	// never make an unqualified base column ambiguous.
	var cte, keptJoin string
	var cteArgs []any
	if h := p.Having; h != nil {
		op, ok := gridHavingOps[h.Op]
		if !ok {
			return "", nil, "", nil, fmt.Errorf("having operator %q is not supported", h.Op)
		}
		want := 1
		if h.Op == "between" {
			want = 2
		}
		if len(h.Values) != want {
			return "", nil, "", nil, fmt.Errorf("having %q needs %d value(s), got %d", h.Op, want, len(h.Values))
		}
		subject := valExpr
		if h.OnCount {
			subject = "COUNT(*)"
		}
		cond := fmt.Sprintf("%s %s CAST(? AS DECIMAL(38,10))", subject, op)
		if h.Op == "between" {
			cond = fmt.Sprintf("%s BETWEEN CAST(? AS DECIMAL(38,10)) AND CAST(? AS DECIMAL(38,10))", subject)
		}
		kept := make([]string, n)
		on := make([]string, n)
		for i, g := range groupExprs {
			kept[i] = fmt.Sprintf("%s AS `_k%d`", g, i+1)
			on[i] = fmt.Sprintf("%s <=> `_kept`.`_k%d`", g, i+1)
		}
		cte = fmt.Sprintf("WITH `_kept` AS (SELECT %s FROM %s WHERE %s GROUP BY %s HAVING %s) ",
			strings.Join(kept, ", "), fromJoin, where, strings.Join(groupExprs, ", "), cond)
		keptJoin = " INNER JOIN `_kept` ON " + strings.Join(on, " AND ")
		cteArgs = append(append(cteArgs, fwArgs...), anySlice(h.Values)...)
	}

	sel := make([]string, 0, 3*n+2)
	for i, k := range keyExprs {
		sel = append(sel, fmt.Sprintf("%s AS `k_%d`", k, i+1))
	}
	for i, id := range idExprs {
		sel = append(sel, fmt.Sprintf("%s AS `id_%d`", id, i+1))
	}
	for i, g := range groupExprs {
		sel = append(sel, fmt.Sprintf("GROUPING(%s) AS `r_%d`", g, i+1))
	}
	sel = append(sel, valExpr+" AS `agg_value`", "COUNT(*) AS `agg_count`")
	rollupSQL = fmt.Sprintf("%sSELECT %s FROM %s%s WHERE %s GROUP BY %s WITH ROLLUP LIMIT ?",
		cte, strings.Join(sel, ", "), fromJoin, keptJoin, where, strings.Join(groupExprs, ", "))
	rollupArgs = append(append(append([]any{}, cteArgs...), fwArgs...), n*MaxGridCells+2)

	if p.CrossTotals {
		last := n - 1
		colSQL = fmt.Sprintf("%sSELECT %s AS `k_1`, %s AS `id_1`, %s AS `agg_value`, COUNT(*) AS `agg_count` FROM %s%s WHERE %s GROUP BY %s LIMIT ?",
			cte, keyExprs[last], idExprs[last], valExpr, fromJoin, keptJoin, where, groupExprs[last])
		colArgs = append(append(append([]any{}, cteArgs...), fwArgs...), MaxGridCells+1)
	}
	return rollupSQL, rollupArgs, colSQL, colArgs, nil
}

func anySlice(vals []string) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = v
	}
	return out
}

// scanGridRows reads n key columns, n identity columns, then (withGrouping) n GROUPING() flags,
// then value + count.
func scanGridRows(ctx context.Context, q sqlx.QueryerContext, stmt string, args []any, n int, withGrouping bool) ([]GridRow, error) {
	rs, err := q.QueryxContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []GridRow{}
	for rs.Next() {
		keys := make([]sql.NullString, n)
		ids := make([]sql.NullString, n)
		flags := make([]int64, n)
		var val sql.NullString
		var cnt int64
		dest := make([]any, 0, 3*n+2)
		for i := range keys {
			dest = append(dest, &keys[i])
		}
		for i := range ids {
			dest = append(dest, &ids[i])
		}
		if withGrouping {
			for i := range flags {
				dest = append(dest, &flags[i])
			}
		}
		dest = append(dest, &val, &cnt)
		if err := rs.Scan(dest...); err != nil {
			return nil, err
		}
		row := GridRow{Keys: make([]*string, n), IDs: make([]*string, n), Rolled: make([]bool, n), Count: cnt}
		for i := range keys {
			if keys[i].Valid {
				k := keys[i].String
				row.Keys[i] = &k
			}
			if ids[i].Valid {
				id := ids[i].String
				row.IDs[i] = &id
			}
			row.Rolled[i] = flags[i] == 1
		}
		if val.Valid {
			v := val.String
			row.Value = &v
		}
		out = append(out, row)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
