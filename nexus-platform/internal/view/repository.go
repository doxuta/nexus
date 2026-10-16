// Package view persists and reads sys_views — the W9b saved-view definitions a user keeps against
// a dynamic entity (../specs/018-saved-views/spec.md). It mirrors the report/dashboard repository style:
// identifiers are static (the fixed sys_views table), values always travel as `?` placeholders,
// and tenant_id pins every statement (multi-tenant invariant). Ownership/visibility rules (shared
// vs personal, admin-gate for shared) are enforced at the service layer; this repo only provides
// tenant-scoped persistence primitives.
package view

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// SQLRepository is the sqlx-backed sys_views store.
type SQLRepository struct {
	db  *sqlx.DB
	now func() time.Time // injectable clock; defaults to time.Now
}

// NewSQLRepository wires the view repository over an existing sqlx connection.
func NewSQLRepository(db *sqlx.DB) *SQLRepository {
	return &SQLRepository{db: db, now: time.Now}
}

// ErrViewNotFound is returned by Get/Update/Delete for an unknown, cross-tenant, or already-deleted
// id. The service maps it to 404 VIEW_NOT_FOUND, and also RETURNS it for another user's personal
// view (no cross-user leak — see ViewService.mutableView).
var ErrViewNotFound = fmt.Errorf("view not found")

// viewRow is the persisted sys_views row. Config is kept as raw JSON so the repo forwards it
// verbatim to the read view. UserID is NULL for a shared (tenant-wide) view.
type viewRow struct {
	ID         string          `db:"id"`
	TenantID   string          `db:"tenant_id"`
	UserID     *string         `db:"user_id"`
	EntityName string          `db:"entity_name"`
	Name       string          `db:"name"`
	Config     json.RawMessage `db:"config_json"`
	CreatedBy  *string         `db:"created_by"`
	UpdatedBy  *string         `db:"updated_by"`
	CreatedAt  time.Time       `db:"created_at"`
	UpdatedAt  time.Time       `db:"updated_at"`
}

// Record is the persisted-row view returned by the read API (camelCase via json tags). Shared is a
// derived convenience flag (UserID == nil) so the FE can badge shared vs personal without
// interpreting the nullable userId.
type Record struct {
	ID         string          `json:"id"`
	TenantID   string          `json:"tenantId"`
	UserID     *string         `json:"userId"` // nil = shared/tenant-wide
	Shared     bool            `json:"shared"` // derived: userId == nil
	EntityName string          `json:"entityName"`
	Name       string          `json:"name"`
	Config     json.RawMessage `json:"config"`
	CreatedBy  *string         `json:"createdBy"`
	UpdatedBy  *string         `json:"updatedBy"`
	CreatedAt  time.Time       `json:"createdAt"`
	UpdatedAt  time.Time       `json:"updatedAt"`
}

// CreateInput is the validated payload to insert a new view. UserID nil = shared (the service has
// already verified the caller is an admin for the shared case). Config is validated JSON.
type CreateInput struct {
	TenantID   string
	UserID     *string // nil = shared
	EntityName string
	Name       string
	Config     []byte // JSON (non-nil)
	CreatedBy  string
}

// Create inserts a view and returns its generated id.
func (r *SQLRepository) Create(ctx context.Context, in CreateInput) (string, error) {
	id := uuid.NewString()
	var userID sql.NullString
	if in.UserID != nil && strings.TrimSpace(*in.UserID) != "" {
		userID = sql.NullString{String: *in.UserID, Valid: true}
	}
	var createdBy sql.NullString
	if strings.TrimSpace(in.CreatedBy) != "" {
		createdBy = sql.NullString{String: in.CreatedBy, Valid: true}
	}
	now := r.now().UTC()
	const q = `INSERT INTO sys_views
		(id, tenant_id, user_id, entity_name, name, config_json, created_by, updated_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := r.db.ExecContext(ctx, q,
		id, in.TenantID, userID, in.EntityName, in.Name, string(in.Config), createdBy, createdBy, now, now,
	); err != nil {
		return "", fmt.Errorf("view.SQLRepository.Create: %w", err)
	}
	return id, nil
}

// ListForUser returns the views VISIBLE to a user for an entity, newest-first: the union of shared
// (user_id IS NULL) and the caller's OWN personal (user_id = ?) views. Another user's personal
// views are never returned. tenant_id + entity_name pin the query.
func (r *SQLRepository) ListForUser(ctx context.Context, tenantID, userID, entityName string) ([]Record, error) {
	const q = `SELECT id, tenant_id, user_id, entity_name, name, config_json,
		created_by, updated_by, created_at, updated_at
		FROM sys_views
		WHERE tenant_id = ? AND entity_name = ? AND deleted_at IS NULL
		  AND (user_id IS NULL OR user_id = ?)
		ORDER BY created_at DESC, id DESC`
	var rows []viewRow
	if err := r.db.SelectContext(ctx, &rows, q, tenantID, entityName, userID); err != nil {
		return nil, fmt.Errorf("view.SQLRepository.ListForUser: %w", err)
	}
	out := make([]Record, len(rows))
	for i := range rows {
		out[i] = rowToRecord(&rows[i])
	}
	return out, nil
}

// Get returns one tenant-scoped, non-deleted view (ANY owner), or ErrViewNotFound. It is
// deliberately NOT user-scoped: the service reads the row to inspect ownership, then enforces the
// mutate rules (own-personal / admin-shared) and translates another user's personal view into
// ErrViewNotFound so nothing leaks.
func (r *SQLRepository) Get(ctx context.Context, tenantID, id string) (*Record, error) {
	const q = `SELECT id, tenant_id, user_id, entity_name, name, config_json,
		created_by, updated_by, created_at, updated_at
		FROM sys_views
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	var row viewRow
	if err := r.db.GetContext(ctx, &row, q, tenantID, id); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrViewNotFound
		}
		return nil, fmt.Errorf("view.SQLRepository.Get: %w", err)
	}
	rec := rowToRecord(&row)
	return &rec, nil
}

// UpdateInput carries the mutable fields of a view. entity_name and user_id (ownership) are
// immutable — a view belongs to one entity and one owner for life.
type UpdateInput struct {
	Name      string
	Config    []byte // JSON (non-nil)
	UpdatedBy string
}

// Update applies name + config to a tenant-scoped, non-deleted view. The service has already
// verified the caller may mutate this view. Returns ErrViewNotFound when no live row matched.
func (r *SQLRepository) Update(ctx context.Context, tenantID, id string, in UpdateInput) error {
	var updatedBy sql.NullString
	if strings.TrimSpace(in.UpdatedBy) != "" {
		updatedBy = sql.NullString{String: in.UpdatedBy, Valid: true}
	}
	const q = `UPDATE sys_views SET name = ?, config_json = ?, updated_by = ?
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	res, err := r.db.ExecContext(ctx, q, in.Name, string(in.Config), updatedBy, tenantID, id)
	if err != nil {
		return fmt.Errorf("view.SQLRepository.Update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("view.SQLRepository.Update: rows affected: %w", err)
	}
	if n == 0 {
		return ErrViewNotFound
	}
	return nil
}

// Delete soft-deletes a tenant-scoped view (stamps deleted_at). The service has already verified
// the caller may mutate it. Returns ErrViewNotFound when no live row matched.
func (r *SQLRepository) Delete(ctx context.Context, tenantID, id string) error {
	const q = `UPDATE sys_views SET deleted_at = ? WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	res, err := r.db.ExecContext(ctx, q, r.now().UTC(), tenantID, id)
	if err != nil {
		return fmt.Errorf("view.SQLRepository.Delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("view.SQLRepository.Delete: rows affected: %w", err)
	}
	if n == 0 {
		return ErrViewNotFound
	}
	return nil
}

// rowToRecord projects a persisted row to the read view (raw JSON config forwarded verbatim).
func rowToRecord(row *viewRow) Record {
	cfg := row.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage("null")
	}
	return Record{
		ID:         row.ID,
		TenantID:   row.TenantID,
		UserID:     row.UserID,
		Shared:     row.UserID == nil,
		EntityName: row.EntityName,
		Name:       row.Name,
		Config:     cfg,
		CreatedBy:  row.CreatedBy,
		UpdatedBy:  row.UpdatedBy,
		CreatedAt:  row.CreatedAt,
		UpdatedAt:  row.UpdatedAt,
	}
}
