package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/metadiff"
	"nexus/platform/internal/metaversion"
)

// PropagationSeconds is the ceiling after a publish for another server replica to serve the new
// version (the longest TTL of the caches that hold published documents; plan §12). Returned so the
// UI can say it.
const PropagationSeconds = 60

// maxPublishNoteRunes mirrors sys_meta_versions.note VARCHAR(500): checked up front, otherwise
// MySQL 1406 would surface as a 500.
const maxPublishNoteRunes = 500

var changesetHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// PublishService owns the builder's publish lifecycle on a delivered workspace: state, the change
// list, the first publish ("baseline", which turns draft mode on), publish, and discard. Publish is
// ONE sqlx transaction under tenants ... FOR UPDATE: all documents, the version row, the version docs
// and the draft cleanup commit together or not at all (FR-021). HTTP-agnostic: it returns
// *ValidationError values the handler maps.
type PublishService struct {
	db       *sqlx.DB
	drafts   *metaversion.DraftRepo
	versions *metaversion.VersionRepo
	reg      *metaversion.Registry
	gate     *DraftGate
	auditor  audit.Auditor
	logger   *zap.Logger
	// invalidators drop the engine cache of one document type after a publish commits: (tenant, entity).
	invalidators map[string]func(tenantID, entityName string)
	// fieldDel / entityDel run a deferred field / table delete after the publish commits (S7-9).
	fieldDel  schemaFieldDeleter
	entityDel schemaEntityDeleter
	// revert undoes one applied-at-once entry (S7-7b); nil = Revert is refused.
	revert revertTargets
	// afterPendingRead is a test seam: it runs inside Publish right after the pending set was read (nil in production).
	afterPendingRead func()
	// aclCache is the role-grants cache (O3). A rollback that brings a table back puts its role grants back too, so
	// that commit drops the cached grants. nil = no cache.
	aclCache *CachedACLLoader
}

// NewPublishService wires the service. auditor and logger may be nil.
func NewPublishService(db *sqlx.DB, drafts *metaversion.DraftRepo, versions *metaversion.VersionRepo, reg *metaversion.Registry,
	gate *DraftGate, auditor audit.Auditor, logger *zap.Logger) *PublishService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &PublishService{db: db, drafts: drafts, versions: versions, reg: reg, gate: gate, auditor: auditor, logger: logger,
		invalidators: map[string]func(string, string){}}
}

// WithInvalidator registers the post-commit cache drop of one document type. Chainable.
func (s *PublishService) WithInvalidator(docType string, fn func(tenantID, entityName string)) *PublishService {
	s.invalidators[docType] = fn
	return s
}

// WithACLCache wires the role-grants cache that a table restore refreshes. Chainable.
func (s *PublishService) WithACLCache(c *CachedACLLoader) *PublishService {
	s.aclCache = c
	return s
}

// PublishState is GET /admin/publish/state.
type PublishState struct {
	Delivered          bool         `json:"delivered"`
	HeadVersion        *HeadVersion `json:"headVersion"`
	PendingCount       int          `json:"pendingCount"`
	ChangesetHash      string       `json:"changesetHash"`
	DraftableTypes     []string     `json:"draftableTypes"`
	DirectTypes        []string     `json:"directTypes"`
	PropagationSeconds int          `json:"propagationSeconds"`
	// SchemaRetry (S7-9): the newest version whose field / table deletes did not finish, with what is
	// left; null when nothing needs retrying. The UI shows "Xoá chưa xong · Thử lại" from it.
	SchemaRetry *SchemaRetryRef `json:"schemaRetry"`
}

// SchemaRetryRef names a version with unfinished deferred deletes and the deletes still to run.
type SchemaRetryRef struct {
	VersionID string           `json:"versionId"`
	VersionNo uint             `json:"versionNo"`
	Ops       []SchemaOpResult `json:"ops"`
}

// HeadVersion is the newest numbered version, as the state shows it.
type HeadVersion struct {
	VersionNo   uint      `json:"versionNo"`
	Kind        string    `json:"kind"`
	PublishedAt time.Time `json:"publishedAt"`
	PublishedBy string    `json:"publishedBy"`
}

// ChangeItem is one pending draft as the builder's change list shows it. Bodies are NOT included
// (the list never carries a document, so it cannot leak a secret).
type ChangeItem struct {
	ID         string    `json:"id"`
	DocType    string    `json:"docType"`
	DocKey     string    `json:"docKey"`
	Label      string    `json:"label"`
	Op         string    `json:"op"` // create | update | delete
	EntityName string    `json:"entityName"`
	UpdatedAt  time.Time `json:"updatedAt"`
	UpdatedBy  string    `json:"updatedBy"`
	Conflict   bool      `json:"conflict"`
	// ChangedPaths is where an update differs from the live document (<= metadiff.Cap); MoreChanged
	// counts the paths beyond the cap. Empty for create / delete (S7-6).
	ChangedPaths []string `json:"changedPaths"`
	MoreChanged  int      `json:"moreChanged"`
	// Warnings (S7-9): what a pending delete leaves dangling, e.g. a report that uses the field.
	Warnings []string `json:"warnings,omitempty"`
	// Discardable: false for a table / field born after the last publish that has no draft of its own —
	// there is nothing to drop, Publish releases it (delete it to take it back).
	Discardable bool `json:"discardable"`
}

// PublishRequest is POST /admin/publish.
type PublishRequest struct {
	ChangesetHash string
	Note          string
	Force         bool
}

// PublishResult is the answer of baseline and publish.
type PublishResult struct {
	VersionNo          uint      `json:"versionNo"`
	Kind               string    `json:"kind"`
	DocCount           int       `json:"docCount"`
	PublishedAt        time.Time `json:"publishedAt"`
	PropagationSeconds int       `json:"propagationSeconds"`
	// SchemaStatus / SchemaOps (S7-9): present only when the publish carried a field / table delete.
	SchemaStatus string           `json:"schemaStatus,omitempty"`
	SchemaOps    []SchemaOpResult `json:"schemaOps,omitempty"`
}

func errForbidden() error {
	// Byte-identical with middleware.RequireAdmin (the handler writes only {error, code} for it).
	return &ValidationError{Code: "FORBIDDEN", Msg: "Yêu cầu quyền quản trị viên"}
}

func errInvalidBody(format string, args ...any) error {
	return invalid("INVALID_BODY", format, args...)
}

// builder resolves the caller and refuses anyone who is not the builder of THIS tenant: the route
// sits on builderGated, and the service re-checks (the operator tier reads the DB per request, the
// Caller comes from the Subject the Auth stage rebuilt).
func (s *PublishService) builder(ctx context.Context, tenantID string) (metaversion.Caller, error) {
	c, ok := metaversion.CallerFrom(ctx)
	if !ok || !c.IsBuilder || c.TenantID != tenantID {
		return metaversion.Caller{}, errForbidden()
	}
	return c, nil
}

// State reports whether the tenant is delivered, its head version, how many drafts wait, and which
// document types go through drafts vs apply at once.
func (s *PublishService) State(ctx context.Context, tenantID string) (*PublishState, error) {
	if _, err := s.builder(ctx, tenantID); err != nil {
		return nil, err
	}
	st := &PublishState{DraftableTypes: metaversion.TypesOfKind(metaversion.Draft), DirectTypes: metaversion.TypesOfKind(metaversion.DirectTraced),
		PropagationSeconds: PropagationSeconds}
	if st.DirectTypes == nil {
		st.DirectTypes = []string{}
	}
	delivered, err := s.gate.Delivered(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.PublishService.State: %w", err)
	}
	st.Delivered = delivered
	rows, err := s.drafts.ListRead(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.PublishService.State: %w", err)
	}
	if rows, err = s.pendingRows(ctx, s.db, tenantID, rows); err != nil {
		return nil, fmt.Errorf("services.PublishService.State: %w", err)
	}
	st.PendingCount = len(rows)
	st.ChangesetHash = metaversion.ChangesetHash(rows)
	if delivered {
		head, err := s.versions.HeadVersionRead(ctx, tenantID)
		if err != nil {
			return nil, fmt.Errorf("services.PublishService.State: %w", err)
		}
		if head != nil && head.VersionNo != nil {
			st.HeadVersion = &HeadVersion{VersionNo: *head.VersionNo, Kind: head.Kind, PublishedAt: head.CreatedAt, PublishedBy: head.CreatedByLabel}
		}
		if st.SchemaRetry, err = s.schemaRetryRef(ctx, tenantID); err != nil {
			return nil, fmt.Errorf("services.PublishService.State: %w", err)
		}
	}
	return st, nil
}

// Changes lists the pending drafts with a conflict flag (the live document moved since the draft was
// started) and the fingerprint of exactly this list, which publish and discard must echo back.
func (s *PublishService) Changes(ctx context.Context, tenantID string) ([]ChangeItem, string, error) {
	if _, err := s.builder(ctx, tenantID); err != nil {
		return nil, "", err
	}
	rows, err := s.drafts.ListRead(ctx, tenantID)
	if err != nil {
		return nil, "", fmt.Errorf("services.PublishService.Changes: %w", err)
	}
	if rows, err = s.pendingRows(ctx, s.db, tenantID, rows); err != nil {
		return nil, "", fmt.Errorf("services.PublishService.Changes: %w", err)
	}
	out := make([]ChangeItem, 0, len(rows))
	for _, d := range rows {
		if isHiddenNew(d) { // born live, no draft: nothing to compare, nothing to conflict, nothing to discard
			item := ChangeItem{ID: d.ID, DocType: d.DocType, DocKey: d.DocKey, Label: d.Label, Op: "create", UpdatedAt: d.UpdatedAt, ChangedPaths: []string{}}
			if d.EntityName != nil {
				item.EntityName = *d.EntityName
			}
			out = append(out, item)
			continue
		}
		ad, ok := s.reg.Get(d.DocType)
		if !ok {
			continue // a row of a type this build no longer knows: not publishable, not listed
		}
		live, exists, err := ad.LoadLive(ctx, s.db, tenantID, d.DocKey, false)
		if err != nil {
			return nil, "", fmt.Errorf("services.PublishService.Changes: %w", err)
		}
		conflict, err := draftConflicts(d, live, exists)
		if err != nil {
			return nil, "", fmt.Errorf("services.PublishService.Changes: %w", err)
		}
		item := ChangeItem{ID: d.ID, DocType: d.DocType, DocKey: d.DocKey, Label: d.Label, Op: changeOp(d), UpdatedAt: d.UpdatedAt,
			UpdatedBy: d.CreatedBy, Conflict: conflict, ChangedPaths: []string{}, Discardable: true}
		if d.Op != metaversion.OpDelete && exists {
			if paths, more := changedPaths(d.DocType, live, d.DraftJSON); paths != nil {
				item.ChangedPaths, item.MoreChanged = paths, more
			}
		}
		if d.EntityName != nil {
			item.EntityName = *d.EntityName
		}
		if d.ModifiedBy != nil {
			item.UpdatedBy = *d.ModifiedBy
		}
		if item.Warnings, err = s.schemaWarnings(ctx, tenantID, d); err != nil {
			return nil, "", fmt.Errorf("services.PublishService.Changes: %w", err)
		}
		out = append(out, item)
	}
	return out, metaversion.ChangesetHash(rows), nil
}

func changeOp(d metaversion.DraftRow) string {
	switch {
	case d.Op == metaversion.OpDelete:
		return "delete"
	case d.BaseHash == nil:
		return "create"
	default:
		return "update"
	}
}

// draftConflicts: the live document is not the one the draft was started from.
func draftConflicts(d metaversion.DraftRow, live json.RawMessage, exists bool) (bool, error) {
	if d.BaseHash == nil { // started from "no live document"
		return exists, nil
	}
	if !exists {
		return true, nil
	}
	h, err := metaversion.DocHash(live)
	if err != nil {
		return false, fmt.Errorf("hash live %s/%s: %w", d.DocType, d.DocKey, err)
	}
	return h != *d.BaseHash, nil
}

func isBusy(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == 1213 || me.Number == 1205) // deadlock / lock wait timeout
}

func (s *PublishService) fail(op string, err error) error {
	if isBusy(err) {
		return invalid("PUBLISH_BUSY", "Hệ thống đang bận xuất bản, thử lại sau vài giây")
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		return err
	}
	return fmt.Errorf("services.PublishService.%s: %w", op, err)
}

// lockTenant takes tenants ... FOR UPDATE: publish / baseline / rollback / discard / a direct history entry of one
// tenant run one at a time, and draft saves and schema writes (FOR SHARE on the same row) wait for them. It is the
// first statement of its transaction: the tenants row before any sys_* row, the lock order of all of them.
func lockTenant(ctx context.Context, tx *sqlx.Tx, tenantID string) error {
	var id string
	if err := tx.GetContext(ctx, &id, `SELECT id FROM tenants WHERE id = ? FOR UPDATE`, tenantID); err != nil {
		return fmt.Errorf("lock tenant: %w", err)
	}
	return nil
}

func (s *PublishService) validateNote(note string) error {
	if utf8.RuneCountInString(note) > maxPublishNoteRunes {
		return errInvalidBody("ghi chú tối đa %d ký tự", maxPublishNoteRunes)
	}
	return nil
}

func validateChangesetHash(h string) error {
	if !changesetHashRe.MatchString(h) {
		return errInvalidBody("changesetHash phải là chuỗi 64 ký tự hex")
	}
	return nil
}

// snapshotTypes records the live documents of the given types as "snapshot" version docs (the state a
// later rollback restores) and returns how many it wrote.
func (s *PublishService) snapshotTypes(ctx context.Context, tx *sqlx.Tx, types []string, tenantID string, v *metaversion.Version, userID string) (int, error) {
	n := 0
	for _, t := range types {
		ad, err := s.reg.Must(t)
		if err != nil {
			return 0, err
		}
		refs, err := ad.ListLiveKeys(ctx, tx, tenantID)
		if err != nil {
			return 0, err
		}
		for _, ref := range refs {
			doc, ok, err := ad.LoadLive(ctx, tx, tenantID, ref.Key, false)
			if err != nil {
				return 0, err
			}
			if !ok {
				continue
			}
			if err := s.versions.InsertDoc(ctx, tx, metaversion.VersionDoc{TenantID: tenantID, VersionID: v.ID, Seq: v.Seq, DocType: t, DocKey: ref.Key,
				Label: ref.Label, EntityName: ref.EntityName, Op: "snapshot", AfterJSON: doc, CreatedByID: userID}); err != nil {
				return 0, err
			}
			n++
		}
	}
	return n, nil
}

// Baseline is "Xuất bản lần đầu": it snapshots every document of every registered type as version 1
// and thereby delivers the tenant — from then on writes of draftable types go to drafts. One-way.
func (s *PublishService) Baseline(ctx context.Context, tenantID, note string) (*PublishResult, error) {
	caller, err := s.builder(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if err := s.validateNote(note); err != nil {
		return nil, err
	}
	if err := s.settleSecond(ctx, tenantID); err != nil {
		return nil, s.fail("Baseline", err)
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("services.PublishService.Baseline: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockTenant(ctx, tx, tenantID); err != nil {
		return nil, s.fail("Baseline", err)
	}
	delivered, err := s.versions.HasNumberedTx(ctx, tx, tenantID)
	if err != nil {
		return nil, s.fail("Baseline", err)
	}
	if delivered {
		return nil, invalid("ALREADY_DELIVERED", "Dự án đã được giao: dùng Xuất bản để đẩy thay đổi")
	}
	seq, err := s.versions.NextSeq(ctx, tx, tenantID)
	if err != nil {
		return nil, s.fail("Baseline", err)
	}
	no := uint(1)
	types := s.reg.Types()
	covered, _ := json.Marshal(types)
	v := &metaversion.Version{TenantID: tenantID, Seq: seq, VersionNo: &no, Kind: metaversion.KindBaseline, Note: note, CoveredTypes: covered,
		CreatedBy: caller.UserID, CreatedByLabel: s.versions.ActorLabel(ctx, tx, caller.HomeTenantID, caller.UserID)}
	cutoff, err := s.releaseCutoff(ctx, tx, tenantID, nil) // it releases every definition there is
	if err != nil {
		return nil, s.fail("Baseline", err)
	}
	v.SchemaCutoffAt = &cutoff
	v.ID = uuid.NewString() // the snapshot docs hang from it; the row itself goes in once the count is known (append-only)
	count, err := s.snapshotTypes(ctx, tx, types, tenantID, v, caller.UserID)
	if err != nil {
		return nil, s.fail("Baseline", err)
	}
	v.DocCount = uint(count)
	if err := s.versions.InsertVersion(ctx, tx, v); err != nil {
		return nil, s.fail("Baseline", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, s.fail("Baseline", err)
	}
	s.gate.CacheDelivered(tenantID)
	s.gate.RefreshCutoff(tenantID, cutoff) // S7-8: what this version released stops being hidden at once on this replica
	s.audit(ctx, caller, tenantID, v.ID, "baseline", map[string]any{"versionNo": 1, "kind": metaversion.KindBaseline, "docCount": count})
	s.logger.Info("services.PublishService.Baseline: tenant delivered",
		zap.String("tenant_id", tenantID), zap.String("user_id", caller.UserID), zap.Int("doc_count", count))
	return &PublishResult{VersionNo: 1, Kind: metaversion.KindBaseline, DocCount: count, PublishedAt: time.Now().UTC(), PropagationSeconds: PropagationSeconds}, nil
}

// Publish pushes the exact set of drafts the builder looked at (changesetHash) to the live
// documents, records the version with before/after of each, and clears the drafts — in ONE tx.
func (s *PublishService) Publish(ctx context.Context, tenantID string, req PublishRequest) (*PublishResult, error) {
	caller, err := s.builder(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if err := validateChangesetHash(req.ChangesetHash); err != nil {
		return nil, err
	}
	if err := s.validateNote(req.Note); err != nil {
		return nil, err
	}
	if err := s.settleSecond(ctx, tenantID); err != nil {
		return nil, s.fail("Publish", err)
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("services.PublishService.Publish: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockTenant(ctx, tx, tenantID); err != nil {
		return nil, s.fail("Publish", err)
	}
	delivered, err := s.versions.HasNumberedTx(ctx, tx, tenantID)
	if err != nil {
		return nil, s.fail("Publish", err)
	}
	if !delivered {
		return nil, invalid("NOT_DELIVERED", "Dự án chưa giao: bấm Xuất bản lần đầu trước")
	}
	drafts, err := s.drafts.List(ctx, tx, tenantID, true)
	if err != nil {
		return nil, s.fail("Publish", err)
	}
	// Read in the tx's snapshot, taken after the tenant lock: a schema write committed before it is read here, and
	// one waiting for the lock is stamped after this publish commits (releaseCutoff).
	pending, headCut, err := s.pending(ctx, tx, tenantID, drafts)
	if err != nil {
		return nil, s.fail("Publish", err)
	}
	if s.afterPendingRead != nil {
		s.afterPendingRead()
	}
	if len(pending) == 0 {
		return nil, invalid("NOTHING_TO_PUBLISH", "Không có gì để xuất bản")
	}
	if cur := metaversion.ChangesetHash(pending); cur != req.ChangesetHash {
		return nil, &ValidationError{Code: "DRAFTS_CHANGED", Msg: "Bản nháp đã đổi, đã tải lại danh sách", Details: map[string]any{"changesetHash": cur}}
	}
	_, hidden := splitHidden(pending)
	// Same snapshot as the pending read: everything the new cutoff will uncover, listed or folded into a listed row.
	cutoff, err := s.releaseCutoff(ctx, tx, tenantID, headCut)
	if err != nil {
		return nil, s.fail("Publish", err)
	}
	hidden = releasedBy(hidden, cutoff)

	changes := make([]metaversion.Change, 0, len(drafts))
	var conflicts []map[string]any
	for _, d := range drafts {
		ad, err := s.reg.Must(d.DocType)
		if err != nil {
			return nil, s.fail("Publish", err)
		}
		before, exists, err := ad.LoadLive(ctx, tx, tenantID, d.DocKey, true)
		if err != nil {
			return nil, s.fail("Publish", err)
		}
		conflict, err := draftConflicts(d, before, exists)
		if err != nil {
			return nil, s.fail("Publish", err)
		}
		if conflict {
			conflicts = append(conflicts, map[string]any{"docType": d.DocType, "docKey": d.DocKey, "label": d.Label})
		}
		c := metaversion.Change{DocType: d.DocType, DocKey: d.DocKey, Op: d.Op, Label: d.Label, UserID: caller.UserID, DraftUpdatedAt: d.UpdatedAt, DraftID: d.ID}
		if d.EntityName != nil {
			c.EntityName = *d.EntityName
		}
		if exists {
			c.Before = before
		}
		if d.Op == metaversion.OpUpsert {
			c.After = d.DraftJSON
		}
		changes = append(changes, c)
	}
	if len(conflicts) > 0 && !req.Force {
		return nil, &ValidationError{Code: "PUBLISH_CONFLICT", Msg: "Có tài liệu đã bị đổi ngoài bản nháp từ lúc bạn bắt đầu sửa",
			Details: map[string]any{"conflicts": conflicts}}
	}

	out, err := s.commitChanges(ctx, tx, tenantID, changes, commitOpts{Caller: caller, Kind: metaversion.KindPublish, Note: req.Note,
		Cutoff: cutoff, DraftIDs: draftIDs(drafts), Released: hidden})
	if err != nil {
		return nil, s.fail("Publish", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, s.fail("Publish", err)
	}
	return s.finishCommit(ctx, caller, tenantID, out, "publish"), nil
}

func draftIDs(drafts []metaversion.DraftRow) []string {
	ids := make([]string, len(drafts))
	for i, d := range drafts {
		ids[i] = d.ID
	}
	return ids
}

// commitOpts parameterises the one write path shared by Publish and Rollback (specs/063 plan §9: a
// rollback "goes through the Publish path" with kind='rollback').
type commitOpts struct {
	Caller     metaversion.Caller
	Kind       string
	Note       string
	RollbackOf *uint
	// Cutoff becomes the version's schema_cutoff_at: for a publish the newest definition it released
	// (releaseCutoff), for a rollback the target's own (what the target released, no more and no less).
	Cutoff time.Time
	// DraftIDs are deleted in the same tx (a rollback has none: DRAFTS_PENDING refuses first).
	DraftIDs []string
	// Released are the tables / fields / N-N relations born after the last publish (pending): already live,
	// so nothing is applied — the version's cutoff is what releases them. Each is recorded as a "create"
	// version doc (the live body; none for a relation, which has no adapter), counts in docCount and drops
	// the engine caches. A rollback to an earlier version hides them again through that version's cutoff;
	// it never deletes them (planRollback).
	Released []metaversion.DraftRow
}

// commitOut is what finishCommit needs once the tx has committed.
type commitOut struct {
	v            *metaversion.Version
	no           uint
	count        int
	touched      map[string]map[string]bool
	hasSchemaOps bool
	cutoff       time.Time
	rollbackOf   *uint
	kind         string
	// restoredTable: a rollback brought a deleted table back, with the role grants its delete dropped.
	restoredTable bool
}

// commitChanges runs steps 4-6 of plan §6 on an open tx that already holds the tenant lock: derived
// documents, final-state validation, the version row, the apply, the version docs and the draft
// cleanup. The caller commits.
func (s *PublishService) commitChanges(ctx context.Context, tx *sqlx.Tx, tenantID string, changes []metaversion.Change, o commitOpts) (*commitOut, error) {
	caller := o.Caller

	// Documents that change BECAUSE of other changes (an app's menu loses a dashboard this publish deletes).
	for _, t := range s.reg.Types() {
		ad, _ := s.reg.Get(t)
		if dv, ok := ad.(metaversion.Deriver); ok {
			var derr error
			if changes, derr = dv.Derive(ctx, tx, tenantID, changes); derr != nil {
				return nil, derr
			}
		}
	}

	// Cross-document consistency of the FINAL state, read after the lock.
	byType := map[string][]metaversion.Change{}
	for _, c := range changes {
		byType[c.DocType] = append(byType[c.DocType], c)
	}
	var problems []metaversion.Problem
	for _, t := range s.reg.Types() {
		if _, ok := byType[t]; ok { // the adapter sees EVERY change (an app menu is checked against the dashboards this publish adds)
			ad, _ := s.reg.Get(t)
			problems = append(problems, ad.ValidateFinal(ctx, metaversion.FinalState{TenantID: tenantID, Q: tx, Changes: changes})...)
		}
	}
	if len(problems) > 0 {
		errs := make([]map[string]any, len(problems))
		for i, p := range problems {
			errs[i] = map[string]any{"docType": p.DocType, "docKey": p.DocKey, "code": p.Code, "message": p.Message}
		}
		return nil, &ValidationError{Code: "PUBLISH_INVALID", Msg: "Bản nháp chưa nhất quán, không xuất bản được", Details: map[string]any{"errors": errs}}
	}

	seq, err := s.versions.NextSeq(ctx, tx, tenantID)
	if err != nil {
		return nil, err
	}
	no, err := s.versions.NextVersionNo(ctx, tx, tenantID)
	if err != nil {
		return nil, err
	}
	head, err := s.versions.HeadVersion(ctx, tx, tenantID)
	if err != nil {
		return nil, err
	}
	if head == nil { // unreachable: delivered was just confirmed under the lock
		return nil, errors.New("delivered tenant has no head version")
	}
	coveredTypes, err := s.versions.CoveredTypesUpTo(ctx, tx, tenantID, head.Seq)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, t := range coveredTypes {
		have[t] = true
	}
	var uncovered []string // types added to the registry after the baseline: snapshot them now, BEFORE this publish touches them
	for _, t := range s.reg.Types() {
		if !have[t] {
			uncovered = append(uncovered, t)
		}
	}
	type releasedDoc struct {
		r   metaversion.DraftRow
		ad  metaversion.DocAdapter
		doc json.RawMessage
	}
	released := make([]releasedDoc, 0, len(o.Released))
	for _, r := range o.Released {
		rd := releasedDoc{r: r}
		if ad, ok := s.reg.Get(r.DocType); ok {
			doc, live, err := ad.LoadLive(ctx, tx, tenantID, r.DocKey, false)
			if err != nil {
				return nil, err
			}
			if !live {
				continue // deleted since the list was read: nothing to record
			}
			rd.ad, rd.doc = ad, doc
		}
		released = append(released, rd)
	}
	v := &metaversion.Version{TenantID: tenantID, Seq: seq, VersionNo: &no, Kind: o.Kind, RollbackOf: o.RollbackOf, Note: o.Note, DocCount: uint(len(changes) + len(released)),
		CreatedBy: caller.UserID, CreatedByLabel: s.versions.ActorLabel(ctx, tx, caller.HomeTenantID, caller.UserID)}
	if len(uncovered) > 0 {
		v.CoveredTypes, _ = json.Marshal(uncovered)
	}
	cutoff := o.Cutoff
	v.SchemaCutoffAt = &cutoff
	// FR-066: the sweep instant of a table delete is this commit's own second (schemaDeleteMeta moves it to a
	// free one), not the cutoff, which may lie in the past.
	sweepAt := time.Now().UTC().Truncate(time.Second)
	// S7-9: deferred field / table deletes make the version's schema_status "pending" until they ran.
	hasSchemaOps := false
	for _, c := range changes {
		hasSchemaOps = hasSchemaOps || isSchemaDelete(c)
	}
	if hasSchemaOps {
		v.SchemaStatus = metaversion.StatusPending
	}
	if err := s.versions.InsertVersion(ctx, tx, v); err != nil {
		return nil, err
	}
	if _, err := s.snapshotTypes(ctx, tx, uncovered, tenantID, v, caller.UserID); err != nil {
		return nil, err
	}

	s.reg.SortChanges(changes)
	restoredTable := false
	touched := map[string]map[string]bool{} // docType -> entity names whose engine cache must drop
	remember := func(docType string, ad metaversion.DocAdapter, doc json.RawMessage) {
		for _, e := range ad.Entities(doc) {
			if touched[docType] == nil {
				touched[docType] = map[string]bool{}
			}
			touched[docType][e] = true
		}
	}
	for _, c := range changes {
		ad, _ := s.reg.Get(c.DocType)
		if err := ad.ApplyTx(ctx, tx, tenantID, c); err != nil {
			return nil, err
		}
		restoredTable = restoredTable || (c.DocType == DocTypeEntity && len(c.Restore) > 0)
		op := "update"
		switch {
		case c.Op == metaversion.OpDelete:
			op = "delete"
		case len(c.Before) == 0:
			op = "create"
		}
		vd := metaversion.VersionDoc{TenantID: tenantID, VersionID: v.ID, Seq: v.Seq, DocType: c.DocType, DocKey: c.DocKey,
			Label: c.Label, EntityName: c.EntityName, Op: op, BeforeJSON: c.Before, AfterJSON: c.After, CreatedByID: caller.UserID}
		if isSchemaDelete(c) {
			// The sweep instant and the definition being deleted are written HERE, under the lock and
			// before any sweep, so a retry never picks a new instant (FR-066).
			if vd.Meta, err = schemaDeleteMeta(ctx, tx, tenantID, c, sweepAt); err != nil {
				return nil, err
			}
			vd.ApplyStatus = metaversion.StatusPending
		}
		if err := s.versions.InsertDoc(ctx, tx, vd); err != nil {
			return nil, err
		}
		remember(c.DocType, ad, c.Before)
		remember(c.DocType, ad, c.After)
	}
	for _, rd := range released {
		vd := metaversion.VersionDoc{TenantID: tenantID, VersionID: v.ID, Seq: v.Seq, DocType: rd.r.DocType, DocKey: rd.r.DocKey,
			Label: rd.r.Label, Op: "create", AfterJSON: rd.doc, CreatedByID: caller.UserID}
		if rd.r.EntityName != nil {
			vd.EntityName = *rd.r.EntityName
		}
		if err := s.versions.InsertDoc(ctx, tx, vd); err != nil {
			return nil, err
		}
		if rd.ad != nil {
			remember(rd.r.DocType, rd.ad, rd.doc)
		}
	}
	if len(o.DraftIDs) > 0 {
		if _, err := s.drafts.DeleteIDs(ctx, tx, tenantID, o.DraftIDs); err != nil {
			return nil, err
		}
	}
	return &commitOut{v: v, no: no, count: len(changes) + len(released), touched: touched, hasSchemaOps: hasSchemaOps, cutoff: cutoff, rollbackOf: o.RollbackOf, kind: o.Kind,
		restoredTable: restoredTable}, nil
}

// finishCommit is step 7-8 of plan §6, after the tx committed: engine caches, the delivery cache, the
// audit event and the deferred schema deletes.
func (s *PublishService) finishCommit(ctx context.Context, caller metaversion.Caller, tenantID string, out *commitOut, action string) *PublishResult {
	// After commit only: nothing above may drop a cache, or a rolled-back publish would have flushed it for nothing.
	for docType, entities := range out.touched {
		if inv := s.invalidators[docType]; inv != nil {
			names := make([]string, 0, len(entities))
			for e := range entities {
				names = append(names, e)
			}
			sort.Strings(names)
			for _, e := range names {
				inv(tenantID, e)
			}
		}
	}
	if out.restoredTable {
		// The restored table's role grants are back: cached grants must not keep refusing them until the 30 s
		// backstop. ponytail: every tenant's cached grants (a rollback is a rare builder act), as EntityService does.
		s.aclCache.InvalidateAll()
	}
	s.gate.CacheDelivered(tenantID)
	s.gate.RefreshCutoff(tenantID, out.cutoff) // S7-8: what this version released stops being hidden at once on this replica
	sum := map[string]any{"versionNo": out.no, "kind": out.kind, "docCount": out.count}
	if out.rollbackOf != nil {
		sum["rollbackOf"] = *out.rollbackOf
	}
	s.audit(ctx, caller, tenantID, out.v.ID, action, sum)
	s.logger.Info("services.PublishService."+action+": committed",
		zap.String("tenant_id", tenantID), zap.String("user_id", caller.UserID), zap.Uint("version_no", out.no), zap.Int("doc_count", out.count))
	res := &PublishResult{VersionNo: out.no, Kind: out.kind, DocCount: out.count, PublishedAt: time.Now().UTC(), PropagationSeconds: PropagationSeconds}
	if out.hasSchemaOps {
		// Step 8: the documents are committed; now the deletes run through the existing service paths.
		// A failure here does NOT undo the publish: the version says "failed" and Thử lại finishes it.
		ops, status, oerr := s.runSchemaOps(ctx, caller, tenantID, out.v.ID)
		if oerr != nil {
			s.logger.Error("services.PublishService."+action+": schema ops", zap.String("tenant_id", tenantID), zap.String("version_id", out.v.ID), zap.Error(oerr))
			status = metaversion.StatusFailed
		}
		res.SchemaStatus, res.SchemaOps = status, ops
	}
	return res
}

// Discard drops one pending draft: the document goes back to exactly what is live. A draft id of
// another tenant is the same ErrDraftNotFound as an unknown one — nothing is written.
func (s *PublishService) Discard(ctx context.Context, tenantID, draftID string) error {
	caller, err := s.builder(ctx, tenantID)
	if err != nil {
		return err
	}
	d, err := s.drafts.Get(ctx, tenantID, draftID)
	if errors.Is(err, metaversion.ErrDraftNotFound) {
		return invalid("DRAFT_NOT_FOUND", "Không tìm thấy thay đổi này trong bản nháp")
	}
	if err != nil {
		return fmt.Errorf("services.PublishService.Discard: %w", err)
	}
	n, err := s.drafts.DeleteIDs(ctx, s.db, tenantID, []string{d.ID})
	if err != nil {
		return fmt.Errorf("services.PublishService.Discard: %w", err)
	}
	if n == 0 { // a publish (or another discard) took the draft between the read and the delete: say so, never a false 204
		return invalid("DRAFT_NOT_FOUND", "Không tìm thấy thay đổi này trong bản nháp")
	}
	// Same summary-only event as DiscardAll (no label, no body: /audit-log is read by the customer's admin).
	s.audit(ctx, caller, tenantID, "", "discard", map[string]any{"discardedCount": 1})
	return nil
}

// DiscardAll drops every pending draft, but only the set the builder looked at.
func (s *PublishService) DiscardAll(ctx context.Context, tenantID, changesetHash string) error {
	caller, err := s.builder(ctx, tenantID)
	if err != nil {
		return err
	}
	if err := validateChangesetHash(changesetHash); err != nil {
		return err
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("services.PublishService.DiscardAll: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockTenant(ctx, tx, tenantID); err != nil {
		return s.fail("DiscardAll", err)
	}
	drafts, err := s.drafts.List(ctx, tx, tenantID, true)
	if err != nil {
		return s.fail("DiscardAll", err)
	}
	pending, err := s.pendingRows(ctx, tx, tenantID, drafts) // the fingerprint the builder saw covers the hidden-new rows; they are not dropped
	if err != nil {
		return s.fail("DiscardAll", err)
	}
	if cur := metaversion.ChangesetHash(pending); cur != changesetHash {
		return &ValidationError{Code: "DRAFTS_CHANGED", Msg: "Bản nháp đã đổi, đã tải lại danh sách", Details: map[string]any{"changesetHash": cur}}
	}
	ids := make([]string, len(drafts))
	for i, d := range drafts {
		ids[i] = d.ID
	}
	if _, err := s.drafts.DeleteIDs(ctx, tx, tenantID, ids); err != nil {
		return s.fail("DiscardAll", err)
	}
	if err := tx.Commit(); err != nil {
		return s.fail("DiscardAll", err)
	}
	if len(ids) > 0 {
		s.audit(ctx, caller, tenantID, "", "discard", map[string]any{"discardedCount": len(ids)})
	}
	return nil
}

// audit writes the __meta_version__ event after commit. The actor is set HERE from the Caller (the
// document handlers hand the service the plain request ctx, which carries no actor), and the diff
// holds ONLY the summary keys it is given: /audit-log is read by the customer's admin, so a document
// body, a before/after or a draft label would leak a webhook key and unpublished work.
func (s *PublishService) audit(ctx context.Context, caller metaversion.Caller, tenantID, recordID, action string, after map[string]any) {
	if s.auditor == nil {
		return
	}
	emitMetaAuditFor(audit.WithActor(ctx, caller.UserID), s.auditor, tenantID, "__meta_version__", recordID, action, nil, after)
}

// changedPaths lists where the draft body differs from the live one, both brought to the canonical
// form first so a save that changes nothing reads empty (SC-006). At most metadiff.Cap paths; the
// second value counts the rest. A create / delete (one side missing) or a body this cannot read has no list.
func changedPaths(docType string, before, after json.RawMessage) ([]string, int) {
	if len(before) == 0 || len(after) == 0 {
		return nil, 0
	}
	var a, b any
	if json.Unmarshal(before, &a) != nil || json.Unmarshal(after, &b) != nil {
		return nil, 0
	}
	metadiff.Canonicalize(docType, a)
	metadiff.Canonicalize(docType, b)
	kept, more := metadiff.Limit(metadiff.Diff(a, b), metadiff.Cap)
	if len(kept) == 0 {
		return nil, 0
	}
	return kept, more
}

// VersionSummary is one row of GET /admin/publish/versions.
type VersionSummary struct {
	ID            string    `json:"id"`
	Seq           uint      `json:"seq"`
	VersionNo     *uint     `json:"versionNo"`
	Kind          string    `json:"kind"`
	Note          string    `json:"note"`
	DocCount      uint      `json:"docCount"`
	CreatedAt     time.Time `json:"createdAt"`
	CreatedBy     string    `json:"createdBy"`
	CreatedByName string    `json:"createdByName"`
	RollbackOf    *uint     `json:"rollbackOf"`
	SchemaStatus  string    `json:"schemaStatus"`
}

// VersionDocView is one document of a version as the API shows it: bodies already redacted.
type VersionDocView struct {
	DocType      string          `json:"docType"`
	DocKey       string          `json:"docKey"`
	Label        string          `json:"label"`
	Op           string          `json:"op"`
	ChangedPaths []string        `json:"changedPaths"`
	MoreChanged  int             `json:"moreChanged"`
	Before       json.RawMessage `json:"before"`
	After        json.RawMessage `json:"after"`
	ApplyStatus  string          `json:"applyStatus"`
	ApplyError   string          `json:"applyError,omitempty"`
	// Reassign (S7-7b, FR-056): on a "role deleted" entry, who would get the role back by Hoàn tác.
	Reassign []ReassignUser `json:"reassign,omitempty"`
}

// VersionDetail is GET /admin/publish/versions/:id.
type VersionDetail struct {
	VersionSummary
	// CreatedByLabel is who made the change (the same label as createdByName; named as the spec does).
	CreatedByLabel string           `json:"createdByLabel"`
	Docs           []VersionDocView `json:"docs"`
}

func summaryOf(v metaversion.Version) VersionSummary {
	return VersionSummary{ID: v.ID, Seq: v.Seq, VersionNo: v.VersionNo, Kind: v.Kind, Note: v.Note, DocCount: v.DocCount,
		CreatedAt: v.CreatedAt, CreatedBy: v.CreatedBy, CreatedByName: v.CreatedByLabel, RollbackOf: v.RollbackOf, SchemaStatus: v.SchemaStatus}
}

// versionDocView redacts both sides through the type's adapter (T098). A type this build does not
// know has its bodies dropped, never echoed: they may hold a secret nobody can find.
func versionDocView(reg *metaversion.Registry, d metaversion.VersionDoc) VersionDocView {
	v := VersionDocView{DocType: d.DocType, DocKey: d.DocKey, Label: d.Label, Op: d.Op, ChangedPaths: []string{},
		ApplyStatus: d.ApplyStatus, ApplyError: d.ApplyError}
	if d.Op == "update" {
		if paths, more := changedPaths(d.DocType, d.BeforeJSON, d.AfterJSON); paths != nil {
			v.ChangedPaths, v.MoreChanged = paths, more
		}
	}
	ad, ok := reg.Get(d.DocType)
	if !ok {
		return v
	}
	if len(d.BeforeJSON) > 0 {
		v.Before = ad.Redact(d.BeforeJSON)
	}
	if len(d.AfterJSON) > 0 {
		v.After = ad.Redact(d.AfterJSON)
	}
	return v
}

// ListVersions pages the publish history, newest first. limit is clamped to 1..100.
func (s *PublishService) ListVersions(ctx context.Context, tenantID string, limit, offset int) ([]VersionSummary, int, error) {
	if _, err := s.builder(ctx, tenantID); err != nil {
		return nil, 0, err
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	vs, total, err := s.versions.ListVersions(ctx, tenantID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("services.PublishService.ListVersions: %w", err)
	}
	out := make([]VersionSummary, len(vs))
	for i, v := range vs {
		out[i] = summaryOf(v)
	}
	return out, total, nil
}

// GetVersion returns one version with its documents. An id that is not a UUID, unknown, or another
// tenant's is the SAME VERSION_NOT_FOUND (FR-081).
func (s *PublishService) GetVersion(ctx context.Context, tenantID, id string) (*VersionDetail, error) {
	if _, err := s.builder(ctx, tenantID); err != nil {
		return nil, err
	}
	notFound := invalid("VERSION_NOT_FOUND", "Không tìm thấy phiên bản này")
	if _, err := uuid.Parse(id); err != nil {
		return nil, notFound
	}
	v, err := s.versions.GetVersion(ctx, tenantID, id)
	if errors.Is(err, metaversion.ErrVersionNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, fmt.Errorf("services.PublishService.GetVersion: %w", err)
	}
	docs, err := s.versions.ListDocs(ctx, tenantID, v.ID)
	if err != nil {
		return nil, fmt.Errorf("services.PublishService.GetVersion: %w", err)
	}
	out := &VersionDetail{VersionSummary: summaryOf(*v), CreatedByLabel: v.CreatedByLabel, Docs: make([]VersionDocView, len(docs))}
	for i, d := range docs {
		out.Docs[i] = versionDocView(s.reg, d)
		if d.DocType == "role" && d.Op == "delete" && v.Kind == metaversion.KindDirect {
			if out.Docs[i].Reassign, err = s.reassignOf(ctx, s.db, tenantID, d.Meta); err != nil {
				return nil, fmt.Errorf("services.PublishService.GetVersion: %w", err)
			}
		}
	}
	return out, nil
}
