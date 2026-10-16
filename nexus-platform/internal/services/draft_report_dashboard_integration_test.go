//go:build integration

package services

// draft_report_dashboard_integration_test.go — specs/063-draft-publish S7-2 (T052/T054/T055/T056)
// against a real MySQL: the REAL report/dashboard services over the draft-aware repos, the real
// publish transaction, the real app-nav cascade.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/report"
)

type rdEnv struct {
	dpEnv
	rep  *ReportService
	dash *DashboardService
}

func rdSetup(t *testing.T) rdEnv {
	t.Helper()
	e := dpSetupWith(t, NewWorkflowDocAdapter(), NewReportDocAdapter(), NewDashboardDocAdapter())
	rawReport, rawDash := report.NewSQLRepository(e.sdb), dashboard.NewSQLRepository(e.sdb)
	return rdEnv{dpEnv: e,
		rep:  NewReportService(NewDraftReportRepo(rawReport, e.drafts, e.gate), e.rec).WithEntityChecker(e.meta),
		dash: NewDashboardService(NewDraftDashboardRepo(rawDash, e.drafts, e.gate), e.rec, nil)}
}

func tabular(name string, cols ...string) CreateReportInput {
	return CreateReportInput{Name: name, EntityName: "hk_deals", Type: "tabular", Config: ReportConfig{Columns: cols}}
}

func (e rdEnv) liveReportName(t *testing.T, tenant, id string) string {
	t.Helper()
	var n string
	if err := e.sdb.Get(&n, `SELECT name FROM sys_reports WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, tenant, id); err != nil {
		return ""
	}
	return n
}

// T055 + T054: on a delivered tenant the builder's report edit is a draft; the builder asking for it
// sees it; staff / legacy admin / the builder without the header see the live report; an unchanged
// re-save publishes without PUBLISH_CONFLICT; publish writes the report with the draft's id.
func TestDraftReport_EndToEnd_Integration(t *testing.T) {
	e := rdSetup(t)
	tenant := e.newTenant(t)
	builder := uuid.NewString()
	bctx, sctx := builderCtxOf(tenant, builder), legacyCtxOf(tenant)
	noHeader := metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: builder, TenantID: tenant, HomeTenantID: tenant, IsBuilder: true})

	live, err := e.rep.CreateReport(bctx, tenant, tabular("Pipeline", "name"))
	if err != nil {
		t.Fatal(err)
	}
	e.deliver(t, tenant, builder)

	// Save a draft with a NEW column; the live row does not move.
	upd := UpdateReportInput{Name: "Pipeline", EntityName: "hk_deals", Type: "tabular", Config: ReportConfig{Columns: []string{"name", "stage"}}}
	got, err := e.rep.UpdateReport(bctx, tenant, live.ID, upd)
	if err != nil || got.DraftState != "edited" || len(got.Config) == 0 {
		t.Fatalf("draft save = %+v, %v", got, err)
	}
	if e.count(t, `SELECT COUNT(*) FROM sys_reports WHERE tenant_id = ? AND config LIKE '%stage%'`, tenant) != 0 {
		t.Fatal("the draft reached the live row")
	}
	// A second save keeps ONE draft row (US3: two saves in a row keep both changes).
	upd.Config.Columns = []string{"name", "stage", "code"}
	if _, err := e.rep.UpdateReport(bctx, tenant, live.ID, upd); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM sys_meta_drafts WHERE tenant_id = ? AND doc_type = 'report'`, tenant); n != 1 {
		t.Fatalf("draft rows = %d, want 1", n)
	}

	// Readers.
	if r, err := e.rep.GetReport(bctx, tenant, live.ID); err != nil || r.DraftState != "edited" {
		t.Fatalf("builder+header Get = %+v, %v", r, err)
	}
	for name, c := range map[string]context.Context{"legacy admin/staff": sctx, "builder without header": noHeader} {
		r, err := e.rep.GetReport(c, tenant, live.ID)
		if err != nil || r.DraftState != "" || string(r.Config) == "" {
			t.Fatalf("%s Get = %+v, %v; want the live report", name, r, err)
		}
		list, err := e.rep.ListReports(c, tenant)
		if err != nil || len(list) != 1 || list[0].DraftState != "" {
			t.Fatalf("%s List = %+v, %v", name, list, err)
		}
	}

	// A draft-only report opens (US3 #7) and is not live.
	nr, err := e.rep.CreateReport(bctx, tenant, tabular("Only draft", "name"))
	if err != nil || nr.DraftState != "new" {
		t.Fatalf("new draft report = %+v, %v", nr, err)
	}
	if _, err := e.rep.GetReport(bctx, tenant, nr.ID); err != nil {
		t.Fatalf("a report that exists only in the draft must open: %v", err)
	}
	if _, err := e.rep.GetReport(sctx, tenant, nr.ID); err == nil {
		t.Fatal("staff can open a draft-only report")
	}

	// Publish: the live row changes, the draft-only report is inserted under its draft id.
	res, err := e.publishWithCurrentHash(bctx, tenant, false)
	if err != nil || res.DocCount != 2 {
		t.Fatalf("Publish = %+v, %v", res, err)
	}
	if got := e.liveReportName(t, tenant, nr.ID); got != "Only draft" {
		t.Fatalf("draft-only report after publish = %q", got)
	}
	if r, _ := e.rep.GetReport(sctx, tenant, live.ID); string(r.Config) == "" || e.count(t, `SELECT COUNT(*) FROM sys_reports WHERE tenant_id = ? AND config LIKE '%code%'`, tenant) != 1 {
		t.Fatalf("publish did not write the draft config: %+v", r)
	}
	if e.draftCount(t, tenant) != 0 {
		t.Fatal("drafts survived publish")
	}

	// T054: an unchanged re-save is not a conflict and not a change in content.
	same := UpdateReportInput{Name: "Pipeline", EntityName: "hk_deals", Type: "tabular", Config: ReportConfig{Columns: []string{"name", "stage", "code"}}}
	if _, err := e.rep.UpdateReport(bctx, tenant, live.ID, same); err != nil {
		t.Fatal(err)
	}
	if _, err := e.publishWithCurrentHash(bctx, tenant, false); err != nil {
		t.Fatalf("publishing a draft identical to the live report: %v (SC-006)", err)
	}
}

// T056 + FR-025: a draft delete of a dashboard that sits in an app's menu leaves the menu alone until
// Publish; Publish removes the dashboard AND its menu row in one tx (no dead tab). T052: the same
// SQL runs on the tx as on the pool.
func TestDraftDashboard_DeletePurgesNavOnlyAtPublish_Integration(t *testing.T) {
	e := rdSetup(t)
	tenant := e.newTenant(t)
	builder := uuid.NewString()
	bctx := builderCtxOf(tenant, builder)

	d, err := e.dash.CreateDashboard(bctx, tenant, CreateDashboardInput{Name: "Home", Widgets: []WidgetConfig{{ID: "w1", Title: "Deals", Type: "kpi", Entity: "hk_deals", Metric: "count"}}})
	if err != nil {
		t.Fatal(err)
	}
	appID := uuid.NewString()
	if _, err := e.sdb.Exec(`INSERT INTO sys_apps (id, tenant_id, name, label) VALUES (?, ?, 'app', 'App')`, appID, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := e.sdb.Exec(`INSERT INTO sys_app_nav_items (id, tenant_id, app_id, item_type, ref_id) VALUES (?, ?, ?, 'dashboard', ?)`,
		uuid.NewString(), tenant, appID, d.ID); err != nil {
		t.Fatal(err)
	}
	e.deliver(t, tenant, builder)

	navRows := func() int {
		return e.count(t, `SELECT COUNT(*) FROM sys_app_nav_items WHERE tenant_id = ? AND ref_id = ?`, tenant, d.ID)
	}
	if err := e.dash.DeleteDashboard(bctx, tenant, d.ID); err != nil {
		t.Fatal(err)
	}
	if navRows() != 1 || e.count(t, `SELECT COUNT(*) FROM sys_dashboards WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, tenant, d.ID) != 1 {
		t.Fatal("a draft delete touched the live dashboard or its menu row")
	}
	if got, err := e.dash.GetDashboard(bctx, tenant, d.ID); err != nil || got.DraftState != "deleted" {
		t.Fatalf("builder view = %+v, %v", got, err)
	}
	if _, err := e.publishWithCurrentHash(bctx, tenant, false); err != nil {
		t.Fatal(err)
	}
	if navRows() != 0 {
		t.Fatal("publish left a menu row pointing at the deleted dashboard (dead tab)")
	}
	if e.count(t, `SELECT COUNT(*) FROM sys_dashboards WHERE tenant_id = ? AND id = ? AND deleted_at IS NOT NULL`, tenant, d.ID) != 1 {
		t.Fatal("publish did not delete the dashboard")
	}
}
