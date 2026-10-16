package metaversion

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// Version kinds.
const (
	KindBaseline = "baseline"
	KindPublish  = "publish"
	KindRollback = "rollback"
	KindDirect   = "direct"
)

// Version is one row of sys_meta_versions. VersionNo is nil for a "direct" entry.
type Version struct {
	ID             string     `db:"id"`
	TenantID       string     `db:"tenant_id"`
	Seq            uint       `db:"seq"`
	VersionNo      *uint      `db:"version_no"`
	Kind           string     `db:"kind"`
	RollbackOf     *uint      `db:"rollback_of"`
	Note           string     `db:"note"`
	DocCount       uint       `db:"doc_count"`
	CoveredTypes   []byte     `db:"covered_types"`
	SchemaCutoffAt *time.Time `db:"schema_cutoff_at"`
	SchemaStatus   string     `db:"schema_status"`
	CreatedBy      string     `db:"created_by"`
	CreatedByLabel string     `db:"created_by_label"`
	CreatedAt      time.Time  `db:"created_at"`
}

// VersionDoc is one row of sys_meta_version_docs: a document's before/after in one version.
type VersionDoc struct {
	ID          string
	TenantID    string
	VersionID   string
	Seq         uint
	DocType     string
	DocKey      string
	Label       string
	EntityName  string
	Op          string // create | update | delete | snapshot
	BeforeJSON  json.RawMessage
	AfterJSON   json.RawMessage
	Meta        json.RawMessage
	CreatedByID string
	// ApplyStatus is "applied" (default) or, for a deferred schema delete (S7-9), "pending" until the
	// service has run it; ApplyError is the reason of a "failed" one.
	ApplyStatus string
	ApplyError  string
}

// ErrVersionNotFound: no version with that id in the tenant (also another tenant's id - no leak).
var ErrVersionNotFound = errors.New("metaversion: version not found")

// Apply statuses of a version doc / schema statuses of a version.
const (
	StatusApplied = "applied"
	StatusPending = "pending"
	StatusFailed  = "failed"
	StatusDone    = "done"
	StatusNone    = "none"
)

const versionCols = `id, tenant_id, seq, version_no, kind, rollback_of, note, doc_count, covered_types,
	schema_cutoff_at, schema_status, created_by, created_by_label, created_at`

// VersionRepo reads and appends sys_meta_versions / sys_meta_version_docs. Append-only: nothing
// here updates or deletes a version (FR-031); the only writes after insert are the S7-9 status columns
// (schema_status, apply_status/apply_error/meta of a deferred schema delete). Every statement pins tenant_id. seq / version_no are
// MAX+1 and MUST be called inside a tx holding tenants ... FOR UPDATE.
type VersionRepo struct{ db *sqlx.DB }

// NewVersionRepo wires the repo over the shared sqlx pool.
func NewVersionRepo(db *sqlx.DB) *VersionRepo { return &VersionRepo{db: db} }

// HasNumbered reports whether the tenant has a numbered version: THE definition of "delivered"
// (FR-001). One-way: nothing deletes versions.
func (r *VersionRepo) HasNumbered(ctx context.Context, tenantID string) (bool, error) {
	var ok bool
	if err := r.db.GetContext(ctx, &ok, `SELECT EXISTS(SELECT 1 FROM sys_meta_versions
		WHERE tenant_id = ? AND version_no IS NOT NULL AND deleted_at IS NULL)`, tenantID); err != nil {
		return false, fmt.Errorf("metaversion.VersionRepo.HasNumbered: %w", err)
	}
	return ok, nil
}

// HasNumberedTx is HasNumbered on an open tx (publish: re-checked under the tenant lock).
func (r *VersionRepo) HasNumberedTx(ctx context.Context, q sqlx.QueryerContext, tenantID string) (bool, error) {
	var ok bool
	if err := sqlx.GetContext(ctx, q, &ok, `SELECT EXISTS(SELECT 1 FROM sys_meta_versions
		WHERE tenant_id = ? AND version_no IS NOT NULL AND deleted_at IS NULL)`, tenantID); err != nil {
		return false, fmt.Errorf("metaversion.VersionRepo.HasNumberedTx: %w", err)
	}
	return ok, nil
}

// HeadVersion returns the newest numbered version, nil when the tenant has none.
func (r *VersionRepo) HeadVersion(ctx context.Context, q sqlx.QueryerContext, tenantID string) (*Version, error) {
	var v Version
	err := sqlx.GetContext(ctx, q, &v, `SELECT `+versionCols+` FROM sys_meta_versions
		WHERE tenant_id = ? AND version_no IS NOT NULL AND deleted_at IS NULL ORDER BY version_no DESC LIMIT 1`, tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("metaversion.VersionRepo.HeadVersion: %w", err)
	}
	return &v, nil
}

// HeadCutoff returns schema_cutoff_at of the newest numbered version: fields, tables and relations
// created after it are not yet the customer's (S7-8). nil when the tenant was never delivered or the
// head version carries no cutoff. One indexed single-row read; DraftGate caches it for 5 seconds.
func (r *VersionRepo) HeadCutoff(ctx context.Context, tenantID string) (*time.Time, error) {
	var at sql.NullTime
	err := r.db.GetContext(ctx, &at, `SELECT schema_cutoff_at FROM sys_meta_versions
		WHERE tenant_id = ? AND version_no IS NOT NULL AND deleted_at IS NULL ORDER BY version_no DESC LIMIT 1`, tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("metaversion.VersionRepo.HeadCutoff: %w", err)
	}
	if !at.Valid {
		return nil, nil
	}
	t := at.Time
	return &t, nil
}

// HeadVersionRead is HeadVersion on the repo's own pool.
func (r *VersionRepo) HeadVersionRead(ctx context.Context, tenantID string) (*Version, error) {
	return r.HeadVersion(ctx, r.db, tenantID)
}

// NextSeq is MAX(seq)+1 of the tenant (1 for the first entry). Call under the tenant lock.
func (r *VersionRepo) NextSeq(ctx context.Context, q sqlx.QueryerContext, tenantID string) (uint, error) {
	var n uint
	if err := sqlx.GetContext(ctx, q, &n, `SELECT COALESCE(MAX(seq), 0) + 1 FROM sys_meta_versions WHERE tenant_id = ?`, tenantID); err != nil {
		return 0, fmt.Errorf("metaversion.VersionRepo.NextSeq: %w", err)
	}
	return n, nil
}

// NextVersionNo is MAX(version_no)+1 of the tenant. Call under the tenant lock.
func (r *VersionRepo) NextVersionNo(ctx context.Context, q sqlx.QueryerContext, tenantID string) (uint, error) {
	var n uint
	if err := sqlx.GetContext(ctx, q, &n, `SELECT COALESCE(MAX(version_no), 0) + 1 FROM sys_meta_versions WHERE tenant_id = ?`, tenantID); err != nil {
		return 0, fmt.Errorf("metaversion.VersionRepo.NextVersionNo: %w", err)
	}
	return n, nil
}

// InsertVersion appends a version row (id is generated when empty).
func (r *VersionRepo) InsertVersion(ctx context.Context, ex sqlx.ExtContext, v *Version) error {
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	var covered any
	if len(v.CoveredTypes) > 0 {
		covered = string(v.CoveredTypes)
	}
	status := v.SchemaStatus
	if status == "" {
		status = "none"
	}
	if _, err := ex.ExecContext(ctx, `INSERT INTO sys_meta_versions
		(id, tenant_id, seq, version_no, kind, rollback_of, note, doc_count, covered_types, schema_cutoff_at,
		 schema_status, created_by, created_by_label)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID, v.TenantID, v.Seq, v.VersionNo, v.Kind, v.RollbackOf, v.Note, v.DocCount, covered, v.SchemaCutoffAt,
		status, v.CreatedBy, v.CreatedByLabel); err != nil {
		return fmt.Errorf("metaversion.VersionRepo.InsertVersion: %w", err)
	}
	return nil
}

// InsertDoc appends the before/after of one document to a version.
func (r *VersionRepo) InsertDoc(ctx context.Context, ex sqlx.ExtContext, d VersionDoc) error {
	nullable := func(b json.RawMessage) any {
		if len(b) == 0 {
			return nil
		}
		return string(b)
	}
	var entity any
	if d.EntityName != "" {
		entity = d.EntityName
	}
	status := d.ApplyStatus
	if status == "" {
		status = StatusApplied
	}
	var applyErr any
	if d.ApplyError != "" {
		applyErr = d.ApplyError
	}
	if _, err := ex.ExecContext(ctx, `INSERT INTO sys_meta_version_docs
		(id, tenant_id, version_id, seq, doc_type, doc_key, label, entity_name, op, before_json, after_json, meta, apply_status, apply_error, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), d.TenantID, d.VersionID, d.Seq, d.DocType, d.DocKey, d.Label, entity, d.Op,
		nullable(d.BeforeJSON), nullable(d.AfterJSON), nullable(d.Meta), status, applyErr, d.CreatedByID); err != nil {
		return fmt.Errorf("metaversion.VersionRepo.InsertDoc: %w", err)
	}
	return nil
}

// SchemaDoc is one deferred schema delete of a version (S7-9): a "delete" version doc of type field or
// entity, with the status of running it and the Meta recorded at publish time ({sweptAt, fieldDef,
// entityDef, relationships}, plus sweptCount after the sweep).
type SchemaDoc struct {
	ID          string         `db:"id"`
	DocType     string         `db:"doc_type"`
	DocKey      string         `db:"doc_key"`
	Label       string         `db:"label"`
	EntityName  sql.NullString `db:"entity_name"`
	Meta        []byte         `db:"meta"`
	ApplyStatus string         `db:"apply_status"`
	ApplyError  sql.NullString `db:"apply_error"`
}

// ListSchemaDocs lists the deferred schema deletes of a version in apply order (fields before tables).
func (r *VersionRepo) ListSchemaDocs(ctx context.Context, tenantID, versionID string) ([]SchemaDoc, error) {
	var out []SchemaDoc
	if err := r.db.SelectContext(ctx, &out, `SELECT id, doc_type, doc_key, label, entity_name, meta, apply_status, apply_error
		FROM sys_meta_version_docs WHERE tenant_id = ? AND version_id = ? AND op = 'delete' AND doc_type IN ('field', 'entity')
		ORDER BY doc_type = 'entity', created_at, id`, tenantID, versionID); err != nil {
		return nil, fmt.Errorf("metaversion.VersionRepo.ListSchemaDocs: %w", err)
	}
	return out, nil
}

// SetDocApply records the outcome of one deferred schema delete: its status, the reason of a failure
// and, when meta is not empty, the Meta document replacing the recorded one. Tenant-scoped.
func (r *VersionRepo) SetDocApply(ctx context.Context, tenantID, docID, status, errText string, meta json.RawMessage) error {
	var e any
	if errText != "" {
		e = errText
	}
	var m any
	if len(meta) > 0 {
		m = string(meta)
	}
	if _, err := r.db.ExecContext(ctx, `UPDATE sys_meta_version_docs SET apply_status = ?, apply_error = ?, meta = COALESCE(?, meta)
		WHERE tenant_id = ? AND id = ?`, status, e, m, tenantID, docID); err != nil {
		return fmt.Errorf("metaversion.VersionRepo.SetDocApply: %w", err)
	}
	return nil
}

// StaleSchemaPendingMinutes is how long a version may sit in schema_status "pending" before a retry may
// take it over (the request that was running its deletes died with the server).
const StaleSchemaPendingMinutes = 10

// ClaimSchemaRetry takes the right to re-run a version's deferred schema deletes: it moves "failed" (or
// a "pending" older than StaleSchemaPendingMinutes) to a fresh "pending" and reports whether THIS call
// did. updated_at is set explicitly so that of two concurrent retries exactly one matches the stale
// predicate. Tenant-scoped.
func (r *VersionRepo) ClaimSchemaRetry(ctx context.Context, tenantID, versionID string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `UPDATE sys_meta_versions SET schema_status = 'pending', updated_at = CURRENT_TIMESTAMP(3)
		WHERE tenant_id = ? AND id = ? AND (schema_status = 'failed'
			OR (schema_status = 'pending' AND updated_at < CURRENT_TIMESTAMP(3) - INTERVAL ? MINUTE))`,
		tenantID, versionID, StaleSchemaPendingMinutes)
	if err != nil {
		return false, fmt.Errorf("metaversion.VersionRepo.ClaimSchemaRetry: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("metaversion.VersionRepo.ClaimSchemaRetry: rows affected: %w", err)
	}
	return n > 0, nil
}

// LatestRetryableSchemaVersion is the newest version whose deferred schema deletes ClaimSchemaRetry
// would accept ("failed", or an abandoned "pending"); nil when there is none. The builder's state
// endpoint uses it to show "Xoá chưa xong · Thử lại" long after the publish request returned.
func (r *VersionRepo) LatestRetryableSchemaVersion(ctx context.Context, tenantID string) (*Version, error) {
	var v Version
	err := r.db.GetContext(ctx, &v, `SELECT `+versionCols+` FROM sys_meta_versions
		WHERE tenant_id = ? AND deleted_at IS NULL AND (schema_status = 'failed'
			OR (schema_status = 'pending' AND updated_at < CURRENT_TIMESTAMP(3) - INTERVAL ? MINUTE))
		ORDER BY seq DESC LIMIT 1`, tenantID, StaleSchemaPendingMinutes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("metaversion.VersionRepo.LatestRetryableSchemaVersion: %w", err)
	}
	return &v, nil
}

// SetSchemaStatus moves a version's schema_status from one value to another and reports whether this
// call made the move (a compare-and-set: the WHERE excludes the target state, so of two concurrent
// retries exactly one wins). Tenant-scoped.
func (r *VersionRepo) SetSchemaStatus(ctx context.Context, tenantID, versionID, from, to string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `UPDATE sys_meta_versions SET schema_status = ? WHERE tenant_id = ? AND id = ? AND schema_status = ?`,
		to, tenantID, versionID, from)
	if err != nil {
		return false, fmt.Errorf("metaversion.VersionRepo.SetSchemaStatus: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("metaversion.VersionRepo.SetSchemaStatus: rows affected: %w", err)
	}
	return n > 0, nil
}

// CoveredTypesUpTo is the union of covered_types of every version with seq <= upTo: the types a
// snapshot has ever captured by then. NOT inferred from snapshot rows — a type with zero documents
// at snapshot time has none, yet is covered.
func (r *VersionRepo) CoveredTypesUpTo(ctx context.Context, q sqlx.QueryerContext, tenantID string, upTo uint) ([]string, error) {
	var raws []sql.NullString
	if err := sqlx.SelectContext(ctx, q, &raws, `SELECT covered_types FROM sys_meta_versions
		WHERE tenant_id = ? AND seq <= ? AND covered_types IS NOT NULL`, tenantID, upTo); err != nil {
		return nil, fmt.Errorf("metaversion.VersionRepo.CoveredTypesUpTo: %w", err)
	}
	seen := map[string]bool{}
	for _, raw := range raws {
		if !raw.Valid {
			continue
		}
		var ts []string
		if err := json.Unmarshal([]byte(raw.String), &ts); err != nil {
			return nil, fmt.Errorf("metaversion.VersionRepo.CoveredTypesUpTo: decode: %w", err)
		}
		for _, t := range ts {
			seen[t] = true
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}

// ActorLabel is the display name (full name, else email) of the CALLER's own user row in their home
// tenant, captured when a version is written — history never looks a name up later or across
// tenants. An unknown user yields "" (not an error).
func (r *VersionRepo) ActorLabel(ctx context.Context, q sqlx.QueryerContext, homeTenantID, userID string) string {
	var label string
	err := sqlx.GetContext(ctx, q, &label, `SELECT COALESCE(NULLIF(full_name, ''), email) FROM users
		WHERE tenant_id = ? AND id = ? LIMIT 1`, homeTenantID, userID)
	if err != nil {
		return ""
	}
	return label
}

// ListVersions pages the tenant's history newest first (seq DESC, the unique (tenant_id, seq) index)
// and returns the total. Reads on the repo's own pool.
func (r *VersionRepo) ListVersions(ctx context.Context, tenantID string, limit, offset int) ([]Version, int, error) {
	var total int
	if err := r.db.GetContext(ctx, &total, `SELECT COUNT(*) FROM sys_meta_versions WHERE tenant_id = ? AND deleted_at IS NULL`, tenantID); err != nil {
		return nil, 0, fmt.Errorf("metaversion.VersionRepo.ListVersions: count: %w", err)
	}
	var vs []Version
	if err := r.db.SelectContext(ctx, &vs, `SELECT `+versionCols+` FROM sys_meta_versions
		WHERE tenant_id = ? AND deleted_at IS NULL ORDER BY seq DESC LIMIT ? OFFSET ?`, tenantID, limit, offset); err != nil {
		return nil, 0, fmt.Errorf("metaversion.VersionRepo.ListVersions: %w", err)
	}
	return vs, total, nil
}

// GetVersion reads one version by id, pinned to the tenant BEFORE the id is used (FR-081).
func (r *VersionRepo) GetVersion(ctx context.Context, tenantID, id string) (*Version, error) {
	var v Version
	err := r.db.GetContext(ctx, &v, `SELECT `+versionCols+` FROM sys_meta_versions
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, tenantID, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrVersionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("metaversion.VersionRepo.GetVersion: %w", err)
	}
	return &v, nil
}

// HasLaterDoc reports whether a history entry newer than afterSeq (the entry's own seq) touched the
// same document (tenant-pinned). A "delete" entry is only the latest word on its document if not.
func (r *VersionRepo) HasLaterDoc(ctx context.Context, tenantID, docType, docKey string, afterSeq uint) (bool, error) {
	var n int
	if err := r.db.GetContext(ctx, &n, `SELECT COUNT(*) FROM sys_meta_version_docs d
		JOIN sys_meta_versions v ON v.id = d.version_id AND v.tenant_id = d.tenant_id AND v.deleted_at IS NULL
		WHERE d.tenant_id = ? AND d.doc_type = ? AND d.doc_key = ? AND d.deleted_at IS NULL AND v.seq > ?`,
		tenantID, docType, docKey, afterSeq); err != nil {
		return false, fmt.Errorf("metaversion.VersionRepo.HasLaterDoc: %w", err)
	}
	return n > 0, nil
}

// ListDocs lists the documents of one version of the tenant. Snapshot rows (a baseline) come back
// without bodies: a first publish holds every document of the workspace and the list never needs them.
func (r *VersionRepo) ListDocs(ctx context.Context, tenantID, versionID string) ([]VersionDoc, error) {
	type row struct {
		DocType     string         `db:"doc_type"`
		DocKey      string         `db:"doc_key"`
		Label       string         `db:"label"`
		EntityName  sql.NullString `db:"entity_name"`
		Op          string         `db:"op"`
		BeforeJSON  []byte         `db:"before_json"`
		AfterJSON   []byte         `db:"after_json"`
		ApplyStatus string         `db:"apply_status"`
		ApplyError  sql.NullString `db:"apply_error"`
		Meta        []byte         `db:"meta"`
	}
	var rows []row
	if err := r.db.SelectContext(ctx, &rows, `SELECT doc_type, doc_key, label, entity_name, op,
			IF(op = 'snapshot', NULL, before_json) AS before_json, IF(op = 'snapshot', NULL, after_json) AS after_json,
			apply_status, apply_error, meta
		FROM sys_meta_version_docs WHERE tenant_id = ? AND version_id = ? AND deleted_at IS NULL
		ORDER BY doc_type, doc_key`, tenantID, versionID); err != nil {
		return nil, fmt.Errorf("metaversion.VersionRepo.ListDocs: %w", err)
	}
	out := make([]VersionDoc, len(rows))
	for i, x := range rows {
		out[i] = VersionDoc{TenantID: tenantID, VersionID: versionID, DocType: x.DocType, DocKey: x.DocKey, Label: x.Label,
			EntityName: x.EntityName.String, Op: x.Op, BeforeJSON: x.BeforeJSON, AfterJSON: x.AfterJSON,
			ApplyStatus: x.ApplyStatus, ApplyError: x.ApplyError.String, Meta: x.Meta}
	}
	return out, nil
}
