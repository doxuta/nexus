//go:build integration

// W4 integration tests — run-overlay (dateFrom/dateTo/ownerId) + time-series bucketing + report
// CSV export parity, against a real throwaway MySQL 8 database (o3aSetup harness).
//
// Proofs (the ones a unit fake cannot give):
//  1. The overlay ACTUALLY narrows dashboard-run results (created_at range + created_by eq hit
//     the real physical columns), and a nil overlay is byte-identical to the pre-W4 run.
//  2. Bucketing groups correctly ACROSS month/quarter boundaries on real DATE_FORMAT SQL, in
//     chronological order, with empty buckets omitted — on created_at AND on an indexed date field.
//  3. Report run + CSV export honor the SAME overlay (no divergence between screen and file).
//  4. Cross-tenant isolation: tenant B cannot run/export tenant A's dashboard/report id, and an
//     overlay can never widen a tenant's visible row set.
//  5. Field-ACL fail-closed: an overlay does not bypass FIELD_FORBIDDEN on aggregation, and a
//     hidden column exports as empty cells.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestW4Int -race -v
package services

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/report"
)

// w4Wire builds the full W4 service graph over a SINGLE throwaway DB: entity/record services,
// dashboard + report services, and the sqlx handle (for direct created_at fixture updates).
// withACL additionally wires the REAL field-ACL repository (for the fail-closed proofs).
func w4Wire(t *testing.T, withACL bool) (*EntityService, *RecordService, *DashboardService, *ReportService, *sqlx.DB) {
	t.Helper()
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	mc := NewMetaCache(metaRepo, DefaultMetaTTL)
	entitySvc := NewEntityService(dynRepo, metaRepo).WithMetaCache(mc)
	recordSvc := NewRecordService(mc, dynRepo)
	if withACL {
		recordSvc = recordSvc.WithFieldACL(NewFieldACLService(auth.NewFieldACLRepository(gdb)))
	}
	dashSvc := NewDashboardService(dashboard.NewSQLRepository(sdb), recordSvc, zap.NewNop())
	reportSvc := NewReportService(report.NewSQLRepository(sdb), recordSvc).WithEntityChecker(metaRepo)
	return entitySvc, recordSvc, dashSvc, reportSvc, sdb
}

// w4SeedDeals creates the fixture entity + 4 records with CONTROLLED created_at instants and two
// distinct owners, in the given tenant. Layout:
//
//	id  amount  stage  closed_on    created_at             created_by
//	r1  100     open   2026-01-30   2026-01-30 10:00:00    u1
//	r2  200     open   2026-01-31   2026-01-31 10:00:00    u1
//	r3  300     won    2026-02-01   2026-02-01 10:00:00    u2
//	r4  400     won    2026-04-05   2026-04-05 10:00:00    u2
//
// Returns the record ids in order [r1, r2, r3, r4].
func w4SeedDeals(t *testing.T, ctx context.Context, entitySvc *EntityService, recordSvc *RecordService, sdb *sqlx.DB, tenant string) []string {
	t.Helper()
	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name:  "w4deals",
		Label: "W4 Deals",
		Fields: []FieldInput{
			{Name: "amount", Label: "Amount", FieldType: "number"},
			{Name: "stage", Label: "Stage", FieldType: "text", Options: map[string]any{"indexed": true}},
			{Name: "closed_on", Label: "Closed On", FieldType: "date", Options: map[string]any{"indexed": true}},
		},
	}); err != nil {
		t.Fatalf("CreateEntity(%s): %v", tenant, err)
	}
	admin := RecordScope{IsAdmin: true}
	rows := []struct {
		amount    float64
		stage     string
		closedOn  string
		createdAt string
		owner     string
	}{
		{100, "open", "2026-01-30", "2026-01-30 10:00:00", "u1"},
		{200, "open", "2026-01-31", "2026-01-31 10:00:00", "u1"},
		{300, "won", "2026-02-01", "2026-02-01 10:00:00", "u2"},
		{400, "won", "2026-04-05", "2026-04-05 10:00:00", "u2"},
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		id, err := recordSvc.Create(ctx, admin, tenant, r.owner, "w4deals",
			map[string]any{"amount": r.amount, "stage": r.stage, "closed_on": r.closedOn})
		if err != nil {
			t.Fatalf("seed record: %v", err)
		}
		// Pin created_at to the fixture instant (the insert stamps NOW(); the overlay/bucket
		// tests need deterministic month/quarter boundaries).
		if _, err := sdb.Exec("UPDATE `obj_w4deals` SET `created_at` = ? WHERE `id` = ?", r.createdAt, id); err != nil {
			t.Fatalf("pin created_at: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

// w4DecEq compares a MySQL DECIMAL text against a want float within epsilon.
func w4DecEq(got *string, want float64) bool {
	if got == nil {
		return false
	}
	f, err := strconv.ParseFloat(*got, 64)
	if err != nil {
		return false
	}
	diff := f - want
	return diff < 1e-6 && diff > -1e-6
}

// --- 1+4: overlay narrows dashboard runs; tenant isolation --------------------------------------

func TestW4Int_DashboardOverlayNarrowsAndIsolates(t *testing.T) {
	entitySvc, recordSvc, dashSvc, _, sdb := w4Wire(t, false)
	ctx := context.Background()
	admin := RecordScope{IsAdmin: true}
	w4SeedDeals(t, ctx, entitySvc, recordSvc, sdb, o3aTenantA)

	dash, err := dashSvc.CreateDashboard(ctx, o3aTenantA, CreateDashboardInput{
		Name:    "W4 Overview",
		ActorID: "u1",
		Widgets: []WidgetConfig{
			{ID: "w1", Title: "Deals", Type: "kpi", Entity: "w4deals", Metric: "count"},
			{ID: "w2", Title: "By stage", Type: "bar", Entity: "w4deals", Metric: "sum", Field: "amount", GroupBy: "stage"},
		},
	})
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	countOf := func(res DashboardRunResult) int64 {
		t.Helper()
		w := res.Widgets[0]
		if w.Error != "" || w.Result == nil {
			t.Fatalf("widget w1 failed: code=%q err=%q", w.Code, w.Error)
		}
		return w.Result.Scalar.Count
	}

	// Baseline (nil overlay) — identical to pre-W4: all 4 rows.
	base, err := dashSvc.RunDashboard(ctx, admin, o3aTenantA, dash.ID, nil)
	if err != nil {
		t.Fatalf("RunDashboard baseline: %v", err)
	}
	if got := countOf(base); got != 4 {
		t.Errorf("baseline count = %d; want 4", got)
	}

	// Date range [2026-01-31 .. 2026-02-01] → r2 + r3 only, on EVERY widget (grouped too).
	rng, err := dashSvc.RunDashboard(ctx, admin, o3aTenantA, dash.ID,
		&RunOverlay{DateFrom: "2026-01-31", DateTo: "2026-02-01"})
	if err != nil {
		t.Fatalf("RunDashboard range: %v", err)
	}
	if got := countOf(rng); got != 2 {
		t.Errorf("range count = %d; want 2 (r2+r3)", got)
	}
	w2 := rng.Widgets[1]
	if w2.Error != "" || w2.Result == nil || !w2.Result.Grouped {
		t.Fatalf("widget w2 failed: %+v", w2)
	}
	sums := map[string]float64{}
	for _, g := range w2.Result.Groups {
		if g.Key != nil {
			f, _ := strconv.ParseFloat(strOrEmpty(g.Value), 64)
			sums[*g.Key] = f
		}
	}
	if sums["open"] != 200 || sums["won"] != 300 {
		t.Errorf("range grouped sums = %v; want open=200 won=300", sums)
	}

	// ownerId=u1 → r1 + r2.
	byOwner, err := dashSvc.RunDashboard(ctx, admin, o3aTenantA, dash.ID, &RunOverlay{OwnerID: "u1"})
	if err != nil {
		t.Fatalf("RunDashboard owner: %v", err)
	}
	if got := countOf(byOwner); got != 2 {
		t.Errorf("owner count = %d; want 2 (u1's rows)", got)
	}

	// Combined: range covering everything + ownerId=u2 → r3 + r4.
	combined, err := dashSvc.RunDashboard(ctx, admin, o3aTenantA, dash.ID,
		&RunOverlay{DateFrom: "2026-01-01", DateTo: "2026-12-31", OwnerID: "u2"})
	if err != nil {
		t.Fatalf("RunDashboard combined: %v", err)
	}
	if got := countOf(combined); got != 2 {
		t.Errorf("combined count = %d; want 2 (u2's rows)", got)
	}

	// Inclusive whole-day dateTo: [.. 2026-01-31] must INCLUDE r2 (created 2026-01-31 10:00).
	inclusive, err := dashSvc.RunDashboard(ctx, admin, o3aTenantA, dash.ID,
		&RunOverlay{DateTo: "2026-01-31"})
	if err != nil {
		t.Fatalf("RunDashboard inclusive: %v", err)
	}
	if got := countOf(inclusive); got != 2 {
		t.Errorf("dateTo-inclusive count = %d; want 2 (r1+r2)", got)
	}

	// --- Tenant isolation. Tenant B registers the SAME entity (shared obj_* table) + 1 row. ---
	if _, _, err := entitySvc.CreateEntity(ctx, o3aTenantB, CreateEntityInput{
		Name:  "w4deals",
		Label: "W4 Deals B",
		Fields: []FieldInput{
			{Name: "amount", Label: "Amount", FieldType: "number"},
			{Name: "stage", Label: "Stage", FieldType: "text", Options: map[string]any{"indexed": true}},
			{Name: "closed_on", Label: "Closed On", FieldType: "date", Options: map[string]any{"indexed": true}},
		},
	}); err != nil {
		t.Fatalf("CreateEntity B: %v", err)
	}
	if _, err := recordSvc.Create(ctx, admin, o3aTenantB, "u-b", "w4deals",
		map[string]any{"amount": float64(999), "stage": "open", "closed_on": "2026-01-30"}); err != nil {
		t.Fatalf("seed B record: %v", err)
	}

	// B cannot run A's dashboard id — overlay or not.
	var ve *ValidationError
	_, errRun := dashSvc.RunDashboard(ctx, admin, o3aTenantB, dash.ID,
		&RunOverlay{DateFrom: "2020-01-01", DateTo: "2030-01-01"})
	if !errors.As(errRun, &ve) || ve.Code != "DASHBOARD_NOT_FOUND" {
		t.Errorf("B running A's dashboard: want DASHBOARD_NOT_FOUND, got %v", errRun)
	}

	// B's own dashboard with a maximally-wide overlay still sees ONLY B's single row (the
	// overlay can never widen visibility across tenants on the shared table).
	dashB, err := dashSvc.CreateDashboard(ctx, o3aTenantB, CreateDashboardInput{
		Name: "B Overview", ActorID: "u-b",
		Widgets: []WidgetConfig{{ID: "w1", Title: "Deals", Type: "kpi", Entity: "w4deals", Metric: "count"}},
	})
	if err != nil {
		t.Fatalf("CreateDashboard B: %v", err)
	}
	resB, err := dashSvc.RunDashboard(ctx, admin, o3aTenantB, dashB.ID,
		&RunOverlay{DateFrom: "2020-01-01", DateTo: "2030-01-01"})
	if err != nil {
		t.Fatalf("RunDashboard B: %v", err)
	}
	if got := countOf(resB); got != 1 {
		t.Errorf("tenant B overlay count = %d; want 1 (B's row only — no cross-tenant read)", got)
	}
}

// --- 2: bucketing across boundaries --------------------------------------------------------------

func TestW4Int_BucketBoundaries(t *testing.T) {
	entitySvc, recordSvc, _, _, sdb := w4Wire(t, false)
	ctx := context.Background()
	admin := RecordScope{IsAdmin: true}
	w4SeedDeals(t, ctx, entitySvc, recordSvc, sdb, o3aTenantA)

	agg := func(q AggregateQuery) []AggregateGroupResult {
		t.Helper()
		res, err := recordSvc.Aggregate(ctx, admin, o3aTenantA, "w4deals", q)
		if err != nil {
			t.Fatalf("Aggregate %+v: %v", q, err)
		}
		if !res.Grouped {
			t.Fatalf("Aggregate %+v: want grouped result", q)
		}
		return res.Groups
	}
	keysOf := func(gs []AggregateGroupResult) []string {
		out := make([]string, 0, len(gs))
		for _, g := range gs {
			out = append(out, strOrEmpty(g.Key))
		}
		return out
	}

	// MONTH over created_at: Jan 30 + Jan 31 collapse into 2026-01 despite being 1 day apart
	// from Feb 1 (the month-boundary proof); empty 2026-03 is OMITTED; chronological order.
	months := agg(AggregateQuery{Metric: AggMetricCount, GroupBy: "created_at", Bucket: "month"})
	wantKeys := []string{"2026-01", "2026-02", "2026-04"}
	if got := keysOf(months); strings.Join(got, ",") != strings.Join(wantKeys, ",") {
		t.Fatalf("month keys = %v; want %v (chronological, no empty 2026-03)", got, wantKeys)
	}
	if months[0].Count != 2 || months[1].Count != 1 || months[2].Count != 1 {
		t.Errorf("month counts = %d/%d/%d; want 2/1/1", months[0].Count, months[1].Count, months[2].Count)
	}

	// SUM metric bucketed: Jan=300, Feb=300, Apr=400 (exact DECIMAL, no drift).
	sums := agg(AggregateQuery{Metric: AggMetricSum, Field: "amount", GroupBy: "created_at", Bucket: "month"})
	if !w4DecEq(sums[0].Value, 300) || !w4DecEq(sums[1].Value, 300) || !w4DecEq(sums[2].Value, 400) {
		t.Errorf("month sums = %v/%v/%v; want 300/300/400",
			strOrEmpty(sums[0].Value), strOrEmpty(sums[1].Value), strOrEmpty(sums[2].Value))
	}

	// DAY: 4 distinct days, zero-padded chronological keys.
	days := agg(AggregateQuery{Metric: AggMetricCount, GroupBy: "created_at", Bucket: "day"})
	wantDays := []string{"2026-01-30", "2026-01-31", "2026-02-01", "2026-04-05"}
	if got := keysOf(days); strings.Join(got, ",") != strings.Join(wantDays, ",") {
		t.Errorf("day keys = %v; want %v", got, wantDays)
	}

	// QUARTER: Q1 has 3 rows (Jan+Feb), Q2 has 1 (Apr) — quarter-boundary proof.
	quarters := agg(AggregateQuery{Metric: AggMetricCount, GroupBy: "created_at", Bucket: "quarter"})
	if got := keysOf(quarters); strings.Join(got, ",") != "2026-Q1,2026-Q2" {
		t.Fatalf("quarter keys = %v; want [2026-Q1 2026-Q2]", got)
	}
	if quarters[0].Count != 3 || quarters[1].Count != 1 {
		t.Errorf("quarter counts = %d/%d; want 3/1", quarters[0].Count, quarters[1].Count)
	}

	// YEAR: single 2026 bucket with all 4.
	years := agg(AggregateQuery{Metric: AggMetricCount, GroupBy: "created_at", Bucket: "year"})
	if len(years) != 1 || strOrEmpty(years[0].Key) != "2026" || years[0].Count != 4 {
		t.Errorf("year buckets = %+v; want single 2026/4", years)
	}

	// Indexed DATE FIELD (closed_on, DATETIME generated column) buckets identically.
	fieldMonths := agg(AggregateQuery{Metric: AggMetricCount, GroupBy: "closed_on", Bucket: "month"})
	if got := keysOf(fieldMonths); strings.Join(got, ",") != strings.Join(wantKeys, ",") {
		t.Errorf("closed_on month keys = %v; want %v", got, wantKeys)
	}

	// Overlay + bucket compose: from 2026-02-01 → only Feb and Apr buckets remain.
	narrowed := agg(AggregateQuery{
		Metric: AggMetricCount, GroupBy: "created_at", Bucket: "month",
		Overlay: &RunOverlay{DateFrom: "2026-02-01"},
	})
	if got := keysOf(narrowed); strings.Join(got, ",") != "2026-02,2026-04" {
		t.Errorf("overlay+bucket keys = %v; want [2026-02 2026-04]", got)
	}
}

// --- 3: report run ↔ CSV export parity ----------------------------------------------------------

func TestW4Int_ReportOverlayAndExportParity(t *testing.T) {
	entitySvc, recordSvc, _, reportSvc, sdb := w4Wire(t, false)
	ctx := context.Background()
	admin := RecordScope{IsAdmin: true}
	ids := w4SeedDeals(t, ctx, entitySvc, recordSvc, sdb, o3aTenantA)

	// Tabular report: saved filter stage=won (r3, r4), projected columns [stage, amount].
	tab, err := reportSvc.CreateReport(ctx, o3aTenantA, CreateReportInput{
		Name: "Won deals", EntityName: "w4deals", Type: "tabular",
		Config: ReportConfig{
			Columns: []string{"stage", "amount"},
			Filters: []ReportFilterInput{{Field: "stage", Op: "eq", Values: []string{"won"}}},
		},
		ActorID: "u1",
	})
	if err != nil {
		t.Fatalf("CreateReport tabular: %v", err)
	}

	overlay := &RunOverlay{DateTo: "2026-02-28", OwnerID: "u2"} // narrows won → r3 only

	// On-screen run with overlay → exactly r3.
	run, err := reportSvc.RunReport(ctx, admin, o3aTenantA, tab.ID, overlay)
	if err != nil {
		t.Fatalf("RunReport: %v", err)
	}
	if run.Tabular == nil || run.Tabular.Total != 1 || len(run.Tabular.Records) != 1 {
		t.Fatalf("run tabular = %+v; want exactly 1 row (r3)", run.Tabular)
	}
	if run.Tabular.Records[0].ID != ids[2] {
		t.Errorf("run row id = %s; want r3 %s", run.Tabular.Records[0].ID, ids[2])
	}

	// CSV export with the SAME overlay → same single row. The report export carries the projected
	// columns under their LABELS and drops the raw id column the report screen never shows
	// (services.ExportCSV), so parity is asserted on the values.
	var buf bytes.Buffer
	if err := reportSvc.ExportReportCSV(ctx, admin, o3aTenantA, tab.ID, overlay, &buf); err != nil {
		t.Fatalf("ExportReportCSV: %v", err)
	}
	rows := parseExportCSV(t, buf.String())
	if len(rows) != 2 { // header + r3
		t.Fatalf("export rows = %d; want 2 (header + r3)\n%s", len(rows), buf.String())
	}
	if strings.Join(rows[0], ",") != "Stage,Amount" {
		t.Errorf("export header = %v; want the projected labels [Stage Amount]", rows[0])
	}
	if rows[1][0] != "won" || rows[1][1] != "300" {
		t.Errorf("export row = %v; want [won 300] (the overlay-narrowed row %s)", rows[1], ids[2])
	}

	// WITHOUT overlay the export covers BOTH won rows — proving the overlay narrowed it above.
	buf.Reset()
	if err := reportSvc.ExportReportCSV(ctx, admin, o3aTenantA, tab.ID, nil, &buf); err != nil {
		t.Fatalf("ExportReportCSV no-overlay: %v", err)
	}
	rows = parseExportCSV(t, buf.String())
	if len(rows) != 3 { // header + r3 + r4
		t.Errorf("no-overlay export rows = %d; want 3", len(rows))
	}

	// Summary report with month bucket: run groups == export CSV rows (key,value,count parity).
	sum, err := reportSvc.CreateReport(ctx, o3aTenantA, CreateReportInput{
		Name: "Revenue by month", EntityName: "w4deals", Type: "summary",
		Config:  ReportConfig{GroupBy: "created_at", Metric: "sum", Field: "amount", Bucket: "month"},
		ActorID: "u1",
	})
	if err != nil {
		t.Fatalf("CreateReport summary: %v", err)
	}
	sumRun, err := reportSvc.RunReport(ctx, admin, o3aTenantA, sum.ID, nil)
	if err != nil {
		t.Fatalf("RunReport summary: %v", err)
	}
	if sumRun.Aggregate == nil || !sumRun.Aggregate.Grouped {
		t.Fatalf("summary run = %+v; want grouped", sumRun)
	}
	buf.Reset()
	if err := reportSvc.ExportReportCSV(ctx, admin, o3aTenantA, sum.ID, nil, &buf); err != nil {
		t.Fatalf("ExportReportCSV summary: %v", err)
	}
	rows = parseExportCSV(t, buf.String())
	if len(rows) != len(sumRun.Aggregate.Groups)+1 {
		t.Fatalf("summary export rows = %d; want %d (header + groups)", len(rows), len(sumRun.Aggregate.Groups)+1)
	}
	if strings.Join(rows[0], ",") != "key,value,count" {
		t.Errorf("summary export header = %v; want [key value count]", rows[0])
	}
	for i, g := range sumRun.Aggregate.Groups {
		row := rows[i+1]
		if row[0] != strOrEmpty(g.Key) || row[1] != strOrEmpty(g.Value) || row[2] != strconv.FormatInt(g.Count, 10) {
			t.Errorf("summary export row[%d] = %v; want run group %+v", i, row, g)
		}
	}

	// Unknown report id → REPORT_NOT_FOUND before any byte.
	buf.Reset()
	var ve *ValidationError
	errExp := reportSvc.ExportReportCSV(ctx, admin, o3aTenantA, uuid.NewString(), nil, &buf)
	if !errors.As(errExp, &ve) || ve.Code != "REPORT_NOT_FOUND" {
		t.Errorf("export unknown id: want REPORT_NOT_FOUND, got %v", errExp)
	}
	if buf.Len() != 0 {
		t.Errorf("export error must write no bytes; wrote %d", buf.Len())
	}

	// Cross-tenant: tenant B cannot run/export A's report id.
	_, errB := reportSvc.RunReport(ctx, admin, o3aTenantB, tab.ID, overlay)
	if !errors.As(errB, &ve) || ve.Code != "REPORT_NOT_FOUND" {
		t.Errorf("B running A's report: want REPORT_NOT_FOUND, got %v", errB)
	}
	buf.Reset()
	errBExp := reportSvc.ExportReportCSV(ctx, admin, o3aTenantB, tab.ID, overlay, &buf)
	if !errors.As(errBExp, &ve) || ve.Code != "REPORT_NOT_FOUND" {
		t.Errorf("B exporting A's report: want REPORT_NOT_FOUND, got %v", errBExp)
	}
}

// --- 5: field-ACL fail-closed under overlay/export ----------------------------------------------

func TestW4Int_OverlayACLFailClosed(t *testing.T) {
	entitySvc, recordSvc, dashSvc, reportSvc, sdb := w4Wire(t, true)
	ctx := context.Background()
	w4SeedDeals(t, ctx, entitySvc, recordSvc, sdb, o3aTenantA)

	// Role with can_view=0 on `amount`.
	roleID := uuid.NewString()
	flsSeedTenant(t, sdb, o3aTenantA, "tenant-a-w4")
	flsSeedRole(t, sdb, roleID, o3aTenantA, "w4-viewer")
	flsSeedFieldGrant(t, sdb, o3aTenantA, roleID, "w4deals", "amount", false, false)
	role := RecordScope{UserID: "u-role", AccessLevel: "all", IsAdmin: false, RoleID: roleID}

	// (a) Aggregating the hidden field is refused EVEN WITH an overlay — the overlay must not
	// open a side door around FIELD_FORBIDDEN.
	_, err := recordSvc.Aggregate(ctx, role, o3aTenantA, "w4deals", AggregateQuery{
		Metric: AggMetricSum, Field: "amount",
		Overlay: &RunOverlay{DateFrom: "2026-01-01", DateTo: "2026-12-31"},
	})
	if !fieldForbidden(err) {
		t.Errorf("sum(hidden amount) with overlay: want FIELD_FORBIDDEN, got %v", err)
	}

	// (a2) Bucketing rides groupBy — a bucket over an ACL-hidden date field must be refused the
	// same way (security-audit W4-2 anti-regression pin: bucket must not become a side channel
	// to read a hidden date's distribution).
	flsSeedFieldGrant(t, sdb, o3aTenantA, roleID, "w4deals", "closed_on", false, false)
	_, err = recordSvc.Aggregate(ctx, role, o3aTenantA, "w4deals", AggregateQuery{
		Metric: AggMetricCount, GroupBy: "closed_on", Bucket: "month",
	})
	if !fieldForbidden(err) {
		t.Errorf("bucket(month) over hidden closed_on: want FIELD_FORBIDDEN, got %v", err)
	}

	// (b) A dashboard widget on the hidden field fails per-widget with FIELD_FORBIDDEN under an
	// overlay run; the sibling COUNT widget still succeeds (error isolation preserved).
	dash, err := dashSvc.CreateDashboard(ctx, o3aTenantA, CreateDashboardInput{
		Name: "ACL dash", ActorID: "u1",
		Widgets: []WidgetConfig{
			{ID: "ok", Title: "Count", Type: "kpi", Entity: "w4deals", Metric: "count"},
			{ID: "blocked", Title: "Sum hidden", Type: "kpi", Entity: "w4deals", Metric: "sum", Field: "amount"},
		},
	})
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	res, err := dashSvc.RunDashboard(ctx, role, o3aTenantA, dash.ID, &RunOverlay{OwnerID: "u1"})
	if err != nil {
		t.Fatalf("RunDashboard: %v", err)
	}
	if res.Widgets[0].Error != "" || res.Widgets[0].Result == nil || res.Widgets[0].Result.Scalar.Count != 2 {
		t.Errorf("count widget under role+overlay = %+v; want count=2 (u1's rows)", res.Widgets[0])
	}
	if res.Widgets[1].Code != "FIELD_FORBIDDEN" {
		t.Errorf("hidden-field widget code = %q; want FIELD_FORBIDDEN", res.Widgets[1].Code)
	}

	// (c) Report CSV export projecting the hidden column: rows still export, but the hidden
	// cells are EMPTY (read-strip), and a saved filter on the hidden field is refused outright.
	tab, err := reportSvc.CreateReport(ctx, o3aTenantA, CreateReportInput{
		Name: "Deals listing", EntityName: "w4deals", Type: "tabular",
		Config:  ReportConfig{Columns: []string{"stage", "amount"}},
		ActorID: "u1",
	})
	if err != nil {
		t.Fatalf("CreateReport: %v", err)
	}
	var buf bytes.Buffer
	if err := reportSvc.ExportReportCSV(ctx, role, o3aTenantA, tab.ID, nil, &buf); err != nil {
		t.Fatalf("ExportReportCSV as role: %v", err)
	}
	// The report export is BOM-led and carries the projected columns only — no raw id column
	// (queue #3, ccf6dfb9). So the projection [stage, amount] lands in cells 0 and 1, and the
	// hidden one is cell 1, not cell 2.
	rows := parseExportCSV(t, buf.String())
	if len(rows) != 5 { // header + 4 rows
		t.Fatalf("role export rows = %d; want 5", len(rows))
	}
	if len(rows[0]) != 2 {
		t.Fatalf("role export header = %v; want the 2 projected columns (no id column)", rows[0])
	}
	for i, row := range rows[1:] {
		if row[1] != "" {
			t.Errorf("row[%d] hidden amount cell = %q; want empty (ACL strip)", i, row[1])
		}
		if row[0] == "" {
			t.Errorf("row[%d] visible stage cell empty; want value", i)
		}
	}

	filtered, err := reportSvc.CreateReport(ctx, o3aTenantA, CreateReportInput{
		Name: "Filter hidden", EntityName: "w4deals", Type: "tabular",
		Config: ReportConfig{
			Columns: []string{"stage"},
			Filters: []ReportFilterInput{{Field: "amount", Op: "gte", Values: []string{"100"}}},
		},
		ActorID: "u1",
	})
	if err != nil {
		t.Fatalf("CreateReport filtered: %v", err)
	}
	buf.Reset()
	errExp := reportSvc.ExportReportCSV(ctx, role, o3aTenantA, filtered.ID, nil, &buf)
	if codeOf(errExp) != "LIST_UNKNOWN_FILTER" { // a hidden name reads like a missing one (queue #317)
		t.Errorf("export with hidden-field filter: want LIST_UNKNOWN_FILTER, got %v", errExp)
	}
	if buf.Len() != 0 {
		t.Errorf("forbidden export must write no bytes; wrote %d", buf.Len())
	}
}
