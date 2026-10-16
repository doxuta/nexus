package services

// draft_repo_report_test.go — specs/063-draft-publish T053/T054/T055 (unit part): the wrapped report
// repo against an in-memory live repo and draft store. No DB.

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/report"
)

type draftFakeReportRepo struct {
	rows map[string]report.Record
	n    int
}

func newDraftFakeReportRepo() *draftFakeReportRepo {
	return &draftFakeReportRepo{rows: map[string]report.Record{}}
}

func (f *draftFakeReportRepo) Create(_ context.Context, in report.CreateInput) (string, error) {
	f.n++
	id := uuid.NewString()
	by := in.CreatedBy
	f.rows[id] = report.Record{ID: id, TenantID: in.TenantID, Name: in.Name, EntityName: in.EntityName, Type: string(in.Type),
		Config: in.Config, IsActive: in.IsActive, CreatedBy: &by, CreatedAt: time.Date(2026, 10, 1, 0, 0, f.n, 0, time.UTC)}
	return id, nil
}

func (f *draftFakeReportRepo) Get(_ context.Context, tenantID, id string) (*report.Record, error) {
	if r, ok := f.rows[id]; ok && r.TenantID == tenantID {
		return &r, nil
	}
	return nil, report.ErrReportNotFound
}

func (f *draftFakeReportRepo) List(_ context.Context, tenantID string) ([]report.Record, error) {
	var out []report.Record
	for _, r := range f.rows {
		if r.TenantID == tenantID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (f *draftFakeReportRepo) Update(_ context.Context, tenantID, id string, in report.UpdateInput) error {
	r, ok := f.rows[id]
	if !ok || r.TenantID != tenantID {
		return report.ErrReportNotFound
	}
	r.Name, r.EntityName, r.Type, r.Config, r.IsActive = in.Name, in.EntityName, string(in.Type), in.Config, in.IsActive
	f.rows[id] = r
	return nil
}

func (f *draftFakeReportRepo) Delete(_ context.Context, tenantID, id string) error {
	if r, ok := f.rows[id]; !ok || r.TenantID != tenantID {
		return report.ErrReportNotFound
	}
	delete(f.rows, id)
	return nil
}

type drEnv struct {
	live   *draftFakeReportRepo
	drafts *fakeDraftStore
	deliv  *fakeDelivered
	repo   *DraftReportRepo
}

func newDREnv(delivered bool) *drEnv {
	live, drafts, deliv := newDraftFakeReportRepo(), newFakeDraftStore(), &fakeDelivered{delivered: delivered}
	return &drEnv{live: live, drafts: drafts, deliv: deliv, repo: NewDraftReportRepo(live, drafts, NewDraftGate(deliv))}
}

func drCreate(name string) report.CreateInput {
	return report.CreateInput{TenantID: dwTenant, Name: name, EntityName: "crm_x", Type: report.TypeTabular,
		Config: []byte(`{"columns":["a"]}`), IsActive: true, CreatedBy: "builder"}
}

func drUpdate(name string) report.UpdateInput {
	return report.UpdateInput{Name: name, EntityName: "crm_x", Type: report.TypeTabular, Config: []byte(`{"columns":["a","b"]}`), IsActive: true, UpdatedBy: "builder"}
}

func (e *drEnv) seedLive(t *testing.T, name string) string {
	t.Helper()
	id, err := e.live.Create(context.Background(), drCreate(name))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDraftReportRepo_NotDeliveredGoesStraightLive(t *testing.T) {
	e := newDREnv(false)
	id, err := e.repo.Create(builderCtx(), drCreate("R1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.live.Get(context.Background(), dwTenant, id); err != nil {
		t.Fatalf("not delivered: the report must be live at once: %v", err)
	}
	if err := e.repo.Update(builderCtx(), dwTenant, id, drUpdate("R1b")); err != nil {
		t.Fatal(err)
	}
	if err := e.repo.Delete(builderCtx(), dwTenant, id); err != nil {
		t.Fatal(err)
	}
	if e.drafts.saves != 0 {
		t.Fatal("a draft was written for a tenant that is not delivered")
	}
}

func TestDraftReportRepo_DeliveredBuilderWritesDrafts(t *testing.T) {
	e := newDREnv(true)
	liveID := e.seedLive(t, "live")
	ctx := builderCtx()

	if err := e.repo.Update(ctx, dwTenant, liveID, drUpdate("edited")); err != nil {
		t.Fatal(err)
	}
	if live, _ := e.live.Get(context.Background(), dwTenant, liveID); live.Name != "live" {
		t.Fatalf("live row changed by a draft save: %+v", live)
	}
	got, err := e.repo.Get(ctx, dwTenant, liveID)
	if err != nil || got.Name != "edited" || got.DraftState != "edited" || string(got.Config) != `{"columns":["a","b"]}` {
		t.Fatalf("overlay Get = %+v, %v", got, err)
	}

	// US3 #6 (BE half): a second draft save replaces the first, no duplicate row.
	if err := e.repo.Update(ctx, dwTenant, liveID, drUpdate("edited again")); err != nil {
		t.Fatal(err)
	}
	if rows, _ := e.drafts.ListByType(ctx, dwTenant, DocTypeReport); len(rows) != 1 {
		t.Fatalf("draft rows = %d, want 1", len(rows))
	}

	newID, err := e.repo.Create(ctx, drCreate("brand new"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.live.Get(context.Background(), dwTenant, newID); !errors.Is(err, report.ErrReportNotFound) {
		t.Fatalf("a new draft report exists live: %v", err)
	}
	if got, err = e.repo.Get(ctx, dwTenant, newID); err != nil || got.DraftState != "new" || got.CreatedAt.IsZero() {
		t.Fatalf("new draft Get = %+v, %v", got, err) // US3 #7: opens without a 404
	}

	otherLive := e.seedLive(t, "to delete")
	if err := e.repo.Delete(ctx, dwTenant, otherLive); err != nil {
		t.Fatal(err)
	}
	if _, err := e.live.Get(context.Background(), dwTenant, otherLive); err != nil {
		t.Fatalf("live report deleted by a draft delete: %v", err)
	}
	if got, _ = e.repo.Get(ctx, dwTenant, otherLive); got == nil || got.DraftState != "deleted" {
		t.Fatalf("deleted report = %+v", got)
	}
	if err := e.repo.Delete(ctx, dwTenant, newID); err != nil { // draft-only: the row simply goes
		t.Fatal(err)
	}
	if d, _ := e.drafts.GetByKey(ctx, dwTenant, DocTypeReport, newID); d != nil {
		t.Fatalf("draft of a never-live report survived its deletion")
	}
	if err := e.repo.Update(ctx, dwTenant, "ghost", drUpdate("x")); !errors.Is(err, report.ErrReportNotFound) {
		t.Fatalf("Update of a ghost: %v", err)
	}
}

// FR-012 / SC-008: only the builder asking for the draft sees it; everyone else reads live and costs 0 queries.
func TestDraftReportRepo_OnlyTheBuilderWhoAsksSeesDrafts(t *testing.T) {
	e := newDREnv(true)
	id := e.seedLive(t, "r")
	_ = e.repo.Update(builderCtx(), dwTenant, id, drUpdate("draft name"))

	// DraftCaller never sets ReadDraft for a non-builder, header or not.
	staff := metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: "staff", TenantID: dwTenant, HomeTenantID: dwTenant})
	builderNoHeader := metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: "builder", TenantID: dwTenant, HomeTenantID: dwTenant, IsBuilder: true})
	before := e.deliv.calls
	for name, c := range map[string]context.Context{"staff": staff, "builder without header": builderNoHeader,
		"no caller": context.Background(), "live read": metaversion.WithLiveRead(builderCtx())} {
		got, err := e.repo.Get(c, dwTenant, id)
		if err != nil || got.Name != "r" || got.DraftState != "" {
			t.Fatalf("%s sees %+v, %v; want the live report", name, got, err)
		}
		rows, err := e.repo.List(c, dwTenant)
		if err != nil || len(rows) != 1 || rows[0].Name != "r" {
			t.Fatalf("%s List = %+v, %v", name, rows, err)
		}
	}
	if e.deliv.calls != before {
		t.Fatalf("non-overlay reads asked the version store %d times; want 0 (SC-008)", e.deliv.calls-before)
	}
}

// SC-006 (hash part): a draft saved identical to the live report hashes identical to it, whatever
// order MySQL's JSON column keeps the config keys in.
func TestReportDoc_DraftIdenticalToLiveHashesAlike(t *testing.T) {
	live := &report.Record{Name: "r", EntityName: "crm_x", Type: "summary", IsActive: true,
		Config: []byte(`{"groupBy": "stage", "metric": "count"}`)} // MySQL spacing/order
	liveDoc, _ := reportDocOfRecord(live).marshal()
	draftDoc, _ := reportDoc{Name: "r", EntityName: "crm_x", Type: "summary", IsActive: true,
		Config: rawOrNull([]byte(`{"metric":"count","groupBy":"stage"}`))}.marshal()
	a, err1 := metaversion.DocHash(liveDoc)
	b, err2 := metaversion.DocHash(draftDoc)
	if err1 != nil || err2 != nil || a != b {
		t.Fatalf("hash(live)=%s hash(draft)=%s (%v %v): an unchanged draft would raise PUBLISH_CONFLICT", a, b, err1, err2)
	}
}
