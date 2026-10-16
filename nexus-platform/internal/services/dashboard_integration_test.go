//go:build integration

// Dashboard M2 integration tests — run against a real throwaway MySQL 8 database.
//
// Uses the o3aSetup(t) harness (same throwaway-DB strategy as the O3a metadata-cache tests).
// Each test builds all services over a SINGLE shared DB via flatSetupDash(t) so DashboardService
// and RecordService share the same physical MySQL tables (no split-brain).
//
// Critical design proofs:
//  1. Scalar KPI widget + grouped widget return correct results matching direct Aggregate calls.
//  2. Tenant isolation: dashboard in tenant A → DASHBOARD_NOT_FOUND for tenant B; running as B
//     reflects only B's rows (no cross-tenant leakage).
//  3. A5 row-scope: non-admin scope sees only owned rows; admin scope sees all rows.
//  4. Per-widget error isolation (CRITICAL): one good widget + one non-indexed GroupBy widget →
//     nil overall error (200 overall), good widget has data, bad widget has
//     AGGREGATE_REQUIRES_INDEXED error code. A bad widget must NOT abort the run.
//  5. Unknown dashboard id → DASHBOARD_NOT_FOUND.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestDashboard -race -v
package services

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// flatSetupDash builds all services over a SINGLE throwaway DB so DashboardService and
// RecordService share the same physical tables (no split-brain across two throwaway databases).
func flatSetupDash(t *testing.T) (*DashboardService, *RecordService, *EntityService, *FieldService) {
	t.Helper()
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	mc := NewMetaCache(metaRepo, DefaultMetaTTL)
	entitySvc := NewEntityService(dynRepo, metaRepo).WithMetaCache(mc)
	fieldSvc := NewFieldService(dynRepo, metaRepo).WithMetaCache(mc)
	recordSvc := NewRecordService(mc, dynRepo)
	dashSvc := NewDashboardService(dashboard.NewSQLRepository(sdb), recordSvc, zap.NewNop())
	return dashSvc, recordSvc, entitySvc, fieldSvc
}

// TestDashboard_ScalarAndGrouped proves that a dashboard with two widgets — a scalar KPI and a
// grouped widget — returns correct results matching direct RecordService.Aggregate calls.
func TestDashboard_ScalarAndGrouped(t *testing.T) {
	dashSvc, recordSvc, entitySvc, _ := flatSetupDash(t)
	ctx := context.Background()

	// Create entity "deals" for tenant A.
	// "stage" is declared with options["indexed"]=true so the aggregate engine creates a generated
	// column for it and it lands in the sort/groupBy allowlist — GroupBy on it is valid.
	// Plain text fields WITHOUT the indexed option are JSON-only and trigger AGGREGATE_REQUIRES_INDEXED.
	_, _, err := entitySvc.CreateEntity(ctx, o3aTenantA, CreateEntityInput{
		Name:  "deals",
		Label: "Deals",
		Fields: []FieldInput{
			{Name: "amount", Label: "Amount", FieldType: "number"},
			{Name: "stage", Label: "Stage", FieldType: "text", Options: map[string]any{"indexed": true}},
		},
	})
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	scope := RecordScope{IsAdmin: true}
	// Seed three records: two "open" (sum=300), one "closed" (sum=300); count=3 total.
	// number fields require float64, not string.
	for _, r := range []struct {
		amount float64
		stage  string
	}{
		{100, "open"},
		{200, "open"},
		{300, "closed"},
	} {
		if _, err := recordSvc.Create(ctx, scope, o3aTenantA, "u-a", "deals", map[string]any{"amount": r.amount, "stage": r.stage}); err != nil {
			t.Fatalf("seed record: %v", err)
		}
	}

	// Create dashboard: widget w1 = scalar count; widget w2 = sum(amount) grouped by stage.
	// stage has options["indexed"]=true → generated column → valid GroupBy target.
	rec, err := dashSvc.CreateDashboard(ctx, o3aTenantA, CreateDashboardInput{
		Name:    "Sales Overview",
		ActorID: "u-a",
		Widgets: []WidgetConfig{
			{ID: "w1", Title: "Total Deals", Type: "kpi", Entity: "deals", Metric: "count"},
			{ID: "w2", Title: "Amount by Stage", Type: "bar", Entity: "deals", Metric: "sum", Field: "amount", GroupBy: "stage"},
		},
	})
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	result, err := dashSvc.RunDashboard(ctx, scope, o3aTenantA, rec.ID, nil)
	if err != nil {
		t.Fatalf("RunDashboard: %v", err)
	}
	if len(result.Widgets) != 2 {
		t.Fatalf("expected 2 widget results, got %d", len(result.Widgets))
	}

	// Widget 1: scalar count must equal 3.
	w1 := result.Widgets[0]
	if w1.Error != "" {
		t.Errorf("w1 unexpected error: code=%q err=%q", w1.Code, w1.Error)
	}
	if w1.Result == nil {
		t.Fatal("w1 Result is nil")
	}
	if w1.Result.Grouped {
		t.Error("w1 should be scalar (no groupBy), got Grouped=true")
	}
	if w1.Result.Scalar.Count != 3 {
		t.Errorf("w1 count: want 3, got %d", w1.Result.Scalar.Count)
	}

	// Compare w1 to a direct Aggregate call — must match.
	directW1, err := recordSvc.Aggregate(ctx, scope, o3aTenantA, "deals", AggregateQuery{
		Metric: AggMetricCount,
	})
	if err != nil {
		t.Fatalf("direct Aggregate w1: %v", err)
	}
	if directW1.Scalar.Count != w1.Result.Scalar.Count {
		t.Errorf("w1 count mismatch: direct=%d dashboard=%d", directW1.Scalar.Count, w1.Result.Scalar.Count)
	}

	// Widget 2: grouped sum by stage — must be grouped and non-empty.
	w2 := result.Widgets[1]
	if w2.Error != "" {
		t.Errorf("w2 unexpected error: code=%q err=%q", w2.Code, w2.Error)
	}
	if w2.Result == nil {
		t.Fatal("w2 Result is nil")
	}
	if !w2.Result.Grouped {
		t.Error("w2 should be grouped (groupBy=stage), got Grouped=false")
	}
	if len(w2.Result.Groups) == 0 {
		t.Error("w2 groups should be non-empty")
	}

	// Compare w2 to a direct Aggregate call — group count must match.
	directW2, err := recordSvc.Aggregate(ctx, scope, o3aTenantA, "deals", AggregateQuery{
		Metric:  AggMetricSum,
		Field:   "amount",
		GroupBy: "stage",
	})
	if err != nil {
		t.Fatalf("direct Aggregate w2: %v", err)
	}
	if !directW2.Grouped {
		t.Fatal("direct aggregate w2 should be grouped")
	}
	if len(directW2.Groups) != len(w2.Result.Groups) {
		t.Errorf("w2 group count mismatch: direct=%d dashboard=%d", len(directW2.Groups), len(w2.Result.Groups))
	}
}

// TestDashboard_TenantIsolation proves that a dashboard in tenant A is invisible to tenant B
// (DASHBOARD_NOT_FOUND), and that running as B reflects only B's data (not A's).
func TestDashboard_TenantIsolation(t *testing.T) {
	dashSvc, recordSvc, entitySvc, _ := flatSetupDash(t)
	ctx := context.Background()

	// Create entity "contacts" in both tenants.
	for _, tenantID := range []string{o3aTenantA, o3aTenantB} {
		if _, _, err := entitySvc.CreateEntity(ctx, tenantID, CreateEntityInput{
			Name:   "contacts",
			Label:  "Contacts",
			Fields: []FieldInput{{Name: "fullname", Label: "Name", FieldType: "text"}},
		}); err != nil {
			t.Fatalf("CreateEntity tenant=%s: %v", tenantID, err)
		}
	}

	scope := RecordScope{IsAdmin: true}
	// Seed 1 record in tenant A, 1 in tenant B.
	for _, tc := range []struct{ tenant, user string }{{o3aTenantA, "u-a"}, {o3aTenantB, "u-b"}} {
		if _, err := recordSvc.Create(ctx, scope, tc.tenant, tc.user, "contacts", map[string]any{"fullname": tc.user}); err != nil {
			t.Fatalf("seed record tenant=%s: %v", tc.tenant, err)
		}
	}

	// Create dashboard in tenant A.
	recA, err := dashSvc.CreateDashboard(ctx, o3aTenantA, CreateDashboardInput{
		Name:    "Tenant A Dashboard",
		ActorID: "u-a",
		Widgets: []WidgetConfig{
			{ID: "w1", Title: "All Contacts", Type: "kpi", Entity: "contacts", Metric: "count"},
		},
	})
	if err != nil {
		t.Fatalf("CreateDashboard A: %v", err)
	}

	// Tenant B must NOT be able to get or run tenant A's dashboard.
	var ve *ValidationError
	_, errGet := dashSvc.GetDashboard(ctx, o3aTenantB, recA.ID)
	if !errors.As(errGet, &ve) || ve.Code != "DASHBOARD_NOT_FOUND" {
		t.Errorf("GetDashboard as B: want DASHBOARD_NOT_FOUND, got %v", errGet)
	}
	_, errRun := dashSvc.RunDashboard(ctx, scope, o3aTenantB, recA.ID, nil)
	if !errors.As(errRun, &ve) || ve.Code != "DASHBOARD_NOT_FOUND" {
		t.Errorf("RunDashboard as B with A's id: want DASHBOARD_NOT_FOUND, got %v", errRun)
	}

	// Running A's dashboard as tenant A sees only A's row (count=1).
	resA, err := dashSvc.RunDashboard(ctx, scope, o3aTenantA, recA.ID, nil)
	if err != nil {
		t.Fatalf("RunDashboard as A: %v", err)
	}
	if len(resA.Widgets) != 1 {
		t.Fatalf("A: expected 1 widget, got %d", len(resA.Widgets))
	}
	wA := resA.Widgets[0]
	if wA.Error != "" {
		t.Errorf("A widget error: code=%q err=%q", wA.Code, wA.Error)
	}
	if wA.Result == nil || wA.Result.Scalar.Count != 1 {
		t.Errorf("A run: want count=1, got %v", wA.Result)
	}

	// Create dashboard in tenant B and run it — must see only B's row (count=1).
	recB, err := dashSvc.CreateDashboard(ctx, o3aTenantB, CreateDashboardInput{
		Name:    "Tenant B Dashboard",
		ActorID: "u-b",
		Widgets: []WidgetConfig{
			{ID: "w1", Title: "B Contacts", Type: "kpi", Entity: "contacts", Metric: "count"},
		},
	})
	if err != nil {
		t.Fatalf("CreateDashboard B: %v", err)
	}
	resB, err := dashSvc.RunDashboard(ctx, scope, o3aTenantB, recB.ID, nil)
	if err != nil {
		t.Fatalf("RunDashboard B: %v", err)
	}
	wB := resB.Widgets[0]
	if wB.Error != "" {
		t.Errorf("B widget error: code=%q err=%q", wB.Code, wB.Error)
	}
	if wB.Result == nil || wB.Result.Scalar.Count != 1 {
		t.Errorf("B run: want count=1, got %v", wB.Result)
	}
}

// TestDashboard_A5RowScope proves that a non-admin RecordScope sees only its owned rows, while an
// admin scope sees all rows — both running the same dashboard id.
func TestDashboard_A5RowScope(t *testing.T) {
	dashSvc, recordSvc, entitySvc, _ := flatSetupDash(t)
	ctx := context.Background()

	if _, _, err := entitySvc.CreateEntity(ctx, o3aTenantA, CreateEntityInput{
		Name:  "tasks",
		Label: "Tasks",
		Fields: []FieldInput{
			{Name: "title", Label: "Title", FieldType: "text"},
		},
	}); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	adminScope := RecordScope{IsAdmin: true}
	// AccessLevel "owner" restricts to rows where created_by = UserID.
	// Without setting AccessLevel, the empty string maps to AccessNone (fail-closed) → 0 rows.
	userScope := RecordScope{UserID: "u-scoped", IsAdmin: false, AccessLevel: "owner"}

	// Seed 3 records owned by u-scoped and 2 owned by u-other.
	for i := 0; i < 3; i++ {
		if _, err := recordSvc.Create(ctx, adminScope, o3aTenantA, "u-scoped", "tasks", map[string]any{"title": "mine"}); err != nil {
			t.Fatalf("seed owned record: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := recordSvc.Create(ctx, adminScope, o3aTenantA, "u-other", "tasks", map[string]any{"title": "other"}); err != nil {
			t.Fatalf("seed other record: %v", err)
		}
	}

	rec, err := dashSvc.CreateDashboard(ctx, o3aTenantA, CreateDashboardInput{
		Name:    "Task Count",
		ActorID: "u-scoped",
		Widgets: []WidgetConfig{
			{ID: "w1", Title: "My Tasks", Type: "kpi", Entity: "tasks", Metric: "count"},
		},
	})
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	// Admin scope: sees all 5 records.
	adminRes, err := dashSvc.RunDashboard(ctx, adminScope, o3aTenantA, rec.ID, nil)
	if err != nil {
		t.Fatalf("RunDashboard admin: %v", err)
	}
	adminW := adminRes.Widgets[0]
	if adminW.Error != "" {
		t.Errorf("admin widget error: code=%q err=%q", adminW.Code, adminW.Error)
	}
	if adminW.Result == nil || adminW.Result.Scalar.Count != 5 {
		t.Errorf("admin scope: want count=5, got %v", adminW.Result)
	}

	// Non-admin scope (u-scoped): sees only 3 owned records.
	userRes, err := dashSvc.RunDashboard(ctx, userScope, o3aTenantA, rec.ID, nil)
	if err != nil {
		t.Fatalf("RunDashboard user: %v", err)
	}
	userW := userRes.Widgets[0]
	if userW.Error != "" {
		t.Errorf("user widget error: code=%q err=%q", userW.Code, userW.Error)
	}
	if userW.Result == nil || userW.Result.Scalar.Count != 3 {
		t.Errorf("user scope: want count=3, got %v", userW.Result)
	}
}

// TestDashboard_PerWidgetErrorIsolation is the CRITICAL design proof: one valid widget + one widget
// with a non-indexed GroupBy → RunDashboard returns nil error (200 overall), good widget has data,
// bad widget has AGGREGATE_REQUIRES_INDEXED. A bad widget MUST NOT abort the run.
func TestDashboard_PerWidgetErrorIsolation(t *testing.T) {
	dashSvc, recordSvc, entitySvc, _ := flatSetupDash(t)
	ctx := context.Background()

	if _, _, err := entitySvc.CreateEntity(ctx, o3aTenantA, CreateEntityInput{
		Name:  "orders",
		Label: "Orders",
		Fields: []FieldInput{
			{Name: "amount", Label: "Amount", FieldType: "number"},
			// "note" is a text field stored in JSON data column — NOT a generated/indexed column,
			// so GroupBy on it triggers AGGREGATE_REQUIRES_INDEXED from the aggregate engine.
			{Name: "note", Label: "Note", FieldType: "text"},
		},
	}); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	scope := RecordScope{IsAdmin: true}
	// number fields require numeric values (float64), not strings.
	for _, amt := range []float64{50, 150} {
		if _, err := recordSvc.Create(ctx, scope, o3aTenantA, "u-a", "orders", map[string]any{"amount": amt, "note": "n"}); err != nil {
			t.Fatalf("seed record: %v", err)
		}
	}

	rec, err := dashSvc.CreateDashboard(ctx, o3aTenantA, CreateDashboardInput{
		Name:    "Error Isolation Test",
		ActorID: "u-a",
		Widgets: []WidgetConfig{
			// Good widget: scalar count — succeeds.
			{ID: "good", Title: "Order Count", Type: "kpi", Entity: "orders", Metric: "count"},
			// Bad widget: groupBy on non-indexed JSON field — AGGREGATE_REQUIRES_INDEXED.
			{ID: "bad", Title: "Orders by Note", Type: "bar", Entity: "orders", Metric: "count", GroupBy: "note"},
		},
	})
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	// RunDashboard MUST return nil error overall — per-widget errors live in the body, not here.
	result, err := dashSvc.RunDashboard(ctx, scope, o3aTenantA, rec.ID, nil)
	if err != nil {
		t.Fatalf("RunDashboard: expected nil error (200 overall), got: %v", err)
	}
	if len(result.Widgets) != 2 {
		t.Fatalf("expected 2 widget results, got %d", len(result.Widgets))
	}

	// Good widget: must have result data, no error.
	good := result.Widgets[0]
	if good.Error != "" {
		t.Errorf("good widget should have no error, got code=%q err=%q", good.Code, good.Error)
	}
	if good.Result == nil {
		t.Fatal("good widget Result is nil")
	}
	if good.Result.Scalar.Count != 2 {
		t.Errorf("good widget count: want 2, got %d", good.Result.Scalar.Count)
	}

	// Bad widget: Result must be nil; Code must be AGGREGATE_REQUIRES_INDEXED.
	bad := result.Widgets[1]
	if bad.Result != nil {
		t.Errorf("bad widget Result should be nil, got %+v", bad.Result)
	}
	if bad.Code != "AGGREGATE_REQUIRES_INDEXED" {
		t.Errorf("bad widget code: want AGGREGATE_REQUIRES_INDEXED, got %q (err: %q)", bad.Code, bad.Error)
	}
}

// TestDashboard_UnknownID proves that fetching or running an unknown dashboard id returns
// DASHBOARD_NOT_FOUND (not a 500 or empty result).
func TestDashboard_UnknownID(t *testing.T) {
	dashSvc, _, _, _ := flatSetupDash(t)
	ctx := context.Background()
	scope := RecordScope{IsAdmin: true}
	unknownID := "00000000-dead-beef-0000-000000000000"

	var ve *ValidationError
	_, errGet := dashSvc.GetDashboard(ctx, o3aTenantA, unknownID)
	if !errors.As(errGet, &ve) || ve.Code != "DASHBOARD_NOT_FOUND" {
		t.Errorf("GetDashboard unknown: want DASHBOARD_NOT_FOUND, got %v", errGet)
	}

	_, errRun := dashSvc.RunDashboard(ctx, scope, o3aTenantA, unknownID, nil)
	if !errors.As(errRun, &ve) || ve.Code != "DASHBOARD_NOT_FOUND" {
		t.Errorf("RunDashboard unknown: want DASHBOARD_NOT_FOUND, got %v", errRun)
	}
}
