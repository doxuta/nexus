//go:build integration

package services

// draft_app_nav_integration_test.go - specs/063-draft-publish S7-4 (T077/T079) against a real MySQL:
// the REAL AppService over the draft-aware app store + dashboard repo, the real publish transaction.

import (
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/dashboard"
)

type anEnv struct {
	rdEnv
	apps *AppService
	raw  *appmeta.SQLRepository
	ddr  *DraftDashboardRepo
}

func anSetup(t *testing.T) anEnv {
	t.Helper()
	e := dpSetupWith(t, NewWorkflowDocAdapter(), NewReportDocAdapter(), NewDashboardDocAdapter(), NewAppDocAdapter())
	rawDash := dashboard.NewSQLRepository(e.sdb)
	ddr := NewDraftDashboardRepo(rawDash, e.drafts, e.gate)
	raw := appmeta.NewSQLRepository(e.sdb)
	acl := &fakeAppACL{allow: map[string]bool{"hk_deals": true}}
	return anEnv{rdEnv: rdEnv{dpEnv: e, dash: NewDashboardService(ddr, e.rec, nil)}, raw: raw, ddr: ddr,
		apps: NewAppService(NewDraftAppStore(raw, e.drafts, e.gate), e.meta, ddr, acl, nil)}
}

func (e anEnv) dealsEntityID(t *testing.T, tenant string) string {
	t.Helper()
	var id string
	if err := e.sdb.Get(&id, `SELECT id FROM sys_entities WHERE tenant_id = ? AND name = 'hk_deals' AND deleted_at IS NULL`, tenant); err != nil {
		t.Fatalf("entity id: %v", err)
	}
	return id
}

func (e anEnv) navRefs(t *testing.T, tenant, appID string) []string {
	t.Helper()
	var refs []string
	if err := e.sdb.Select(&refs, `SELECT ref_id FROM sys_app_nav_items WHERE tenant_id = ? AND app_id = ? ORDER BY sort_order`, tenant, appID); err != nil {
		t.Fatal(err)
	}
	return refs
}

func (e anEnv) appLabel(t *testing.T, tenant, id string) string {
	t.Helper()
	var l string
	if err := e.sdb.Get(&l, `SELECT label FROM sys_apps WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, tenant, id); err != nil {
		t.Fatal(err)
	}
	return l
}

// US5 #1/#2/#4: a draft-only dashboard joins the menu and the label changes in the draft; the preview
// shows both, the customer's side shows neither; Publish moves both in one tx and makes isDefault
// single; discarding brings the preview back to the live menu.
func TestDraftAppNav_PreviewThenPublish_Integration(t *testing.T) {
	e := anSetup(t)
	tenant := e.newTenant(t)
	builder := uuid.NewString()
	bctx, sctx := builderCtxOf(tenant, builder), legacyCtxOf(tenant)
	entID := e.dealsEntityID(t, tenant)

	app, err := e.apps.CreateApp(bctx, tenant, CreateAppInput{Name: "crm", Label: "CRM", IsDefault: true})
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.apps.CreateApp(bctx, tenant, CreateAppInput{Name: "support", Label: "Support"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.apps.ReplaceNav(bctx, tenant, app.ID, []AppNavItemInput{{ItemType: "entity", RefID: entID}}); err != nil {
		t.Fatal(err)
	}
	e.deliver(t, tenant, builder)

	nd, err := e.dash.CreateDashboard(bctx, tenant, CreateDashboardInput{Name: "Mới", Widgets: []WidgetConfig{{ID: "w1", Title: "Deals", Type: "kpi", Entity: "hk_deals", Metric: "count"}}})
	if err != nil || nd.DraftState != "new" {
		t.Fatalf("draft dashboard = %+v, %v", nd, err)
	}
	// The second app becomes default in the draft; the label of the first changes.
	if _, err := e.apps.UpdateApp(bctx, tenant, other.ID, UpdateAppInput{Label: "Support", IsDefault: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.apps.UpdateApp(bctx, tenant, app.ID, UpdateAppInput{Label: "CRM mới", IsDefault: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.apps.ReplaceNav(bctx, tenant, app.ID, []AppNavItemInput{{ItemType: "entity", RefID: entID}, {ItemType: "dashboard", RefID: nd.ID}}); err != nil {
		t.Fatalf("menu with a draft-only dashboard: %v", err)
	}
	if got := e.appLabel(t, tenant, app.ID); got != "CRM" || len(e.navRefs(t, tenant, app.ID)) != 1 {
		t.Fatalf("a draft save reached the live app: label %q", got)
	}

	adm := RecordScope{IsAdmin: true}
	a, items, err := e.apps.GetNav(bctx, adm, tenant, "crm")
	if err != nil || a.Label != "CRM mới" || len(items) != 2 || items[1].RefID != nd.ID {
		t.Fatalf("preview = %+v %+v, %v", a, items, err)
	}
	a, items, err = e.apps.GetNav(sctx, adm, tenant, "crm")
	if err != nil || a.Label != "CRM" || len(items) != 1 {
		t.Fatalf("customer side = %+v %+v, %v", a, items, err)
	}
	list, err := e.apps.ListApps(bctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defaults := 0
	for _, x := range list {
		if x.IsDefault {
			defaults++
		}
	}
	if defaults != 1 {
		t.Fatalf("preview shows %d default apps, want 1", defaults)
	}

	// US5 #4: dropping the drafts returns the preview to the running menu.
	changes, hash, err := e.pub.Changes(bctx, tenant)
	if err != nil || len(changes) != 3 {
		t.Fatalf("changes = %+v, %v (want 2 apps + 1 dashboard)", changes, err)
	}
	if err := e.pub.DiscardAll(bctx, tenant, hash); err != nil {
		t.Fatal(err)
	}
	if _, items, _ = e.apps.GetNav(bctx, adm, tenant, "crm"); len(items) != 1 {
		t.Fatalf("after discard preview menu = %+v", items)
	}

	// Again, then publish (US5 #2).
	nd, err = e.dash.CreateDashboard(bctx, tenant, CreateDashboardInput{Name: "Mới", Widgets: []WidgetConfig{{ID: "w1", Title: "Deals", Type: "kpi", Entity: "hk_deals", Metric: "count"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.apps.UpdateApp(bctx, tenant, other.ID, UpdateAppInput{Label: "Support", IsDefault: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.apps.UpdateApp(bctx, tenant, app.ID, UpdateAppInput{Label: "CRM mới", IsDefault: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.apps.ReplaceNav(bctx, tenant, app.ID, []AppNavItemInput{{ItemType: "entity", RefID: entID}, {ItemType: "dashboard", RefID: nd.ID}}); err != nil {
		t.Fatal(err)
	}
	res, err := e.publishWithCurrentHash(bctx, tenant, false)
	if err != nil || res.DocCount != 3 {
		t.Fatalf("Publish = %+v, %v %+v", res, err, err.(*ValidationError).Details)
	}
	if e.appLabel(t, tenant, app.ID) != "CRM mới" || len(e.navRefs(t, tenant, app.ID)) != 2 {
		t.Fatal("publish did not write the app + menu")
	}
	if n := e.count(t, `SELECT COUNT(*) FROM sys_apps WHERE tenant_id = ? AND is_default = 1 AND deleted_at IS NULL`, tenant); n != 1 {
		t.Fatalf("default apps after publish = %d, want 1 (ClearDefaultExec in the publish tx)", n)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM sys_apps WHERE tenant_id = ? AND id = ? AND is_default = 1`, tenant, other.ID); n != 1 {
		t.Fatal("the app that took the default in the draft is not the default after publish")
	}
	if _, items, err = e.apps.GetNav(sctx, adm, tenant, "crm"); err != nil || len(items) != 2 {
		t.Fatalf("customer side after publish = %+v, %v", items, err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM sys_meta_version_docs WHERE tenant_id = ? AND doc_type = 'app' AND op = 'update'`, tenant); n != 2 {
		t.Fatalf("version_docs app rows = %d, want 2", n)
	}
}

// US5 #3 + T079: a dashboard deleted by a publish leaves no menu item: for an app that has no draft the
// publish SYNTHESIZES the app change (recorded in version_docs with before/after); for an app whose draft
// menu still lists it, the draft is rewritten in place. Never a tab pointing at a missing dashboard.
func TestDraftAppNav_DashboardDeleteBecomesAppChange_Integration(t *testing.T) {
	e := anSetup(t)
	tenant := e.newTenant(t)
	builder := uuid.NewString()
	bctx := builderCtxOf(tenant, builder)

	d, err := e.dash.CreateDashboard(bctx, tenant, CreateDashboardInput{Name: "Home", Widgets: []WidgetConfig{{ID: "w1", Title: "Deals", Type: "kpi", Entity: "hk_deals", Metric: "count"}}})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := e.apps.CreateApp(bctx, tenant, CreateAppInput{Name: "plain", Label: "Plain"})
	if err != nil {
		t.Fatal(err)
	}
	edited, err := e.apps.CreateApp(bctx, tenant, CreateAppInput{Name: "edited", Label: "Edited"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{plain.ID, edited.ID} {
		if _, err := e.apps.ReplaceNav(bctx, tenant, id, []AppNavItemInput{{ItemType: "dashboard", RefID: d.ID}}); err != nil {
			t.Fatal(err)
		}
	}
	home := d.ID
	if _, err := e.apps.UpdateApp(bctx, tenant, plain.ID, UpdateAppInput{Label: "Plain home", HomeDashboardID: &home}); err != nil {
		t.Fatal(err)
	}
	e.deliver(t, tenant, builder)

	// `edited` gets a draft whose menu still lists the dashboard; then the dashboard is deleted in the draft.
	if _, err := e.apps.UpdateApp(bctx, tenant, edited.ID, UpdateAppInput{Label: "Edited v2"}); err != nil {
		t.Fatal(err)
	}
	if err := e.dash.DeleteDashboard(bctx, tenant, d.ID); err != nil {
		t.Fatal(err)
	}
	if len(e.navRefs(t, tenant, plain.ID)) != 1 {
		t.Fatal("a draft delete removed the live menu item before Publish")
	}
	res, err := e.publishWithCurrentHash(bctx, tenant, false)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.DocCount != 3 { // dashboard delete + edited (draft) + plain (synthesized)
		t.Fatalf("DocCount = %d, want 3 (the synthesized app change counts)", res.DocCount)
	}
	for _, id := range []string{plain.ID, edited.ID} {
		if refs := e.navRefs(t, tenant, id); len(refs) != 0 {
			t.Fatalf("app %s still has menu rows %v after its dashboard was deleted", id, refs)
		}
	}
	if e.appLabel(t, tenant, edited.ID) != "Edited v2" {
		t.Fatal("the app's own draft change was lost")
	}
	if n := e.count(t, `SELECT COUNT(*) FROM sys_apps WHERE tenant_id = ? AND id = ? AND options IS NOT NULL AND JSON_EXTRACT(options, '$.homeDashboardId') IS NOT NULL`, tenant, plain.ID); n != 0 {
		t.Fatal("home-page pointer to the deleted dashboard survived")
	}
	var before, after string
	if err := e.sdb.QueryRow(`SELECT CAST(before_json AS CHAR), CAST(after_json AS CHAR) FROM sys_meta_version_docs
		WHERE tenant_id = ? AND doc_type = 'app' AND op = 'update' AND doc_key = ?`, tenant, plain.ID).Scan(&before, &after); err != nil {
		t.Fatalf("no version_docs row for the synthesized app change: %v", err)
	}
	if before == "" || after == "" || before == after {
		t.Fatalf("synthesized change must carry a real before/after: %q / %q", before, after)
	}
}

// T079 (table half): a table is deleted live (S7-9 has not drafted that yet), so an app draft can still
// list it. Publish prunes the dead item from the draft instead of failing PUBLISH_INVALID.
func TestDraftAppNav_DeadEntityItemPrunedOnPublish_Integration(t *testing.T) {
	e := anSetup(t)
	tenant := e.newTenant(t)
	builder := uuid.NewString()
	bctx := builderCtxOf(tenant, builder)

	app, err := e.apps.CreateApp(bctx, tenant, CreateAppInput{Name: "sales", Label: "Sales"})
	if err != nil {
		t.Fatal(err)
	}
	ent := e.dealsEntityID(t, tenant)
	if _, err := e.apps.ReplaceNav(bctx, tenant, app.ID, []AppNavItemInput{{ItemType: "entity", RefID: ent}}); err != nil {
		t.Fatal(err)
	}
	e.deliver(t, tenant, builder)
	if _, err := e.apps.UpdateApp(bctx, tenant, app.ID, UpdateAppInput{Label: "Sales v2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.sdb.Exec(`UPDATE sys_entities SET deleted_at = NOW() WHERE tenant_id = ? AND id = ?`, tenant, ent); err != nil {
		t.Fatal(err)
	}
	if _, err := e.publishWithCurrentHash(bctx, tenant, false); err != nil {
		t.Fatalf("Publish must not be blocked by a menu item whose table is gone: %v", err)
	}
	if refs := e.navRefs(t, tenant, app.ID); len(refs) != 0 {
		t.Fatalf("dead entity item survived: %v", refs)
	}
	if e.appLabel(t, tenant, app.ID) != "Sales v2" {
		t.Fatal("the app's own draft change was lost")
	}
}
