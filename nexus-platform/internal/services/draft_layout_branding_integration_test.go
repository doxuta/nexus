//go:build integration

package services

// draft_layout_branding_integration_test.go — specs/063-draft-publish S7-3 (T066 + the layout/branding
// halves of T052/T054) against a real MySQL: the REAL layout and branding services over the
// draft-aware repos and the real publish transaction.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metadata"
)

type lbEnv struct {
	dpEnv
	layout   *LayoutService
	branding *TenantBrandingService
	fields   *FieldService
}

func lbSetup(t *testing.T) lbEnv {
	t.Helper()
	e := dpSetupWith(t, NewLayoutDocAdapter(), NewWorkflowDocAdapter(), NewBrandingDocAdapter())
	rawLayout := metadata.NewLayoutRepository(e.gdb)
	return lbEnv{dpEnv: e,
		layout:   NewLayoutService(e.meta, NewDraftLayoutRepo(rawLayout, e.drafts, e.gate), nil).WithAuditor(e.audit, e.gate),
		branding: NewTenantBrandingService(NewDraftBrandingStore(auth.NewUserRepository(e.gdb), e.drafts, e.gate)),
		fields:   NewFieldService(e.dyn, e.meta)}
}

func editLayout(fields ...any) map[string]any {
	return map[string]any{"version": 1, "sections": []any{map[string]any{"title": "Chính", "columns": 2, "fields": fields}}}
}

// sectionFields lists the field names of the section called title ("" ⇒ not present).
func sectionFields(view *LayoutViewResult, title string) []string {
	var out []string
	secs, _ := view.LayoutData["sections"].([]any)
	for _, s := range secs {
		sec, _ := s.(map[string]any)
		if sec["title"] != title {
			continue
		}
		cells, _ := sec["fields"].([]any)
		for _, c := range cells {
			if name, ok := c.(string); ok {
				out = append(out, name)
			}
		}
	}
	return out
}

func hasName(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// T066: a form saved to the draft, a field born AFTER that save, then Publish. The customer's form
// must show the draft AND the later field in "Mới thêm" (the cutoff is the draft-save instant, not
// the publish instant), while fields that already existed when the builder saved and were left off
// the form on purpose stay off.
func TestDraftLayout_MoiThemFollowsTheDraftSaveInstant_Integration(t *testing.T) {
	e := lbSetup(t)
	tenant := e.newTenant(t) // hk_deals: name, code, stage, close_date, docs
	builder := uuid.NewString()
	bctx, sctx := builderCtxOf(tenant, builder), legacyCtxOf(tenant)
	e.deliver(t, tenant, builder)

	time.Sleep(1200 * time.Millisecond) // layout/field timestamps have second precision
	saved, err := e.layout.SaveLayout(bctx, tenant, SaveLayoutInput{EntityName: "hk_deals", ViewType: LayoutViewEdit, LayoutJSON: editLayout("name", "code")})
	if err != nil {
		t.Fatalf("draft save: %v", err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM sys_layouts WHERE tenant_id = ?`, tenant); n != 0 {
		t.Fatalf("the draft reached sys_layouts (%d rows)", n)
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := e.fields.AddField(context.Background(), tenant, AddFieldInput{EntityName: "hk_deals", Field: FieldInput{Name: "later_note", Label: "Later", FieldType: "text"}}); err != nil {
		t.Fatalf("AddField: %v", err)
	}

	time.Sleep(1200 * time.Millisecond) // the publish instant must be strictly later than the field's birth

	// Before Publish: the customer sees the generated default, the builder sees the draft.
	got, err := e.layout.GetLayout(sctx, tenant, "hk_deals", LayoutViewEdit)
	if err != nil || !got.Views[LayoutViewEdit].IsDefault {
		t.Fatalf("customer before publish = %+v, %v; want the default screen", got, err)
	}
	got, err = e.layout.GetLayout(bctx, tenant, "hk_deals", LayoutViewEdit)
	if err != nil || got.Views[LayoutViewEdit].IsDefault || got.Views[LayoutViewEdit].DraftState != "new" {
		t.Fatalf("builder overlay = %+v, %v; want the draft screen marked new", got.Views[LayoutViewEdit], err)
	}

	if _, err := e.publishWithCurrentHash(bctx, tenant, false); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	got, err = e.layout.GetLayout(sctx, tenant, "hk_deals", LayoutViewEdit)
	if err != nil {
		t.Fatal(err)
	}
	v := got.Views[LayoutViewEdit]
	if v.IsDefault || v.DraftState != "" {
		t.Fatalf("customer after publish = %+v; want the published custom screen with no draft state", v)
	}
	if v.ID != saved.ID {
		t.Errorf("published screen id = %q, want the id the builder held while it was a draft (%q)", v.ID, saved.ID)
	}
	if f := sectionFields(v, "Chính"); len(f) != 2 || f[0] != "name" || f[1] != "code" {
		t.Errorf("published section = %v, want the draft [name code]", f)
	}
	fresh := sectionFields(v, "Mới thêm")
	if !hasName(fresh, "later_note") {
		t.Errorf(`"Mới thêm" = %v, want the field created after the draft save (cutoff must be the save instant, not the publish)`, fresh)
	}
	for _, left := range []string{"stage", "close_date", "docs"} {
		if hasName(fresh, left) {
			t.Errorf(`"Mới thêm" re-added %q, which the builder left off the form on purpose`, left)
		}
	}
}

// "Về mặc định" on a published screen is a draft: the customer keeps the custom screen until Publish,
// then gets the default and the row is gone.
func TestDraftLayout_BackToDefaultWaitsForPublish_Integration(t *testing.T) {
	e := lbSetup(t)
	tenant := e.newTenant(t)
	builder := uuid.NewString()
	bctx, sctx := builderCtxOf(tenant, builder), legacyCtxOf(tenant)

	saved, err := e.layout.SaveLayout(bctx, tenant, SaveLayoutInput{EntityName: "hk_deals", ViewType: LayoutViewEdit, LayoutJSON: editLayout("name")}) // not delivered: live
	if err != nil {
		t.Fatal(err)
	}
	e.deliver(t, tenant, builder)

	if err := e.layout.DeleteLayout(bctx, tenant, saved.ID); err != nil {
		t.Fatalf("draft delete: %v", err)
	}
	if e.count(t, `SELECT COUNT(*) FROM sys_layouts WHERE tenant_id = ?`, tenant) != 1 {
		t.Fatal("a draft 'back to default' removed the live row")
	}
	if got, _ := e.layout.GetLayout(sctx, tenant, "hk_deals", LayoutViewEdit); got.Views[LayoutViewEdit].IsDefault {
		t.Fatal("the customer lost the custom screen before Publish")
	}
	if got, _ := e.layout.GetLayout(bctx, tenant, "hk_deals", LayoutViewEdit); !got.Views[LayoutViewEdit].IsDefault {
		t.Fatal("the builder's overlay does not show the screen as default")
	}
	if _, err := e.publishWithCurrentHash(bctx, tenant, false); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if e.count(t, `SELECT COUNT(*) FROM sys_layouts WHERE tenant_id = ?`, tenant) != 0 {
		t.Fatal("Publish left the layout row")
	}
	if got, _ := e.layout.GetLayout(sctx, tenant, "hk_deals", LayoutViewEdit); !got.Views[LayoutViewEdit].IsDefault {
		t.Fatal("the customer still has the custom screen after Publish")
	}
}

// Branding: the builder's colour is a draft; the public login page and the customer admin read the
// live document until Publish; Publish writes the draft to tenants.branding (this tenant only).
func TestDraftBranding_PublishWritesTheTenantRowOnly_Integration(t *testing.T) {
	e := lbSetup(t)
	tenant, other := e.newTenant(t), e.newTenant(t)
	builder := uuid.NewString()
	bctx, sctx := builderCtxOf(tenant, builder), legacyCtxOf(tenant)

	if _, err := e.branding.Update(bctx, tenant, Branding{AccentColor: "#111111"}); err != nil { // not delivered: live
		t.Fatal(err)
	}
	e.deliver(t, tenant, builder)
	if _, err := e.branding.Update(bctx, tenant, Branding{AccentColor: "#222222", AppName: "Phòng khám A"}); err != nil {
		t.Fatalf("draft save: %v", err)
	}

	var slug string
	if err := e.sdb.Get(&slug, `SELECT slug FROM tenants WHERE id = ?`, tenant); err != nil {
		t.Fatal(err)
	}
	if b := e.branding.GetPublicBySlug(bctx, slug); b.AccentColor != "#111111" {
		t.Fatalf("PUBLIC branding = %+v, want the live colour (FR-013)", b)
	}
	if b, st, _ := e.branding.GetState(sctx, tenant); b.AccentColor != "#111111" || st != "" {
		t.Fatalf("customer admin = %+v state %q, want live", b, st)
	}
	if b, st, _ := e.branding.GetState(bctx, tenant); b.AccentColor != "#222222" || st != "edited" {
		t.Fatalf("builder overlay = %+v state %q, want the draft", b, st)
	}

	if _, err := e.publishWithCurrentHash(bctx, tenant, false); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if b := e.branding.GetPublicBySlug(context.Background(), slug); b.AccentColor != "#222222" || b.AppName != "Phòng khám A" {
		t.Fatalf("public branding after publish = %+v", b)
	}
	var otherDoc *string
	if err := e.sdb.Get(&otherDoc, `SELECT CAST(branding AS CHAR) FROM tenants WHERE id = ?`, other); err != nil || otherDoc != nil {
		t.Fatalf("another tenant's branding = %v err %v, want untouched NULL", otherDoc, err)
	}
}
