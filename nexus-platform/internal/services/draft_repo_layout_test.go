package services

// draft_repo_layout_test.go — specs/063-draft-publish T065 (+ branding store): the wrapped layout repo
// and branding store against in-memory live repos and the shared fake draft store. No DB.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

type dlEnv struct {
	live   *memLayoutRepo
	drafts *fakeDraftStore
	deliv  *fakeDelivered
	repo   *DraftLayoutRepo
}

func newDLEnv(delivered bool) *dlEnv {
	live, drafts, deliv := &memLayoutRepo{}, newFakeDraftStore(), &fakeDelivered{delivered: delivered}
	return &dlEnv{live: live, drafts: drafts, deliv: deliv, repo: NewDraftLayoutRepo(live, drafts, NewDraftGate(deliv))}
}

const dlBody = `{"sections":[{"title":"A","fields":["name"]}]}`

func (e *dlEnv) seedLive(id, entity, view, js string, updated time.Time) {
	e.live.rows = append(e.live.rows, metadata.LayoutDefinition{ID: id, TenantID: dwTenant, EntityName: entity, ViewType: view, LayoutJSON: js, CreatedAt: updated, UpdatedAt: updated})
}

func dlLayout(entity, view, js string) *metadata.LayoutDefinition {
	return &metadata.LayoutDefinition{ID: "fresh-uuid", TenantID: dwTenant, EntityName: entity, ViewType: view, LayoutJSON: js}
}

func TestDraftLayoutRepo_NotDeliveredGoesStraightLive(t *testing.T) {
	e := newDLEnv(false)
	if err := e.repo.Upsert(builderCtx(), dlLayout("deals", "edit", dlBody)); err != nil {
		t.Fatal(err)
	}
	if len(e.live.rows) != 1 || len(e.drafts.rows) != 0 {
		t.Fatalf("not delivered: want 1 live row and no draft, got %d / %d", len(e.live.rows), len(e.drafts.rows))
	}
}

func TestDraftLayoutRepo_BuilderEditIsADraftAndOverlayCarriesTheSaveInstant(t *testing.T) {
	e := newDLEnv(true)
	liveAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	e.seedLive("L1", "deals", "edit", `{"sections":[]}`, liveAt)

	if err := e.repo.Upsert(builderCtx(), dlLayout("deals", "edit", dlBody)); err != nil {
		t.Fatal(err)
	}
	if e.live.rows[0].LayoutJSON != `{"sections":[]}` {
		t.Fatalf("a builder save on a delivered tenant touched the live row: %s", e.live.rows[0].LayoutJSON)
	}
	// the customer (and the builder without the overlay) still reads the live row
	for _, ctx := range []context.Context{legacyAdminCtx(), context.Background()} {
		rows, err := e.repo.GetByEntity(ctx, dwTenant, "deals")
		if err != nil || len(rows) != 1 || rows[0].LayoutJSON != `{"sections":[]}` || rows[0].DraftState != "" {
			t.Fatalf("non-overlay read must be the live row untouched: rows=%v err=%v", rows, err)
		}
	}
	rows, err := e.repo.GetByEntity(builderCtx(), dwTenant, "deals")
	if err != nil || len(rows) != 1 {
		t.Fatalf("overlay read: rows=%v err=%v", rows, err)
	}
	got := rows[0]
	d, _ := e.drafts.GetByKey(context.Background(), dwTenant, DocTypeLayout, "deals:edit")
	if got.ID != "L1" || got.DraftState != draftStateEdited || got.LayoutJSON == `{"sections":[]}` {
		t.Errorf("overlay row = %+v, want live id L1, state edited, the draft body", got)
	}
	if !got.UpdatedAt.Equal(d.UpdatedAt) || !got.UpdatedAt.After(liveAt) {
		t.Errorf("overlay UpdatedAt = %v, want the draft-save instant %v (the 'Mới thêm' cutoff)", got.UpdatedAt, d.UpdatedAt)
	}
	if got.CreatedAt != liveAt {
		t.Errorf("CreatedAt = %v, want the live row's %v", got.CreatedAt, liveAt)
	}
}

func TestDraftLayoutRepo_DraftOnlyScreenKeepsAnIdPutAndDeleteCanUse(t *testing.T) {
	e := newDLEnv(true)
	ctx := builderCtx()
	l := dlLayout("deals", "detail", dlBody)
	if err := e.repo.Upsert(ctx, l); err != nil {
		t.Fatal(err)
	}
	if l.ID != "d-deals:detail" {
		t.Fatalf("draft-only layout id = %q, want the draft row id", l.ID)
	}
	if err := e.repo.UpdateByID(ctx, dwTenant, l.ID, `{"sections":[{"title":"B","fields":["x"]}]}`); err != nil {
		t.Fatalf("UpdateByID on a draft-only screen: %v", err)
	}
	got, err := e.repo.GetByID(ctx, dwTenant, l.ID)
	if err != nil || got.DraftState != draftStateNew || got.LayoutJSON != `{"sections":[{"title":"B","fields":["x"]}]}` {
		t.Fatalf("GetByID overlay = %+v err=%v", got, err)
	}
	if _, err := e.repo.GetByID(legacyAdminCtx(), dwTenant, l.ID); !errors.Is(err, metadata.ErrLayoutNotFound) {
		t.Fatalf("the customer's read of a draft-only id = %v, want not found", err)
	}
	if err := e.repo.HardDelete(ctx, dwTenant, l.ID); err != nil {
		t.Fatal(err)
	}
	if len(e.drafts.rows) != 0 || len(e.live.rows) != 0 {
		t.Fatalf("deleting a draft-only screen must drop the draft and write nothing live: %d drafts %d live", len(e.drafts.rows), len(e.live.rows))
	}
	if err := e.repo.HardDelete(ctx, dwTenant, l.ID); !errors.Is(err, metadata.ErrLayoutNotFound) {
		t.Fatalf("second delete = %v, want not found", err)
	}
}

func TestDraftLayoutRepo_HardDeleteIsABackToDefaultDraftAndTheLiveRowStays(t *testing.T) {
	e := newDLEnv(true)
	e.seedLive("L1", "deals", "edit", dlBody, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err := e.repo.HardDelete(builderCtx(), dwTenant, "L1"); err != nil {
		t.Fatal(err)
	}
	if len(e.live.rows) != 1 {
		t.Fatal("live row must stay until Publish")
	}
	d, _ := e.drafts.GetByKey(context.Background(), dwTenant, DocTypeLayout, "deals:edit")
	if d == nil || d.Op != metaversion.OpDelete {
		t.Fatalf("want a delete draft, got %+v", d)
	}
	rows, _ := e.repo.GetByEntity(builderCtx(), dwTenant, "deals")
	if len(rows) != 0 {
		t.Errorf("overlay must show the screen as default (no custom row), got %v", rows)
	}
	rows, _ = e.repo.GetByEntity(legacyAdminCtx(), dwTenant, "deals")
	if len(rows) != 1 {
		t.Errorf("the customer must still see the custom screen, got %v", rows)
	}
	// saving again brings it back as an edit
	if err := e.repo.Upsert(builderCtx(), dlLayout("deals", "edit", dlBody)); err != nil {
		t.Fatal(err)
	}
	if d, _ = e.drafts.GetByKey(context.Background(), dwTenant, DocTypeLayout, "deals:edit"); d.Op != metaversion.OpUpsert {
		t.Errorf("re-save after 'back to default' must be an upsert draft, got %s", d.Op)
	}
}

func TestDraftLayoutRepo_OtherTenantsDraftsAreNotRead(t *testing.T) {
	e := newDLEnv(true)
	if err := e.repo.Upsert(builderCtx(), dlLayout("deals", "edit", dlBody)); err != nil {
		t.Fatal(err)
	}
	// overlay for a tenant that is not the caller's reads live (cross-tenant ?targetTenant= style)
	rows, err := e.repo.GetByEntity(builderCtx(), "t2", "deals")
	if err != nil || len(rows) != 0 {
		t.Fatalf("cross-tenant overlay read: rows=%v err=%v", rows, err)
	}
	if err := e.repo.Upsert(builderCtx(), &metadata.LayoutDefinition{TenantID: "t2", EntityName: "deals", ViewType: "edit", LayoutJSON: dlBody}); !errors.Is(err, metaversion.ErrCrossTenantDraftWrite) {
		t.Fatalf("cross-tenant write on a delivered tenant = %v, want ErrCrossTenantDraftWrite", err)
	}
}

// ---- branding store ----

type fakeBrandingLive struct {
	tenant  auth.Tenant
	updates int
}

func (f *fakeBrandingLive) ResolveTenantBySlug(context.Context, string) (*auth.Tenant, error) {
	t := f.tenant
	return &t, nil
}
func (f *fakeBrandingLive) GetTenantByID(_ context.Context, id string) (*auth.Tenant, error) {
	if id != f.tenant.ID {
		return nil, auth.ErrTenantNotFound
	}
	t := f.tenant
	return &t, nil
}
func (f *fakeBrandingLive) UpdateTenantBranding(_ context.Context, _ string, raw json.RawMessage) error {
	f.updates++
	f.tenant.Branding = raw
	return nil
}

func newDBEnv(delivered bool) (*fakeBrandingLive, *fakeDraftStore, *DraftBrandingStore) {
	live := &fakeBrandingLive{tenant: auth.Tenant{ID: dwTenant, Slug: "acme", Branding: json.RawMessage(`{"accentColor":"#111111"}`)}}
	drafts := newFakeDraftStore()
	return live, drafts, NewDraftBrandingStore(live, drafts, NewDraftGate(&fakeDelivered{delivered: delivered}))
}

func TestDraftBrandingStore_BuilderSaveIsADraftAndOnlyTheOverlayShowsIt(t *testing.T) {
	live, drafts, store := newDBEnv(true)
	svc := NewTenantBrandingService(store)
	if _, err := svc.Update(builderCtx(), dwTenant, Branding{AccentColor: "#222222", AppName: "Acme"}); err != nil {
		t.Fatal(err)
	}
	if live.updates != 0 || string(live.tenant.Branding) != `{"accentColor":"#111111"}` {
		t.Fatalf("live branding touched by a builder save on a delivered tenant: %s (updates %d)", live.tenant.Branding, live.updates)
	}
	if len(drafts.rows) != 1 {
		t.Fatalf("want 1 draft, got %d", len(drafts.rows))
	}
	b, state, err := svc.GetState(builderCtx(), dwTenant)
	if err != nil || b.AccentColor != "#222222" || state != draftStateEdited {
		t.Fatalf("overlay read = %+v state=%q err=%v", b, state, err)
	}
	b, state, _ = svc.GetState(legacyAdminCtx(), dwTenant)
	if b.AccentColor != "#111111" || state != "" {
		t.Fatalf("non-overlay read = %+v state=%q, want live and no state", b, state)
	}
	// FR-013: the public login-page path never overlays, whoever is in the ctx
	if got := svc.GetPublicBySlug(builderCtx(), "acme"); got.AccentColor != "#111111" || got.AppName != "" {
		t.Fatalf("PUBLIC branding leaked the draft: %+v", got)
	}
	// "reset to default" is a draft too: an empty document
	if _, err := svc.Update(builderCtx(), dwTenant, Branding{}); err != nil {
		t.Fatal(err)
	}
	if b, _, _ = svc.GetState(builderCtx(), dwTenant); b != (Branding{}) {
		t.Errorf("overlay after reset = %+v, want empty", b)
	}
	if got := svc.GetPublicBySlug(context.Background(), "acme"); got.AccentColor != "#111111" {
		t.Errorf("public branding changed by a draft: %+v", got)
	}
}

func TestDraftBrandingStore_StaffAndNotDeliveredWriteLive(t *testing.T) {
	for name, tc := range map[string]struct {
		delivered bool
		ctx       context.Context
	}{"not delivered": {false, builderCtx()}, "legacy customer admin": {true, legacyAdminCtx()}} {
		t.Run(name, func(t *testing.T) {
			live, drafts, store := newDBEnv(tc.delivered)
			if _, err := NewTenantBrandingService(store).Update(tc.ctx, dwTenant, Branding{AppName: "Live"}); err != nil {
				t.Fatal(err)
			}
			if live.updates != 1 || len(drafts.rows) != 0 {
				t.Fatalf("want a live write and no draft, got updates=%d drafts=%d", live.updates, len(drafts.rows))
			}
		})
	}
}

func TestDraftBrandingStore_UnknownTenantSameAnswerAsLive(t *testing.T) {
	_, _, store := newDBEnv(true)
	err := store.UpdateTenantBranding(metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: "b", TenantID: "t9", IsBuilder: true}), "t9", nil)
	if !errors.Is(err, auth.ErrTenantNotFound) {
		t.Fatalf("got %v, want ErrTenantNotFound", err)
	}
}
