package services

// specs/062-customer-workspace L4 T064 — a dashboard's create / update (pinning a widget) / delete
// leaves a __dashboard__ audit row; a draft-only write leaves none (FR-022).

import (
	"context"
	"testing"

	"nexus/platform/internal/audit"
)

func kpiWidget(id string) WidgetConfig {
	return WidgetConfig{ID: id, Title: "Số deal", Type: "kpi", Entity: "crm_x", Metric: "count", W: 3, H: 2}
}

func TestDashboardService_WritesEmitDashboardAudit(t *testing.T) {
	aud := &captureAuditor{}
	svc := NewDashboardService(newDraftFakeDashRepo(), nil, nil).WithAuditor(aud, nil)
	ctx := audit.WithActor(context.Background(), "builder-1")

	created, err := svc.CreateDashboard(ctx, dwTenant, CreateDashboardInput{Name: "Tổng quan", Widgets: []WidgetConfig{kpiWidget("w1")}, ActorID: "builder-1"})
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	// "Ghim widget": the full-replace update adds a second widget.
	if _, err := svc.UpdateDashboard(ctx, dwTenant, created.ID, UpdateDashboardInput{Name: "Tổng quan", Widgets: []WidgetConfig{kpiWidget("w1"), kpiWidget("w2")}}); err != nil {
		t.Fatalf("UpdateDashboard: %v", err)
	}
	if err := svc.DeleteDashboard(ctx, dwTenant, created.ID); err != nil {
		t.Fatalf("DeleteDashboard: %v", err)
	}

	want := []string{audit.ActionCreate, audit.ActionUpdate, audit.ActionDelete}
	if len(aud.events) != len(want) {
		t.Fatalf("events = %d; want %d: %+v", len(aud.events), len(want), aud.events)
	}
	for i, e := range aud.events {
		if e.EntityName != audit.DashboardMeta || e.Action != want[i] || e.TenantID != dwTenant || e.ActorUserID != "builder-1" || e.RecordID != created.ID {
			t.Errorf("event %d = %+v; want %s/%s by builder-1 on %s", i, e, audit.DashboardMeta, want[i], created.ID)
		}
		if e.Diff == nil {
			t.Fatalf("event %d has no diff", i)
		}
	}
	if aud.events[1].Diff.Before["widgets"] == nil || aud.events[1].Diff.After["widgets"] == nil {
		t.Errorf("update diff = %+v; want both sides of widgets", aud.events[1].Diff)
	}
	if aud.events[2].Diff.After != nil || aud.events[2].Diff.Before["name"] != "Tổng quan" {
		t.Errorf("delete diff = %+v; want before-only", aud.events[2].Diff)
	}
}

func TestDashboardService_RejectedWriteEmitsNothing(t *testing.T) {
	aud := &captureAuditor{}
	svc := NewDashboardService(newDraftFakeDashRepo(), nil, nil).WithAuditor(aud, nil)
	if _, err := svc.CreateDashboard(context.Background(), dwTenant, CreateDashboardInput{Name: ""}); err == nil {
		t.Fatal("want DASHBOARD_NAME_REQUIRED")
	}
	if err := svc.DeleteDashboard(context.Background(), dwTenant, "ghost"); err == nil {
		t.Fatal("want DASHBOARD_NOT_FOUND")
	}
	if len(aud.events) != 0 {
		t.Fatalf("events after rejected writes = %d; want 0", len(aud.events))
	}
}

func TestDashboardService_DraftOnlyWriteLeavesNoRow(t *testing.T) {
	live, _, repo := newDDEnv(true)
	aud := &captureAuditor{}
	svc := NewDashboardService(repo, nil, nil).WithAuditor(aud, NewDraftGate(&fakeDelivered{delivered: true}))

	if _, err := svc.CreateDashboard(builderCtx(), dwTenant, CreateDashboardInput{Name: "draft only", Widgets: []WidgetConfig{kpiWidget("w1")}}); err != nil {
		t.Fatalf("builder CreateDashboard: %v", err)
	}
	if len(aud.events) != 0 {
		t.Fatalf("a draft-only create wrote %d audit row(s); want 0", len(aud.events))
	}

	liveID, err := live.Create(context.Background(), ddCreate("live"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateDashboard(legacyAdminCtx(), dwTenant, liveID, UpdateDashboardInput{Name: "live", Widgets: []WidgetConfig{kpiWidget("w9")}}); err != nil {
		t.Fatalf("legacy admin UpdateDashboard: %v", err)
	}
	if len(aud.events) != 1 || aud.events[0].RecordID != liveID {
		t.Fatalf("a live write must be audited; events = %+v", aud.events)
	}
}
