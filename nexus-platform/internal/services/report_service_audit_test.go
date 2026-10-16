package services

// specs/062-customer-workspace L4 T064 — a report's create / update / delete leaves a __report__
// audit row with the actor from ctx and a before/after diff; a write that only reached the builder's
// draft leaves none (FR-022); a rejected write leaves none.

import (
	"context"
	"testing"

	"nexus/platform/internal/audit"
)

func reportAuditCtx() context.Context { return audit.WithActor(context.Background(), "builder-1") }

func tabularReport(name string, cols ...string) CreateReportInput {
	return CreateReportInput{Name: name, EntityName: "crm_x", Type: "tabular", Config: ReportConfig{Columns: cols}, ActorID: "builder-1"}
}

func TestReportService_WritesEmitReportAudit(t *testing.T) {
	aud := &captureAuditor{}
	svc := NewReportService(newDraftFakeReportRepo(), nil).WithAuditor(aud, nil)
	ctx := reportAuditCtx()

	created, err := svc.CreateReport(ctx, dwTenant, tabularReport("Doanh thu", "a"))
	if err != nil {
		t.Fatalf("CreateReport: %v", err)
	}
	if _, err := svc.UpdateReport(ctx, dwTenant, created.ID, UpdateReportInput{Name: "Doanh thu", EntityName: "crm_x", Type: "tabular", Config: ReportConfig{Columns: []string{"a", "b"}}}); err != nil {
		t.Fatalf("UpdateReport: %v", err)
	}
	if err := svc.DeleteReport(ctx, dwTenant, created.ID); err != nil {
		t.Fatalf("DeleteReport: %v", err)
	}

	want := []string{audit.ActionCreate, audit.ActionUpdate, audit.ActionDelete}
	if len(aud.events) != len(want) {
		t.Fatalf("events = %d; want %d: %+v", len(aud.events), len(want), aud.events)
	}
	for i, e := range aud.events {
		if e.EntityName != audit.ReportMeta || e.Action != want[i] || e.TenantID != dwTenant || e.ActorUserID != "builder-1" || e.RecordID != created.ID {
			t.Errorf("event %d = %+v; want %s/%s by builder-1 on %s", i, e, audit.ReportMeta, want[i], created.ID)
		}
		if e.Diff == nil {
			t.Fatalf("event %d has no diff", i)
		}
	}
	if aud.events[0].Diff.Before != nil || aud.events[0].Diff.After["name"] != "Doanh thu" {
		t.Errorf("create diff = %+v; want after-only with the name", aud.events[0].Diff)
	}
	if aud.events[1].Diff.Before["config"] == nil || aud.events[1].Diff.After["config"] == nil {
		t.Errorf("update diff = %+v; want both sides of config", aud.events[1].Diff)
	}
	if aud.events[2].Diff.After != nil || aud.events[2].Diff.Before["name"] != "Doanh thu" {
		t.Errorf("delete diff = %+v; want before-only", aud.events[2].Diff)
	}
}

func TestReportService_RejectedWriteEmitsNothing(t *testing.T) {
	aud := &captureAuditor{}
	svc := NewReportService(newDraftFakeReportRepo(), nil).WithAuditor(aud, nil)
	if _, err := svc.CreateReport(reportAuditCtx(), dwTenant, CreateReportInput{Name: "", EntityName: "crm_x", Type: "tabular"}); err == nil {
		t.Fatal("want REPORT_NAME_REQUIRED")
	}
	if err := svc.DeleteReport(reportAuditCtx(), dwTenant, "ghost"); err == nil {
		t.Fatal("want REPORT_NOT_FOUND")
	}
	if len(aud.events) != 0 {
		t.Fatalf("events after rejected writes = %d; want 0", len(aud.events))
	}
}

// FR-022: the builder's save on a delivered workspace only reaches the draft => no row; the legacy
// customer admin's save is live => a row.
func TestReportService_DraftOnlyWriteLeavesNoRow(t *testing.T) {
	e := newDREnv(true)
	aud := &captureAuditor{}
	svc := NewReportService(e.repo, nil).WithAuditor(aud, NewDraftGate(e.deliv))

	created, err := svc.CreateReport(builderCtx(), dwTenant, tabularReport("draft only", "a"))
	if err != nil {
		t.Fatalf("builder CreateReport: %v", err)
	}
	if created.DraftState != "new" || len(aud.events) != 0 {
		t.Fatalf("DraftState=%q events=%d; want a draft and no audit row", created.DraftState, len(aud.events))
	}

	liveID := e.seedLive(t, "live")
	if _, err := svc.UpdateReport(legacyAdminCtx(), dwTenant, liveID, UpdateReportInput{Name: "live", EntityName: "crm_x", Type: "tabular", Config: ReportConfig{Columns: []string{"z"}}}); err != nil {
		t.Fatalf("legacy admin UpdateReport: %v", err)
	}
	if len(aud.events) != 1 || aud.events[0].RecordID != liveID {
		t.Fatalf("a live write must be audited; events = %+v", aud.events)
	}
}
