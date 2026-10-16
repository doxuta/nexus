package services

// specs/062-customer-workspace L3 T055 — saving / editing / deleting a layout leaves an
// audit row (__layout__) with the caller as actor and a before/after diff, so the customer's
// trail answers "who changed my form". Nothing is written when the write fails; the default
// (noop) auditor changes no behaviour.

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

// memLayoutRepo is a stateful in-memory LayoutRepo (one row per tenant+entity+view, like the table).
type memLayoutRepo struct {
	rows      []metadata.LayoutDefinition
	getErr    error // returned by GetByEntity / GetByID when set
	upsertErr error
	next      int
}

func (r *memLayoutRepo) GetByEntity(_ context.Context, tenant, entity string) ([]metadata.LayoutDefinition, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	var out []metadata.LayoutDefinition
	for _, l := range r.rows {
		if l.TenantID == tenant && l.EntityName == entity {
			out = append(out, l)
		}
	}
	return out, nil
}

func (r *memLayoutRepo) GetByID(_ context.Context, tenant, id string) (*metadata.LayoutDefinition, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	for i := range r.rows {
		if r.rows[i].TenantID == tenant && r.rows[i].ID == id {
			l := r.rows[i]
			return &l, nil
		}
	}
	return nil, metadata.ErrLayoutNotFound
}

func (r *memLayoutRepo) Upsert(_ context.Context, l *metadata.LayoutDefinition) error {
	if r.upsertErr != nil {
		return r.upsertErr
	}
	for i := range r.rows {
		if r.rows[i].TenantID == l.TenantID && r.rows[i].EntityName == l.EntityName && r.rows[i].ViewType == l.ViewType {
			r.rows[i].LayoutJSON = l.LayoutJSON
			l.ID = r.rows[i].ID // like the real upsert: the persisted id wins
			return nil
		}
	}
	r.rows = append(r.rows, *l)
	return nil
}

func (r *memLayoutRepo) UpdateByID(_ context.Context, tenant, id, layoutJSON string) error {
	for i := range r.rows {
		if r.rows[i].TenantID == tenant && r.rows[i].ID == id {
			r.rows[i].LayoutJSON = layoutJSON
			return nil
		}
	}
	return metadata.ErrLayoutNotFound
}

func (r *memLayoutRepo) HardDelete(_ context.Context, tenant, id string) error {
	for i := range r.rows {
		if r.rows[i].TenantID == tenant && r.rows[i].ID == id {
			r.rows = append(r.rows[:i], r.rows[i+1:]...)
			return nil
		}
	}
	return metadata.ErrLayoutNotFound
}

func layoutAuditSvc(repo *memLayoutRepo) (*LayoutService, *captureAuditor) {
	aud := &captureAuditor{}
	svc := NewLayoutService(&sortableFakeMeta{fields: sortableFixtureFields()}, repo, nil).WithAuditor(aud, nil)
	return svc, aud
}

func layoutAuditCtx() context.Context { return audit.WithActor(context.Background(), "builder-1") }

func listLayout(cols ...string) map[string]any {
	cs := make([]any, 0, len(cols))
	for _, c := range cols {
		cs = append(cs, map[string]any{"field": c})
	}
	return map[string]any{"version": float64(1), "columns": cs, "defaultSort": map[string]any{"field": "created_at", "order": "desc"}}
}

func TestLayoutService_WritesEmitLayoutAudit(t *testing.T) {
	repo := &memLayoutRepo{}
	svc, aud := layoutAuditSvc(repo)
	ctx := layoutAuditCtx()

	// 1. first save of (deals, list) => create, after-only diff
	saved, err := svc.SaveLayout(ctx, "tenant-1", SaveLayoutInput{EntityName: "deals", ViewType: LayoutViewList, LayoutJSON: listLayout("name")})
	if err != nil {
		t.Fatalf("SaveLayout: %v", err)
	}
	// 2. re-save the same view => update, diff carries both sides
	if _, err := svc.SaveLayout(ctx, "tenant-1", SaveLayoutInput{EntityName: "deals", ViewType: LayoutViewList, LayoutJSON: listLayout("name", "stage")}); err != nil {
		t.Fatalf("SaveLayout again: %v", err)
	}
	// 3. update by id
	if _, err := svc.UpdateLayout(ctx, "tenant-1", UpdateLayoutInput{ID: saved.ID, LayoutJSON: listLayout("stage")}); err != nil {
		t.Fatalf("UpdateLayout: %v", err)
	}
	// 4. delete by id => before-only diff
	if err := svc.DeleteLayout(ctx, "tenant-1", saved.ID); err != nil {
		t.Fatalf("DeleteLayout: %v", err)
	}

	wantActions := []string{audit.ActionCreate, audit.ActionUpdate, audit.ActionUpdate, audit.ActionDelete}
	if len(aud.events) != len(wantActions) {
		t.Fatalf("events = %d; want %d: %+v", len(aud.events), len(wantActions), aud.events)
	}
	for i, e := range aud.events {
		if e.EntityName != audit.LayoutMeta || e.Action != wantActions[i] {
			t.Errorf("event %d = %s/%s; want %s/%s", i, e.EntityName, e.Action, audit.LayoutMeta, wantActions[i])
		}
		if e.TenantID != "tenant-1" || e.ActorUserID != "builder-1" || e.RecordID != saved.ID {
			t.Errorf("event %d tenant/actor/record = %q/%q/%q", i, e.TenantID, e.ActorUserID, e.RecordID)
		}
		if e.Diff == nil {
			t.Fatalf("event %d has no diff", i)
		}
	}
	const key = "deals/list" // the diff names the table and the view: they never change, so a bare layout key would hide them
	if aud.events[0].Diff.After[key] == nil || aud.events[0].Diff.Before != nil {
		t.Errorf("create diff = %+v; want after-only under %q", aud.events[0].Diff, key)
	}
	if aud.events[1].Diff.Before[key] == nil || aud.events[1].Diff.After[key] == nil {
		t.Errorf("update diff = %+v; want both sides under %q", aud.events[1].Diff, key)
	}
	if aud.events[3].Diff.Before[key] == nil || aud.events[3].Diff.After != nil {
		t.Errorf("delete diff = %+v; want before-only under %q", aud.events[3].Diff, key)
	}
}

func TestLayoutService_FailedWritesEmitNothing(t *testing.T) {
	t.Run("validation failure", func(t *testing.T) {
		svc, aud := layoutAuditSvc(&memLayoutRepo{})
		_, err := svc.SaveLayout(layoutAuditCtx(), "tenant-1", SaveLayoutInput{EntityName: "deals", ViewType: LayoutViewList, LayoutJSON: listLayout("nope")})
		if err == nil {
			t.Fatal("want a validation error for an unknown field")
		}
		if len(aud.events) != 0 {
			t.Fatalf("events after a rejected save = %d; want 0", len(aud.events))
		}
	})
	t.Run("storage failure", func(t *testing.T) {
		boom := errors.New("db down")
		svc, aud := layoutAuditSvc(&memLayoutRepo{upsertErr: boom})
		if _, err := svc.SaveLayout(layoutAuditCtx(), "tenant-1", SaveLayoutInput{EntityName: "deals", ViewType: LayoutViewList, LayoutJSON: listLayout("name")}); !errors.Is(err, boom) {
			t.Fatalf("err = %v; want wrapped %v", err, boom)
		}
		if len(aud.events) != 0 {
			t.Fatalf("events after a failed upsert = %d; want 0", len(aud.events))
		}
	})
	t.Run("delete of an unknown id keeps its 404 and writes nothing", func(t *testing.T) {
		svc, aud := layoutAuditSvc(&memLayoutRepo{})
		err := svc.DeleteLayout(layoutAuditCtx(), "tenant-1", "ghost")
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "LAYOUT_NOT_FOUND" {
			t.Fatalf("err = %v; want LAYOUT_NOT_FOUND", err)
		}
		if len(aud.events) != 0 {
			t.Fatalf("events = %d; want 0", len(aud.events))
		}
	})
	t.Run("another tenant's layout id is not found and not audited", func(t *testing.T) {
		repo := &memLayoutRepo{rows: []metadata.LayoutDefinition{{ID: "lay-b", TenantID: "tenant-b", EntityName: "deals", ViewType: LayoutViewList, LayoutJSON: `{"version":1,"columns":[]}`}}}
		svc, aud := layoutAuditSvc(repo)
		if err := svc.DeleteLayout(layoutAuditCtx(), "tenant-1", "lay-b"); err == nil {
			t.Fatal("cross-tenant delete must fail")
		}
		if len(aud.events) != 0 || len(repo.rows) != 1 {
			t.Fatalf("events=%d rows=%d; want 0 events and the other tenant's row intact", len(aud.events), len(repo.rows))
		}
	})
}

// A service that never got an auditor behaves as before (noop default).
func TestLayoutService_NoAuditorIsNoop(t *testing.T) {
	svc := NewLayoutService(&sortableFakeMeta{fields: sortableFixtureFields()}, &memLayoutRepo{}, nil)
	if _, err := svc.SaveLayout(context.Background(), "tenant-1", SaveLayoutInput{EntityName: "deals", ViewType: LayoutViewList, LayoutJSON: listLayout("name")}); err != nil {
		t.Fatalf("SaveLayout without auditor: %v", err)
	}
}

// Layout IS a draft type since specs/063 S7-3: main.go hands the DraftGate to LayoutService.WithAuditor
// so a builder's draft-only save leaves no audit row. If it stops being one, a live write by the
// builder would go unaudited (nil-safe path skipped) — this test points at the wiring.
func TestLayoutAudit_LayoutIsADraftType(t *testing.T) {
	if k := metaversion.ClassOf("layout").Kind; k != metaversion.Draft {
		t.Fatal(`"layout" stopped being a draft type: main.go passes the DraftGate to LayoutService.WithAuditor, so a builder's live write would go unaudited`)
	}
}
