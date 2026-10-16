package services

import (
	"context"
	"strings"
	"testing"
)

// Queue #294 (pin-joined-report-widget): a widget carries the report's joins and runs through the
// SAME aggregate engine, so the card shows the number the report shows.

func joinedWidget() WidgetConfig {
	return WidgetConfig{
		ID: "w1", Title: "Theo phòng", Type: "bar", Entity: "tickets", Metric: "count",
		GroupBy: "dept.id", Joins: []ReportJoinInput{{Relationship: "assignee"}, {Relationship: "dept", Optional: true}},
	}
}

func TestValidateDashboardWidgets_Joins(t *testing.T) {
	tests := []struct {
		name     string
		mut      func(w *WidgetConfig)
		wantCode string
	}{
		{"valid two-hop widget", func(w *WidgetConfig) {}, ""},
		{"three hops", func(w *WidgetConfig) { w.Joins = append(w.Joins, ReportJoinInput{Relationship: "tagged"}) }, "REPORT_JOIN_TOO_MANY"},
		{"blank relationship", func(w *WidgetConfig) { w.Joins = []ReportJoinInput{{Relationship: " "}} }, "REPORT_JOIN_RELATION_UNKNOWN"},
		{"token prefix names no join", func(w *WidgetConfig) { w.GroupBy = "tagged.id" }, "REPORT_JOIN_ENTITY_MISMATCH"},
		{"namespaced filter", func(w *WidgetConfig) {
			w.Filters = []ReportFilterInput{{Field: "dept.region", Op: "eq", Values: []string{"x"}}}
		}, "REPORT_JOIN_FILTER_UNSUPPORTED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := joinedWidget()
			tt.mut(&w)
			b, err := ValidateDashboardWidgets([]WidgetConfig{w})
			if got := codeOf(err); got != tt.wantCode {
				t.Fatalf("code = %q, want %q (err=%v)", got, tt.wantCode, err)
			}
			if tt.wantCode == "" && !strings.Contains(string(b), `"joins":[{"relationship":"assignee"},{"relationship":"dept","optional":true}]`) {
				t.Fatalf("joins not persisted: %s", b)
			}
		})
	}
}

func TestDashboardService_SaveChecksWidgetJoinsAgainstRegistry(t *testing.T) {
	s := NewDashboardService(nil, buildRJService(&rjStore{}, &rjAuthz{allow: map[string]bool{}}, nil), nil)
	ctx := context.Background()
	if err := s.checkWidgetJoinsDeep(ctx, "t1", []WidgetConfig{joinedWidget()}); err != nil {
		t.Fatalf("valid chain: %v", err)
	}
	bad := joinedWidget()
	bad.Joins = []ReportJoinInput{{Relationship: "dept"}, {Relationship: "assignee"}}
	if got := codeOf(s.checkWidgetJoinsDeep(ctx, "t1", []WidgetConfig{bad})); got != "REPORT_JOIN_ENTITY_MISMATCH" {
		t.Fatalf("misordered chain: code = %q, want REPORT_JOIN_ENTITY_MISMATCH (would save then die at run)", got)
	}
}

func TestRunWidget_JoinedWidgetRunsThroughTheJoinEngine(t *testing.T) {
	ctx := context.Background()
	scope := RecordScope{UserID: "u1", RoleID: "role1", AccessLevel: "all"}
	allow := &rjAuthz{allow: map[string]bool{"tickets": true, "employees": true, "departments": true}}

	t.Run("allowed caller: store sees both hops", func(t *testing.T) {
		store := &rjStore{}
		rs := buildRJService(store, allow, &rjFieldACL{})
		res := runWidget(ctx, rs, nil, scope, "t1", joinedWidget(), nil)
		if res.Error != "" || res.Result == nil {
			t.Fatalf("widget failed: %q %q", res.Code, res.Error)
		}
		if len(store.groupedParams.Joins) != 2 || store.groupedParams.GroupByAlias != "j1" {
			t.Fatalf("params = joins %d alias %q, want 2 / j1", len(store.groupedParams.Joins), store.groupedParams.GroupByAlias)
		}
	})

	t.Run("caller without a grant on the middle entity gets a per-widget REPORT_JOIN_FORBIDDEN", func(t *testing.T) {
		store := &rjStore{}
		denied := &rjAuthz{allow: map[string]bool{"tickets": true, "employees": false, "departments": true}}
		rs := buildRJService(store, denied, &rjFieldACL{})
		res := runWidget(ctx, rs, nil, scope, "t1", joinedWidget(), nil)
		if res.Code != "REPORT_JOIN_FORBIDDEN" || res.Result != nil {
			t.Fatalf("code = %q result=%v, want REPORT_JOIN_FORBIDDEN and no result", res.Code, res.Result)
		}
		if store.groupedCalled {
			t.Fatalf("store must not be reached")
		}
	})
}

// The deep check must be WIRED into both save paths, not merely exist: a misordered chain is
// refused by Create and by Update, and a good chain persists its joins.
func TestDashboardService_CreateUpdateRunDeepJoinCheck(t *testing.T) {
	svc := NewDashboardService(newDraftFakeDashRepo(), buildRJService(&rjStore{}, &rjAuthz{allow: map[string]bool{}}, nil), nil)
	ctx := context.Background()
	bad := joinedWidget()
	bad.Joins = []ReportJoinInput{{Relationship: "dept"}, {Relationship: "assignee"}}

	if _, err := svc.CreateDashboard(ctx, dwTenant, CreateDashboardInput{Name: "x", Widgets: []WidgetConfig{bad}}); codeOf(err) != "REPORT_JOIN_ENTITY_MISMATCH" {
		t.Fatalf("Create misordered: code = %q, want REPORT_JOIN_ENTITY_MISMATCH (err=%v)", codeOf(err), err)
	}
	rec, err := svc.CreateDashboard(ctx, dwTenant, CreateDashboardInput{Name: "x", Widgets: []WidgetConfig{joinedWidget()}})
	if err != nil {
		t.Fatalf("Create valid chain: %v", err)
	}
	if !strings.Contains(string(rec.Widgets), `"joins"`) {
		t.Fatalf("joins not persisted: %s", rec.Widgets)
	}
	if _, err := svc.UpdateDashboard(ctx, dwTenant, rec.ID, UpdateDashboardInput{Name: "x", Widgets: []WidgetConfig{bad}}); codeOf(err) != "REPORT_JOIN_ENTITY_MISMATCH" {
		t.Fatalf("Update misordered: code = %q, want REPORT_JOIN_ENTITY_MISMATCH (err=%v)", codeOf(err), err)
	}
}
