package services

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metaversion"
)

// VersionWarningMessage is the notice a response carries when the change applied but its direct
// version entry could not be written (specs/063 FR-033).
const VersionWarningMessage = "Thay đổi đã áp dụng nhưng chưa ghi được vào lịch sử phiên bản — không hoàn tác được từ lịch sử."

// DirectDoc is one document of a "direct" version entry: what a change that applied at once did to it.
type DirectDoc struct {
	DocType    string
	DocKey     string
	Label      string
	EntityName string
	Op         string // create | update | delete
	Before     any
	After      any
	Meta       any
}

// directStore is what the recorder needs from storage; *sqlDirectStore in production.
type directStore interface {
	Delivered(ctx context.Context, tenantID string) (bool, error)
	ActorLabel(ctx context.Context, homeTenantID, userID string) string
	// Append writes ONE version row (kind=direct, version_no NULL, seq=MAX+1) with its docs in a tx
	// that holds tenants ... FOR UPDATE.
	Append(ctx context.Context, v metaversion.Version, docs []metaversion.VersionDoc) error
}

// VersionRecorder writes the "direct" entries of a delivered workspace (S7-5, FR-032): changes that
// apply at once (roles, grants, field permissions, app create/delete, a legacy customer admin's
// writes) still leave a before/after trail. A nil recorder records nothing.
type VersionRecorder struct {
	store  directStore
	logger *zap.Logger
}

// NewVersionRecorder wires the recorder over the SQL store.
func NewVersionRecorder(db *sqlx.DB, versions *metaversion.VersionRepo, gate *DraftGate, logger *zap.Logger) *VersionRecorder {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &VersionRecorder{store: &sqlDirectStore{db: db, versions: versions, gate: gate}, logger: logger}
}

// Active reports whether the tenant is delivered and a human caller is on ctx: the only case in which
// a direct entry exists. Callers use it to skip reading a "before" image they would throw away.
func (r *VersionRecorder) Active(ctx context.Context, tenantID string) bool {
	if r == nil {
		return false
	}
	if _, ok := metaversion.CallerFrom(ctx); !ok {
		return false
	}
	delivered, err := r.store.Delivered(ctx, tenantID)
	if err != nil {
		r.logger.Error("services.VersionRecorder.Active", zap.String("tenant_id", tenantID), zap.Error(err))
		return false
	}
	return delivered
}

// RecordDirect appends ONE direct entry holding docs, AFTER the change committed. It never fails the
// request: on error it logs and adds a warning the handler puts in the response (FR-033).
func (r *VersionRecorder) RecordDirect(ctx context.Context, tenantID, note string, docs ...DirectDoc) {
	if r == nil || len(docs) == 0 || !r.Active(ctx, tenantID) {
		return
	}
	if err := r.record(ctx, tenantID, note, docs); err != nil {
		r.logger.Error("services.VersionRecorder.RecordDirect", zap.String("tenant_id", tenantID),
			zap.String("doc_type", docs[0].DocType), zap.String("doc_key", docs[0].DocKey), zap.Error(err))
		metaversion.AddWarning(ctx, VersionWarningMessage)
	}
}

func (r *VersionRecorder) record(ctx context.Context, tenantID, note string, docs []DirectDoc) error {
	caller, _ := metaversion.CallerFrom(ctx)
	v := metaversion.Version{
		TenantID:       tenantID,
		Kind:           metaversion.KindDirect,
		Note:           note,
		DocCount:       uint(len(docs)),
		CreatedBy:      caller.UserID,
		CreatedByLabel: r.store.ActorLabel(ctx, caller.HomeTenantID, caller.UserID),
	}
	rows := make([]metaversion.VersionDoc, 0, len(docs))
	for i, d := range docs {
		before, err := directRaw(d.Before)
		if err != nil {
			return fmt.Errorf("services.VersionRecorder.record: before: %w", err)
		}
		after, err := directRaw(d.After)
		if err != nil {
			return fmt.Errorf("services.VersionRecorder.record: after: %w", err)
		}
		meta, err := directRaw(d.Meta)
		if err != nil {
			return fmt.Errorf("services.VersionRecorder.record: meta: %w", err)
		}
		rows = append(rows, metaversion.VersionDoc{
			TenantID: tenantID, Seq: uint(i + 1), DocType: d.DocType, DocKey: d.DocKey, Label: d.Label,
			EntityName: d.EntityName, Op: d.Op, BeforeJSON: before, AfterJSON: after, Meta: meta,
			CreatedByID: caller.UserID,
		})
	}
	if err := r.store.Append(ctx, v, rows); err != nil {
		return fmt.Errorf("services.VersionRecorder.record: %w", err)
	}
	return nil
}

// directRaw marshals a value; nil (or an already-raw empty value) stays nil so the column is NULL.
func directRaw(v any) (json.RawMessage, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case json.RawMessage:
		return x, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// TraceLive runs one LIVE write of a configuration document and records its direct entry when the
// caller is a non-builder on a delivered tenant (mode == ModeLiveDirect). write returns the document's
// key (for a create, the new id). Used by every wrapped repo's non-draft branch.
func (r *VersionRecorder) TraceLive(ctx context.Context, mode metaversion.Mode, ad metaversion.DocAdapter, tenantID, op, key string, write func() (string, error)) error {
	if r == nil || ad == nil || mode != metaversion.ModeLiveDirect {
		_, err := write()
		return err
	}
	var before json.RawMessage
	if key != "" && op != "create" {
		before = r.liveDoc(ctx, ad, tenantID, key)
	}
	k, err := write()
	if err != nil {
		return err
	}
	if k == "" {
		k = key
	}
	var after json.RawMessage
	if op != "delete" {
		after = r.liveDoc(ctx, ad, tenantID, k)
	}
	label, entity := docLabel(after, before)
	r.RecordDirect(ctx, tenantID, "", DirectDoc{DocType: ad.Type(), DocKey: k, Label: label, EntityName: entity, Op: op,
		Before: ad.Redact(before), After: ad.Redact(after)})
	return nil
}

func (r *VersionRecorder) liveDoc(ctx context.Context, ad metaversion.DocAdapter, tenantID, key string) json.RawMessage {
	s, ok := r.store.(*sqlDirectStore)
	if !ok {
		return nil
	}
	doc, found, err := ad.LoadLive(ctx, s.db, tenantID, key, false)
	if err != nil || !found {
		if err != nil {
			r.logger.Warn("services.VersionRecorder.liveDoc", zap.String("tenant_id", tenantID),
				zap.String("doc_type", ad.Type()), zap.String("doc_key", key), zap.Error(err))
		}
		return nil
	}
	return doc
}

// docLabel picks a display label / entity name out of canonical documents (first non-empty wins).
func docLabel(docs ...json.RawMessage) (label, entity string) {
	for _, d := range docs {
		if len(d) == 0 {
			continue
		}
		var m map[string]any
		if json.Unmarshal(d, &m) != nil {
			continue
		}
		for _, k := range []string{"name", "label", "title"} {
			if s, ok := m[k].(string); ok && s != "" && label == "" {
				label = s
			}
		}
		if s, ok := m["entityName"].(string); ok && entity == "" {
			entity = s
		}
	}
	return label, entity
}

// sqlDirectStore is the MySQL implementation of directStore.
type sqlDirectStore struct {
	db       *sqlx.DB
	versions *metaversion.VersionRepo
	gate     *DraftGate
}

func (s *sqlDirectStore) Delivered(ctx context.Context, tenantID string) (bool, error) {
	return s.gate.Delivered(ctx, tenantID)
}

func (s *sqlDirectStore) ActorLabel(ctx context.Context, homeTenantID, userID string) string {
	return s.versions.ActorLabel(ctx, s.db, homeTenantID, userID)
}

func (s *sqlDirectStore) Append(ctx context.Context, v metaversion.Version, docs []metaversion.VersionDoc) (err error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("services.sqlDirectStore.Append: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback() //nolint:errcheck // the original error is the one that matters
		}
	}()
	if err = lockTenant(ctx, tx, v.TenantID); err != nil {
		return fmt.Errorf("services.sqlDirectStore.Append: %w", err)
	}
	if v.Seq, err = s.versions.NextSeq(ctx, tx, v.TenantID); err != nil {
		return fmt.Errorf("services.sqlDirectStore.Append: %w", err)
	}
	if err = s.versions.InsertVersion(ctx, tx, &v); err != nil {
		return fmt.Errorf("services.sqlDirectStore.Append: %w", err)
	}
	for _, d := range docs {
		d.VersionID, d.Seq = v.ID, v.Seq
		if err = s.versions.InsertDoc(ctx, tx, d); err != nil {
			return fmt.Errorf("services.sqlDirectStore.Append: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("services.sqlDirectStore.Append: commit: %w", err)
	}
	return nil
}

// sqlRoleFootprint is the MySQL roleFootprint.
type sqlRoleFootprint struct{ db *sqlx.DB }

// NewRoleFootprint wires the footprint reader over the shared pool.
func NewRoleFootprint(db *sqlx.DB) *sqlRoleFootprint { return &sqlRoleFootprint{db: db} }

func (f *sqlRoleFootprint) AssignedUserIDs(ctx context.Context, tenantID, roleID string) ([]string, error) {
	ids := []string{}
	if err := f.db.SelectContext(ctx, &ids, `SELECT id FROM users WHERE tenant_id = ? AND role_id = ? ORDER BY id`, tenantID, roleID); err != nil {
		return nil, fmt.Errorf("services.sqlRoleFootprint.AssignedUserIDs: %w", err)
	}
	return ids, nil
}

func (f *sqlRoleFootprint) FieldACLRows(ctx context.Context, tenantID, roleID string) ([]auth.FieldACL, error) {
	var raw []struct {
		ID         string `db:"id"`
		TenantID   string `db:"tenant_id"`
		RoleID     string `db:"role_id"`
		EntityName string `db:"entity_name"`
		FieldName  string `db:"field_name"`
		CanView    bool   `db:"can_view"`
		CanEdit    bool   `db:"can_edit"`
	}
	if err := f.db.SelectContext(ctx, &raw, `SELECT id, tenant_id, role_id, entity_name, field_name, can_view, can_edit
		FROM sys_field_acl WHERE tenant_id = ? AND role_id = ? ORDER BY entity_name, field_name`, tenantID, roleID); err != nil {
		return nil, fmt.Errorf("services.sqlRoleFootprint.FieldACLRows: %w", err)
	}
	rows := make([]auth.FieldACL, 0, len(raw))
	for _, r := range raw {
		rows = append(rows, auth.FieldACL{ID: r.ID, TenantID: r.TenantID, RoleID: r.RoleID, EntityName: r.EntityName,
			FieldName: r.FieldName, CanView: r.CanView, CanEdit: r.CanEdit})
	}
	return rows, nil
}
