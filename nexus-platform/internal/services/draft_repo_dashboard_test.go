package services

// draft_repo_dashboard_test.go — specs/063-draft-publish T053/T054: the wrapped dashboard repo.

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/metaversion"
)

type draftFakeDashRepo struct {
	rows  map[string]dashboard.Record
	n     int
	purge int // times Delete ran (the live delete is what purges the nav in production)
}

func newDraftFakeDashRepo() *draftFakeDashRepo {
	return &draftFakeDashRepo{rows: map[string]dashboard.Record{}}
}

func (f *draftFakeDashRepo) Create(_ context.Context, in dashboard.CreateInput) (string, error) {
	f.n++
	id := uuid.NewString()
	by := in.CreatedBy
	f.rows[id] = dashboard.Record{ID: id, TenantID: in.TenantID, Name: in.Name, Widgets: in.Widgets, CreatedBy: &by,
		CreatedAt: time.Date(2026, 10, 1, 0, 0, f.n, 0, time.UTC)}
	return id, nil
}

func (f *draftFakeDashRepo) Get(_ context.Context, tenantID, id string) (*dashboard.Record, error) {
	if r, ok := f.rows[id]; ok && r.TenantID == tenantID {
		return &r, nil
	}
	return nil, dashboard.ErrDashboardNotFound
}

func (f *draftFakeDashRepo) List(_ context.Context, tenantID string) ([]dashboard.Record, error) {
	var out []dashboard.Record
	for _, r := range f.rows {
		if r.TenantID == tenantID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (f *draftFakeDashRepo) Update(_ context.Context, tenantID, id string, in dashboard.UpdateInput) error {
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return dashboard.ErrDashboardNotFound
	}
	r.Name, r.Widgets = in.Name, in.Widgets
	f.rows[id] = r
	return nil
}

func (f *draftFakeDashRepo) Delete(_ context.Context, tenantID, id string) error {
	if r, ok := f.rows[id]; !ok || r.TenantID != tenantID {
		return dashboard.ErrDashboardNotFound
	}
	f.purge++
	delete(f.rows, id)
	return nil
}

func newDDEnv(delivered bool) (*draftFakeDashRepo, *fakeDraftStore, *DraftDashboardRepo) {
	live, drafts := newDraftFakeDashRepo(), newFakeDraftStore()
	return live, drafts, NewDraftDashboardRepo(live, drafts, NewDraftGate(&fakeDelivered{delivered: delivered}))
}

func ddCreate(name string) dashboard.CreateInput {
	return dashboard.CreateInput{TenantID: dwTenant, Name: name, Widgets: []byte(`[{"id":"w1","entity":"crm_x"}]`), CreatedBy: "builder"}
}

func TestDraftDashboardRepo_DeliveredBuilderWritesDrafts(t *testing.T) {
	live, drafts, repo := newDDEnv(true)
	ctx := builderCtx()
	id, _ := live.Create(context.Background(), ddCreate("live"))

	if err := repo.Update(ctx, dwTenant, id, dashboard.UpdateInput{Name: "edited", Description: "d", Widgets: []byte(`[]`)}); err != nil {
		t.Fatal(err)
	}
	if l, _ := live.Get(context.Background(), dwTenant, id); l.Name != "live" {
		t.Fatalf("live dashboard changed by a draft save: %+v", l)
	}
	got, err := repo.Get(ctx, dwTenant, id)
	if err != nil || got.Name != "edited" || got.DraftState != "edited" || got.Description == nil || *got.Description != "d" {
		t.Fatalf("overlay Get = %+v, %v", got, err)
	}

	newID, err := repo.Create(ctx, ddCreate("new"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err = repo.Get(ctx, dwTenant, newID); err != nil || got.DraftState != "new" {
		t.Fatalf("new draft Get = %+v, %v", got, err)
	}

	// Deleting a live dashboard in the draft MUST NOT touch the live row — so the app-nav purge (done by
	// the live delete) does not run: the customer's sidebar keeps working until Publish (FR-025).
	if err := repo.Delete(ctx, dwTenant, id); err != nil {
		t.Fatal(err)
	}
	if live.purge != 0 {
		t.Fatalf("draft delete ran the live delete (nav purged) %d times", live.purge)
	}
	if got, _ = repo.Get(ctx, dwTenant, id); got == nil || got.DraftState != "deleted" {
		t.Fatalf("deleted dashboard = %+v", got)
	}
	rows, _ := repo.List(ctx, dwTenant)
	if len(rows) != 2 {
		t.Fatalf("List = %d rows, want deleted-marked + new", len(rows))
	}
	if err := repo.Delete(ctx, dwTenant, newID); err != nil {
		t.Fatal(err)
	}
	if d, _ := drafts.GetByKey(ctx, dwTenant, DocTypeDashboard, newID); d != nil {
		t.Fatal("draft of a never-live dashboard survived its deletion")
	}
	if err := repo.Delete(ctx, dwTenant, "ghost"); !errors.Is(err, dashboard.ErrDashboardNotFound) {
		t.Fatalf("Delete of a ghost: %v", err)
	}
}

func TestDraftDashboardRepo_NonBuilderReadsAndWritesLive(t *testing.T) {
	live, _, repo := newDDEnv(true)
	id, _ := live.Create(context.Background(), ddCreate("live"))
	_ = repo.Update(builderCtx(), dwTenant, id, dashboard.UpdateInput{Name: "draft", Widgets: []byte(`[]`)})

	if got, err := repo.Get(legacyAdminCtx(), dwTenant, id); err != nil || got.Name != "live" || got.DraftState != "" {
		t.Fatalf("legacy admin sees %+v, %v", got, err)
	}
	if err := repo.Update(legacyAdminCtx(), dwTenant, id, dashboard.UpdateInput{Name: "direct", Widgets: []byte(`[]`)}); err != nil {
		t.Fatal(err)
	}
	if l, _ := live.Get(context.Background(), dwTenant, id); l.Name != "direct" {
		t.Fatalf("legacy admin write did not land live: %q", l.Name)
	}
	if err := repo.Update(context.Background(), dwTenant, id, dashboard.UpdateInput{Name: "x", Widgets: []byte(`[]`)}); !errors.Is(err, metaversion.ErrNoCaller) {
		t.Fatalf("no caller on a delivered tenant: %v", err)
	}
}

func TestDashboardDoc_DraftIdenticalToLiveHashesAlike(t *testing.T) {
	desc := ""
	live := &dashboard.Record{Name: "d", Description: &desc, Widgets: []byte(`[{"id": "w1", "entity": "crm_x"}]`)}
	liveDoc, _ := dashboardDocOfRecord(live).marshal()
	draftDoc, _ := dashboardDocOfCreate(dashboard.CreateInput{Name: "d", Widgets: []byte(`[{"entity":"crm_x","id":"w1"}]`)}).marshal()
	a, _ := metaversion.DocHash(liveDoc)
	b, _ := metaversion.DocHash(draftDoc)
	if a != b {
		t.Fatalf("hash(live)=%s hash(draft)=%s: an unchanged draft would raise PUBLISH_CONFLICT", a, b)
	}
}
