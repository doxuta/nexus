package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

// DraftLayoutRepo implements LayoutRepo around the live layout repository (specs/063-draft-publish
// S7-3, same contract as DraftReportRepo):
//
//	not delivered            → writes go live, reads are live
//	delivered + builder      → writes become drafts; reads overlay them when the caller asked (ReadDraft)
//	delivered + anyone else  → writes go live (direct), reads are live — ZERO extra queries
//
// LayoutService (validation, serve-time tolerance, "Mới thêm") runs unchanged on top of it. A
// layout's draft key is "entity:view"; its id is the live row's id, or — while it only exists as a
// draft — the draft row's id, so PUT / DELETE by id keep working on a layout nobody has published.
// The overlay stamps UpdatedAt = the draft's last save, which is the "Mới thêm" cutoff the customer
// will get after Publish (UpsertLayoutExec writes the same instant).
type DraftLayoutRepo struct {
	live   LayoutRepo
	drafts draftStore
	gate   *DraftGate
}

// NewDraftLayoutRepo wraps live.
func NewDraftLayoutRepo(live LayoutRepo, drafts draftStore, gate *DraftGate) *DraftLayoutRepo {
	return &DraftLayoutRepo{live: live, drafts: drafts, gate: gate}
}

var _ LayoutRepo = (*DraftLayoutRepo)(nil)

// layoutViewLabels name a view for the builder's change list (no engine vocabulary — S13).
var layoutViewLabels = map[string]string{"edit": "Form nhập", "detail": "Trang chi tiết", "list": "Danh sách"}

func layoutDraftLabel(entity, view string) string {
	l, ok := layoutViewLabels[view]
	if !ok {
		l = view
	}
	return l + " · " + entity
}

func (r *DraftLayoutRepo) saveDraft(ctx context.Context, tenantID, entity, view, layoutJSON string) (*metaversion.DraftRow, error) {
	body, err := layoutDoc{EntityName: entity, ViewType: view, Layout: rawOrNull([]byte(layoutJSON))}.marshal()
	if err != nil {
		return nil, err
	}
	if err := r.drafts.Upsert(ctx, tenantID, metaversion.UpsertInput{DocType: DocTypeLayout, DocKey: layoutDocKey(entity, view), Op: metaversion.OpUpsert,
		Doc: body, EntityName: entity, Label: layoutDraftLabel(entity, view), UserID: draftActor(ctx, "")}); err != nil {
		return nil, err
	}
	d, err := r.drafts.GetByKey(ctx, tenantID, DocTypeLayout, layoutDocKey(entity, view))
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, errors.New("draft vanished after save")
	}
	return d, nil
}

// liveRow finds the live row of (entity, view), nil when the screen is still the generated default.
func (r *DraftLayoutRepo) liveRow(ctx context.Context, tenantID, entity, view string) (*metadata.LayoutDefinition, error) {
	rows, err := r.live.GetByEntity(ctx, tenantID, entity)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].ViewType == view {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// Upsert implements LayoutRepo.
func (r *DraftLayoutRepo) Upsert(ctx context.Context, layout *metadata.LayoutDefinition) error {
	mode, err := r.gate.WriteMode(ctx, layout.TenantID)
	if err != nil {
		return fmt.Errorf("services.DraftLayoutRepo.Upsert: %w", err)
	}
	if mode != metaversion.ModeDraft {
		return r.live.Upsert(ctx, layout)
	}
	live, err := r.liveRow(ctx, layout.TenantID, layout.EntityName, layout.ViewType)
	if err != nil {
		return fmt.Errorf("services.DraftLayoutRepo.Upsert: %w", err)
	}
	d, err := r.saveDraft(ctx, layout.TenantID, layout.EntityName, layout.ViewType, layout.LayoutJSON)
	if err != nil {
		return fmt.Errorf("services.DraftLayoutRepo.Upsert: %w", err)
	}
	layout.ID = d.ID
	if live != nil {
		layout.ID = live.ID
	}
	return nil
}

// locate finds the layout an id names — live first, else a draft-only layout — plus its draft.
func (r *DraftLayoutRepo) locate(ctx context.Context, tenantID, id string) (*metadata.LayoutDefinition, *metaversion.DraftRow, error) {
	live, err := r.live.GetByID(ctx, tenantID, id)
	if err != nil && !errors.Is(err, metadata.ErrLayoutNotFound) {
		return nil, nil, err
	}
	if live != nil {
		d, err := r.drafts.GetByKey(ctx, tenantID, DocTypeLayout, layoutDocKey(live.EntityName, live.ViewType))
		return live, d, err
	}
	rows, err := r.drafts.ListByType(ctx, tenantID, DocTypeLayout)
	if err != nil {
		return nil, nil, err
	}
	for i := range rows {
		if rows[i].ID == id {
			return nil, &rows[i], nil
		}
	}
	return nil, nil, nil
}

// UpdateByID implements LayoutRepo.
func (r *DraftLayoutRepo) UpdateByID(ctx context.Context, tenantID, id, layoutJSON string) error {
	mode, err := r.gate.WriteMode(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("services.DraftLayoutRepo.UpdateByID: %w", err)
	}
	if mode != metaversion.ModeDraft {
		return r.live.UpdateByID(ctx, tenantID, id, layoutJSON)
	}
	live, d, err := r.locate(ctx, tenantID, id)
	if err != nil {
		return fmt.Errorf("services.DraftLayoutRepo.UpdateByID: %w", err)
	}
	var entity, view string
	switch {
	case live != nil:
		entity, view = live.EntityName, live.ViewType
	case d != nil:
		var ok bool
		if entity, view, ok = splitLayoutKey(d.DocKey); !ok {
			return fmt.Errorf("services.DraftLayoutRepo.UpdateByID: malformed draft key %q", d.DocKey)
		}
	default:
		return metadata.ErrLayoutNotFound
	}
	// Saving a screen marked "back to default" brings it back as an edit.
	if _, err := r.saveDraft(ctx, tenantID, entity, view, layoutJSON); err != nil {
		return fmt.Errorf("services.DraftLayoutRepo.UpdateByID: %w", err)
	}
	return nil
}

// HardDelete implements LayoutRepo: in draft mode "back to default" is a draft the customer sees only
// after Publish; a layout that never went live is simply dropped.
func (r *DraftLayoutRepo) HardDelete(ctx context.Context, tenantID, id string) error {
	mode, err := r.gate.WriteMode(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("services.DraftLayoutRepo.HardDelete: %w", err)
	}
	if mode != metaversion.ModeDraft {
		return r.live.HardDelete(ctx, tenantID, id)
	}
	live, d, err := r.locate(ctx, tenantID, id)
	if err != nil {
		return fmt.Errorf("services.DraftLayoutRepo.HardDelete: %w", err)
	}
	switch {
	case live == nil && d == nil:
		return metadata.ErrLayoutNotFound
	case live == nil: // exists only as a draft
		if _, err := r.drafts.DeleteByKey(ctx, tenantID, DocTypeLayout, d.DocKey); err != nil {
			return fmt.Errorf("services.DraftLayoutRepo.HardDelete: %w", err)
		}
		return nil
	}
	if err := r.drafts.Upsert(ctx, tenantID, metaversion.UpsertInput{DocType: DocTypeLayout, DocKey: layoutDocKey(live.EntityName, live.ViewType),
		Op: metaversion.OpDelete, EntityName: live.EntityName, Label: layoutDraftLabel(live.EntityName, live.ViewType), UserID: draftActor(ctx, "")}); err != nil {
		return fmt.Errorf("services.DraftLayoutRepo.HardDelete: %w", err)
	}
	return nil
}

// GetByEntity implements LayoutRepo: live rows, or (overlay) the rows the draft layer would leave
// after Publish — edited / new screens in, screens marked "back to default" out.
func (r *DraftLayoutRepo) GetByEntity(ctx context.Context, tenantID, entityName string) ([]metadata.LayoutDefinition, error) {
	liveRows, err := r.live.GetByEntity(ctx, tenantID, entityName)
	if err != nil {
		return nil, err
	}
	overlay, err := r.gate.ReadOverlay(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.DraftLayoutRepo.GetByEntity: %w", err)
	}
	if !overlay {
		return liveRows, nil
	}
	drafts, err := r.drafts.ListByType(ctx, tenantID, DocTypeLayout)
	if err != nil {
		return nil, fmt.Errorf("services.DraftLayoutRepo.GetByEntity: %w", err)
	}
	byView := map[string]metaversion.DraftRow{}
	for _, d := range drafts {
		if e, v, ok := splitLayoutKey(d.DocKey); ok && e == entityName {
			byView[v] = d
		}
	}
	out := make([]metadata.LayoutDefinition, 0, len(liveRows)+len(byView))
	for i := range liveRows {
		live := liveRows[i]
		d, ok := byView[live.ViewType]
		if !ok {
			live.DraftState = draftStateLive
			out = append(out, live)
			continue
		}
		delete(byView, live.ViewType)
		rec, err := mergeLayoutDraft(tenantID, d, &live)
		if err != nil {
			return nil, fmt.Errorf("services.DraftLayoutRepo.GetByEntity: %w", err)
		}
		if rec != nil {
			out = append(out, *rec)
		}
	}
	for _, d := range byView { // draft-only screens
		rec, err := mergeLayoutDraft(tenantID, d, nil)
		if err != nil {
			return nil, fmt.Errorf("services.DraftLayoutRepo.GetByEntity: %w", err)
		}
		if rec != nil {
			out = append(out, *rec)
		}
	}
	return out, nil
}

// GetByID implements LayoutRepo: live, or (overlay) the edited / draft-only screen; a screen marked
// "back to default" is not found (it has no custom row left to edit).
func (r *DraftLayoutRepo) GetByID(ctx context.Context, tenantID, id string) (*metadata.LayoutDefinition, error) {
	overlay, err := r.gate.ReadOverlay(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.DraftLayoutRepo.GetByID: %w", err)
	}
	if !overlay {
		return r.live.GetByID(ctx, tenantID, id)
	}
	live, d, err := r.locate(ctx, tenantID, id)
	if err != nil {
		return nil, fmt.Errorf("services.DraftLayoutRepo.GetByID: %w", err)
	}
	if d == nil {
		if live == nil {
			return nil, metadata.ErrLayoutNotFound
		}
		live.DraftState = draftStateLive
		return live, nil
	}
	rec, err := mergeLayoutDraft(tenantID, *d, live)
	if err != nil {
		return nil, fmt.Errorf("services.DraftLayoutRepo.GetByID: %w", err)
	}
	if rec == nil {
		return nil, metadata.ErrLayoutNotFound
	}
	return rec, nil
}

// mergeLayoutDraft renders a draft over its live row (live may be nil). A delete draft yields nil:
// after Publish the screen is the generated default.
func mergeLayoutDraft(tenantID string, d metaversion.DraftRow, live *metadata.LayoutDefinition) (*metadata.LayoutDefinition, error) {
	if d.Op == metaversion.OpDelete {
		return nil, nil
	}
	var doc layoutDoc
	if err := json.Unmarshal(d.DraftJSON, &doc); err != nil {
		return nil, fmt.Errorf("decode draft %s: %w", d.DocKey, err)
	}
	rec := metadata.LayoutDefinition{ID: d.ID, TenantID: tenantID, EntityName: doc.EntityName, ViewType: doc.ViewType,
		LayoutJSON: string(doc.Layout), CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, DraftState: draftStateNew}
	if live != nil {
		rec.ID, rec.CreatedAt, rec.DraftState = live.ID, live.CreatedAt, draftStateEdited
	}
	return &rec, nil
}

func layoutDocKey(entity, view string) string { return entity + ":" + view }

// splitLayoutKey is the inverse of layoutDocKey; entity names never contain ':'.
func splitLayoutKey(key string) (entity, view string, ok bool) {
	i := strings.LastIndex(key, ":")
	if i <= 0 || i == len(key)-1 {
		return "", "", false
	}
	return key[:i], key[i+1:], true
}
