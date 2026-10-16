package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/metaversion"
)

// DraftDashboardRepo implements dashboardRepo around the live SQL repository (specs/063-draft-publish
// S7-2; same contract as DraftReportRepo). Deleting a dashboard in draft mode only marks the draft:
// the app-nav rows that point at it are NOT purged until Publish removes them in the same tx (FR-025),
// so the customer's sidebar keeps working until then. The app service keeps the raw repo (it reads
// dashboards to validate/filter nav — the customer's live view).
type DraftDashboardRepo struct {
	live   dashboardRepo
	drafts draftStore
	gate   *DraftGate
	// rec + adapter: direct version entry of a non-builder\'s live write (S7-5); nil = none.
	rec     *VersionRecorder
	adapter metaversion.DocAdapter
}

// NewDraftDashboardRepo wraps live.
func NewDraftDashboardRepo(live dashboardRepo, drafts draftStore, gate *DraftGate) *DraftDashboardRepo {
	return &DraftDashboardRepo{live: live, drafts: drafts, gate: gate}
}

var _ dashboardRepo = (*DraftDashboardRepo)(nil)

func dashboardDocOfCreate(in dashboard.CreateInput) dashboardDoc {
	return dashboardDoc{Name: in.Name, Description: in.Description, Widgets: rawOrNull(in.Widgets)}
}

// Create implements dashboardRepo.
func (r *DraftDashboardRepo) Create(ctx context.Context, in dashboard.CreateInput) (string, error) {
	mode, err := r.gate.WriteMode(ctx, in.TenantID)
	if err != nil {
		return "", fmt.Errorf("services.DraftDashboardRepo.Create: %w", err)
	}
	if mode != metaversion.ModeDraft {
		var id string
		err := r.rec.TraceLive(ctx, mode, r.adapter, in.TenantID, "create", "", func() (string, error) {
			var e error
			id, e = r.live.Create(ctx, in)
			return id, e
		})
		return id, err
	}
	id := uuid.NewString()
	body, err := dashboardDocOfCreate(in).marshal()
	if err != nil {
		return "", fmt.Errorf("services.DraftDashboardRepo.Create: %w", err)
	}
	if err := r.drafts.Upsert(ctx, in.TenantID, metaversion.UpsertInput{DocType: DocTypeDashboard, DocKey: id, Op: metaversion.OpUpsert,
		Doc: body, Label: in.Name, UserID: draftActor(ctx, in.CreatedBy)}); err != nil {
		return "", fmt.Errorf("services.DraftDashboardRepo.Create: %w", err)
	}
	return id, nil
}

// Update implements dashboardRepo.
func (r *DraftDashboardRepo) Update(ctx context.Context, tenantID, id string, in dashboard.UpdateInput) error {
	mode, err := r.gate.WriteMode(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("services.DraftDashboardRepo.Update: %w", err)
	}
	if mode != metaversion.ModeDraft {
		return r.rec.TraceLive(ctx, mode, r.adapter, tenantID, "update", id, func() (string, error) { return id, r.live.Update(ctx, tenantID, id, in) })
	}
	draft, live, err := r.state(ctx, tenantID, id)
	if err != nil {
		return fmt.Errorf("services.DraftDashboardRepo.Update: %w", err)
	}
	if draft == nil && live == nil {
		return dashboard.ErrDashboardNotFound
	}
	body, err := dashboardDoc{Name: in.Name, Description: in.Description, Widgets: rawOrNull(in.Widgets)}.marshal()
	if err != nil {
		return fmt.Errorf("services.DraftDashboardRepo.Update: %w", err)
	}
	if err := r.drafts.Upsert(ctx, tenantID, metaversion.UpsertInput{DocType: DocTypeDashboard, DocKey: id, Op: metaversion.OpUpsert,
		Doc: body, Label: in.Name, UserID: draftActor(ctx, "")}); err != nil {
		return fmt.Errorf("services.DraftDashboardRepo.Update: %w", err)
	}
	return nil
}

// Delete implements dashboardRepo.
func (r *DraftDashboardRepo) Delete(ctx context.Context, tenantID, id string) error {
	mode, err := r.gate.WriteMode(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("services.DraftDashboardRepo.Delete: %w", err)
	}
	if mode != metaversion.ModeDraft {
		return r.rec.TraceLive(ctx, mode, r.adapter, tenantID, "delete", id, func() (string, error) { return id, r.live.Delete(ctx, tenantID, id) })
	}
	draft, live, err := r.state(ctx, tenantID, id)
	if err != nil {
		return fmt.Errorf("services.DraftDashboardRepo.Delete: %w", err)
	}
	switch {
	case live == nil && draft == nil:
		return dashboard.ErrDashboardNotFound
	case live == nil:
		if _, err := r.drafts.DeleteByKey(ctx, tenantID, DocTypeDashboard, id); err != nil {
			return fmt.Errorf("services.DraftDashboardRepo.Delete: %w", err)
		}
		return nil
	}
	if err := r.drafts.Upsert(ctx, tenantID, metaversion.UpsertInput{DocType: DocTypeDashboard, DocKey: id, Op: metaversion.OpDelete,
		Label: live.Name, UserID: draftActor(ctx, "")}); err != nil {
		return fmt.Errorf("services.DraftDashboardRepo.Delete: %w", err)
	}
	return nil
}

func (r *DraftDashboardRepo) state(ctx context.Context, tenantID, id string) (*metaversion.DraftRow, *dashboard.Record, error) {
	live, err := r.live.Get(ctx, tenantID, id)
	if err != nil {
		if !errors.Is(err, dashboard.ErrDashboardNotFound) {
			return nil, nil, err
		}
		live = nil
	}
	draft, err := r.drafts.GetByKey(ctx, tenantID, DocTypeDashboard, id)
	if err != nil {
		return nil, nil, err
	}
	return draft, live, nil
}

// Get implements dashboardRepo.
func (r *DraftDashboardRepo) Get(ctx context.Context, tenantID, id string) (*dashboard.Record, error) {
	overlay, err := r.gate.ReadOverlay(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.DraftDashboardRepo.Get: %w", err)
	}
	if !overlay {
		return r.live.Get(ctx, tenantID, id)
	}
	draft, live, err := r.state(ctx, tenantID, id)
	if err != nil {
		return nil, fmt.Errorf("services.DraftDashboardRepo.Get: %w", err)
	}
	switch {
	case draft == nil && live == nil:
		return nil, dashboard.ErrDashboardNotFound
	case draft == nil:
		live.DraftState = draftStateLive
		return live, nil
	}
	rec, err := mergeDashboardDraft(tenantID, *draft, live)
	if err != nil {
		return nil, fmt.Errorf("services.DraftDashboardRepo.Get: %w", err)
	}
	if rec == nil {
		return nil, dashboard.ErrDashboardNotFound
	}
	return rec, nil
}

// List implements dashboardRepo.
func (r *DraftDashboardRepo) List(ctx context.Context, tenantID string) ([]dashboard.Record, error) {
	liveRows, err := r.live.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	overlay, err := r.gate.ReadOverlay(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.DraftDashboardRepo.List: %w", err)
	}
	if !overlay {
		return liveRows, nil
	}
	drafts, err := r.drafts.ListByType(ctx, tenantID, DocTypeDashboard)
	if err != nil {
		return nil, fmt.Errorf("services.DraftDashboardRepo.List: %w", err)
	}
	byID := make(map[string]metaversion.DraftRow, len(drafts))
	for _, d := range drafts {
		byID[d.DocKey] = d
	}
	out := make([]dashboard.Record, 0, len(liveRows)+len(drafts))
	for i := range liveRows {
		live := liveRows[i]
		d, ok := byID[live.ID]
		if !ok {
			live.DraftState = draftStateLive
			out = append(out, live)
			continue
		}
		delete(byID, live.ID)
		rec, err := mergeDashboardDraft(tenantID, d, &live)
		if err != nil {
			return nil, fmt.Errorf("services.DraftDashboardRepo.List: %w", err)
		}
		out = append(out, *rec)
	}
	for _, d := range byID {
		rec, err := mergeDashboardDraft(tenantID, d, nil)
		if err != nil {
			return nil, fmt.Errorf("services.DraftDashboardRepo.List: %w", err)
		}
		if rec != nil {
			out = append(out, *rec)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { // same order as dashboard.SQLRepository.List
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

func mergeDashboardDraft(tenantID string, d metaversion.DraftRow, live *dashboard.Record) (*dashboard.Record, error) {
	if d.Op == metaversion.OpDelete {
		if live == nil {
			return nil, nil
		}
		out := *live
		out.DraftState = draftStateDeleted
		return &out, nil
	}
	var doc dashboardDoc
	if err := json.Unmarshal(d.DraftJSON, &doc); err != nil {
		return nil, fmt.Errorf("decode draft %s: %w", d.DocKey, err)
	}
	rec := dashboard.Record{ID: d.DocKey, TenantID: tenantID, Name: doc.Name, Widgets: rawOrNull(doc.Widgets),
		UpdatedAt: d.UpdatedAt, DraftState: draftStateNew}
	if doc.Description != "" {
		desc := doc.Description
		rec.Description = &desc
	}
	if live != nil {
		rec.CreatedBy, rec.CreatedAt, rec.DraftState = live.CreatedBy, live.CreatedAt, draftStateEdited
	} else {
		by := d.CreatedBy
		rec.CreatedBy, rec.CreatedAt = &by, d.CreatedAt
	}
	return &rec, nil
}

// WithRecorder arms the direct version entries (specs/063 S7-5) with the type's document adapter.
func (r *DraftDashboardRepo) WithRecorder(rec *VersionRecorder, ad metaversion.DocAdapter) *DraftDashboardRepo {
	r.rec, r.adapter = rec, ad
	return r
}
