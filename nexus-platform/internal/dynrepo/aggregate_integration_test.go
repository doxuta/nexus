//go:build integration

// Integration tests for the EPIC 6 M1 aggregation query engine (dynrepo layer).
//
// Run with:
//
//	go test -tags=integration ./internal/dynrepo/
//
// They require a reachable MySQL 8. Set TEST_DATABASE_URL to override the default DSN.
package dynrepo

import (
	"context"
	"math/big"
	"testing"

	"nexus/platform/internal/ddl"
)

// decEq reports whether two decimal-string representations are numerically equal, ignoring
// trailing-zero / formatting differences (MySQL DECIMAL(38,10) returns e.g. "700.0000000000").
// big.Rat parses exact decimal text so "700" == "700.0000000000" and "1" == "1.0000000000".
func decEq(got, want string) bool {
	g, ok1 := new(big.Rat).SetString(got)
	w, ok2 := new(big.Rat).SetString(want)
	if !ok1 || !ok2 {
		return false
	}
	return g.Cmp(w) == 0
}

// TestAggregate_ScalarAndGrouped proves the aggregate SQL builder computes SUM/COUNT/AVG/
// MIN/MAX correctly, groups by an indexed physical column, stays tenant-scoped, and (the
// money-correctness case) sums currency amounts EXACTLY via CAST to DECIMAL — no float drift.
func TestAggregate_ScalarAndGrouped(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_agg_deals"
	const tenantA = "11111111-1111-1111-1111-1111111111aa"
	const tenantB = "22222222-2222-2222-2222-2222222222bb"
	const user = "00000000-0000-0000-0000-0000000000u1"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "status", FieldType: "picklist", Indexed: true}, // generated col `status` + index
		{Name: "amount", FieldType: "number"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	mustInsert := func(tenant string, data map[string]any) {
		if _, err := repo.InsertRecord(ctx, entity, tenant, user, data, nil); err != nil {
			t.Fatalf("InsertRecord: %v", err)
		}
	}
	// Tenant A: 3 won (100,200,300 = 600), 2 lost (50,50 = 100). Tenant B: 1 won (9999).
	mustInsert(tenantA, map[string]any{"status": "won", "amount": float64(100)})
	mustInsert(tenantA, map[string]any{"status": "won", "amount": float64(200)})
	mustInsert(tenantA, map[string]any{"status": "won", "amount": float64(300)})
	mustInsert(tenantA, map[string]any{"status": "lost", "amount": float64(50)})
	mustInsert(tenantA, map[string]any{"status": "lost", "amount": float64(50)})
	mustInsert(tenantB, map[string]any{"status": "won", "amount": float64(9999)})

	// --- Scalar SUM over all of tenant A: 600 + 100 = 700 ---
	sc, err := repo.AggregateScalar(ctx, entity, tenantA, AdminScope(), AggregateParams{
		Metric: MetricSum, FieldColumn: "amount",
	})
	if err != nil {
		t.Fatalf("AggregateScalar sum: %v", err)
	}
	if sc.Value == nil || decEq(*sc.Value, "700") == false {
		t.Fatalf("sum(amount) tenantA = %v; want 700", strOrNil(sc.Value))
	}
	if sc.Count != 5 {
		t.Fatalf("sum count tenantA = %d; want 5", sc.Count)
	}

	// --- Tenant isolation: tenant B sum is its single row only (9999), never A's rows ---
	scB, err := repo.AggregateScalar(ctx, entity, tenantB, AdminScope(), AggregateParams{
		Metric: MetricSum, FieldColumn: "amount",
	})
	if err != nil {
		t.Fatalf("AggregateScalar sum tenantB: %v", err)
	}
	if scB.Value == nil || decEq(*scB.Value, "9999") == false || scB.Count != 1 {
		t.Fatalf("tenantB sum=%v count=%d; want 9999/1", strOrNil(scB.Value), scB.Count)
	}

	// --- COUNT(*) over tenant A = 5 (no field needed) ---
	cnt, err := repo.AggregateScalar(ctx, entity, tenantA, AdminScope(), AggregateParams{Metric: MetricCount})
	if err != nil {
		t.Fatalf("AggregateScalar count: %v", err)
	}
	if cnt.Count != 5 {
		t.Fatalf("count tenantA = %d; want 5", cnt.Count)
	}

	// --- AVG over tenant A won deals (filter status=won): (100+200+300)/3 = 200 ---
	avg, err := repo.AggregateScalar(ctx, entity, tenantA, AdminScope(), AggregateParams{
		Metric: MetricAvg, FieldColumn: "amount",
		Filters: []Filter{{Column: "status", Value: "won", PhysicalColumn: "status"}},
	})
	if err != nil {
		t.Fatalf("AggregateScalar avg: %v", err)
	}
	if avg.Value == nil || decEq(*avg.Value, "200") == false || avg.Count != 3 {
		t.Fatalf("avg(amount) won = %v count=%d; want 200/3", strOrNil(avg.Value), avg.Count)
	}

	// --- MIN / MAX over tenant A ---
	mn, _ := repo.AggregateScalar(ctx, entity, tenantA, AdminScope(), AggregateParams{Metric: MetricMin, FieldColumn: "amount"})
	mx, _ := repo.AggregateScalar(ctx, entity, tenantA, AdminScope(), AggregateParams{Metric: MetricMax, FieldColumn: "amount"})
	if mn.Value == nil || decEq(*mn.Value, "50") == false {
		t.Fatalf("min(amount) = %v; want 50", strOrNil(mn.Value))
	}
	if mx.Value == nil || decEq(*mx.Value, "300") == false {
		t.Fatalf("max(amount) = %v; want 300", strOrNil(mx.Value))
	}

	// --- GROUP BY status, SUM(amount): won=600, lost=100 ---
	groups, err := repo.AggregateGrouped(ctx, entity, tenantA, AdminScope(), AggregateParams{
		Metric: MetricSum, FieldColumn: "amount", GroupByPhysical: "status",
	})
	if err != nil {
		t.Fatalf("AggregateGrouped: %v", err)
	}
	byKey := map[string]AggregateGroup{}
	for _, g := range groups {
		if g.Key != nil {
			byKey[*g.Key] = g
		}
	}
	if len(byKey) != 2 {
		t.Fatalf("grouped sum got %d groups; want 2 (%v)", len(byKey), groups)
	}
	if w := byKey["won"]; w.Value == nil || decEq(*w.Value, "600") == false || w.Count != 3 {
		t.Fatalf("group won value=%v count=%d; want 600/3", strOrNil(w.Value), w.Count)
	}
	if l := byKey["lost"]; l.Value == nil || decEq(*l.Value, "100") == false || l.Count != 2 {
		t.Fatalf("group lost value=%v count=%d; want 100/2", strOrNil(l.Value), l.Count)
	}
	// ORDER BY value DESC: won (600) must come before lost (100).
	if len(groups) >= 2 && (groups[0].Key == nil || *groups[0].Key != "won") {
		t.Fatalf("grouped result not ordered by value DESC: %v", groups)
	}
}

// TestAggregate_CurrencyExactNoFloatDrift is the money-correctness regression (ENGINE-COVERAGE
// §EPIC6: currency stored as JSON float would drift on SUM). It sums currency .amount values
// whose float64 sum is famously inexact (0.1+0.2 = 0.30000000000000004) and asserts the CAST-
// to-DECIMAL path yields the EXACT decimal total.
func TestAggregate_CurrencyExactNoFloatDrift(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_agg_money"
	const tenant = "33333333-3333-3333-3333-3333333333cc"
	const user = "00000000-0000-0000-0000-0000000000u1"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{
		{Name: "price", FieldType: "currency"},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })

	// Currency stored as {"amount":N,"currencyCode":"USD"}; the aggregate targets "$.price.amount".
	amts := []float64{0.1, 0.2, 0.1, 0.2, 0.1, 0.2, 0.1} // float sum drifts; exact total = 1.0
	for _, a := range amts {
		if _, err := repo.InsertRecord(ctx, entity, tenant, user, map[string]any{
			"price": map[string]any{"amount": a, "currencyCode": "USD"},
		}, nil); err != nil {
			t.Fatalf("InsertRecord: %v", err)
		}
	}

	sc, err := repo.AggregateScalar(ctx, entity, tenant, AdminScope(), AggregateParams{
		Metric: MetricSum, FieldColumn: "price", FieldSubPath: "amount",
	})
	if err != nil {
		t.Fatalf("AggregateScalar currency sum: %v", err)
	}
	// 0.1*4 + 0.2*3 = 0.4 + 0.6 = 1.0 exactly. The DECIMAL sum must equal 1, not 0.9999999...
	if sc.Value == nil || decEq(*sc.Value, "1") == false {
		t.Fatalf("sum(price.amount) = %v; want exactly 1 (no float drift)", strOrNil(sc.Value))
	}
	if sc.Count != int64(len(amts)) {
		t.Fatalf("currency sum count = %d; want %d", sc.Count, len(amts))
	}
}

// TestAggregate_JSONNullIsNotZero pins #448: a number cleared to JSON null (or a formula that
// divided by zero) is stored as the string 'null' after JSON_UNQUOTE; CAST('null' AS DECIMAL)
// is 0, which dragged AVG/MIN down. Rows {null, 10, key missing} must average 10, not 5.
func TestAggregate_JSONNullIsNotZero(t *testing.T) {
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()
	const entity = "it_agg_jsonnull"
	const tenant = "44444444-4444-4444-4444-4444444444dd"
	const user = "00000000-0000-0000-0000-0000000000u1"

	_ = repo.DropTable(ctx, entity)
	if err := repo.CreateTable(ctx, entity, []ddl.FieldDef{{Name: "score", FieldType: "number"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = repo.DropTable(ctx, entity) })
	for _, d := range []map[string]any{{"score": nil}, {"score": float64(10)}, {}} {
		if _, err := repo.InsertRecord(ctx, entity, tenant, user, d, nil); err != nil {
			t.Fatalf("InsertRecord: %v", err)
		}
	}
	for _, c := range []struct {
		m    AggregateMetric
		want string
	}{{MetricAvg, "10"}, {MetricMin, "10"}, {MetricMax, "10"}, {MetricSum, "10"}} {
		sc, err := repo.AggregateScalar(ctx, entity, tenant, AdminScope(), AggregateParams{Metric: c.m, FieldColumn: "score"})
		if err != nil {
			t.Fatalf("AggregateScalar %s: %v", c.m, err)
		}
		if sc.Value == nil || !decEq(*sc.Value, c.want) {
			t.Fatalf("%s(score) = %v; want %s (JSON null must be skipped, not 0)", c.m, strOrNil(sc.Value), c.want)
		}
	}
}

// strOrNil renders a *string for test messages.
func strOrNil(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
