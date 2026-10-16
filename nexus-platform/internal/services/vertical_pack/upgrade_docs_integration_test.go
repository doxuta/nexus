//go:build integration

// DAILY-QUEUE #430 / specs/069 L2b against real MySQL and the real report, dashboard and layout
// services: the builder hand-edits a screen, the pack ships a new report / dashboard / screen
// version, and the upgrade (1) keeps the hand-edited screen by default, (2) brings the untouched
// report and dashboard to the new version and creates the new dashboard, (3) soft-deletes a report
// the pack dropped only when the builder says so, (4) "Luôn giữ" flags a screen so later plans
// default to keep, and taking the pack's version afterwards clears the flag.
//
//	TEST_DATABASE_URL=... bash scripts/run-integration.sh ./internal/services/vertical_pack/ -run UpgradeDocs
package vertical_pack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/report"
	"nexus/platform/internal/services"
)

// ticketingNext returns the builtin Ticketing pack as another version, deep-copied so a mutation
// never leaks into the shared builtin.
func ticketingNext(t *testing.T, version string, mutate func(*Pack)) *Pack {
	t.Helper()
	base, ok := BuiltinPack("ticketing")
	if !ok {
		t.Fatal("builtin ticketing pack not found")
	}
	b, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	var p Pack
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	p.Manifest.Version = version
	if mutate != nil {
		mutate(&p)
	}
	return &p
}

func listLayoutJSON(t *testing.T, svc *Service, entity, view string) string {
	t.Helper()
	ls, err := svc.layoutRd.GetByEntity(context.Background(), vpTenantA, entity)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ls {
		if l.ViewType == view {
			return l.LayoutJSON
		}
	}
	t.Fatalf("no %s:%s layout", entity, view)
	return ""
}

func TestUpgradeDocs_ReportDashboardLayoutAndKeepAlways(t *testing.T) {
	gdb, sdb := vpSetup(t)
	st := vpBuildSLA(gdb, sdb)
	defer st.drain(t)
	slaSeedInstaller(t, gdb)
	store := &protStore{upgStore: newUpgStore(), prot: map[string]bool{}}
	st.pack.WithItemStore(store)
	st.pack.reportWr = services.NewReportService(report.NewSQLRepository(sdb), st.records).WithEntityChecker(metadata.NewRepository(gdb))
	ctx := context.Background()
	if _, err := services.SeedDefaultBusinessCalendar(ctx, gdb, vpTenantA); err != nil {
		t.Fatalf("seed calendar: %v", err)
	}
	v1 := ticketingNext(t, "1.8.0", nil)
	if _, err := st.pack.Install(ctx, vpTenantA, vpSeedUserID, v1); err != nil {
		t.Fatalf("install: %v", err)
	}
	const (
		layoutKey  = "tk_tickets:list"
		statusRep  = "Ticket theo trạng thái"
		dropRep    = "Danh sách ticket đang mở"
		overview   = "Tổng quan Ticketing"
		newDash    = "Tổng quan mới"
		handEdited = `"channel"`
	)

	// The builder hand-edits the ticket list screen: the "channel" column goes away.
	var pl PackLayout
	for _, l := range v1.Layouts {
		if l.EntityName == "tk_tickets" && l.ViewType == "list" {
			pl = l
		}
	}
	cols := pl.LayoutJSON["columns"].([]any)
	var kept []any
	for _, c := range cols {
		if c.(map[string]any)["field"] != "channel" {
			kept = append(kept, c)
		}
	}
	custom := map[string]any{"version": 1, "columns": kept, "defaultSort": pl.LayoutJSON["defaultSort"]}
	if _, err := st.pack.layoutWr.SaveLayout(ctx, vpTenantA, services.SaveLayoutInput{EntityName: "tk_tickets", ViewType: "list", LayoutJSON: custom}); err != nil {
		t.Fatalf("hand edit: %v", err)
	}

	// 1.9.0: a report changes, another is dropped, a dashboard changes, a new one is added.
	v2 := ticketingNext(t, "1.9.0", func(p *Pack) {
		var reps []PackReport
		for _, r := range p.Reports {
			switch r.Name {
			case dropRep:
				continue
			case statusRep:
				r.Config.GroupBy = "priority"
			}
			reps = append(reps, r)
		}
		p.Reports = reps
		p.Dashboards[0].Widgets[0].Title = "Tất cả phiếu"
		p.Dashboards = append(p.Dashboards, PackDashboard{Name: newDash, Widgets: []PackWidget{{ID: "w1", Title: "Tổng", Type: "kpi", Entity: "tk_tickets", Metric: "count"}}})
	})
	plan, err := st.pack.PlanUpgrade(ctx, vpTenantA, "ticketing", v2, "upload")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	want := map[string]struct{ status, def string }{
		ItemLayout + "/" + layoutKey:   {StatusKeptCustom, ActionKeep},
		ItemReport + "/" + statusRep:   {StatusUpdateSafe, ActionApply},
		ItemReport + "/" + dropRep:     {StatusRemovedInPack, ActionKeep},
		ItemDashboard + "/" + overview: {StatusUpdateSafe, ActionApply},
		ItemDashboard + "/" + newDash:  {StatusAdded, ActionApply},
	}
	for _, it := range plan.Items {
		w, ok := want[it.ItemType+"/"+it.ItemKey]
		if !ok {
			continue
		}
		if it.Status != w.status || it.DefaultAction != w.def {
			t.Errorf("%s/%s = %s default=%s; want %s/%s", it.ItemType, it.ItemKey, it.Status, it.DefaultAction, w.status, w.def)
		}
		delete(want, it.ItemType+"/"+it.ItemKey)
		switch it.ItemKey {
		case layoutKey:
			if !contains(it.Actions, ActionRestore) || !contains(it.Actions, ActionKeepAlways) || contains(it.Actions, ActionDelete) {
				t.Errorf("layout actions = %v; want restore + keep_always, never delete", it.Actions)
			}
		case dropRep:
			if !contains(it.Actions, ActionDelete) {
				t.Errorf("dropped report actions = %v; want delete offered", it.Actions)
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("plan is missing items: %v", want)
	}

	run, err := st.pack.ApplyUpgrade(ctx, vpTenantA, vpSeedUserID, "ticketing", v2, "upload", plan.PlanHash, nil)
	if err != nil {
		t.Fatalf("apply defaults: %v", err)
	}
	if run.Status != RunDone || run.Counts[OutcomeApplied] != 3 {
		t.Fatalf("run = %s counts=%v results=%+v; want done with 3 applied", run.Status, run.Counts, run.Results)
	}
	if got := listLayoutJSON(t, st.pack, "tk_tickets", "list"); strings.Contains(got, handEdited) {
		t.Errorf("the hand-edited screen was overwritten by a default apply: %s", got)
	}
	reps, err := st.pack.reportWr.ListReports(ctx, vpTenantA)
	if err != nil {
		t.Fatal(err)
	}
	var sawStatus, sawDrop bool
	for _, r := range reps {
		if r.Name == statusRep {
			sawStatus = strings.Contains(string(r.Config), "priority")
		}
		if r.Name == dropRep {
			sawDrop = true
		}
	}
	if !sawStatus || !sawDrop {
		t.Errorf("report updated=%v, dropped report still there=%v; want true/true (delete is never the default)", sawStatus, sawDrop)
	}
	dashes, _ := st.pack.dashboardWr.ListDashboards(ctx, vpTenantA)
	names := map[string]int{}
	for _, d := range dashes {
		names[d.Name]++
	}
	if names[newDash] != 1 || names[overview] != 1 {
		t.Errorf("dashboards = %v; want exactly one of each", names)
	}

	// Now the builder confirms the dropped report goes: soft delete, row kept.
	plan2, err := st.pack.PlanUpgrade(ctx, vpTenantA, "ticketing", v2, "upload")
	if err != nil {
		t.Fatal(err)
	}
	run, err = st.pack.ApplyUpgrade(ctx, vpTenantA, vpSeedUserID, "ticketing", v2, "upload", plan2.PlanHash,
		[]UpgradeDecision{{ItemType: ItemReport, ItemKey: dropRep, Action: ActionDelete}})
	if err != nil || run.Counts[OutcomeDeleted] != 1 {
		t.Fatalf("delete run: err=%v run=%+v", err, run)
	}
	var soft, live int
	gdb.Raw("SELECT COUNT(*) FROM sys_reports WHERE tenant_id = ? AND name = ? AND deleted_at IS NOT NULL", vpTenantA, dropRep).Row().Scan(&soft)
	gdb.Raw("SELECT COUNT(*) FROM sys_reports WHERE tenant_id = ? AND name = ? AND deleted_at IS NULL", vpTenantA, dropRep).Row().Scan(&live)
	if soft != 1 || live != 0 {
		t.Errorf("dropped report rows soft=%d live=%d; want a soft delete (1/0)", soft, live)
	}

	// 2.0.0 also changes that very screen: both sides moved → conflict → "Luôn giữ".
	v3 := ticketingNext(t, "2.0.0", func(p *Pack) {
		for i := range p.Layouts {
			if p.Layouts[i].EntityName == "tk_tickets" && p.Layouts[i].ViewType == "list" {
				cs := p.Layouts[i].LayoutJSON["columns"].([]any)
				p.Layouts[i].LayoutJSON["columns"] = cs[:len(cs)-1]
			}
		}
		p.Reports = v2.Reports
		p.Dashboards = v2.Dashboards
	})
	plan3, err := st.pack.PlanUpgrade(ctx, vpTenantA, "ticketing", v3, "upload")
	if err != nil {
		t.Fatal(err)
	}
	lay := find(t, plan3, ItemLayout, layoutKey)
	if lay.Status != StatusConflict || !contains(lay.Actions, ActionKeepAlways) || lay.DefaultAction != ActionKeep {
		t.Fatalf("layout = %s default=%s actions=%v; want conflict, keep by default, keep_always offered", lay.Status, lay.DefaultAction, lay.Actions)
	}
	run, err = st.pack.ApplyUpgrade(ctx, vpTenantA, vpSeedUserID, "ticketing", v3, "upload", plan3.PlanHash,
		[]UpgradeDecision{{ItemType: ItemLayout, ItemKey: layoutKey, Action: ActionKeepAlways}})
	if err != nil || run.Counts[OutcomeProtected] != 1 {
		t.Fatalf("keep_always run: err=%v run=%+v", err, run)
	}
	if got := listLayoutJSON(t, st.pack, "tk_tickets", "list"); strings.Contains(got, handEdited) {
		t.Errorf("keep_always touched the screen: %s", got)
	}
	plan4, err := st.pack.PlanUpgrade(ctx, vpTenantA, "ticketing", v3, "upload")
	if err != nil {
		t.Fatal(err)
	}
	lay = find(t, plan4, ItemLayout, layoutKey)
	if lay.Status != StatusProtected || lay.DefaultAction != ActionKeep || !contains(lay.Actions, ActionApply) {
		t.Fatalf("after keep_always: %s default=%s actions=%v; want protected / keep / apply offered", lay.Status, lay.DefaultAction, lay.Actions)
	}
	rep, err := st.pack.DiffInstalled(ctx, vpTenantA, "ticketing")
	if err != nil {
		t.Fatal(err)
	}
	var diffSeen bool
	for _, it := range rep.Items {
		if it.ItemType == ItemLayout && it.ItemKey == layoutKey {
			diffSeen = true
			if !it.Protected || it.Status == "protected" {
				t.Errorf("diff layout: protected=%v status=%s; want the flag set and the status untouched", it.Protected, it.Status)
			}
		}
	}
	if !diffSeen {
		t.Error("diff has no tk_tickets list layout")
	}

	// Taking the pack's version on purpose overwrites the screen and clears the flag.
	run, err = st.pack.ApplyUpgrade(ctx, vpTenantA, vpSeedUserID, "ticketing", v3, "upload", plan4.PlanHash,
		[]UpgradeDecision{{ItemType: ItemLayout, ItemKey: layoutKey, Action: ActionApply}})
	if err != nil || run.Counts[OutcomeApplied] != 1 {
		t.Fatalf("apply protected: err=%v run=%+v", err, run)
	}
	if store.prot["layout/"+layoutKey] {
		t.Error("the protected flag survived an explicit apply")
	}
}

// Dry-run of an install for a pack that already has a baseline nudges toward "Nâng cấp" (soft).
func TestUpgradeDocs_DryRunSuggestsUpgradeOnlyWhenABaselineExists(t *testing.T) {
	gdb, sdb := vpSetup(t)
	st := vpBuildSLA(gdb, sdb)
	defer st.drain(t)
	slaSeedInstaller(t, gdb)
	store := newUpgStore()
	st.pack.WithItemStore(store)
	ctx := context.Background()
	v1 := ticketingNext(t, "1.8.0", nil)
	kinds := func() []string {
		plan, err := st.pack.DryRunInstall(ctx, vpTenantA, v1)
		if err != nil {
			t.Fatalf("dry-run: %v", err)
		}
		var ks []string
		for _, c := range plan.Conflicts {
			ks = append(ks, c.Kind)
		}
		return ks
	}
	if got := kinds(); contains(got, "upgrade_available") {
		t.Fatalf("fresh tenant: conflicts %v", got)
	}
	if _, err := st.pack.Install(ctx, vpTenantA, vpSeedUserID, v1); err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := kinds(); !contains(got, "upgrade_available") {
		t.Fatalf("installed tenant: conflicts %v; want upgrade_available", got)
	}
}
