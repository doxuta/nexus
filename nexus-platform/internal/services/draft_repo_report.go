package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/report"
)

// DraftReportRepo implements reportRepo around the live SQL repository (specs/063-draft-publish S7-2,
// same contract as DraftWorkflowRepo):
//
//	not delivered            → writes go live, reads are live
//	delivered + builder      → writes become drafts; reads overlay them when the caller asked (ReadDraft)
//	delivered + anyone else  → writes go live (direct), reads are live — ZERO extra queries
//
// The customer app shares ReportService with the admin surface, so the gate (Caller + header) is what
// keeps a draft away from staff: a non-builder never gets ReadDraft.
type DraftReportRepo struct {
	live   reportRepo
	drafts draftStore
	gate   *DraftGate
	// rec + adapter: direct version entry of a non-builder\'s live write (S7-5); nil = none.
	rec     *VersionRecorder
	adapter metaversion.DocAdapter
}

// NewDraftReportRepo wraps live.
func NewDraftReportRepo(live reportRepo, drafts draftStore, gate *DraftGate) *DraftReportRepo {
	return &DraftReportRepo{live: live, drafts: drafts, gate: gate}
}

var _ reportRepo = (*DraftReportRepo)(nil)

// Create implements reportRepo.
func (r *DraftReportRepo) Create(ctx context.Context, in report.CreateInput) (string, error) {
	mode, err := r.gate.WriteMode(ctx, in.TenantID)
	if err != nil {
		return "", fmt.Errorf("services.DraftReportRepo.Create: %w", err)
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
	id := uuid.NewString() // the draft's id is the report's id for ever (FR-007)
	body, err := reportDoc{Name: in.Name, EntityName: in.EntityName, Type: string(in.Type), Config: rawOrNull(in.Config), IsActive: in.IsActive}.marshal()
	if err != nil {
		return "", fmt.Errorf("services.DraftReportRepo.Create: %w", err)
	}
	if err := r.drafts.Upsert(ctx, in.TenantID, metaversion.UpsertInput{DocType: DocTypeReport, DocKey: id, Op: metaversion.OpUpsert,
		Doc: body, EntityName: in.EntityName, Label: in.Name, UserID: draftActor(ctx, in.CreatedBy)}); err != nil {
		return "", fmt.Errorf("services.DraftReportRepo.Create: %w", err)
	}
	return id, nil
}

// Update implements reportRepo.
func (r *DraftReportRepo) Update(ctx context.Context, tenantID, id string, in report.UpdateInput) error {
	mode, err := r.gate.WriteMode(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("services.DraftReportRepo.Update: %w", err)
	}
	if mode != metaversion.ModeDraft {
		return r.rec.TraceLive(ctx, mode, r.adapter, tenantID, "update", id, func() (string, error) { return id, r.live.Update(ctx, tenantID, id, in) })
	}
	draft, live, err := r.state(ctx, tenantID, id)
	if err != nil {
		return fmt.Errorf("services.DraftReportRepo.Update: %w", err)
	}
	if draft == nil && live == nil {
		return report.ErrReportNotFound
	}
	// Saving a report marked "to delete" brings it back as an edit (FR-008).
	body, err := reportDoc{Name: in.Name, EntityName: in.EntityName, Type: string(in.Type), Config: rawOrNull(in.Config), IsActive: in.IsActive}.marshal()
	if err != nil {
		return fmt.Errorf("services.DraftReportRepo.Update: %w", err)
	}
	if err := r.drafts.Upsert(ctx, tenantID, metaversion.UpsertInput{DocType: DocTypeReport, DocKey: id, Op: metaversion.OpUpsert,
		Doc: body, EntityName: in.EntityName, Label: in.Name, UserID: draftActor(ctx, in.UpdatedBy)}); err != nil {
		return fmt.Errorf("services.DraftReportRepo.Update: %w", err)
	}
	return nil
}

// Delete implements reportRepo.
func (r *DraftReportRepo) Delete(ctx context.Context, tenantID, id string) error {
	mode, err := r.gate.WriteMode(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("services.DraftReportRepo.Delete: %w", err)
	}
	if mode != metaversion.ModeDraft {
		return r.rec.TraceLive(ctx, mode, r.adapter, tenantID, "delete", id, func() (string, error) { return id, r.live.Delete(ctx, tenantID, id) })
	}
	draft, live, err := r.state(ctx, tenantID, id)
	if err != nil {
		return fmt.Errorf("services.DraftReportRepo.Delete: %w", err)
	}
	switch {
	case live == nil && draft == nil:
		return report.ErrReportNotFound
	case live == nil: // exists only as a draft: drop it, nothing was ever live
		if _, err := r.drafts.DeleteByKey(ctx, tenantID, DocTypeReport, id); err != nil {
			return fmt.Errorf("services.DraftReportRepo.Delete: %w", err)
		}
		return nil
	}
	if err := r.drafts.Upsert(ctx, tenantID, metaversion.UpsertInput{DocType: DocTypeReport, DocKey: id, Op: metaversion.OpDelete,
		EntityName: live.EntityName, Label: live.Name, UserID: draftActor(ctx, "")}); err != nil {
		return fmt.Errorf("services.DraftReportRepo.Delete: %w", err)
	}
	return nil
}

func (r *DraftReportRepo) state(ctx context.Context, tenantID, id string) (*metaversion.DraftRow, *report.Record, error) {
	live, err := r.live.Get(ctx, tenantID, id)
	if err != nil {
		if !errors.Is(err, report.ErrReportNotFound) {
			return nil, nil, err
		}
		live = nil
	}
	draft, err := r.drafts.GetByKey(ctx, tenantID, DocTypeReport, id)
	if err != nil {
		return nil, nil, err
	}
	return draft, live, nil
}

// Get implements reportRepo: live, or (overlay) the edited / new / deleted-marked report.
func (r *DraftReportRepo) Get(ctx context.Context, tenantID, id string) (*report.Record, error) {
	overlay, err := r.gate.ReadOverlay(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.DraftReportRepo.Get: %w", err)
	}
	if !overlay {
		return r.live.Get(ctx, tenantID, id)
	}
	draft, live, err := r.state(ctx, tenantID, id)
	if err != nil {
		return nil, fmt.Errorf("services.DraftReportRepo.Get: %w", err)
	}
	switch {
	case draft == nil && live == nil:
		return nil, report.ErrReportNotFound
	case draft == nil:
		live.DraftState = draftStateLive
		return live, nil
	}
	rec, err := mergeReportDraft(tenantID, *draft, live)
	if err != nil {
		return nil, fmt.Errorf("services.DraftReportRepo.Get: %w", err)
	}
	if rec == nil {
		return nil, report.ErrReportNotFound
	}
	return rec, nil
}

// List implements reportRepo: live reports plus (overlay) the changes of the draft layer, newest first.
func (r *DraftReportRepo) List(ctx context.Context, tenantID string) ([]report.Record, error) {
	liveRows, err := r.live.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	overlay, err := r.gate.ReadOverlay(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.DraftReportRepo.List: %w", err)
	}
	if !overlay {
		return liveRows, nil
	}
	drafts, err := r.drafts.ListByType(ctx, tenantID, DocTypeReport)
	if err != nil {
		return nil, fmt.Errorf("services.DraftReportRepo.List: %w", err)
	}
	byID := make(map[string]metaversion.DraftRow, len(drafts))
	for _, d := range drafts {
		byID[d.DocKey] = d
	}
	out := make([]report.Record, 0, len(liveRows)+len(drafts))
	for i := range liveRows {
		live := liveRows[i]
		d, ok := byID[live.ID]
		if !ok {
			live.DraftState = draftStateLive
			out = append(out, live)
			continue
		}
		delete(byID, live.ID)
		rec, err := mergeReportDraft(tenantID, d, &live)
		if err != nil {
			return nil, fmt.Errorf("services.DraftReportRepo.List: %w", err)
		}
		out = append(out, *rec)
	}
	for _, d := range byID { // draft-only reports
		rec, err := mergeReportDraft(tenantID, d, nil)
		if err != nil {
			return nil, fmt.Errorf("services.DraftReportRepo.List: %w", err)
		}
		if rec != nil {
			out = append(out, *rec)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { // same order as report.SQLRepository.List
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// mergeReportDraft renders a draft over its live report (live may be nil). A delete draft keeps the
// live content marked "deleted"; it yields nil when there is no live report left to mark.
func mergeReportDraft(tenantID string, d metaversion.DraftRow, live *report.Record) (*report.Record, error) {
	if d.Op == metaversion.OpDelete {
		if live == nil {
			return nil, nil
		}
		out := *live
		out.DraftState = draftStateDeleted
		return &out, nil
	}
	var doc reportDoc
	if err := json.Unmarshal(d.DraftJSON, &doc); err != nil {
		return nil, fmt.Errorf("decode draft %s: %w", d.DocKey, err)
	}
	rec := report.Record{ID: d.DocKey, TenantID: tenantID, Name: doc.Name, EntityName: doc.EntityName, Type: doc.Type,
		Config: rawOrNull(doc.Config), IsActive: doc.IsActive, UpdatedBy: d.ModifiedBy, UpdatedAt: d.UpdatedAt, DraftState: draftStateNew}
	if live != nil {
		rec.CreatedBy, rec.CreatedAt, rec.DraftState = live.CreatedBy, live.CreatedAt, draftStateEdited
	} else {
		by := d.CreatedBy
		rec.CreatedBy, rec.CreatedAt = &by, d.CreatedAt
	}
	return &rec, nil
}

// WithRecorder arms the direct version entries (specs/063 S7-5) with the type's document adapter.
func (r *DraftReportRepo) WithRecorder(rec *VersionRecorder, ad metaversion.DocAdapter) *DraftReportRepo {
	r.rec, r.adapter = rec, ad
	return r
}
