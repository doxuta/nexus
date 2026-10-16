package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/workflow"
)

// Draft states a record carries when it is read over the overlay.
const (
	draftStateLive    = "live"
	draftStateEdited  = "edited"
	draftStateNew     = "new"
	draftStateDeleted = "deleted"
)

// DraftWorkflowRepo implements workflowRepo around the live SQL repository. The admin
// WorkflowService is handed THIS repo, so its whole validation body runs unchanged and only the
// destination of a write differs. The engine's loader, scheduler and every other reader keep the raw
// repo: a draft is therefore never executed, by construction (FR-016), and the customer's hot path
// gains no query.
//
//	not delivered            → writes go live, reads are live            (today's behaviour)
//	delivered + builder      → writes become drafts, reads overlay them
//	delivered + anyone else  → writes go live (direct), reads are live
type DraftWorkflowRepo struct {
	live   workflowRepo
	drafts draftStore
	gate   *DraftGate
	// rec + adapter: direct version entry of a non-builder\'s live write (S7-5); nil = none.
	rec     *VersionRecorder
	adapter metaversion.DocAdapter
}

// draftStore is the slice of the draft repository the wrapped repos use. Satisfied by
// *metaversion.DraftRepo (built over a registry that holds the document type's adapter).
type draftStore interface {
	Upsert(ctx context.Context, tenantID string, in metaversion.UpsertInput) error
	GetByKey(ctx context.Context, tenantID, docType, docKey string) (*metaversion.DraftRow, error)
	ListByType(ctx context.Context, tenantID, docType string) ([]metaversion.DraftRow, error)
	DeleteByKey(ctx context.Context, tenantID, docType, docKey string) (bool, error)
}

// NewDraftWorkflowRepo wraps live.
func NewDraftWorkflowRepo(live workflowRepo, drafts draftStore, gate *DraftGate) *DraftWorkflowRepo {
	return &DraftWorkflowRepo{live: live, drafts: drafts, gate: gate}
}

var _ workflowRepo = (*DraftWorkflowRepo)(nil)

func draftActor(ctx context.Context, fallback string) string {
	if fallback != "" {
		return fallback
	}
	if c, ok := metaversion.CallerFrom(ctx); ok {
		return c.UserID
	}
	return ""
}

// Create implements workflowRepo.
func (r *DraftWorkflowRepo) Create(ctx context.Context, in workflow.CreateInput) (string, error) {
	mode, err := r.gate.WriteMode(ctx, in.TenantID)
	if err != nil {
		return "", fmt.Errorf("services.DraftWorkflowRepo.Create: %w", err)
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
	id := uuid.NewString() // the draft's id is the rule's id for ever (FR-007)
	body, err := workflowDocOfCreate(in).marshal()
	if err != nil {
		return "", fmt.Errorf("services.DraftWorkflowRepo.Create: %w", err)
	}
	if err := r.drafts.Upsert(ctx, in.TenantID, metaversion.UpsertInput{DocType: DocTypeWorkflow, DocKey: id, Op: metaversion.OpUpsert,
		Doc: body, EntityName: in.EntityName, Label: in.Name, UserID: draftActor(ctx, in.CreatedBy)}); err != nil {
		return "", fmt.Errorf("services.DraftWorkflowRepo.Create: %w", err)
	}
	return id, nil
}

// Update implements workflowRepo.
func (r *DraftWorkflowRepo) Update(ctx context.Context, tenantID, id string, in workflow.UpdateInput) error {
	mode, err := r.gate.WriteMode(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("services.DraftWorkflowRepo.Update: %w", err)
	}
	if mode != metaversion.ModeDraft {
		return r.rec.TraceLive(ctx, mode, r.adapter, tenantID, "update", id, func() (string, error) { return id, r.live.Update(ctx, tenantID, id, in) })
	}
	draft, live, err := r.state(ctx, tenantID, id)
	if err != nil {
		return fmt.Errorf("services.DraftWorkflowRepo.Update: %w", err)
	}
	if draft == nil && live == nil {
		return workflow.ErrWorkflowNotFound
	}
	// Saving a rule that was marked "to delete" brings it back as an edit (FR-008).
	body, err := workflowDocOfUpdate(in).marshal()
	if err != nil {
		return fmt.Errorf("services.DraftWorkflowRepo.Update: %w", err)
	}
	if err := r.drafts.Upsert(ctx, tenantID, metaversion.UpsertInput{DocType: DocTypeWorkflow, DocKey: id, Op: metaversion.OpUpsert,
		Doc: body, EntityName: in.EntityName, Label: in.Name, UserID: draftActor(ctx, in.UpdatedBy)}); err != nil {
		return fmt.Errorf("services.DraftWorkflowRepo.Update: %w", err)
	}
	return nil
}

// Delete implements workflowRepo.
func (r *DraftWorkflowRepo) Delete(ctx context.Context, tenantID, id string) error {
	mode, err := r.gate.WriteMode(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("services.DraftWorkflowRepo.Delete: %w", err)
	}
	if mode != metaversion.ModeDraft {
		return r.rec.TraceLive(ctx, mode, r.adapter, tenantID, "delete", id, func() (string, error) { return id, r.live.Delete(ctx, tenantID, id) })
	}
	draft, live, err := r.state(ctx, tenantID, id)
	if err != nil {
		return fmt.Errorf("services.DraftWorkflowRepo.Delete: %w", err)
	}
	switch {
	case live == nil && draft == nil:
		return workflow.ErrWorkflowNotFound
	case live == nil: // exists only as a draft: drop the draft, nothing was ever live
		if _, err := r.drafts.DeleteByKey(ctx, tenantID, DocTypeWorkflow, id); err != nil {
			return fmt.Errorf("services.DraftWorkflowRepo.Delete: %w", err)
		}
		return nil
	}
	if err := r.drafts.Upsert(ctx, tenantID, metaversion.UpsertInput{DocType: DocTypeWorkflow, DocKey: id, Op: metaversion.OpDelete,
		EntityName: live.EntityName, Label: live.Name, UserID: draftActor(ctx, "")}); err != nil {
		return fmt.Errorf("services.DraftWorkflowRepo.Delete: %w", err)
	}
	return nil
}

// SetActive implements workflowRepo (pause / resume without touching the actions). Draft mode: the
// switch becomes a draft edit of the rule (its other columns carried over verbatim, so a legacy key in
// the actions is neither stripped nor re-sent) — nothing live changes until Publish. A rule marked
// "to delete" is not switchable (ErrWorkflowNotFound), so a resume can never un-delete it silently.
func (r *DraftWorkflowRepo) SetActive(ctx context.Context, tenantID, id string, active bool, updatedBy string) error {
	mode, err := r.gate.WriteMode(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("services.DraftWorkflowRepo.SetActive: %w", err)
	}
	if mode != metaversion.ModeDraft {
		return r.rec.TraceLive(ctx, mode, r.adapter, tenantID, "update", id, func() (string, error) { return id, r.live.SetActive(ctx, tenantID, id, active, updatedBy) })
	}
	draft, live, err := r.state(ctx, tenantID, id)
	if err != nil {
		return fmt.Errorf("services.DraftWorkflowRepo.SetActive: %w", err)
	}
	var doc workflowDoc
	switch {
	case draft != nil && draft.Op == metaversion.OpDelete, draft == nil && live == nil:
		return workflow.ErrWorkflowNotFound
	case draft != nil:
		if err := json.Unmarshal(draft.DraftJSON, &doc); err != nil {
			return fmt.Errorf("services.DraftWorkflowRepo.SetActive: decode draft %s: %w", id, err)
		}
	default:
		doc = workflowDocOfRecord(live)
	}
	doc.IsActive = active
	body, err := doc.marshal()
	if err != nil {
		return fmt.Errorf("services.DraftWorkflowRepo.SetActive: %w", err)
	}
	if err := r.drafts.Upsert(ctx, tenantID, metaversion.UpsertInput{DocType: DocTypeWorkflow, DocKey: id, Op: metaversion.OpUpsert,
		Doc: body, EntityName: doc.EntityName, Label: doc.Name, UserID: draftActor(ctx, updatedBy)}); err != nil {
		return fmt.Errorf("services.DraftWorkflowRepo.SetActive: %w", err)
	}
	return nil
}

// state reads the live row and the draft row of one rule (either may be nil).
func (r *DraftWorkflowRepo) state(ctx context.Context, tenantID, id string) (*metaversion.DraftRow, *workflow.WorkflowRecord, error) {
	live, err := r.live.Get(ctx, tenantID, id)
	if err != nil {
		if !errors.Is(err, workflow.ErrWorkflowNotFound) {
			return nil, nil, err
		}
		live = nil
	}
	draft, err := r.drafts.GetByKey(ctx, tenantID, DocTypeWorkflow, id)
	if err != nil {
		return nil, nil, err
	}
	return draft, live, nil
}

// Get implements workflowRepo: the live rule, or — for the builder reading over the overlay — the
// draft of it ("edited"), the draft-only rule ("new") or the live rule marked "deleted".
func (r *DraftWorkflowRepo) Get(ctx context.Context, tenantID, id string) (*workflow.WorkflowRecord, error) {
	overlay, err := r.gate.ReadOverlay(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.DraftWorkflowRepo.Get: %w", err)
	}
	if !overlay {
		return r.live.Get(ctx, tenantID, id)
	}
	draft, live, err := r.state(ctx, tenantID, id)
	if err != nil {
		return nil, fmt.Errorf("services.DraftWorkflowRepo.Get: %w", err)
	}
	switch {
	case draft == nil && live == nil:
		return nil, workflow.ErrWorkflowNotFound
	case draft == nil:
		live.DraftState = draftStateLive
		return live, nil
	}
	rec, err := mergeWorkflowDraft(tenantID, *draft, live)
	if err != nil {
		return nil, fmt.Errorf("services.DraftWorkflowRepo.Get: %w", err)
	}
	if rec == nil { // a delete draft of a rule that is no longer live
		return nil, workflow.ErrWorkflowNotFound
	}
	return rec, nil
}

// List implements workflowRepo: live rules plus (overlay) the changes of the draft layer, newest first.
func (r *DraftWorkflowRepo) List(ctx context.Context, tenantID string) ([]workflow.WorkflowRecord, error) {
	liveRows, err := r.live.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	overlay, err := r.gate.ReadOverlay(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.DraftWorkflowRepo.List: %w", err)
	}
	if !overlay {
		return liveRows, nil
	}
	drafts, err := r.drafts.ListByType(ctx, tenantID, DocTypeWorkflow)
	if err != nil {
		return nil, fmt.Errorf("services.DraftWorkflowRepo.List: %w", err)
	}
	byID := make(map[string]metaversion.DraftRow, len(drafts))
	for _, d := range drafts {
		byID[d.DocKey] = d
	}
	out := make([]workflow.WorkflowRecord, 0, len(liveRows)+len(drafts))
	for i := range liveRows {
		live := liveRows[i]
		d, ok := byID[live.ID]
		if !ok {
			live.DraftState = draftStateLive
			out = append(out, live)
			continue
		}
		delete(byID, live.ID)
		rec, err := mergeWorkflowDraft(tenantID, d, &live)
		if err != nil {
			return nil, fmt.Errorf("services.DraftWorkflowRepo.List: %w", err)
		}
		out = append(out, *rec)
	}
	for _, d := range byID { // draft-only rules; a delete draft of a vanished rule yields nil
		rec, err := mergeWorkflowDraft(tenantID, d, nil)
		if err != nil {
			return nil, fmt.Errorf("services.DraftWorkflowRepo.List: %w", err)
		}
		if rec != nil {
			out = append(out, *rec)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { // same order as workflow.SQLRepository.List
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// CountActive implements workflowRepo. Over the overlay it counts the state AFTER publish (deleted
// rules out, new ones in, the draft's own isActive), so the cap on active before rules is judged on
// what the customer will run (FR-006).
func (r *DraftWorkflowRepo) CountActive(ctx context.Context, tenantID, entityName string, trigger workflow.Trigger, excludeID string) (int, error) {
	overlay, err := r.gate.ReadOverlay(ctx, tenantID)
	if err != nil {
		return 0, fmt.Errorf("services.DraftWorkflowRepo.CountActive: %w", err)
	}
	if !overlay {
		return r.live.CountActive(ctx, tenantID, entityName, trigger, excludeID)
	}
	rows, err := r.List(ctx, tenantID)
	if err != nil {
		return 0, fmt.Errorf("services.DraftWorkflowRepo.CountActive: %w", err)
	}
	n := 0
	for _, rec := range rows {
		if rec.ID != excludeID && rec.IsActive && rec.EntityName == entityName && rec.Trigger == string(trigger) && rec.DraftState != draftStateDeleted {
			n++
		}
	}
	return n, nil
}

// mergeWorkflowDraft renders a draft over its live rule (live may be nil). A delete draft keeps the
// live content and marks it "deleted"; it yields nil when there is no live rule left to mark.
func mergeWorkflowDraft(tenantID string, d metaversion.DraftRow, live *workflow.WorkflowRecord) (*workflow.WorkflowRecord, error) {
	if d.Op == metaversion.OpDelete {
		if live == nil {
			return nil, nil
		}
		out := *live
		out.DraftState = draftStateDeleted
		return &out, nil
	}
	var doc workflowDoc
	if err := json.Unmarshal(d.DraftJSON, &doc); err != nil {
		return nil, fmt.Errorf("decode draft %s: %w", d.DocKey, err)
	}
	rec := workflow.WorkflowRecord{
		ID: d.DocKey, TenantID: tenantID, Name: doc.Name, EntityName: doc.EntityName, Trigger: doc.Trigger,
		Condition: rawOrNull(doc.Condition), Actions: rawOrNull(doc.Actions), IsActive: doc.IsActive, IntervalSeconds: doc.IntervalSeconds,
		UpdatedBy: d.ModifiedBy, UpdatedAt: d.UpdatedAt, DraftState: draftStateNew,
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
func (r *DraftWorkflowRepo) WithRecorder(rec *VersionRecorder, ad metaversion.DocAdapter) *DraftWorkflowRepo {
	r.rec, r.adapter = rec, ad
	return r
}
