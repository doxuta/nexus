package services

// draft_app_store_test.go - specs/063-draft-publish T078/T080: the wrapped app store and the menu
// reference check running on the overlay.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

type daEnv struct {
	live   *fakeAppStore
	drafts *fakeDraftStore
	store  *DraftAppStore
	dashes *draftFakeDashRepo
	svc    *AppService
	appID  string
}

func newDAEnv(t *testing.T, delivered bool) *daEnv {
	t.Helper()
	live, drafts := newFakeAppStore(), newFakeDraftStore()
	gate := NewDraftGate(&fakeDelivered{delivered: delivered})
	dashes := newDraftFakeDashRepo()
	draftDash := NewDraftDashboardRepo(dashes, drafts, gate)
	ents := &fakeAppEntities{byTenant: map[string][]metadata.EntityDefinition{dwTenant: {{ID: "ent-1", TenantID: dwTenant, Name: "accounts", Label: "Khách"}}}}
	store := NewDraftAppStore(live, drafts, gate)
	svc := NewAppService(store, ents, draftDash, &fakeAppACL{allow: map[string]bool{"accounts": true}}, nil)
	id, err := live.Create(context.Background(), appmeta.CreateInput{TenantID: dwTenant, Name: "sales", Label: "Sales"})
	if err != nil {
		t.Fatal(err)
	}
	return &daEnv{live: live, drafts: drafts, store: store, dashes: dashes, svc: svc, appID: id}
}

func TestDraftAppStore_UpdateAndNavAreOneDraftDocument(t *testing.T) {
	e := newDAEnv(t, true)
	ctx := builderCtx()
	if err := e.store.Update(ctx, dwTenant, e.appID, appmeta.UpdateInput{Label: "Bán hàng", SortOrder: 2}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.ReplaceNav(ctx, dwTenant, e.appID, []appmeta.NavItemInput{{ItemType: appmeta.ItemTypeEntity, RefID: "ent-1", Label: "Khách"}}); err != nil {
		t.Fatal(err)
	}
	if len(e.drafts.rows) != 1 {
		t.Fatalf("draft rows = %d, want ONE app document for properties + menu", len(e.drafts.rows))
	}
	var doc appDoc
	d, _ := e.drafts.GetByKey(ctx, dwTenant, DocTypeApp, e.appID)
	if err := json.Unmarshal(d.DraftJSON, &doc); err != nil || doc.Label != "Bán hàng" || len(doc.Nav) != 1 || doc.Name != "sales" {
		t.Fatalf("draft doc = %+v (%v); want new label AND the menu", doc, err)
	}
	if live, _ := e.live.GetByID(ctx, dwTenant, e.appID); live.Label != "Sales" {
		t.Fatalf("live label = %q: a draft save wrote the customer's app", live.Label)
	}
	if nav, _ := e.live.ListNav(ctx, dwTenant, e.appID); len(nav) != 0 {
		t.Fatal("live menu changed by a draft save")
	}

	// The builder (ReadDraft) sees the overlay; the customer's staff and a legacy admin see live.
	got, err := e.store.GetByID(ctx, dwTenant, e.appID)
	if err != nil || got.Label != "Bán hàng" || got.DraftState != "edited" || got.NavCount != 1 {
		t.Fatalf("overlay GetByID = %+v, %v", got, err)
	}
	if nav, _ := e.store.ListNav(ctx, dwTenant, e.appID); len(nav) != 1 || nav[0].RefID != "ent-1" {
		t.Fatalf("overlay ListNav = %+v", nav)
	}
	byName, _ := e.store.GetByName(ctx, dwTenant, "sales")
	if byName == nil || byName.Label != "Bán hàng" {
		t.Fatalf("overlay GetByName = %+v", byName)
	}
	for _, c := range []context.Context{legacyAdminCtx(), context.Background()} {
		got, err := e.store.GetByID(c, dwTenant, e.appID)
		if err != nil || got.Label != "Sales" || got.DraftState != "" {
			t.Fatalf("non-builder sees %+v, %v; want live", got, err)
		}
		if nav, _ := e.store.ListNav(c, dwTenant, e.appID); len(nav) != 0 {
			t.Fatalf("non-builder sees draft menu %+v", nav)
		}
	}
}

func TestDraftAppStore_CreateAndDeleteApplyAtOnce(t *testing.T) {
	e := newDAEnv(t, true)
	ctx := builderCtx()
	if err := e.store.Update(ctx, dwTenant, e.appID, appmeta.UpdateInput{Label: "X"}); err != nil {
		t.Fatal(err)
	}
	id, err := e.store.Create(ctx, appmeta.CreateInput{TenantID: dwTenant, Name: "support", Label: "Support"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.live.GetByID(ctx, dwTenant, id); err != nil {
		t.Fatalf("created app not live at once: %v", err)
	}
	if err := e.store.SoftDelete(ctx, dwTenant, e.appID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.live.GetByID(ctx, dwTenant, e.appID); !errors.Is(err, appmeta.ErrAppNotFound) {
		t.Fatalf("deleted app still live: %v", err)
	}
	if d, _ := e.drafts.GetByKey(ctx, dwTenant, DocTypeApp, e.appID); d != nil {
		t.Fatal("the draft of a deleted app survived: a forced publish would resurrect it")
	}
}

func TestDraftAppStore_NotDeliveredAndLegacyAdminWriteLive(t *testing.T) {
	for name, tc := range map[string]struct {
		delivered bool
		ctx       context.Context
	}{"not delivered": {false, builderCtx()}, "legacy admin": {true, legacyAdminCtx()}} {
		t.Run(name, func(t *testing.T) {
			e := newDAEnv(t, tc.delivered)
			if err := e.store.Update(tc.ctx, dwTenant, e.appID, appmeta.UpdateInput{Label: "Live now"}); err != nil {
				t.Fatal(err)
			}
			if live, _ := e.live.GetByID(tc.ctx, dwTenant, e.appID); live.Label != "Live now" {
				t.Fatalf("label = %q, want a direct write", live.Label)
			}
			if len(e.drafts.rows) != 0 {
				t.Fatalf("drafts = %d, want none", len(e.drafts.rows))
			}
		})
	}
	// Delivered, no Caller at all: fail closed, never a silent write to the customer's app.
	e := newDAEnv(t, true)
	if err := e.store.Update(context.Background(), dwTenant, e.appID, appmeta.UpdateInput{Label: "x"}); !errors.Is(err, metaversion.ErrNoCaller) {
		t.Fatalf("no-caller write err = %v, want ErrNoCaller", err)
	}
	if err := e.store.Update(builderCtx(), dwTenant, "ghost", appmeta.UpdateInput{Label: "x"}); !errors.Is(err, appmeta.ErrAppNotFound) {
		t.Fatalf("ghost app err = %v, want ErrAppNotFound", err)
	}
}

func TestAppService_NavRefCheckRunsOnTheOverlay(t *testing.T) {
	e := newDAEnv(t, true)
	ctx := builderCtx()
	// A dashboard that only exists in the draft can join the menu (before S7-4: APP_NAV_REF_INVALID).
	newDash, err := NewDraftDashboardRepo(e.dashes, e.drafts, NewDraftGate(&fakeDelivered{delivered: true})).Create(ctx,
		dashboard.CreateInput{TenantID: dwTenant, Name: "Mới", Widgets: []byte(`[]`), CreatedBy: "builder"})
	if err != nil {
		t.Fatal(err)
	}
	in := []AppNavItemInput{{ItemType: "dashboard", RefID: newDash}}
	if _, err := e.svc.ReplaceNav(ctx, dwTenant, e.appID, in); err != nil {
		t.Fatalf("menu with a draft-only dashboard: %v", err)
	}
	// ... and the preview serves it, while staff (no overlay) see no such tab.
	if _, items, err := e.svc.GetNav(ctx, RecordScope{IsAdmin: true}, dwTenant, "sales"); err != nil || len(items) != 1 || items[0].RefID != newDash {
		t.Fatalf("preview nav = %+v, %v", items, err)
	}
	if _, items, err := e.svc.GetNav(legacyAdminCtx(), RecordScope{IsAdmin: true}, dwTenant, "sales"); err != nil || len(items) != 0 {
		t.Fatalf("customer nav = %+v, %v; want the live (empty) menu", items, err)
	}

	// A live dashboard marked deleted in the draft is no longer a valid menu target, and disappears
	// from the preview menu (the customer keeps it until Publish).
	liveID, _ := e.dashes.Create(context.Background(), dashboard.CreateInput{TenantID: dwTenant, Name: "Live", Widgets: []byte(`[]`)})
	if _, err := e.svc.ReplaceNav(ctx, dwTenant, e.appID, []AppNavItemInput{{ItemType: "dashboard", RefID: liveID}}); err != nil {
		t.Fatal(err)
	}
	if err := NewDraftDashboardRepo(e.dashes, e.drafts, NewDraftGate(&fakeDelivered{delivered: true})).Delete(ctx, dwTenant, liveID); err != nil {
		t.Fatal(err)
	}
	if _, items, _ := e.svc.GetNav(ctx, RecordScope{IsAdmin: true}, dwTenant, "sales"); len(items) != 0 {
		t.Fatalf("preview nav still shows the dashboard deleted in the draft: %+v", items)
	}
	_, err = e.svc.ReplaceNav(ctx, dwTenant, e.appID, []AppNavItemInput{{ItemType: "dashboard", RefID: liveID}})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "APP_NAV_REF_INVALID" {
		t.Fatalf("menu pointing at a draft-deleted dashboard err = %v, want APP_NAV_REF_INVALID", err)
	}
}

func TestAppDoc_WithoutDashboards(t *testing.T) {
	d := appDoc{Name: "a", Label: "A", HomeDashboardID: "d1", Nav: []appDocNav{{"dashboard", "d1", ""}, {"entity", "e1", "E"}, {"dashboard", "d2", ""}}}
	got, changed := d.withoutDashboards(map[string]bool{"d1": true})
	if !changed || got.HomeDashboardID != "" || len(got.Nav) != 2 || got.Nav[0].RefID != "e1" || got.Nav[1].RefID != "d2" {
		t.Fatalf("got %+v changed=%v", got, changed)
	}
	if _, changed := d.withoutDashboards(map[string]bool{"zzz": true}); changed {
		t.Fatal("unrelated dashboard reported as a change")
	}
	a, _ := appDoc{Name: "a"}.marshal()
	if string(a) == "" || !json.Valid(a) {
		t.Fatal("marshal")
	}
	var back appDoc
	_ = json.Unmarshal(a, &back)
	if back.Nav == nil {
		t.Fatal("an empty menu must marshal as [] (one canonical form)")
	}
}
