// Package notes is the record-notes subsystem (docs/NEXUS-MOT-XUONG-2026-07-29.md §3d mục 1
// "Trao đổi trên hồ sơ"): a free-text note ("đã gọi, khách hẹn tuần sau") a user attaches to ANY
// record — an ENGINE capability for every entity, not a CRM-only feature. It catalogs notes in the
// tenant-scoped sys_record_notes table (this repository) and serves them through Service
// (service.go), which authorizes every read/write against the parent record's tenant + A5 scope via
// services.RecordService.ResolveNoteTarget — mirroring how internal/files sits on top of
// services.RecordService.ResolveFileField for attachments.
//
// Safety: EVERY query filters tenant_id (rules: đa tổ chức) — a note row is only ever read, listed,
// or deleted under the caller's verified tenant. Values always go through `?` placeholders
// (parameterized sqlx); identifiers are static.
package notes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// ErrNoteNotFound is returned when a note row does not exist for the given (tenant, entity,
// record, id) — unknown id, wrong tenant, wrong parent record, or soft-deleted all collapse to
// this ONE sentinel so a cross-record probe (guessing a noteId that belongs to a different record)
// cannot be distinguished from a genuine miss (no existence oracle).
var ErrNoteNotFound = errors.New("notes: note not found")

// Note is a sys_record_notes row.
type Note struct {
	ID         string       `db:"id"`
	TenantID   string       `db:"tenant_id"`
	EntityName string       `db:"entity_name"`
	RecordID   string       `db:"record_id"`
	Body       string       `db:"body"`
	CreatedBy  string       `db:"created_by"`
	CreatedAt  time.Time    `db:"created_at"`
	UpdatedAt  time.Time    `db:"updated_at"`
	DeletedAt  sql.NullTime `db:"deleted_at"`
}

// Repository runs sys_record_notes DML over a sqlx connection. Every method is tenant-scoped.
type Repository struct {
	db *sqlx.DB
}

// NewRepository wires a note repository over an existing sqlx connection.
func NewRepository(db *sqlx.DB) *Repository {
	return &Repository{db: db}
}

// Insert persists a new note row. The caller (Service) supplies a server-generated id, the
// verified tenant, the resolved (canonical) entity/record linkage, and the real authenticated
// author id.
func (r *Repository) Insert(ctx context.Context, n Note) error {
	const q = `INSERT INTO sys_record_notes
	    (id, tenant_id, entity_name, record_id, body, created_by, created_at, updated_at)
	    VALUES (:id, :tenant_id, :entity_name, :record_id, :body, :created_by, :created_at, :updated_at)`
	if _, err := r.db.NamedExecContext(ctx, q, n); err != nil {
		return fmt.Errorf("notes.Insert: %w", err)
	}
	return nil
}

// ListByRecord returns a page of live (not soft-deleted) notes attached to (entityName, recordID)
// within a tenant, newest-first (created_at DESC, id DESC as a stable tie-break).
func (r *Repository) ListByRecord(ctx context.Context, tenantID, entityName, recordID string, limit, offset int) ([]Note, error) {
	const q = `SELECT id, tenant_id, entity_name, record_id, body, created_by, created_at, updated_at, deleted_at
	           FROM sys_record_notes
	           WHERE tenant_id = ? AND entity_name = ? AND record_id = ? AND deleted_at IS NULL
	           ORDER BY created_at DESC, id DESC
	           LIMIT ? OFFSET ?`
	var out []Note
	if err := r.db.SelectContext(ctx, &out, q, tenantID, entityName, recordID, limit, offset); err != nil {
		return nil, fmt.Errorf("notes.ListByRecord: %w", err)
	}
	return out, nil
}

// CountByRecord returns the total live-note count for (entityName, recordID) within a tenant, for
// the offset-pagination envelope (api-conventions.md §Phân trang chế độ 1).
func (r *Repository) CountByRecord(ctx context.Context, tenantID, entityName, recordID string) (int, error) {
	const q = `SELECT COUNT(*) FROM sys_record_notes
	           WHERE tenant_id = ? AND entity_name = ? AND record_id = ? AND deleted_at IS NULL`
	var n int
	if err := r.db.GetContext(ctx, &n, q, tenantID, entityName, recordID); err != nil {
		return 0, fmt.Errorf("notes.CountByRecord: %w", err)
	}
	return n, nil
}

// GetForRecord fetches a single live note, scoped to the EXACT (tenant, entity, record) triple —
// not just tenant + id. This is deliberate: the delete route is nested under
// /:entity/:id/notes/:noteId, so a noteId that belongs to a DIFFERENT record must 404 here rather
// than let the caller's (entity, record)-level authorization apply to a note it does not own.
func (r *Repository) GetForRecord(ctx context.Context, tenantID, entityName, recordID, id string) (*Note, error) {
	const q = `SELECT id, tenant_id, entity_name, record_id, body, created_by, created_at, updated_at, deleted_at
	           FROM sys_record_notes
	           WHERE tenant_id = ? AND entity_name = ? AND record_id = ? AND id = ? AND deleted_at IS NULL`
	var n Note
	if err := r.db.GetContext(ctx, &n, q, tenantID, entityName, recordID, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoteNotFound
		}
		return nil, fmt.Errorf("notes.GetForRecord: %w", err)
	}
	return &n, nil
}

// SoftDelete marks a note row deleted (tenant-pinned). Returns ErrNoteNotFound when no live row
// matched (already deleted, wrong tenant, or unknown id) so a double-delete is a clean 404 rather
// than a silent success.
func (r *Repository) SoftDelete(ctx context.Context, tenantID, id string) error {
	const q = `UPDATE sys_record_notes SET deleted_at = ?
	           WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	res, err := r.db.ExecContext(ctx, q, time.Now().UTC(), tenantID, id)
	if err != nil {
		return fmt.Errorf("notes.SoftDelete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("notes.SoftDelete: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNoteNotFound
	}
	return nil
}
