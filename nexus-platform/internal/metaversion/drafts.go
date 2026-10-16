package metaversion

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// DraftRow is one row of sys_meta_drafts: the verbatim canonical form of the document the builder
// changed (DraftJSON nil for a delete) plus the fingerprint of the live document it was started from.
type DraftRow struct {
	ID         string    `db:"id"`
	TenantID   string    `db:"tenant_id"`
	DocType    string    `db:"doc_type"`
	DocKey     string    `db:"doc_key"`
	Op         string    `db:"op"`
	DraftJSON  []byte    `db:"draft_json"`
	BaseHash   *string   `db:"base_hash"`
	EntityName *string   `db:"entity_name"`
	Label      string    `db:"label"`
	CreatedBy  string    `db:"created_by"`
	ModifiedBy *string   `db:"modified_by"`
	CreatedAt  time.Time `db:"created_at"`
	UpdatedAt  time.Time `db:"updated_at"`
}

const draftCols = `id, tenant_id, doc_type, doc_key, op, draft_json, base_hash, entity_name, label,
	created_by, modified_by, created_at, updated_at`

// ErrDraftNotFound: no draft with that id in the tenant (also another tenant's id — no leak).
var ErrDraftNotFound = errors.New("metaversion: draft not found")

// DraftRepo reads and writes sys_meta_drafts. It is the ONLY code that names that table (FR-016).
// Every statement pins tenant_id; doc_type only ever comes from the registry whitelist.
type DraftRepo struct {
	db  *sqlx.DB
	reg *Registry
}

// NewDraftRepo wires the repo over the shared sqlx pool and the adapter registry.
func NewDraftRepo(db *sqlx.DB, reg *Registry) *DraftRepo { return &DraftRepo{db: db, reg: reg} }

// UpsertInput is one draft save. Doc is the canonical form of the document (nil for OpDelete).
type UpsertInput struct {
	DocType    string
	DocKey     string
	Op         string
	Doc        json.RawMessage
	EntityName string
	Label      string
	UserID     string
}

// Upsert saves (or re-saves) the draft of one document. base_hash — the fingerprint of the live
// document when the draft was FIRST created — is written once and never overwritten by a re-save.
// It runs in a small tx that takes tenants ... FOR SHARE first: that waits while a publish holds
// the row FOR UPDATE, so a save can never slip in carrying the fingerprint of the live state the
// publish is just replacing (a false PUBLISH_CONFLICT on the next publish).
func (r *DraftRepo) Upsert(ctx context.Context, tenantID string, in UpsertInput) error {
	ad, err := r.reg.Must(in.DocType) // before any SQL
	if err != nil {
		return fmt.Errorf("metaversion.DraftRepo.Upsert: %w", err)
	}
	if in.Op != OpUpsert && in.Op != OpDelete {
		return fmt.Errorf("metaversion.DraftRepo.Upsert: bad op %q", in.Op)
	}
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("metaversion.DraftRepo.Upsert: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var lockID string
	if err := tx.GetContext(ctx, &lockID, `SELECT id FROM tenants WHERE id = ? FOR SHARE`, tenantID); err != nil {
		return fmt.Errorf("metaversion.DraftRepo.Upsert: lock tenant: %w", err)
	}
	live, ok, err := ad.LoadLive(ctx, tx, tenantID, in.DocKey, false)
	if err != nil {
		return fmt.Errorf("metaversion.DraftRepo.Upsert: load live: %w", err)
	}
	var base any
	if ok {
		h, err := DocHash(live)
		if err != nil {
			return fmt.Errorf("metaversion.DraftRepo.Upsert: hash live: %w", err)
		}
		base = h
	}
	var doc any
	if in.Op == OpUpsert {
		if len(in.Doc) == 0 {
			return fmt.Errorf("metaversion.DraftRepo.Upsert: empty document for upsert of %s/%s", in.DocType, in.DocKey)
		}
		doc = string(in.Doc)
	}
	var entity any
	if in.EntityName != "" {
		entity = in.EntityName
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sys_meta_drafts
		(id, tenant_id, doc_type, doc_key, op, draft_json, base_hash, entity_name, label, created_by, modified_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE op = VALUES(op), draft_json = VALUES(draft_json), label = VALUES(label),
			entity_name = VALUES(entity_name), modified_by = VALUES(modified_by), updated_at = CURRENT_TIMESTAMP(3)`,
		uuid.NewString(), tenantID, in.DocType, in.DocKey, in.Op, doc, base, entity, in.Label, in.UserID, in.UserID); err != nil {
		return fmt.Errorf("metaversion.DraftRepo.Upsert: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("metaversion.DraftRepo.Upsert: commit: %w", err)
	}
	return nil
}

// ListByType lists the drafts of one document type (the read overlay).
func (r *DraftRepo) ListByType(ctx context.Context, tenantID, docType string) ([]DraftRow, error) {
	if _, err := r.reg.Must(docType); err != nil {
		return nil, fmt.Errorf("metaversion.DraftRepo.ListByType: %w", err)
	}
	var out []DraftRow
	if err := r.db.SelectContext(ctx, &out, `SELECT `+draftCols+` FROM sys_meta_drafts
		WHERE tenant_id = ? AND doc_type = ? ORDER BY updated_at, id`, tenantID, docType); err != nil {
		return nil, fmt.Errorf("metaversion.DraftRepo.ListByType: %w", err)
	}
	return out, nil
}

// List lists every draft of the tenant, optionally locking them (the publish tx).
func (r *DraftRepo) List(ctx context.Context, q sqlx.QueryerContext, tenantID string, forUpdate bool) ([]DraftRow, error) {
	query := `SELECT ` + draftCols + ` FROM sys_meta_drafts WHERE tenant_id = ? ORDER BY updated_at, id`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var out []DraftRow
	if err := sqlx.SelectContext(ctx, q, &out, query, tenantID); err != nil {
		return nil, fmt.Errorf("metaversion.DraftRepo.List: %w", err)
	}
	return out, nil
}

// ListRead is List on the repo's own pool.
func (r *DraftRepo) ListRead(ctx context.Context, tenantID string) ([]DraftRow, error) {
	return r.List(ctx, r.db, tenantID, false)
}

// Get returns one draft of the tenant by id (ErrDraftNotFound otherwise, also for another tenant's id).
func (r *DraftRepo) Get(ctx context.Context, tenantID, id string) (*DraftRow, error) {
	var d DraftRow
	if err := r.db.GetContext(ctx, &d, `SELECT `+draftCols+` FROM sys_meta_drafts WHERE tenant_id = ? AND id = ?`, tenantID, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrDraftNotFound
		}
		return nil, fmt.Errorf("metaversion.DraftRepo.Get: %w", err)
	}
	return &d, nil
}

// GetByKey returns the draft of one document of the tenant, nil when there is none.
func (r *DraftRepo) GetByKey(ctx context.Context, tenantID, docType, docKey string) (*DraftRow, error) {
	if _, err := r.reg.Must(docType); err != nil {
		return nil, fmt.Errorf("metaversion.DraftRepo.GetByKey: %w", err)
	}
	var d DraftRow
	err := r.db.GetContext(ctx, &d, `SELECT `+draftCols+` FROM sys_meta_drafts WHERE tenant_id = ? AND doc_type = ? AND doc_key = ?`,
		tenantID, docType, docKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("metaversion.DraftRepo.GetByKey: %w", err)
	}
	return &d, nil
}

// DeleteByKey removes the draft of one document; deleted reports whether a row existed.
func (r *DraftRepo) DeleteByKey(ctx context.Context, tenantID, docType, docKey string) (bool, error) {
	if _, err := r.reg.Must(docType); err != nil {
		return false, fmt.Errorf("metaversion.DraftRepo.DeleteByKey: %w", err)
	}
	res, err := r.db.ExecContext(ctx, `DELETE FROM sys_meta_drafts WHERE tenant_id = ? AND doc_type = ? AND doc_key = ?`,
		tenantID, docType, docKey)
	if err != nil {
		return false, fmt.Errorf("metaversion.DraftRepo.DeleteByKey: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("metaversion.DraftRepo.DeleteByKey: rows affected: %w", err)
	}
	return n > 0, nil
}

// DeleteIDs removes the drafts with those ids of the tenant (on a tx: publish / discard). It
// returns how many rows went.
func (r *DraftRepo) DeleteIDs(ctx context.Context, ex sqlx.ExtContext, tenantID string, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	q, args, err := sqlx.In(`DELETE FROM sys_meta_drafts WHERE tenant_id = ? AND id IN (?)`, tenantID, ids)
	if err != nil {
		return 0, fmt.Errorf("metaversion.DraftRepo.DeleteIDs: %w", err)
	}
	res, err := ex.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("metaversion.DraftRepo.DeleteIDs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("metaversion.DraftRepo.DeleteIDs: rows affected: %w", err)
	}
	return n, nil
}

// Count is the number of pending drafts of the tenant.
func (r *DraftRepo) Count(ctx context.Context, tenantID string) (int, error) {
	var n int
	if err := r.db.GetContext(ctx, &n, `SELECT COUNT(*) FROM sys_meta_drafts WHERE tenant_id = ?`, tenantID); err != nil {
		return 0, fmt.Errorf("metaversion.DraftRepo.Count: %w", err)
	}
	return n, nil
}
