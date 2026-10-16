package vertical_pack

// Unit tests for the report / dashboard / layout upgrade adapters (specs/069 L2b): each writes through
// the admin service, updates in place by live id, recreates only when the live row is gone, soft
// deletes only reports and dashboards, and a layout is never deleted.

import (
	"context"
	"encoding/json"
	"testing"

	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/report"
	"nexus/platform/internal/services"
)

type fakeRep struct {
	created []services.CreateReportInput
	updated map[string]services.UpdateReportInput
	deleted []string
}

func (f *fakeRep) ListReports(context.Context, string) ([]report.Record, error) { return nil, nil }
func (f *fakeRep) CreateReport(_ context.Context, _ string, in services.CreateReportInput) (*report.Record, error) {
	f.created = append(f.created, in)
	return &report.Record{ID: "rep-new"}, nil
}
func (f *fakeRep) UpdateReport(_ context.Context, _, id string, in services.UpdateReportInput) (*report.Record, error) {
	if f.updated == nil {
		f.updated = map[string]services.UpdateReportInput{}
	}
	f.updated[id] = in
	return &report.Record{ID: id}, nil
}
func (f *fakeRep) DeleteReport(_ context.Context, _, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

type fakeDash struct {
	created []services.CreateDashboardInput
	updated map[string]services.UpdateDashboardInput
	deleted []string
}

func (f *fakeDash) ListDashboards(context.Context, string) ([]dashboard.Record, error) {
	return nil, nil
}
func (f *fakeDash) CreateDashboard(_ context.Context, _ string, in services.CreateDashboardInput) (*dashboard.Record, error) {
	f.created = append(f.created, in)
	return &dashboard.Record{ID: "dash-new"}, nil
}
func (f *fakeDash) UpdateDashboard(_ context.Context, _, id string, in services.UpdateDashboardInput) (*dashboard.Record, error) {
	if f.updated == nil {
		f.updated = map[string]services.UpdateDashboardInput{}
	}
	f.updated[id] = in
	return &dashboard.Record{ID: id}, nil
}
func (f *fakeDash) DeleteDashboard(_ context.Context, _, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

type fakeLay struct{ saved []services.SaveLayoutInput }

func (f *fakeLay) SaveLayout(_ context.Context, _ string, in services.SaveLayoutInput) (*metadata.LayoutDefinition, error) {
	f.saved = append(f.saved, in)
	return &metadata.LayoutDefinition{ID: "lay-1"}, nil
}

func newDocsSvc(t *testing.T) (*Service, *upgStore, *fakeRep, *fakeDash, *fakeLay) {
	t.Helper()
	st := newUpgStore()
	rep, dash, lay := &fakeRep{}, &fakeDash{}, &fakeLay{}
	svc := NewService(Deps{EntityReader: &fakeMintMeta{}, LayoutReader: fakeApLayouts{}, RoleReader: fakeApRoleReader{},
		ReportWriter: rep, DashboardWriter: dash, LayoutWriter: lay}).WithItemStore(st)
	return svc, st, rep, dash, lay
}

func docRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDocsAdapters_ReadyOnlyWhenTheServiceIsWired(t *testing.T) {
	svc, _, _, _, _ := newDocsSvc(t)
	for _, typ := range []string{ItemReport, ItemDashboard, ItemLayout} {
		if !svc.adapterReady(typ) {
			t.Errorf("%s: adapter should be ready", typ)
		}
	}
	bare := NewService(Deps{EntityReader: &fakeMintMeta{}, LayoutReader: fakeApLayouts{}, RoleReader: fakeApRoleReader{}})
	for _, typ := range []string{ItemReport, ItemDashboard, ItemLayout} {
		if bare.adapterReady(typ) {
			t.Errorf("%s: adapter must not be ready without a writer", typ)
		}
	}
	if upgradeAdapters[ItemLayout].remove != nil {
		t.Error("a layout must never be deletable by an upgrade")
	}
}

func TestReportAdapter_UpdatesInPlaceRecreatesAndDeletes(t *testing.T) {
	svc, st, rep, _, _ := newDocsSvc(t)
	pr := PackReport{Name: "R", EntityName: "e", Type: "summary", Config: PackReportConfig{Metric: "count", GroupBy: "status"}}
	env := branchEnv(svc, map[string]json.RawMessage{ItemReport + "/R": docRaw(t, pr)})

	res := UpgradeResult{}
	svc.applyItem(context.Background(), context.Background(), env, &UpgradeItem{ItemType: ItemReport, ItemKey: "R", Status: StatusUpdateSafe, LiveID: "rep-7"}, ActionApply, &res)
	if res.Outcome != OutcomeApplied || rep.updated["rep-7"].Name != "R" || rep.updated["rep-7"].Config.GroupBy != "status" || len(rep.created) != 0 {
		t.Fatalf("in-place update: res=%+v updated=%+v created=%d", res, rep.updated, len(rep.created))
	}
	if row := st.rows[upgTenant+"|p|report|R"]; row.PackVersion != "1.1.0" || row.LiveID != "rep-7" {
		t.Errorf("baseline = %+v", row)
	}

	res = UpgradeResult{}
	svc.applyItem(context.Background(), context.Background(), env, &UpgradeItem{ItemType: ItemReport, ItemKey: "R", Status: StatusAdded}, ActionApply, &res)
	if res.Outcome != OutcomeApplied || len(rep.created) != 1 || res.LiveID != "rep-new" {
		t.Fatalf("create: res=%+v created=%d", res, len(rep.created))
	}

	// A protected row whose live report is gone is recreated, not updated against an empty id.
	res = UpgradeResult{}
	svc.applyItem(context.Background(), context.Background(), env, &UpgradeItem{ItemType: ItemReport, ItemKey: "R", Status: StatusProtected}, ActionApply, &res)
	if res.Outcome != OutcomeApplied || len(rep.created) != 2 {
		t.Fatalf("protected + live gone should recreate: res=%+v created=%d", res, len(rep.created))
	}

	res = UpgradeResult{}
	svc.applyItem(context.Background(), context.Background(), env, &UpgradeItem{ItemType: ItemReport, ItemKey: "gone", Status: StatusRemovedInPack, LiveID: "rep-9"}, ActionDelete, &res)
	if res.Outcome != OutcomeDeleted || len(rep.deleted) != 1 || rep.deleted[0] != "rep-9" {
		t.Fatalf("delete: res=%+v deleted=%v", res, rep.deleted)
	}
	if len(st.deleted) != 1 || st.deleted[0] != [2]string{ItemReport, "gone"} {
		t.Errorf("baseline drop = %v", st.deleted)
	}

	res = UpgradeResult{}
	svc.applyItem(context.Background(), context.Background(), env, &UpgradeItem{ItemType: ItemReport, ItemKey: "R", Status: StatusUpdateSafe}, ActionApply, &res)
	if res.Outcome != OutcomeFailed || res.Code != "UPGRADE_LIVE_ID_UNKNOWN" {
		t.Errorf("update without a live id must fail cleanly: %+v", res)
	}
}

func TestDashboardAdapter_CarriesWidgetsAndSoftDeletes(t *testing.T) {
	svc, _, _, dash, _ := newDocsSvc(t)
	pd := PackDashboard{Name: "D", Description: "d", Widgets: []PackWidget{{ID: "w1", Title: "T", Type: "kpi", Entity: "e", Metric: "count", X: 1, Y: 2, W: 3, H: 4, Bucket: "month"}}}
	env := branchEnv(svc, map[string]json.RawMessage{ItemDashboard + "/D": docRaw(t, pd)})

	res := UpgradeResult{}
	svc.applyItem(context.Background(), context.Background(), env, &UpgradeItem{ItemType: ItemDashboard, ItemKey: "D", Status: StatusUpdateSafe, LiveID: "dash-3"}, ActionApply, &res)
	got := dash.updated["dash-3"]
	if res.Outcome != OutcomeApplied || len(got.Widgets) != 1 || got.Widgets[0].W != 3 || got.Widgets[0].Bucket != "month" || got.Description != "d" {
		t.Fatalf("update: res=%+v got=%+v", res, got)
	}
	res = UpgradeResult{}
	svc.applyItem(context.Background(), context.Background(), env, &UpgradeItem{ItemType: ItemDashboard, ItemKey: "D", Status: StatusAdded}, ActionApply, &res)
	if res.Outcome != OutcomeApplied || len(dash.created) != 1 || dash.created[0].ActorID != "u1" {
		t.Fatalf("create: res=%+v created=%+v", res, dash.created)
	}
	res = UpgradeResult{}
	svc.applyItem(context.Background(), context.Background(), env, &UpgradeItem{ItemType: ItemDashboard, ItemKey: "x", Status: StatusRemovedInPack, LiveID: "dash-8"}, ActionDelete, &res)
	if res.Outcome != OutcomeDeleted || len(dash.deleted) != 1 {
		t.Fatalf("delete: %+v", res)
	}
}

// Install and the adapter must build the SAME dashboard input from a pack (no drift between paths).
func TestToDashboardInput_KeepsEveryWidgetField(t *testing.T) {
	in := toDashboardInput(PackDashboard{Name: " D ", Description: " x ", Widgets: []PackWidget{
		{ID: "w", Title: "t", Type: "bar", Entity: "e", Metric: "sum", Field: "amt", GroupBy: "stage", Bucket: "week", X: 1, Y: 2, W: 3, H: 4},
	}}, "u9")
	w := in.Widgets[0]
	if in.Name != "D" || in.Description != "x" || in.ActorID != "u9" || w.Field != "amt" || w.GroupBy != "stage" || w.Bucket != "week" || w.X != 1 || w.Y != 2 || w.W != 3 || w.H != 4 {
		t.Fatalf("lost a field: %+v", in)
	}
}

func TestLayoutAdapter_SavesTheScreenAndNeverDeletes(t *testing.T) {
	svc, st, _, _, lay := newDocsSvc(t)
	pl := PackLayout{EntityName: "e", ViewType: "edit", LayoutJSON: map[string]any{"sections": []any{}}}
	env := branchEnv(svc, map[string]json.RawMessage{ItemLayout + "/e:edit": docRaw(t, pl)})
	res := UpgradeResult{}
	svc.applyItem(context.Background(), context.Background(), env, &UpgradeItem{ItemType: ItemLayout, ItemKey: "e:edit", Status: StatusUpdateSafe}, ActionApply, &res)
	if res.Outcome != OutcomeApplied || len(lay.saved) != 1 || lay.saved[0].EntityName != "e" || lay.saved[0].ViewType != "edit" {
		t.Fatalf("res=%+v saved=%+v", res, lay.saved)
	}
	if row := st.rows[upgTenant+"|p|layout|e:edit"]; row.PackVersion != "1.1.0" {
		t.Errorf("baseline = %+v", row)
	}
	res = UpgradeResult{}
	svc.applyItem(context.Background(), context.Background(), env, &UpgradeItem{ItemType: ItemLayout, ItemKey: "e:edit"}, ActionDelete, &res)
	if res.Outcome != OutcomeFailed || len(lay.saved) != 1 {
		t.Fatalf("a layout delete must be refused: %+v", res)
	}
}

// The plan offers "Khôi phục" for a customised screen but never pre-selects it: the hand-edited
// form survives an upgrade run with default choices (T034b).
func TestPlan_CustomisedLayoutDefaultsToKeepAndIsNeverDeletable(t *testing.T) {
	f := newFixture()
	f.in.ready = func(string) bool { return true }
	k := ItemLayout + "/e:edit"
	f.in.base[k] = StoredPackItem{Type: ItemLayout, Key: "e:edit", Content: json.RawMessage(`{"entityName":"e","viewType":"edit","layoutJson":{"v":1}}`)}
	f.in.theirs[k] = json.RawMessage(`{"entityName":"e","viewType":"edit","layoutJson":{"v":1}}`)
	f.in.live[k] = json.RawMessage(`{"entityName":"e","viewType":"edit","layoutJson":{"v":2}}`)
	it := find(t, f.plan(t), ItemLayout, "e:edit")
	if it.Status != StatusKeptCustom || it.DefaultAction != ActionKeep || !contains(it.Actions, ActionRestore) {
		t.Fatalf("customised layout: %+v", it)
	}
	// Pack drops the screen, tenant still has it unchanged: removed_in_pack offers no delete.
	delete(f.in.theirs, k)
	f.in.live[k] = f.in.base[k].Content
	it = find(t, f.plan(t), ItemLayout, "e:edit")
	if contains(it.Actions, ActionDelete) {
		t.Fatalf("a layout must never offer Xoá: %+v", it)
	}
}
