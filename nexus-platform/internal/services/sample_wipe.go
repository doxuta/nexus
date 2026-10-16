package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// Wipe (specs/064-preview-as-role-sample-data S9-7) removes every sample row the builder sees on the
// card — and nothing else. The delete itself is RecordService.PurgeSample, i.e. the 056 hard-delete
// in "root is a sample row" mode: blobs after the commit, the hard_delete trail, the dependents
// graph all come from there; this service only decides WHICH roots, in WHAT order, what is pinned
// and how a refusal reads to the builder. A row real data points at is kept, with its blockers.

// samplePurger is RecordService.PurgeSample.
type samplePurger interface {
	PurgeSample(ctx context.Context, scope RecordScope, tenantID, entityName, id string, pinned map[string]dynrepo.PurgePin) (*RecordPurgeReport, error)
}

// sampleEntityLister lists the tenant's live entities (the pinned-default scan covers all of them,
// not only the tables being wiped). Satisfied by *metadata.Repository.
type sampleEntityLister interface {
	ListEntities(tenantID string) ([]metadata.EntityDefinition, error)
}

var errWipeUnavailable = errors.New("sample wipe is not wired on this service")

// WithWipe wires the pieces Wipe needs. Without them Wipe is a wiring fault (500), never a no-op.
func (s *SampleDataService) WithWipe(p samplePurger, l sampleEntityLister) *SampleDataService {
	s.purger, s.lister = p, l
	return s
}

// WithLogger sets the logger of the wipe (a nil logger stays silent).
func (s *SampleDataService) WithLogger(l *zap.Logger) *SampleDataService {
	s.logger = l
	return s
}

// WipeRequest is POST /admin/sample-data/wipe. ExpectTotal is the number of sample rows the builder
// saw on the confirmation dialog: a pointer so that "missing" and "0" differ.
type WipeRequest struct {
	Entities    []string
	ExpectTotal *int
}

// SampleDeleted is how many sample rows one table lost (rows taken along by a cluster included).
type SampleDeleted struct {
	Entity string `json:"entity"`
	Count  int    `json:"count"`
}

// Why a sample row stays (WipeKept.Reason).
const (
	keptReferencedByReal = "referenced_by_real"
	keptFieldDefault     = "field_default"
	keptTooLarge         = "too_large"
	keptBusy             = "busy"
)

// SampleKept is a sample row the wipe left alone, with what blocks it.
type SampleKept struct {
	Entity    string                 `json:"entity"`
	RecordID  string                 `json:"recordId"`
	Reason    string                 `json:"reason"`
	BlockedBy []dynrepo.PurgeBlocker `json:"blockedBy"`
}

// SampleSequenceRef names an auto_number counter that was started over.
type SampleSequenceRef struct {
	Entity string `json:"entity"`
	Field  string `json:"field"`
}

// WipeResult is the report of one wipe; the arrays are never null.
type WipeResult struct {
	Deleted        []SampleDeleted     `json:"deleted"`
	Kept           []SampleKept        `json:"kept"`
	BlobsRemoved   int                 `json:"blobsRemoved"`
	BlobsNote      string              `json:"blobsNote,omitempty"`
	SequencesReset []SampleSequenceRef `json:"sequencesReset"`
}

type wipeRoot struct{ entity, id string }

// Wipe empties the tenant's sample data (entities empty = every table that has marks). 422
// SAMPLE_WIPE_CONFIRM_REQUIRED without ExpectTotal; 409 SAMPLE_WIPE_STALE {current} when the roots it
// reads are not exactly that many (nothing is deleted); 409 SAMPLE_BUSY while another sample batch runs.
// A clean error mid-way keeps the clusters already committed and the next run finishes the rest.
func (s *SampleDataService) Wipe(ctx context.Context, scope RecordScope, tenantID, userID string, req WipeRequest) (WipeResult, error) {
	res := WipeResult{Deleted: []SampleDeleted{}, Kept: []SampleKept{}, SequencesReset: []SampleSequenceRef{}}
	if req.ExpectTotal == nil {
		return res, invalid("SAMPLE_WIPE_CONFIRM_REQUIRED", "Cần xác nhận số dòng mẫu trước khi xoá")
	}
	if s.purger == nil || s.lister == nil {
		return res, fmt.Errorf("services.SampleDataService.Wipe: %w", errWipeUnavailable)
	}
	scopeNames, err := s.wipeScope(ctx, tenantID, dedupe(req.Entities))
	if err != nil {
		return res, err
	}
	release, ok := s.tryLock(tenantID)
	if !ok {
		return res, invalid("SAMPLE_BUSY", "Đang sinh hoặc xoá dữ liệu mẫu, thử lại sau")
	}
	defer release()

	// ONE read of the root list: the confirmed total is compared with, and the delete works through,
	// exactly this list (a row seeded after it stays for the next run).
	roots := map[string][]dynrepo.SampleRootRow{}
	total := 0
	for _, name := range scopeNames {
		rows, err := s.store.SampleRoots(ctx, tenantID, name)
		if err != nil {
			return res, fmt.Errorf("services.SampleDataService.Wipe: roots of %q: %w", name, err)
		}
		roots[name] = rows
		total += len(rows)
	}
	if total != *req.ExpectTotal {
		e := invalid("SAMPLE_WIPE_STALE", "Số dòng mẫu đã thay đổi, hãy xem lại").(*ValidationError)
		e.Details["current"] = total
		return res, e
	}

	meta, err := s.wipeMetadata(ctx, tenantID, scopeNames)
	if err != nil {
		return res, err
	}
	batches := map[string]string{} // "entity/id" -> batch id, for the audit summary
	var todo []wipeRoot
	for _, name := range meta.order {
		for _, r := range roots[name] {
			todo = append(todo, wipeRoot{name, r.ID})
			batches[name+"/"+r.ID] = r.BatchID
		}
	}

	w := &wipeRun{s: s, scope: scope, tenantID: tenantID, pinned: meta.pinned,
		gone: map[string]bool{}, kept: map[string]SampleKept{}, deleted: map[string]int{}}
	// The summary is written for whatever was deleted, also when the run stops on an error.
	defer func() { s.wipeSummary(ctx, tenantID, userID, w, batches) }()

	// Pass 1 walks child tables first; a root refused as too_large gets a second look in pass 2, when
	// the clusters that were too heavy because of OTHER sample rows have shrunk.
	for pass := 0; pass < 2 && len(todo) > 0; pass++ {
		var again []wipeRoot
		for _, r := range todo {
			retry, err := w.purge(ctx, r)
			if err != nil {
				return w.result(res), err
			}
			if retry {
				again = append(again, r)
			}
		}
		todo = again
	}
	out := w.result(res)
	if err := s.resetSequences(ctx, tenantID, meta, w.deleted, &out); err != nil {
		return out, err
	}
	return out, nil
}

// wipeScope resolves which tables a wipe covers: the named ones (each must be live) or every live
// table with marks (a table gone from metadata is skipped, as in Summary).
func (s *SampleDataService) wipeScope(ctx context.Context, tenantID string, named []string) ([]string, error) {
	if len(named) > 0 {
		for _, name := range named {
			if _, err := s.liveEntity(ctx, tenantID, name); err != nil {
				return nil, err
			}
		}
		return named, nil
	}
	names, err := s.store.SampleEntityNames(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.SampleDataService.Wipe: %w", err)
	}
	var live []string
	for _, name := range names {
		if _, err := s.liveEntity(ctx, tenantID, name); err != nil {
			if codeIs(err, "SAMPLE_ENTITY_NOT_FOUND") {
				continue
			}
			return nil, err
		}
		live = append(live, name)
	}
	return live, nil
}

// wipeMeta is what one wipe reads from metadata at run time.
type wipeMeta struct {
	order      []string                    // tables to wipe, child tables before the tables they point at
	pinned     map[string]dynrepo.PurgePin // records a live relation field defaults to
	entityID   map[string]string           // table -> entity id
	autoFields map[string][]string         // table -> its auto_number fields
}

// wipeMetadata reads every live table's fields once: the pinned set (relation fields with a default
// value), the relation edges that order the walk, and the auto_number fields to reset.
func (s *SampleDataService) wipeMetadata(ctx context.Context, tenantID string, scopeNames []string) (wipeMeta, error) {
	m := wipeMeta{pinned: map[string]dynrepo.PurgePin{}, entityID: map[string]string{}, autoFields: map[string][]string{}}
	ents, err := s.lister.ListEntities(tenantID)
	if err != nil {
		return m, fmt.Errorf("services.SampleDataService.Wipe: list entities: %w", err)
	}
	parents := map[string][]string{} // table -> tables its relation fields point at
	for _, ent := range ents {
		m.entityID[ent.Name] = ent.ID
		fields, err := s.catalog.ListFields(ctx, tenantID, ent.ID)
		if err != nil {
			return m, fmt.Errorf("services.SampleDataService.Wipe: fields of %q: %w", ent.Name, err)
		}
		for _, f := range fields {
			switch f.FieldType {
			case "auto_number":
				m.autoFields[ent.Name] = append(m.autoFields[ent.Name], f.Name)
			case "relation":
				target := wipeRelationTarget(f)
				if target == "" {
					continue
				}
				parents[ent.Name] = append(parents[ent.Name], target)
				if def := strings.TrimSpace(f.DefaultVal); def != "" {
					key := dynrepo.PinKey(target, def)
					if _, taken := m.pinned[key]; !taken {
						m.pinned[key] = dynrepo.PurgePin{Entity: ent.Name, Field: f.Name}
					}
				}
			}
		}
	}
	m.order = childrenFirst(scopeNames, parents)
	return m, nil
}

// wipeRelationTarget is the target table of a relation field (options.relatedEntity), "" if unreadable.
func wipeRelationTarget(f metadata.FieldDefinition) string {
	if f.Options == nil || *f.Options == "" {
		return ""
	}
	var opts struct {
		RelatedEntity string `json:"relatedEntity"`
	}
	if err := json.Unmarshal([]byte(*f.Options), &opts); err != nil {
		return ""
	}
	return opts.RelatedEntity
}

// childrenFirst orders names so a table comes before the tables it points at (parents[child] =
// targets). A cycle is broken at the first remaining name in alphabetical order: the order only
// saves noise (clearing references on rows that are deleted anyway), the repository is the referee.
func childrenFirst(names []string, parents map[string][]string) []string {
	remaining := append([]string(nil), names...)
	sort.Strings(remaining)
	in := map[string]bool{}
	for _, n := range remaining {
		in[n] = true
	}
	var out []string
	for len(remaining) > 0 {
		pick := -1
		for i, cand := range remaining {
			referenced := false
			for _, other := range remaining {
				if other == cand {
					continue
				}
				for _, p := range parents[other] {
					if p == cand {
						referenced = true
					}
				}
			}
			if !referenced {
				pick = i
				break
			}
		}
		if pick < 0 {
			pick = 0
		}
		out = append(out, remaining[pick])
		remaining = append(remaining[:pick], remaining[pick+1:]...)
	}
	return out
}

// wipeRun is the mutable state of one Wipe.
type wipeRun struct {
	s        *SampleDataService
	scope    RecordScope
	tenantID string
	pinned   map[string]dynrepo.PurgePin
	gone     map[string]bool       // "entity/id" removed by an earlier cluster
	kept     map[string]SampleKept // "entity/id" -> why it stayed
	order    []string              // kept keys in first-seen order
	deleted  map[string]int
	blobs    int
	notes    []string
}

// purge runs one root through RecordService.PurgeSample and files the outcome. retry=true asks for a
// second pass (too_large). A write conflict is retried once at once, then the row is kept as busy.
func (w *wipeRun) purge(ctx context.Context, r wipeRoot) (retry bool, err error) {
	key := r.entity + "/" + r.id
	if w.gone[key] {
		return false, nil
	}
	rep, err := w.s.purger.PurgeSample(ctx, w.scope, w.tenantID, r.entity, r.id, w.pinned)
	if errors.Is(err, dynrepo.ErrWriteConflict) {
		rep, err = w.s.purger.PurgeSample(ctx, w.scope, w.tenantID, r.entity, r.id, w.pinned)
	}
	var ve *ValidationError
	switch {
	case err == nil:
		for _, rec := range rep.Records {
			if k := rec.Entity + "/" + rec.ID; !w.gone[k] {
				w.gone[k] = true
				w.deleted[rec.Entity]++
			}
		}
		w.blobs += rep.BlobsRemoved
		if rep.BlobsNote != "" {
			w.notes = append(w.notes, rep.BlobsNote)
		}
		delete(w.kept, key)
		return false, nil
	case errors.Is(err, dynrepo.ErrRecordNotFound):
		return false, nil // already taken along by another cluster, or removed meanwhile
	case errors.Is(err, dynrepo.ErrWriteConflict):
		w.keep(r, keptBusy, nil)
		return false, nil
	case errors.As(err, &ve) && ve.Code == "RECORD_PURGE_LIVE_CHILDREN":
		blockers, _ := ve.Details["blockers"].([]dynrepo.PurgeBlocker)
		reason := keptReferencedByReal
		for _, b := range blockers {
			if b.Kind == dynrepo.BlockerFieldDefault {
				reason = keptFieldDefault
			}
		}
		w.keep(r, reason, blockers)
		return false, nil
	case errors.As(err, &ve) && ve.Code == "RECORD_PURGE_TOO_LARGE":
		w.keep(r, keptTooLarge, nil)
		return true, nil
	}
	return false, fmt.Errorf("services.SampleDataService.Wipe: purge %s/%s: %w", r.entity, r.id, err)
}

func (w *wipeRun) keep(r wipeRoot, reason string, blockers []dynrepo.PurgeBlocker) {
	key := r.entity + "/" + r.id
	if _, seen := w.kept[key]; !seen {
		w.order = append(w.order, key)
	}
	w.kept[key] = SampleKept{Entity: r.entity, RecordID: r.id, Reason: reason, BlockedBy: append([]dynrepo.PurgeBlocker{}, blockers...)}
}

// result folds the run into res: deleted per table (sorted), kept rows still standing (a row that a
// later cluster took along is not kept), the blob tally.
func (w *wipeRun) result(res WipeResult) WipeResult {
	names := make([]string, 0, len(w.deleted))
	for name := range w.deleted {
		names = append(names, name)
	}
	sort.Strings(names)
	res.Deleted = res.Deleted[:0]
	for _, name := range names {
		res.Deleted = append(res.Deleted, SampleDeleted{Entity: name, Count: w.deleted[name]})
	}
	res.Kept = res.Kept[:0]
	for _, key := range w.order {
		if k, ok := w.kept[key]; ok && !w.gone[key] {
			res.Kept = append(res.Kept, k)
		}
	}
	res.BlobsRemoved = w.blobs
	res.BlobsNote = strings.Join(w.notes, "; ")
	return res
}

// resetSequences starts over the auto_number counter of every table the wipe deleted from, but only
// where the table is now empty (bin included) — decided by the repository under a row lock.
func (s *SampleDataService) resetSequences(ctx context.Context, tenantID string, meta wipeMeta, deleted map[string]int, out *WipeResult) error {
	names := make([]string, 0, len(deleted))
	for name := range deleted {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, field := range meta.autoFields[name] {
			reset, err := s.store.ResetSequenceIfEmpty(ctx, tenantID, name, field)
			if err != nil {
				return fmt.Errorf("services.SampleDataService.Wipe: reset %s.%s: %w", name, field, err)
			}
			if reset {
				out.SequencesReset = append(out.SequencesReset, SampleSequenceRef{Entity: name, Field: field})
			}
		}
	}
	return nil
}

// wipeSummary appends the single __sample_data__/delete row of the run (the per-record hard_delete
// rows were appended by RecordService.Purge). Nothing deleted = nothing to record.
func (s *SampleDataService) wipeSummary(ctx context.Context, tenantID, userID string, w *wipeRun, batches map[string]string) {
	if len(w.deleted) == 0 {
		return
	}
	seen := map[string]bool{}
	batchIDs := []string{}
	for key := range w.gone {
		if b := batches[key]; b != "" && !seen[b] {
			seen[b] = true
			batchIDs = append(batchIDs, b)
		}
	}
	sort.Strings(batchIDs)
	kept := 0
	for key := range w.kept {
		if !w.gone[key] {
			kept++
		}
	}
	s.auditor.Record(ctx, audit.Event{
		TenantID: tenantID, ActorUserID: userID, EntityName: audit.SampleDataMeta, RecordID: uuid.NewString(),
		Action: audit.ActionDelete,
		Diff:   &audit.Diff{After: map[string]any{"deleted": w.deleted, "kept": kept, "batchIds": batchIDs}},
		At:     time.Now().UTC(),
	})
	if s.logger != nil {
		s.logger.Warn("services.SampleDataService.Wipe: sample data wiped",
			zap.String("tenant_id", tenantID), zap.String("user_id", userID), zap.Any("deleted", w.deleted), zap.Int("kept", kept))
	}
}
