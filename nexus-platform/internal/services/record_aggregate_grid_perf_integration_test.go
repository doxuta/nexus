//go:build integration

// specs/049 S7b T069 / SC-009: one tree / cross-tab run over 100 000 rows inside the caller's scope
// finishes in under 2 s on the dev machine. The data is created INSIDE the throwaway test database
// (never the dev data). The numbers are logged (-v) so they can be copied into plan §10; the 2 s
// budget is only ENFORCED when NEXUS_PERF_STRICT=1 (the CI runner is 4-5x slower than the dev Mac).
//
//	NEXUS_PERF_STRICT=1 go test -tags=integration -count=1 -run TestRecordAggregateGrid_Perf100k -v ./internal/services/
package services

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRecordAggregateGrid_Perf100k(t *testing.T) {
	recordSvc, entitySvc, sdb := gridSetup(t)
	ctx := context.Background()
	idx := map[string]any{"indexed": true}
	if _, _, err := entitySvc.CreateEntity(ctx, o3aTenantA, CreateEntityInput{
		Name: "grid_perf", Label: "Grid perf",
		Fields: []FieldInput{
			{Name: "stage", Label: "Stage", FieldType: "text", Options: idx},
			{Name: "close_date", Label: "Close date", FieldType: "date", Options: idx},
			{Name: "amount", Label: "Amount", FieldType: "number"},
		},
	}); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	admin := RecordScope{UserID: gridUserA, IsAdmin: true, AccessLevel: "all"}
	if _, err := recordSvc.Create(ctx, admin, o3aTenantA, gridUserB, "grid_perf",
		map[string]any{"stage": "x", "close_date": "2026-01-01", "amount": 1.5}); err != nil {
		t.Fatalf("seed template row: %v", err)
	}
	// 100k rows: 6 stages x 24 months, half owned by employee B (the scope under test).
	const rows = 100000
	if _, err := sdb.Exec(`INSERT /*+ SET_VAR(cte_max_recursion_depth=100000) */ INTO obj_grid_perf
	  (id, tenant_id, team_set_id, created_by, modified_by, deleted_at, owner_id, version, data)
	  WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < ?)
	  SELECT UUID(), tpl.tenant_id, tpl.team_set_id, tpl.created_by, tpl.modified_by, NULL,
	         IF(seq.n % 2 = 0, tpl.owner_id, ?), tpl.version,
	         JSON_OBJECT('stage', ELT(seq.n % 6 + 1,'Prospecting','Qualification','Proposal','Negotiation','Closed Won','Closed Lost'),
	                     'close_date', DATE_FORMAT(DATE_ADD('2025-01-01', INTERVAL (seq.n % 24) MONTH), '%Y-%m-%d'),
	                     'amount', (seq.n % 1000) + 0.5)
	  FROM seq CROSS JOIN (SELECT * FROM obj_grid_perf LIMIT 1) tpl`, rows, gridUserA); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}
	var n int
	if err := sdb.Get(&n, "SELECT COUNT(*) FROM obj_grid_perf WHERE tenant_id = ?", o3aTenantA); err != nil || n < rows {
		t.Fatalf("rows = %d (%v), want >= %d", n, err, rows)
	}
	rep := RecordScope{UserID: gridUserB, RoleID: "crm_sales_rep", AccessLevel: "owner"}
	two := []GridAxisInput{{Field: "stage"}, {Field: "close_date", Bucket: "month"}}
	having := &ReportHavingInput{On: "value", Op: "gt", Values: []string{"1000"}}
	for _, c := range []struct {
		name  string
		scope RecordScope
		q     GridQuery
	}{
		{"matrix N=2 rep", rep, GridQuery{Metric: AggMetricSum, Field: "amount", Matrix: true, Axes: two}},
		{"matrix N=2 rep +having", rep, GridQuery{Metric: AggMetricSum, Field: "amount", Matrix: true, Axes: two, Having: having}},
		{"tree N=2 rep", rep, GridQuery{Metric: AggMetricSum, Field: "amount", Axes: two}},
		{"tree N=2 rep +having", rep, GridQuery{Metric: AggMetricSum, Field: "amount", Axes: two, Having: having}},
		{"matrix N=2 admin (all rows)", admin, GridQuery{Metric: AggMetricSum, Field: "amount", Matrix: true, Axes: two}},
	} {
		start := time.Now()
		res, err := recordSvc.AggregateGrid(ctx, c.scope, o3aTenantA, "grid_perf", c.q)
		took := time.Since(start)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		t.Logf("SC-009 %-30s %8s  total.count=%d", c.name, took.Round(time.Millisecond), res.Total.Count)
		if os.Getenv("NEXUS_PERF_STRICT") == "1" && took > 2*time.Second {
			t.Errorf("%s took %s, want < 2s (SC-009)", c.name, took)
		}
	}

	// EXPLAIN of the statement shape the matrix runs (cell GROUP BY over both axes, owner scope).
	rowsX, err := sdb.Queryx(`EXPLAIN SELECT stage, DATE_FORMAT(close_date,'%Y-%m'), SUM(CAST(JSON_EXTRACT(data,'$.amount') AS DECIMAL(30,6)))
	  FROM obj_grid_perf WHERE tenant_id = ? AND deleted_at IS NULL AND owner_id = ?
	  GROUP BY stage, DATE_FORMAT(close_date,'%Y-%m')`, o3aTenantA, gridUserB)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rowsX.Close()
	for rowsX.Next() {
		m := map[string]any{}
		if err := rowsX.MapScan(m); err != nil {
			t.Fatalf("explain scan: %v", err)
		}
		var parts []string
		for _, k := range []string{"type", "key", "rows", "Extra"} {
			if b, ok := m[k].([]byte); ok {
				parts = append(parts, k+"="+string(b))
			} else {
				parts = append(parts, fmt.Sprintf("%s=%v", k, m[k]))
			}
		}
		t.Logf("EXPLAIN %s", strings.Join(parts, " "))
	}
}
