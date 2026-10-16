// Package files is the file-attachment subsystem (File-upload M1, ENGINE-COVERAGE-SPEC §EPIC
// CROSS "File / media / document attachment"). It catalogs uploaded blobs in the tenant-scoped
// sys_files table (this repository) and serves them through FileService (service.go), which sits
// on top of a storage.Backend for the bytes and the record API for tenant + A5 authorization.
//
// Safety: EVERY query filters tenant_id (rules: đa tổ chức) — a file row is only ever read,
// served, or deleted under the caller's verified tenant. Values always go through `?`
// placeholders (parameterized sqlx); identifiers are static.
package files

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// ErrFileNotFound is returned when a file row does not exist (or is soft-deleted, or belongs to
// another tenant). The service maps it to a 404 so a cross-tenant probe is indistinguishable
// from a genuine miss (no existence oracle).
var ErrFileNotFound = errors.New("files: file not found")

// File is a sys_files row: the metadata catalog entry for one uploaded blob. The blob itself
// lives on the storage.Backend at Path; this struct is what the API lists/serves/deletes.
type File struct {
	ID         string `db:"id"`
	TenantID   string `db:"tenant_id"`
	EntityName string `db:"entity_name"`
	RecordID   string `db:"record_id"`
	FieldName  string `db:"field_name"`
	Name       string `db:"name"` // original client filename (display)
	Size       int64  `db:"size"`
	Mime       string `db:"mime"`
	Path       string `db:"path"` // storage backend key
	// Sha256 is the lowercase-hex digest of the stored bytes (migration 000068), written at insert
	// so restore-drill.sh can prove a restored blob byte-for-byte. Insert-only: the read queries
	// below do not select it (nothing serves it), so it is "" on rows they return.
	Sha256    string       `db:"sha256"`
	CreatedBy string       `db:"created_by"`
	CreatedAt time.Time    `db:"created_at"`
	DeletedAt sql.NullTime `db:"deleted_at"`
}

// Repository runs sys_files DML over a sqlx connection. Every method is tenant-scoped.
type Repository struct {
	db *sqlx.DB
}

// NewRepository wires a file repository over an existing sqlx connection.
func NewRepository(db *sqlx.DB) *Repository {
	return &Repository{db: db}
}

// Insert persists a new file catalog row. The caller (FileService) supplies a server-generated
// id, the verified tenant, and the resolved record/field linkage. created_at defaults at the DB.
func (r *Repository) Insert(ctx context.Context, f File) error {
	// NULLIF: a row without a digest stores NULL — what the drill and the backfill both key on —
	// never '', which the drill would read as a digest and fail on every such row.
	const q = `INSERT INTO sys_files
	    (id, tenant_id, entity_name, record_id, field_name, name, size, mime, path, created_by, sha256)
	    VALUES (:id, :tenant_id, :entity_name, :record_id, :field_name, :name, :size, :mime, :path, :created_by,
	            NULLIF(:sha256, ''))`
	if _, err := r.db.NamedExecContext(ctx, q, f); err != nil {
		return fmt.Errorf("files.Insert: %w", err)
	}
	return nil
}

// GetByID fetches a single live (not soft-deleted) file row, tenant-pinned. A row in another
// tenant, a soft-deleted row, or a missing id all return ErrFileNotFound (no cross-tenant leak).
func (r *Repository) GetByID(ctx context.Context, tenantID, id string) (*File, error) {
	const q = `SELECT id, tenant_id, entity_name, record_id, field_name, name, size, mime, path,
	                  created_by, created_at, deleted_at
	           FROM sys_files
	           WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	var f File
	if err := r.db.GetContext(ctx, &f, q, tenantID, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFileNotFound
		}
		return nil, fmt.Errorf("files.GetByID: %w", err)
	}
	return &f, nil
}

// ListByRecordField returns the live file rows attached to a (record, field) within a tenant,
// oldest-first (created_at, id). Used to assemble the [{id,name}] reference array a record's
// file field carries, and to surface the attachment list to the FE.
func (r *Repository) ListByRecordField(ctx context.Context, tenantID, entityName, recordID, fieldName string) ([]File, error) {
	const q = `SELECT id, tenant_id, entity_name, record_id, field_name, name, size, mime, path,
	                  created_by, created_at, deleted_at
	           FROM sys_files
	           WHERE tenant_id = ? AND entity_name = ? AND record_id = ? AND field_name = ?
	             AND deleted_at IS NULL
	           ORDER BY created_at ASC, id ASC`
	var out []File
	if err := r.db.SelectContext(ctx, &out, q, tenantID, entityName, recordID, fieldName); err != nil {
		return nil, fmt.Errorf("files.ListByRecordField: %w", err)
	}
	return out, nil
}

// TenantsWithUnhashed lists the tenants that still have live rows without a sha256 (uploaded before
// migration 000068, or by an older image after it). It is the backfill's ONE cross-tenant read and it
// returns tenant ids only — no row data — so every statement that touches a row stays tenant-pinned.
//
// ponytail: scans idx_files_tenant and filters sha256 on the row (the column is not indexed) — fine
// at today's attachment counts, and after the first backfill it runs once per server start. Add
// KEY (sha256, deleted_at, tenant_id) if sys_files grows past ~1M rows.
func (r *Repository) TenantsWithUnhashed(ctx context.Context) ([]string, error) {
	const q = `SELECT DISTINCT tenant_id FROM sys_files WHERE sha256 IS NULL AND deleted_at IS NULL`
	var out []string
	if err := r.db.SelectContext(ctx, &out, q); err != nil {
		return nil, fmt.Errorf("files.TenantsWithUnhashed: %w", err)
	}
	return out, nil
}

// ListUnhashed returns up to limit live rows of one tenant that carry no sha256, id > afterID, in id
// order (keyset: a row the caller could not hash stays NULL, and paging past it by id is what keeps
// the loop from re-reading it forever). Only id and path are selected — all the backfill needs.
func (r *Repository) ListUnhashed(ctx context.Context, tenantID, afterID string, limit int) ([]File, error) {
	const q = `SELECT id, path FROM sys_files
	           WHERE tenant_id = ? AND sha256 IS NULL AND deleted_at IS NULL AND id > ?
	           ORDER BY id LIMIT ?`
	var out []File
	if err := r.db.SelectContext(ctx, &out, q, tenantID, afterID, limit); err != nil {
		return nil, fmt.Errorf("files.ListUnhashed: %w", err)
	}
	return out, nil
}

// SetSha256 records the digest of a row that has none yet (tenant-pinned). `sha256 IS NULL` in the
// WHERE means it can never overwrite the digest an upload wrote — at worst a concurrent backfill
// makes it a zero-row no-op.
func (r *Repository) SetSha256(ctx context.Context, tenantID, id, sum string) error {
	const q = `UPDATE sys_files SET sha256 = ? WHERE tenant_id = ? AND id = ? AND sha256 IS NULL`
	if _, err := r.db.ExecContext(ctx, q, sum, tenantID, id); err != nil {
		return fmt.Errorf("files.SetSha256: %w", err)
	}
	return nil
}

// SoftDelete marks a file row deleted (tenant-pinned). It returns ErrFileNotFound when no live
// row matched (already deleted, wrong tenant, or unknown id) so a double-delete is a clean 404
// rather than a silent success. The on-disk blob is removed separately by the service.
func (r *Repository) SoftDelete(ctx context.Context, tenantID, id string) error {
	const q = `UPDATE sys_files SET deleted_at = ?
	           WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	res, err := r.db.ExecContext(ctx, q, time.Now().UTC(), tenantID, id)
	if err != nil {
		return fmt.Errorf("files.SoftDelete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("files.SoftDelete: rows affected: %w", err)
	}
	if n == 0 {
		return ErrFileNotFound
	}
	return nil
}

// Restore undoes SoftDelete (tenant-pinned). The service calls it when the record write that
// follows a delete was REFUSED, so a refused delete leaves the attachment exactly as it was.
func (r *Repository) Restore(ctx context.Context, tenantID, id string) error {
	const q = `UPDATE sys_files SET deleted_at = NULL
	           WHERE tenant_id = ? AND id = ? AND deleted_at IS NOT NULL`
	if _, err := r.db.ExecContext(ctx, q, tenantID, id); err != nil {
		return fmt.Errorf("files.Restore: %w", err)
	}
	return nil
}
