package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

// fieldLive is the slice of the registry repository DraftFieldRepo wraps (*metadata.Repository).
type fieldLive interface {
	ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error)
	GetFieldByID(ctx context.Context, tenantID, fieldID string) (*metadata.FieldDefinition, error)
	UpdateFieldMutable(ctx context.Context, tenantID string, f *metadata.FieldDefinition) error
	GetEntityNameByID(ctx context.Context, tenantID, entityID string) (string, error)
}

// DraftFieldRepo is the draft layer of a field's editable attributes (specs/063-draft-publish S7-8),
// same contract as DraftLayoutRepo:
//
//	not delivered            -> writes go live, reads are live
//	delivered + builder      -> writes become drafts (options.indexed excepted: it applies at once);
//	                            reads overlay them when the caller asked (ReadDraft)
//	delivered + anyone else  -> writes go live (direct), reads are live — ZERO extra queries
//
// It is NOT a FieldMetadataRepo: FieldService keeps its repo untouched (its guards type-assert the
// concrete repository) and asks this one at the three points a field is read for editing, listed, or
// persisted.
type DraftFieldRepo struct {
	live   fieldLive
	drafts draftStore
	gate   *DraftGate
}

// NewDraftFieldRepo wraps live.
func NewDraftFieldRepo(live fieldLive, drafts draftStore, gate *DraftGate) *DraftFieldRepo {
	return &DraftFieldRepo{live: live, drafts: drafts, gate: gate}
}

// DropDraft removes a field's pending draft. Delete is live-immediate (S7-9 turns it into a draft), so
// a draft left behind would be an orphan: Publish would conflict, or ApplyTx fail with ErrFieldNotFound.
// Idempotent; tenant-scoped.
func (r *DraftFieldRepo) DropDraft(ctx context.Context, tenantID, fieldID string) error {
	if _, err := r.drafts.DeleteByKey(ctx, tenantID, DocTypeField, fieldID); err != nil {
		return fmt.Errorf("services.DraftFieldRepo.DropDraft: %w", err)
	}
	return nil
}

// DropEntityDrafts removes the pending drafts of every live field of an entity (entity delete).
func (r *DraftFieldRepo) DropEntityDrafts(ctx context.Context, tenantID, entityID string) error {
	fields, err := r.live.ListFields(ctx, tenantID, entityID)
	if err != nil {
		return fmt.Errorf("services.DraftFieldRepo.DropEntityDrafts: list fields: %w", err)
	}
	for _, f := range fields {
		if err := r.DropDraft(ctx, tenantID, f.ID); err != nil {
			return err
		}
	}
	return nil
}

// draftDocs returns the tenant's field drafts decoded, keyed by field id, and the set of field ids the
// builder marked for deletion (S7-9).
func (r *DraftFieldRepo) draftDocs(ctx context.Context, tenantID string) (map[string]fieldDoc, map[string]bool, error) {
	rows, err := r.drafts.ListByType(ctx, tenantID, DocTypeField)
	if err != nil {
		return nil, nil, fmt.Errorf("services.DraftFieldRepo.draftDocs: %w", err)
	}
	out := make(map[string]fieldDoc, len(rows))
	deleted := map[string]bool{}
	for _, row := range rows {
		if row.Op == metaversion.OpDelete {
			deleted[row.DocKey] = true
			continue
		}
		var d fieldDoc
		if json.Unmarshal(row.DraftJSON, &d) != nil {
			continue
		}
		out[row.DocKey] = d
	}
	return out, deleted, nil
}

// SaveFieldDelete is the draft branch of FieldService.DeleteField (S7-9): on a delivered workspace, for
// the builder, deleting a field becomes a "sẽ xoá khi xuất bản" draft (op=delete) and NOTHING live
// changes — the customer keeps the field, its data and its pivot until Publish runs the real delete.
// It replaces a pending edit draft of the same field. drafted=false means "not a draft write: delete
// live as before" (not delivered / non-builder / publish applying).
func (r *DraftFieldRepo) SaveFieldDelete(ctx context.Context, tenantID string, f *metadata.FieldDefinition) (drafted bool, err error) {
	mode, err := r.gate.WriteMode(ctx, tenantID)
	if err != nil {
		return false, fmt.Errorf("services.DraftFieldRepo.SaveFieldDelete: %w", err)
	}
	if mode != metaversion.ModeDraft {
		return false, nil
	}
	entityName, err := r.live.GetEntityNameByID(ctx, tenantID, f.EntityID)
	if err != nil {
		return false, fmt.Errorf("services.DraftFieldRepo.SaveFieldDelete: resolve entity: %w", err)
	}
	if err := r.drafts.Upsert(ctx, tenantID, metaversion.UpsertInput{DocType: DocTypeField, DocKey: f.ID, Op: metaversion.OpDelete,
		EntityName: entityName, Label: f.Label + " · " + entityName, UserID: draftActor(ctx, "")}); err != nil {
		return false, fmt.Errorf("services.DraftFieldRepo.SaveFieldDelete: %w", err)
	}
	return true, nil
}

// SaveEntityDelete is SaveFieldDelete for a table (EntityService.SoftDeleteEntity).
func (r *DraftFieldRepo) SaveEntityDelete(ctx context.Context, tenantID string, e *metadata.EntityDefinition) (drafted bool, err error) {
	mode, err := r.gate.WriteMode(ctx, tenantID)
	if err != nil {
		return false, fmt.Errorf("services.DraftFieldRepo.SaveEntityDelete: %w", err)
	}
	if mode != metaversion.ModeDraft {
		return false, nil
	}
	if err := r.drafts.Upsert(ctx, tenantID, metaversion.UpsertInput{DocType: DocTypeEntity, DocKey: e.ID, Op: metaversion.OpDelete,
		EntityName: e.Name, Label: e.Label, UserID: draftActor(ctx, "")}); err != nil {
		return false, fmt.Errorf("services.DraftFieldRepo.SaveEntityDelete: %w", err)
	}
	return true, nil
}

// MarkDeletedEntities stamps draftState "deleted" on the tables the builder marked for deletion, for a
// draft-overlay reader only (everyone else: the list is returned untouched, zero queries).
func (r *DraftFieldRepo) MarkDeletedEntities(ctx context.Context, tenantID string, es []metadata.EntityDefinition) ([]metadata.EntityDefinition, error) {
	overlay, err := r.gate.ReadOverlay(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.DraftFieldRepo.MarkDeletedEntities: %w", err)
	}
	if !overlay {
		return es, nil
	}
	rows, err := r.drafts.ListByType(ctx, tenantID, DocTypeEntity)
	if err != nil {
		return nil, fmt.Errorf("services.DraftFieldRepo.MarkDeletedEntities: %w", err)
	}
	// A table created after the last publish is not the customer's yet: mark it "new" (same cutoff as fields).
	cutoff, err := r.gate.HeadCutoff(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.DraftFieldRepo.MarkDeletedEntities: %w", err)
	}
	if len(rows) == 0 && cutoff == nil {
		return es, nil
	}
	gone := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.Op == metaversion.OpDelete {
			gone[row.DocKey] = true
		}
	}
	out := make([]metadata.EntityDefinition, len(es))
	for i, e := range es {
		if cutoff != nil && e.CreatedAt.After(*cutoff) {
			e.DraftState = draftStateNew
		}
		if gone[e.ID] {
			e.DraftState = draftStateDeleted
		}
		out[i] = e
	}
	return out, nil
}

// overlayList is ListFields with the drafts laid over, for a caller known to be the builder: a field
// born after the last publish is marked "new", else one with an unpublished edit "edited".
func (r *DraftFieldRepo) overlayList(ctx context.Context, tenantID, entityID string, live []metadata.FieldDefinition) ([]metadata.FieldDefinition, error) {
	docs, gone, err := r.draftDocs(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	cutoff, err := r.gate.HeadCutoff(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.DraftFieldRepo.overlayList: %w", err)
	}
	out := make([]metadata.FieldDefinition, len(live))
	for i, f := range live {
		merged := f
		if d, ok := docs[f.ID]; ok {
			if merged, err = applyFieldDoc(f, d); err != nil {
				return nil, fmt.Errorf("services.DraftFieldRepo.overlayList: field %s: %w", f.ID, err)
			}
			merged.DraftState = draftStateEdited
			merged.PendingRequired = d.IsRequired && !f.IsRequired
		}
		if cutoff != nil && f.CreatedAt.After(*cutoff) {
			merged.DraftState = draftStateNew
		}
		if gone[f.ID] {
			merged.DraftState = draftStateDeleted
		}
		out[i] = merged
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].FieldOrder != out[j].FieldOrder {
			return out[i].FieldOrder < out[j].FieldOrder
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// ListFields serves the live list, or — for the builder's draft-overlay read — the list with their
// unpublished edits and "new" / "edited" markers. Everyone else pays no extra query.
func (r *DraftFieldRepo) ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error) {
	live, err := r.live.ListFields(ctx, tenantID, entityID)
	if err != nil {
		return nil, err
	}
	overlay, err := r.gate.ReadOverlay(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.DraftFieldRepo.ListFields: %w", err)
	}
	if !overlay {
		return live, nil
	}
	return r.overlayList(ctx, tenantID, entityID, live)
}

// GetFieldByID resolves a field the way the builder sees it (live + draft) for an overlay reader.
func (r *DraftFieldRepo) GetFieldByID(ctx context.Context, tenantID, fieldID string) (*metadata.FieldDefinition, error) {
	live, err := r.live.GetFieldByID(ctx, tenantID, fieldID)
	if err != nil {
		return nil, err
	}
	overlay, err := r.gate.ReadOverlay(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.DraftFieldRepo.GetFieldByID: %w", err)
	}
	if !overlay {
		return live, nil
	}
	row, err := r.drafts.GetByKey(ctx, tenantID, DocTypeField, fieldID)
	if err != nil {
		return nil, fmt.Errorf("services.DraftFieldRepo.GetFieldByID: %w", err)
	}
	if row != nil && row.Op == metaversion.OpDelete {
		l := *live
		l.DraftState = draftStateDeleted
		return &l, nil
	}
	if row == nil || row.Op != metaversion.OpUpsert {
		return live, nil
	}
	var d fieldDoc
	if err := json.Unmarshal(row.DraftJSON, &d); err != nil {
		return nil, fmt.Errorf("services.DraftFieldRepo.GetFieldByID: decode draft: %w", err)
	}
	merged, err := applyFieldDoc(*live, d)
	if err != nil {
		return nil, fmt.Errorf("services.DraftFieldRepo.GetFieldByID: %w", err)
	}
	merged.DraftState = draftStateEdited
	merged.PendingRequired = d.IsRequired && !live.IsRequired
	return &merged, nil
}

// SaveEdit persists the editable attributes of a field.
//
//	live write (not delivered / non-builder): the registry repository, as before; draftOnly=false.
//	builder on a delivered tenant: options.indexed (a DDL-backed toggle EditField has already run)
//	goes live NOW; everything else becomes the "field" draft, or no draft at all when the result
//	equals the live row (an edit reverted by hand). draftOnly=true when nothing live changed, so the
//	caller can keep the customer's /audit-log free of unpublished work.
func (r *DraftFieldRepo) SaveEdit(ctx context.Context, tenantID string, effective *metadata.FieldDefinition) (draftOnly bool, err error) {
	mode, err := r.gate.WriteMode(ctx, tenantID)
	if err != nil {
		return false, fmt.Errorf("services.DraftFieldRepo.SaveEdit: %w", err)
	}
	if mode != metaversion.ModeDraft {
		if err := r.live.UpdateFieldMutable(ctx, tenantID, effective); err != nil {
			return false, err
		}
		return false, nil
	}
	liveRow, err := r.live.GetFieldByID(ctx, tenantID, effective.ID)
	if err != nil {
		return false, err
	}
	// A field marked "sẽ xoá khi xuất bản" is not editable: saving would silently replace the delete
	// draft with an edit. The builder undoes the delete first (Hoàn tác).
	if pending, derr := r.drafts.GetByKey(ctx, tenantID, DocTypeField, effective.ID); derr != nil {
		return false, fmt.Errorf("services.DraftFieldRepo.SaveEdit: %w", derr)
	} else if pending != nil && pending.Op == metaversion.OpDelete {
		return false, invalid("FIELD_PENDING_DELETE", "Trường này đang chờ xoá khi xuất bản; bấm Hoàn tác trước khi sửa")
	}
	// options.indexed applies now (FR-063): the toggle already ran its DDL.
	wantIdx, err := optionsMap(effective.Options)
	if err != nil {
		return false, fmt.Errorf("services.DraftFieldRepo.SaveEdit: %w", err)
	}
	haveIdx, err := optionsMap(liveRow.Options)
	if err != nil {
		return false, fmt.Errorf("services.DraftFieldRepo.SaveEdit: %w", err)
	}
	if wantIdx["indexed"] != haveIdx["indexed"] {
		l := *liveRow
		if v, ok := wantIdx["indexed"]; ok {
			haveIdx["indexed"] = v
		} else {
			delete(haveIdx, "indexed")
		}
		if len(haveIdx) == 0 {
			l.Options = nil
		} else {
			b, merr := json.Marshal(haveIdx)
			if merr != nil {
				return false, fmt.Errorf("services.DraftFieldRepo.SaveEdit: %w", merr)
			}
			s := string(b)
			l.Options = &s
		}
		if err := r.live.UpdateFieldMutable(ctx, tenantID, &l); err != nil {
			return false, err
		}
		draftOnly = false
	} else {
		draftOnly = true
	}
	want, err := fieldDocOf(effective)
	if err != nil {
		return false, fmt.Errorf("services.DraftFieldRepo.SaveEdit: %w", err)
	}
	have, err := fieldDocOf(liveRow)
	if err != nil {
		return false, fmt.Errorf("services.DraftFieldRepo.SaveEdit: %w", err)
	}
	wantJSON, err := want.marshal()
	if err != nil {
		return false, err
	}
	haveJSON, err := have.marshal()
	if err != nil {
		return false, err
	}
	wh, werr := metaversion.DocHash(wantJSON)
	hh, herr := metaversion.DocHash(haveJSON)
	if err := errors.Join(werr, herr); err != nil {
		return false, fmt.Errorf("services.DraftFieldRepo.SaveEdit: hash: %w", err)
	}
	if wh == hh {
		if _, err := r.drafts.DeleteByKey(ctx, tenantID, DocTypeField, effective.ID); err != nil {
			return false, fmt.Errorf("services.DraftFieldRepo.SaveEdit: drop reverted draft: %w", err)
		}
		return draftOnly, nil
	}
	entityName, err := r.live.GetEntityNameByID(ctx, tenantID, effective.EntityID)
	if err != nil {
		return false, fmt.Errorf("services.DraftFieldRepo.SaveEdit: resolve entity: %w", err)
	}
	if err := r.drafts.Upsert(ctx, tenantID, metaversion.UpsertInput{DocType: DocTypeField, DocKey: effective.ID, Op: metaversion.OpUpsert,
		Doc: wantJSON, EntityName: entityName, Label: effective.Label + " · " + entityName, UserID: draftActor(ctx, "")}); err != nil {
		return false, fmt.Errorf("services.DraftFieldRepo.SaveEdit: %w", err)
	}
	return draftOnly, nil
}

// SaveReorder turns "the i-th id gets field_order (i+1)*10" into one field draft per field whose
// position changes. Each draft starts from the field AS THE BUILDER SEES IT (live + any pending edit),
// so a reorder merges into an existing edit draft of the same field; a field put back at its live
// position drops its draft (SaveEdit). sys_fields.field_order is never written. ponytail: one small tx
// per changed field, not one for the batch — a failure midway leaves a partly drafted order the
// builder can finish or discard; nothing live is touched either way.
func (r *DraftFieldRepo) SaveReorder(ctx context.Context, tenantID, entityID string, order []string) error {
	live, err := r.live.ListFields(ctx, tenantID, entityID)
	if err != nil {
		return err
	}
	eff, err := r.overlayList(ctx, tenantID, entityID, live)
	if err != nil {
		return err
	}
	byID := make(map[string]metadata.FieldDefinition, len(eff))
	for _, f := range eff {
		byID[f.ID] = f
	}
	for i, id := range order {
		f, ok := byID[id]
		if !ok {
			return fmt.Errorf("services.DraftFieldRepo.SaveReorder: field %s: %w", id, metadata.ErrFieldNotFound)
		}
		want := (i + 1) * 10
		if f.FieldOrder == want || f.DraftState == draftStateDeleted { // a field going away keeps its place
			continue
		}
		f.FieldOrder = want
		if _, err := r.SaveEdit(ctx, tenantID, &f); err != nil {
			return fmt.Errorf("services.DraftFieldRepo.SaveReorder: %w", err)
		}
	}
	return nil
}
