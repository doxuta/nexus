//go:build integration

// Integration tests for the report grid service (specs/049-report-grouping-pivot, slice S2a
// report-grid-service, T035b) on a real throwaway MySQL 8 database (o3aSetup): the matrix a
// NON-admin employee gets adds up only the rows they may read — every cell, row total, column
// total and the grand total — and each row/column total equals the single-axis Aggregate the
// dashboard already trusts, for the same scope, to the decimal digit. The dense-cell cap (Q3:
// reject over 1000, never truncate) is pinned on real rows with one definition of "cell".
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -run 'TestRecordAggregateGrid' ./internal/services/
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

const (
	gridUserA = "11111111-0000-0000-0000-00000000049a"
	gridUserB = "22222222-0000-0000-0000-00000000049b"
)

func gridSetup(t *testing.T) (*RecordService, *EntityService, *sqlx.DB) {
	t.Helper()
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	mc := NewMetaCache(metaRepo, DefaultMetaTTL)
	return NewRecordService(mc, dynRepo), NewEntityService(dynRepo, metaRepo).WithMetaCache(mc), sdb
}

func gridDecEq(got *string, want string) bool {
	if got == nil {
		return false
	}
	g, ok1 := new(big.Rat).SetString(*got)
	w, ok2 := new(big.Rat).SetString(want)
	return ok1 && ok2 && g.Cmp(w) == 0
}

// TestRecordAggregateGrid_MatrixOwnerScoped: 5 opportunities, employee B (not admin, view@owner)
// owns 2. B's matrix adds up only those 2 everywhere; the admin's adds up all 5; and for both,
// every row total == Aggregate(groupBy stage), every column total == Aggregate(groupBy close_date
// month), the grand total == the scalar Aggregate — same scope, compared as exact decimals.
func TestRecordAggregateGrid_MatrixOwnerScoped(t *testing.T) {
	recordSvc, entitySvc, _ := gridSetup(t)
	ctx := context.Background()
	idx := map[string]any{"indexed": true}
	if _, _, err := entitySvc.CreateEntity(ctx, o3aTenantA, CreateEntityInput{
		Name: "grid_opps", Label: "Grid opps",
		Fields: []FieldInput{
			{Name: "stage", Label: "Stage", FieldType: "text", Options: idx},
			{Name: "close_date", Label: "Close date", FieldType: "date", Options: idx},
			{Name: "amount", Label: "Amount", FieldType: "number"},
		},
	}); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	admin := RecordScope{UserID: gridUserA, IsAdmin: true, AccessLevel: "all"}
	for _, r := range []struct {
		owner, stage, date string
		amount             float64
	}{
		{gridUserA, "Proposal", "2026-01-15", 100.25},
		{gridUserA, "Proposal", "2026-02-10", 200.5},
		{gridUserA, "Won", "2026-01-20", 300},
		{gridUserB, "Proposal", "2026-01-05", 10.1},
		{gridUserB, "Won", "2026-02-28", 20.2},
	} {
		if _, err := recordSvc.Create(ctx, admin, o3aTenantA, r.owner, "grid_opps",
			map[string]any{"stage": r.stage, "close_date": r.date, "amount": r.amount}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	rep := RecordScope{UserID: gridUserB, RoleID: "crm_sales_rep", AccessLevel: "owner"}
	q := GridQuery{Metric: AggMetricSum, Field: "amount", Matrix: true,
		Axes: []GridAxisInput{{Field: "stage"}, {Field: "close_date", Bucket: "month"}}}

	t.Run("employee B sees only the 2 rows B owns, in every number", func(t *testing.T) {
		res, err := recordSvc.AggregateGrid(ctx, rep, o3aTenantA, "grid_opps", q)
		if err != nil {
			t.Fatalf("AggregateGrid: %v", err)
		}
		if res.Kind != GridKindMatrix {
			t.Fatalf("kind = %q, want matrix", res.Kind)
		}
		var rows, cols, cells []string
		for _, h := range res.Rows {
			rows = append(rows, keyText(h.Key))
		}
		for _, h := range res.Columns {
			cols = append(cols, keyText(h.Key))
		}
		for _, c := range res.Cells {
			cells = append(cells, fmt.Sprintf("%d,%d", c.R, c.C))
		}
		// Rows by row total DESC (Won 20.2 > Proposal 10.1); month columns chronological.
		if !reflect.DeepEqual(rows, []string{"Won", "Proposal"}) || !reflect.DeepEqual(cols, []string{"2026-01", "2026-02"}) ||
			!reflect.DeepEqual(cells, []string{"0,1", "1,0"}) {
			t.Fatalf("rows=%v cols=%v cells=%v, want [Won Proposal] [2026-01 2026-02] [0,1 1,0]", rows, cols, cells)
		}
		if !gridDecEq(res.Cells[0].Value, "20.2") || !gridDecEq(res.Cells[1].Value, "10.1") {
			t.Fatalf("cells = %s / %s, want 20.2 / 10.1", valText(res.Cells[0].Value), valText(res.Cells[1].Value))
		}
		if !gridDecEq(res.Total.Value, "30.3") || res.Total.Count != 2 {
			t.Fatalf("B total = %s/%d, want 30.3/2", valText(res.Total.Value), res.Total.Count)
		}
	})

	t.Run("admin sees all 5", func(t *testing.T) {
		res, err := recordSvc.AggregateGrid(ctx, admin, o3aTenantA, "grid_opps", q)
		if err != nil {
			t.Fatalf("AggregateGrid: %v", err)
		}
		if !gridDecEq(res.Total.Value, "631.05") || res.Total.Count != 5 || len(res.Cells) != 4 {
			t.Fatalf("admin total = %s/%d cells=%d, want 631.05/5 over 4 cells", valText(res.Total.Value), res.Total.Count, len(res.Cells))
		}
	})

	for _, sc := range []struct {
		name  string
		scope RecordScope
	}{{"employee B", rep}, {"admin", admin}} {
		t.Run("totals equal the single-axis Aggregate — "+sc.name, func(t *testing.T) {
			res, err := recordSvc.AggregateGrid(ctx, sc.scope, o3aTenantA, "grid_opps", q)
			if err != nil {
				t.Fatalf("AggregateGrid: %v", err)
			}
			check := func(what string, headers []AggregateGroupResult, single AggregateQuery) {
				agg, err := recordSvc.Aggregate(ctx, sc.scope, o3aTenantA, "grid_opps", single)
				if err != nil {
					t.Fatalf("Aggregate %s: %v", what, err)
				}
				if len(agg.Groups) != len(headers) {
					t.Fatalf("%s: %d grid headers vs %d single-axis groups", what, len(headers), len(agg.Groups))
				}
				want := map[string]AggregateGroupResult{}
				for _, g := range agg.Groups {
					want[keyText(g.Key)] = g
				}
				for _, h := range headers {
					w, ok := want[keyText(h.Key)]
					if !ok || w.Value == nil || !gridDecEq(h.Value, *w.Value) || h.Count != w.Count {
						t.Fatalf("%s %s: grid %s/%d vs single-axis %s/%d", what, keyText(h.Key), valText(h.Value), h.Count, valText(w.Value), w.Count)
					}
				}
			}
			check("row totals", res.Rows, AggregateQuery{Metric: AggMetricSum, Field: "amount", GroupBy: "stage"})
			check("column totals", res.Columns, AggregateQuery{Metric: AggMetricSum, Field: "amount", GroupBy: "close_date", Bucket: "month"})
			scalar, err := recordSvc.Aggregate(ctx, sc.scope, o3aTenantA, "grid_opps", AggregateQuery{Metric: AggMetricSum, Field: "amount"})
			if err != nil {
				t.Fatalf("scalar Aggregate: %v", err)
			}
			if scalar.Scalar.Value == nil || !gridDecEq(res.Total.Value, *scalar.Scalar.Value) || res.Total.Count != scalar.Scalar.Count {
				t.Fatalf("grand total %s/%d vs scalar %s/%d", valText(res.Total.Value), res.Total.Count, valText(scalar.Scalar.Value), scalar.Scalar.Count)
			}
		})
	}

	t.Run("no row matches: empty grid, total built by the service", func(t *testing.T) {
		empty := q
		empty.Filters = []FilterInput{{Field: "stage", Values: []string{"Nowhere"}}}
		res, err := recordSvc.AggregateGrid(ctx, rep, o3aTenantA, "grid_opps", empty)
		if err != nil {
			t.Fatalf("AggregateGrid: %v", err)
		}
		if res.Total.Value != nil || res.Total.Count != 0 || res.Rows == nil || len(res.Rows) != 0 || res.Cells == nil {
			t.Fatalf("empty grid = %+v, want [] parts and total {null 0}", res)
		}
	})
}

// TestRecordAggregateGrid_CollationEqualKeys: MySQL groups an indexed text column by its
// collation (utf8mb4_unicode_ci — case-insensitive, trailing spaces ignored), and for each group
// it returns the text of ONE member, not always the same one: a leaf row, its ROLLUP sub-total
// and the separate column-total statement can each show a different spelling. The grid must
// match cells and tree paths on the group itself, never on that text: the matrix builds (no
// "cell without a row/column total"), the tree's children add up to their parent (no phantom
// sibling holding a child), and both agree with the single-axis Aggregate.
func TestRecordAggregateGrid_CollationEqualKeys(t *testing.T) {
	recordSvc, entitySvc, sdb := gridSetup(t)
	ctx := context.Background()
	idx := map[string]any{"indexed": true}
	if _, _, err := entitySvc.CreateEntity(ctx, o3aTenantA, CreateEntityInput{
		Name: "grid_case", Label: "Grid case",
		Fields: []FieldInput{
			{Name: "stage", Label: "Stage", FieldType: "text", Options: idx},
			{Name: "region", Label: "Region", FieldType: "text", Options: idx},
			{Name: "tier", Label: "Tier", FieldType: "number", Options: idx},
			{Name: "due", Label: "Due", FieldType: "date", Options: idx},
			{Name: "amount", Label: "Amount", FieldType: "number"},
		},
	}); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	table, err := dynrepo.TableFor("grid_case")
	if err != nil {
		t.Fatalf("TableFor: %v", err)
	}
	// Written straight to the table so the stored text keeps its exact case and trailing spaces;
	// fixed ids keep MySQL's scan order (and so the spelling it shows) the same on every run.
	// tier/due: a DECIMAL and a DATETIME axis (nil = key absent), grouped by value.
	for i, r := range []struct {
		stage, region string
		tier          any
		due           any
		amount        int
	}{
		{"proposal", "North", -1.5, "2026-01-01", 1},
		{"Proposal", "South", 1.5, "2026-01-01", 2},
		{"PROPOSAL", "north", 1.5, "2026-01-02", 4},
		{"Proposal ", "North ", 15, "2026-01-02", 8},
		{"Won", "North", nil, "2026-01-01", 16},
		{"won ", "SOUTH", 2, nil, 32},
	} {
		data := map[string]any{"stage": r.stage, "region": r.region, "amount": r.amount}
		if r.tier != nil {
			data["tier"] = r.tier
		}
		if r.due != nil {
			data["due"] = r.due
		}
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := sdb.Exec("INSERT INTO `"+table+"` (`id`, `tenant_id`, `created_by`, `owner_id`, `data`) VALUES (?, ?, ?, ?, ?)",
			fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1), o3aTenantA, gridUserA, gridUserA, string(raw)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	emp := RecordScope{UserID: gridUserB, RoleID: "crm_sales_rep", AccessLevel: "all"}
	// Which member's spelling a header shows is MySQL's choice; the group it names is not.
	norm := func(k *string) string {
		if k == nil {
			return "∅"
		}
		return strings.ToLower(strings.TrimSpace(*k))
	}
	num := func(v *string) string {
		if v == nil {
			return "null"
		}
		r, ok := new(big.Rat).SetString(*v)
		if !ok {
			return *v
		}
		return r.RatString()
	}
	hdr := func(g AggregateGroupResult) string {
		return fmt.Sprintf("%s=%s/%d", norm(g.Key), num(g.Value), g.Count)
	}
	grid := func(t *testing.T, matrix bool, axes ...string) GridRunResult {
		t.Helper()
		if len(axes) == 0 {
			axes = []string{"stage", "region"}
		}
		ax := make([]GridAxisInput, len(axes))
		for i, f := range axes {
			ax[i] = GridAxisInput{Field: f}
		}
		res, err := recordSvc.AggregateGrid(ctx, emp, o3aTenantA, "grid_case", GridQuery{
			Metric: AggMetricSum, Field: "amount", Matrix: matrix, Axes: ax,
		})
		if err != nil {
			t.Fatalf("AggregateGrid: %v", err)
		}
		if num(res.Total.Value) != "63" || res.Total.Count != 6 {
			t.Fatalf("total = %s/%d, want 63/6", num(res.Total.Value), res.Total.Count)
		}
		return res
	}

	t.Run("matrix: one row / column per group, every cell under its headers", func(t *testing.T) {
		res := grid(t, true)
		var rows, cols, cells []string
		for _, h := range res.Rows {
			rows = append(rows, hdr(h))
		}
		for _, h := range res.Columns {
			cols = append(cols, hdr(h))
		}
		for _, c := range res.Cells {
			cells = append(cells, fmt.Sprintf("%d,%d=%s/%d", c.R, c.C, num(c.Value), c.Count))
		}
		wantRows := []string{"won=48/2", "proposal=15/4"}
		wantCols := []string{"south=34/2", "north=29/4"}
		wantCells := []string{"0,0=32/1", "0,1=16/1", "1,0=2/1", "1,1=13/3"}
		if !reflect.DeepEqual(rows, wantRows) || !reflect.DeepEqual(cols, wantCols) || !reflect.DeepEqual(cells, wantCells) {
			t.Fatalf("rows=%v cols=%v cells=%v\nwant rows=%v cols=%v cells=%v", rows, cols, cells, wantRows, wantCols, wantCells)
		}
		for what, single := range map[string]struct {
			headers []AggregateGroupResult
			groupBy string
		}{"row totals": {res.Rows, "stage"}, "column totals": {res.Columns, "region"}} {
			agg, err := recordSvc.Aggregate(ctx, emp, o3aTenantA, "grid_case", AggregateQuery{Metric: AggMetricSum, Field: "amount", GroupBy: single.groupBy})
			if err != nil {
				t.Fatalf("Aggregate %s: %v", what, err)
			}
			want := map[string]bool{}
			for _, g := range agg.Groups {
				want[hdr(g)] = true
			}
			for _, h := range single.headers {
				if !want[hdr(h)] || len(agg.Groups) != len(single.headers) {
					t.Fatalf("%s %s not in the single-axis Aggregate %v", what, hdr(h), want)
				}
			}
		}
	})

	t.Run("tree: children add up to their parent, no phantom sibling", func(t *testing.T) {
		res := grid(t, false)
		var got []string
		var walk func(prefix string, nodes []GridNode)
		walk = func(prefix string, nodes []GridNode) {
			for _, n := range nodes {
				got = append(got, prefix+hdr(n.AggregateGroupResult))
				walk(prefix+norm(n.Key)+">", n.Children)
			}
		}
		walk("", res.Groups)
		want := []string{
			"won=48/2", "won>south=32/1", "won>north=16/1",
			"proposal=15/4", "proposal>north=13/3", "proposal>south=2/1",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("tree =\n%v\nwant\n%v", got, want)
		}
	})

	t.Run("number × date axes: grouped by value, -1.5 apart from 1.5", func(t *testing.T) {
		res := grid(t, true, "tier", "due")
		var rows, cols, cells []string
		for _, h := range res.Rows {
			rows = append(rows, hdr(h))
		}
		for _, h := range res.Columns {
			cols = append(cols, hdr(h))
		}
		for _, c := range res.Cells {
			cells = append(cells, fmt.Sprintf("%d,%d=%s/%d", c.R, c.C, num(c.Value), c.Count))
		}
		wantRows := []string{"2.0000000000=32/1", "15.0000000000=8/1", "1.5000000000=6/2", "-1.5000000000=1/1", "∅=16/1"}
		wantCols := []string{"2026-01-01 00:00:00=19/3", "2026-01-02 00:00:00=12/2", "∅=32/1"}
		wantCells := []string{"0,2=32/1", "1,1=8/1", "2,0=2/1", "2,1=4/1", "3,0=1/1", "4,0=16/1"}
		if !reflect.DeepEqual(rows, wantRows) || !reflect.DeepEqual(cols, wantCols) || !reflect.DeepEqual(cells, wantCells) {
			t.Fatalf("rows=%v cols=%v cells=%v\nwant rows=%v cols=%v cells=%v", rows, cols, cells, wantRows, wantCols, wantCells)
		}
	})
}

// bulkInsertGridCells writes n rows in ONE statement (a digit cross join): row key r, column key
// c computed from the row number n by the given SQL expressions, all tagged with batch.
func bulkInsertGridCells(t *testing.T, db *sqlx.DB, tenant, batch, rExpr, cExpr string, n int) {
	t.Helper()
	table, err := dynrepo.TableFor("grid_cells")
	if err != nil {
		t.Fatalf("TableFor: %v", err)
	}
	digits := "(SELECT 0 d UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 " +
		"UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9)"
	// rExpr/cExpr are test constants below, never input.
	stmt := fmt.Sprintf("INSERT INTO `%s` (`id`, `tenant_id`, `created_by`, `owner_id`, `data`) "+
		"SELECT UUID(), ?, ?, ?, JSON_OBJECT('r', %s, 'c', %s, 'batch', ?) FROM ("+
		"SELECT a.d + 10*b.d + 100*c.d + 1000*e.d AS n FROM %s a, %s b, %s c, (SELECT 0 d UNION ALL SELECT 1) e) s "+
		"WHERE s.n < ?", table, rExpr, cExpr, digits, digits, digits)
	if _, err := db.Exec(stmt, tenant, gridUserA, gridUserA, batch, n); err != nil {
		t.Fatalf("bulkInsertGridCells: %v", err)
	}
}

// TestRecordAggregateGrid_DenseCap pins ONE definition of a matrix "cell" (Q3, plan D-5): the
// dense rows × columns grid the screen must draw, not the cells holding data. 10×100 with only
// 100 data cells passes; 11×100 with 110 data cells is rejected with the exact shape; more than
// 1000 leaves stops the engine itself, so only a lower bound is known.
func TestRecordAggregateGrid_DenseCap(t *testing.T) {
	recordSvc, entitySvc, sdb := gridSetup(t)
	ctx := context.Background()
	idx := map[string]any{"indexed": true}
	if _, _, err := entitySvc.CreateEntity(ctx, o3aTenantA, CreateEntityInput{
		Name: "grid_cells", Label: "Grid cells",
		Fields: []FieldInput{
			{Name: "r", Label: "R", FieldType: "text", Options: idx},
			{Name: "c", Label: "C", FieldType: "text", Options: idx},
			{Name: "batch", Label: "Batch", FieldType: "text", Options: idx},
		},
	}); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	bulkInsertGridCells(t, sdb, o3aTenantA, "b10", "CONCAT('r', LPAD(s.n MOD 10, 2, '0'))", "CONCAT('c', LPAD(s.n, 3, '0'))", 100)
	bulkInsertGridCells(t, sdb, o3aTenantA, "b11", "CONCAT('r', LPAD(s.n MOD 11, 2, '0'))", "CONCAT('c', LPAD(s.n MOD 100, 3, '0'))", 110)
	bulkInsertGridCells(t, sdb, o3aTenantA, "big", "'r00'", "CONCAT('c', LPAD(s.n, 4, '0'))", 1001)

	// A non-admin employee who may read every row: the cap is about shape, not permissions.
	emp := RecordScope{UserID: gridUserB, RoleID: "crm_sales_rep", AccessLevel: "all"}
	run := func(batch string, matrix bool, axisFields ...string) (GridRunResult, error) {
		ax := make([]GridAxisInput, len(axisFields))
		for i, f := range axisFields {
			ax[i] = GridAxisInput{Field: f}
		}
		return recordSvc.AggregateGrid(ctx, emp, o3aTenantA, "grid_cells", GridQuery{
			Metric: AggMetricCount, Matrix: matrix, Axes: ax,
			Filters: []FilterInput{{Field: "batch", Values: []string{batch}}},
		})
	}
	details := func(t *testing.T, err error, code string) map[string]any {
		t.Helper()
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != code {
			t.Fatalf("err = %v, want ValidationError %s", err, code)
		}
		return ve.Details
	}

	t.Run("10×100 = 1000 dense cells (100 with data) passes", func(t *testing.T) {
		res, err := run("b10", true, "r", "c")
		if err != nil {
			t.Fatalf("AggregateGrid: %v", err)
		}
		if len(res.Rows) != 10 || len(res.Columns) != 100 || len(res.Cells) != 100 || res.Total.Count != 100 {
			t.Fatalf("got %d×%d with %d cells, total %d; want 10×100 with 100 cells, total 100",
				len(res.Rows), len(res.Columns), len(res.Cells), res.Total.Count)
		}
	})
	t.Run("11×100 = 1100 dense cells (110 with data) is rejected with its shape", func(t *testing.T) {
		_, err := run("b11", true, "r", "c")
		want := map[string]any{"cap": 1000, "cells": 1100, "rows": 11, "columns": 100, "lowerBound": false}
		if got := details(t, err, "REPORT_MATRIX_TOO_MANY_CELLS"); !reflect.DeepEqual(got, want) {
			t.Fatalf("details = %v, want %v", got, want)
		}
	})
	t.Run("1001 leaves: the engine stops, only a lower bound is known", func(t *testing.T) {
		_, err := run("big", true, "r", "c")
		want := map[string]any{"cap": 1000, "cells": 1001, "lowerBound": true}
		if got := details(t, err, "REPORT_MATRIX_TOO_MANY_CELLS"); !reflect.DeepEqual(got, want) {
			t.Fatalf("details = %v, want %v", got, want)
		}
	})
	t.Run("1001 groups in a one-level tree hit the LIMIT: groups is a lower bound", func(t *testing.T) {
		_, err := run("big", false, "c")
		want := map[string]any{"cap": 1000, "groups": 1001, "lowerBound": true}
		if got := details(t, err, "REPORT_TOO_MANY_GROUPS"); !reflect.DeepEqual(got, want) {
			t.Fatalf("details = %v, want %v", got, want)
		}
	})
}
