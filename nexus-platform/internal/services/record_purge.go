package services

// Record purge — the bottom of the recycle bin (../specs/056-record-hard-delete/spec.md).
//
// The service owns what the repository cannot know: who may do it (an admin scope — the route adds
// the platform-operator gate in front), what depends on the record (the relationship graph, read
// from metadata — never hardcoded), what the operator is told (a report that never implies the
// bytes are gone when they are not), and the trail (one APPENDED hard_delete row per removed record
// and one update row per cleared reference, sensitive values masked — earlier rows are never
// rewritten, Q5 2026-09-05).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// recordPurger is the repository seam Purge runs through. Satisfied by *dynrepo.Repository.
type recordPurger interface {
	PurgeRecord(ctx context.Context, p dynrepo.PurgeParams) (*dynrepo.PurgeResult, error)
}

// relationshipLister reads what the purge graph is built from: the relationship registry, and the
// tenant's entities (a polymorphic_relation field registers no relationship row — its targets live
// in the field's own options, so the graph scans the fields). Satisfied by *metadata.Repository
// (WithRelationshipLister in main.go).
type relationshipLister interface {
	ListRelationshipsForEntity(ctx context.Context, tenantID, entityName string) ([]metadata.RelationshipDefinition, error)
	ListEntities(tenantID string) ([]metadata.EntityDefinition, error)
}

// errPurgeUnavailable: a seam the purge needs is not wired. A wiring fault (500), never a silent
// no-op — and never a purge that cannot see the record's dependents.
var errPurgeUnavailable = errors.New("record purge is not wired on this service")

// PurgedRecordRef names one record a purge removed (or, on a dry run, would remove).
type PurgedRecordRef struct {
	Entity string `json:"entity"`
	ID     string `json:"id"`
}

// PurgeClearedRef names one record that STAYS but whose optional reference to a purged record was
// emptied.
type PurgeClearedRef struct {
	Entity string `json:"entity"`
	ID     string `json:"id"`
	Field  string `json:"field"`
}

// RecordPurgeReport is what the operator reads before (dry run) and after a purge.
type RecordPurgeReport struct {
	Entity  string            `json:"entity"`
	ID      string            `json:"id"`
	DryRun  bool              `json:"dryRun"`
	Records []PurgedRecordRef `json:"records"`
	Cleared []PurgeClearedRef `json:"cleared"`
	// Blocked (dry run only): LIVE records that require a record in the purge. A real purge with
	// blockers is refused (409 RECORD_PURGE_LIVE_CHILDREN) — bin them first.
	Blocked   []dynrepo.PurgeBlocker `json:"blocked"`
	Tables    []PurgeTableCount      `json:"tables"`
	TotalRows int64                  `json:"totalRows"`
	// Blobs* describe the attachment BYTES, which no SQL statement reaches — stated separately so
	// a report can never read as "the files are gone" while they sit in storage (tenant-purge
	// lesson, tenant_purge.go PurgeReport).
	BlobsRemoved int    `json:"blobsRemoved"`
	BlobsNote    string `json:"blobsNote,omitempty"`
}

// WithBlobRemover wires the storage side of a record purge — production passes the same closure
// as TenantProvisionService.WithBlobRemover. Chainable.
func (s *RecordService) WithBlobRemover(f blobRemover) *RecordService {
	s.blobs = f
	return s
}

// WithRelationshipLister wires the relationship registry the purge graph is read from. Without it
// Purge refuses (it cannot see what depends on the record). Chainable.
func (s *RecordService) WithRelationshipLister(l relationshipLister) *RecordService {
	s.relationships = l
	return s
}

// Purge permanently removes one record that is already in the recycle bin, with everything that
// depends on it (specs/056 D1–D8).
//
//   - scope must be an admin (the route stands behind RequireAdmin → RequirePlatformOperator; this
//     re-check fails closed if the route is ever mounted on a weaker group) — checked before any read;
//   - the dependents graph comes from metadata (purgeGraph); the repository applies it in ONE tx;
//   - dryRun runs that transaction and rolls it back: no blob is touched, no audit row written —
//     the report (incl. blockers) is the exact preview of the real call;
//   - a real purge: commit → blobs (best-effort, reported) → the trail.
//
// Errors: ValidationError FORBIDDEN (403) / RECORD_NOT_IN_RECYCLE_BIN (409) /
// RECORD_PURGE_LIVE_CHILDREN (409, details.blockers) / RECORD_PURGE_TOO_LARGE (422, details.max);
// ErrEntityNotFound, ErrRecordNotFound (404) and ErrWriteConflict (503) pass through errors.Is.
func (s *RecordService) Purge(ctx context.Context, scope RecordScope, tenantID, entityName, id string, dryRun bool) (*RecordPurgeReport, error) {
	return s.purge(ctx, scope, tenantID, entityName, id, dryRun, false, nil)
}

// PurgeSample is Purge for a SAMPLE row (specs/064 S9-7): the root may be live or in the bin but must
// carry a sample mark, required children follow only when marked, and anything unmarked that depends
// on the cluster refuses it (RECORD_PURGE_LIVE_CHILDREN, blockers in details — Kind
// dynrepo.BlockerFieldDefault for a record pinned by a field default). pinned is
// dynrepo.PurgeParams.Pinned. Same tail as Purge: blobs after the commit, one hard_delete row per
// removed record. Always a real purge — the wipe's preview is the confirmation dialog's count.
func (s *RecordService) PurgeSample(ctx context.Context, scope RecordScope, tenantID, entityName, id string, pinned map[string]dynrepo.PurgePin) (*RecordPurgeReport, error) {
	return s.purge(ctx, scope, tenantID, entityName, id, false, true, pinned)
}

func (s *RecordService) purge(ctx context.Context, scope RecordScope, tenantID, entityName, id string, dryRun, sample bool, pinned map[string]dynrepo.PurgePin) (*RecordPurgeReport, error) {
	if !scope.IsAdmin {
		return nil, &ValidationError{Code: "FORBIDDEN", Msg: "Chỉ người vận hành nền tảng được xoá vĩnh viễn bản ghi"}
	}
	e, specs, err := s.resolve(ctx, tenantID, entityName)
	if err != nil {
		return nil, err
	}
	if s.purger == nil || s.relationships == nil {
		return nil, fmt.Errorf("services.RecordService.Purge: %w", errPurgeUnavailable)
	}
	graph, metas, err := s.purgeGraph(ctx, tenantID, e, specs)
	if err != nil {
		return nil, err
	}
	res, err := s.purger.PurgeRecord(ctx, dynrepo.PurgeParams{
		Entity: e.Name, TenantID: tenantID, ID: id, DryRun: dryRun, ActorID: scope.UserID, Graph: graph,
		SampleRoot: sample, Pinned: pinned,
	})
	if err != nil {
		return nil, purgeRefusal(err)
	}

	// Lists are [] (never null) so a client iterates without a guard.
	report := &RecordPurgeReport{Entity: e.Name, ID: id, DryRun: dryRun, Tables: res.Tables,
		Records: []PurgedRecordRef{}, Cleared: []PurgeClearedRef{}, Blocked: append([]dynrepo.PurgeBlocker{}, res.Blocked...)}
	for _, rec := range res.Records {
		report.Records = append(report.Records, PurgedRecordRef{Entity: rec.Entity, ID: rec.ID})
	}
	for _, c := range res.Cleared {
		report.Cleared = append(report.Cleared, PurgeClearedRef{Entity: c.Entity, ID: c.ID, Field: c.Field})
	}
	for _, tc := range res.Tables {
		report.TotalRows += tc.Rows
	}
	files := len(res.FileIDs)
	if dryRun {
		if files > 0 {
			report.BlobsNote = fmt.Sprintf("%d tệp đính kèm sẽ bị xoá khỏi kho lưu trữ", files)
		}
		return report, nil
	}

	// Bytes AFTER the commit: a rolled-back purge must never have destroyed files, and a blob the
	// catalog no longer names is dead weight while a missing blob under a live row is a broken
	// download. Best-effort — a storage fault must not undo a committed purge — but never silent.
	switch {
	case files == 0:
	case s.blobs == nil:
		report.BlobsNote = fmt.Sprintf(
			"CHƯA xoá %d tệp trong kho lưu trữ (chức năng xoá tệp chưa được nối) — dữ liệu tệp VẪN CÒN trên đĩa", files)
	default:
		removed, berr := s.blobs(ctx, tenantID, res.FileIDs)
		report.BlobsRemoved = removed
		if berr != nil || removed < files {
			report.BlobsNote = fmt.Sprintf("xoá được %d/%d tệp; số còn lại vẫn nằm trong kho lưu trữ", removed, files)
			s.warn("services.RecordService.Purge: some blobs survived", tenantID, e.Name, id, scope.UserID,
				zap.Int("removed", removed), zap.Int("total", files), zap.Error(berr))
		}
	}

	s.purgeTrail(ctx, tenantID, scope.UserID, res, metas)
	s.warn("services.RecordService.Purge: record purged", tenantID, e.Name, id, scope.UserID,
		zap.Int("records", len(res.Records)), zap.Int("cleared", len(res.Cleared)),
		zap.Int64("rows", report.TotalRows), zap.Int("files", files))
	return report, nil
}

// purgeRefusal maps the repository's refusals to the API contract; anything else passes through
// (errors.Is keeps ErrRecordNotFound / ErrWriteConflict visible to the handler).
func purgeRefusal(err error) error {
	var blocked *dynrepo.PurgeBlockedError
	var tooLarge *dynrepo.PurgeTooLargeError
	switch {
	case errors.Is(err, dynrepo.ErrRecordNotInBin):
		return &ValidationError{
			Code: "RECORD_NOT_IN_RECYCLE_BIN",
			Msg:  "bản ghi này còn đang dùng — xoá nó (vào thùng rác) trước rồi mới xoá vĩnh viễn được",
		}
	case errors.As(err, &blocked):
		return &ValidationError{
			Code:    "RECORD_PURGE_LIVE_CHILDREN",
			Msg:     "còn bản ghi đang dùng bắt buộc phải có bản ghi này — xoá chúng (vào thùng rác) trước rồi mới xoá vĩnh viễn được",
			Details: map[string]any{"blockers": blocked.Blocked},
		}
	case errors.As(err, &tooLarge):
		return &ValidationError{
			Code:    "RECORD_PURGE_TOO_LARGE",
			Msg:     fmt.Sprintf("lần xoá vĩnh viễn này chạm tới hơn %d bản ghi — quá lớn cho một lần", tooLarge.Max),
			Details: map[string]any{"max": tooLarge.Max},
		}
	}
	return fmt.Errorf("services.RecordService.Purge: %w", err)
}

// purgeEntityMeta is what the trail needs per entity of the graph (redaction is per entity).
type purgeEntityMeta struct {
	id    string
	specs []FieldSpec
}

// purgeGraph resolves, from the relationship registry + each child's LIVE relation field, what
// depends on records of root — and, transitively, of every entity whose records the purge may take
// along (required children). Metadata-first: nothing about entities is hardcoded.
//
//   - N-N (either side) ⇒ the partner is a join peer (link rows go);
//   - 1-N / 1-1 where this entity is the PARENT ⇒ the child's field is a dependent: required ⇒ the
//     child entity joins the graph (its binned records go along), optional ⇒ references are cleared;
//   - where this entity is the CHILD, the FK lives on the purged row itself — nothing to do;
//   - a live polymorphic_relation field anywhere in the tenant whose options.targetEntities names
//     this entity ⇒ a dependent too (Polymorphic: matched on {type, id}), same required/optional rule;
//   - a child entity that no longer exists, or a registry row whose field was removed (the column
//     may be gone — Doctor B2's drift), is skipped: there is nothing live to reach through it.
func (s *RecordService) purgeGraph(ctx context.Context, tenantID string, root *metadata.EntityDefinition, rootSpecs []FieldSpec) (map[string]dynrepo.PurgeEntityDeps, map[string]purgeEntityMeta, error) {
	graph := map[string]dynrepo.PurgeEntityDeps{}
	metas := map[string]purgeEntityMeta{root.Name: {id: root.ID, specs: rootSpecs}}
	poly, err := s.polymorphicRefs(ctx, tenantID, metas)
	if err != nil {
		return nil, nil, err
	}
	queue := []string{root.Name}
	for i := 0; i < len(queue); i++ {
		name := queue[i]
		rels, err := s.relationships.ListRelationshipsForEntity(ctx, tenantID, name)
		if err != nil {
			return nil, nil, fmt.Errorf("services.RecordService.purgeGraph: %w", err)
		}
		deps := dynrepo.PurgeEntityDeps{}
		for _, rel := range rels {
			switch rel.RelationshipType {
			case "N-N":
				peer := rel.ChildEntityName
				if peer == name {
					peer = rel.ParentEntityName
				}
				if peer != name {
					deps.JoinPeers = append(deps.JoinPeers, peer)
				}
			case "1-N", "1-1":
				if rel.ParentEntityName != name {
					continue
				}
				child, ok := metas[rel.ChildEntityName]
				if !ok {
					ce, cspecs, err := s.resolve(ctx, tenantID, rel.ChildEntityName)
					if errors.Is(err, ErrEntityNotFound) {
						continue
					}
					if err != nil {
						return nil, nil, fmt.Errorf("services.RecordService.purgeGraph: %w", err)
					}
					child = purgeEntityMeta{id: ce.ID, specs: cspecs}
					metas[ce.Name] = child
				}
				field := strings.TrimSuffix(rel.ChildFKColumn, "_id")
				var spec *FieldSpec
				for j := range child.specs {
					if child.specs[j].Name == field && child.specs[j].FieldType == "relation" {
						spec = &child.specs[j]
						break
					}
				}
				if spec == nil {
					continue
				}
				deps.Children = append(deps.Children, dynrepo.PurgeChildRef{Entity: rel.ChildEntityName, Field: field,
					Required: spec.IsRequired, OneToOne: relationCardinality(spec.Options) == "1-1"})
			}
		}
		deps.Children = append(deps.Children, poly[name]...)
		for _, ref := range deps.Children {
			if _, seen := graph[ref.Entity]; ref.Required && !seen && !slices.Contains(queue, ref.Entity) {
				queue = append(queue, ref.Entity)
			}
		}
		graph[name] = deps
	}
	return graph, metas, nil
}

// polymorphicRefs maps each target entity to the live polymorphic_relation fields that may point at
// it, scanning every live entity's fields once per purge (through the catalog — cached in
// production). Entities it reads join metas so the trail can redact their cleared references.
// ponytail: O(entities) field reads per purge — an operator action; a registry row per polymorphic
// field would make it one query if purges ever become frequent.
func (s *RecordService) polymorphicRefs(ctx context.Context, tenantID string, metas map[string]purgeEntityMeta) (map[string][]dynrepo.PurgeChildRef, error) {
	ents, err := s.relationships.ListEntities(tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.RecordService.polymorphicRefs: %w", err)
	}
	out := map[string][]dynrepo.PurgeChildRef{}
	for _, ent := range ents {
		m, ok := metas[ent.Name]
		if !ok {
			fields, err := s.catalog.ListFields(ctx, tenantID, ent.ID)
			if err != nil {
				return nil, fmt.Errorf("services.RecordService.polymorphicRefs: %w", err)
			}
			specs, err := toFieldSpecs(fields)
			if err != nil {
				return nil, err
			}
			m = purgeEntityMeta{id: ent.ID, specs: specs}
			metas[ent.Name] = m
		}
		for _, sp := range m.specs {
			if sp.FieldType != "polymorphic_relation" {
				continue
			}
			targets, _ := polymorphicTargetList(sp.Options)
			for _, target := range targets {
				out[target] = append(out[target], dynrepo.PurgeChildRef{Entity: ent.Name, Field: sp.Name,
					Required: sp.IsRequired, Polymorphic: true})
			}
		}
	}
	return out, nil
}

// purgeTrail appends the audit rows of a committed purge. Appended, never rewritten: the records'
// create/update/delete rows keep their values (Q5a). Removed records: hard_delete, before-only, same
// shape as the soft delete's row. Cleared references: an update row (field: purged id → null), the
// way a partial PUT clearing it would read. Sensitive values masked per entity (#19).
func (s *RecordService) purgeTrail(ctx context.Context, tenantID, actor string, res *dynrepo.PurgeResult, metas map[string]purgeEntityMeta) {
	redact := map[string][]FieldSpec{}
	specsFor := func(entity string) []FieldSpec {
		if r, ok := redact[entity]; ok {
			return r
		}
		m := metas[entity]
		r := s.redactionSpecs(ctx, tenantID, m.id, m.specs)
		redact[entity] = r
		return r
	}
	now := time.Now().UTC()
	for _, rec := range res.Records {
		s.auditor.Record(ctx, audit.Event{
			TenantID: tenantID, ActorUserID: actor, EntityName: rec.Entity, RecordID: rec.ID,
			Action: audit.ActionHardDelete,
			Diff:   redactSensitiveDiff(specsFor(rec.Entity), audit.ComputeDiff(rec.Data, nil)),
			At:     now,
		})
	}
	for _, c := range res.Cleared {
		s.auditor.Record(ctx, audit.Event{
			TenantID: tenantID, ActorUserID: actor, EntityName: c.Entity, RecordID: c.ID,
			Action: audit.ActionUpdate,
			Diff:   redactSensitiveDiff(specsFor(c.Entity), audit.ComputeDiff(map[string]any{c.Field: c.Was}, map[string]any{c.Field: nil})),
			At:     now,
		})
	}
}

// warn logs a purge event with the fields an operator greps for. Nil-logger safe (unit tests).
func (s *RecordService) warn(msg, tenantID, entity, id, actor string, extra ...zap.Field) {
	if s.logger == nil {
		return
	}
	s.logger.Warn(msg, append([]zap.Field{
		zap.String("tenant_id", tenantID), zap.String("entity_name", entity),
		zap.String("record_id", id), zap.String("user_id", actor),
	}, extra...)...)
}
