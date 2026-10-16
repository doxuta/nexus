package report

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

// SQLRepository persists and reads sys_reports over sqlx with parameterized queries. It mirrors
// the workflow/audit repository style: identifiers are static (the fixed sys_reports table),
// values always travel as `?` placeholders, and tenant_id pins every statement (multi-tenant
// invariant). It serves the admin CRUD surface (Create/Get/List/Update/Delete); the report RUN
// path reads a single row via Get and then executes its config through the aggregate/list engine.
type SQLRepository struct {
	db  *sqlx.DB
	now func() time.Time // injectable clock; defaults to time.Now
}

// NewSQLRepository wires the report repository over an existing sqlx connection.
func NewSQLRepository(db *sqlx.DB) *SQLRepository {
	return &SQLRepository{db: db, now: time.Now}
}

// reportRow is the persisted sys_reports row. Config is kept as raw JSON so the repo forwards it
// verbatim to the admin read view and the service decodes it once at run time.
type reportRow struct {
	ID         string          `db:"id"`
	TenantID   string          `db:"tenant_id"`
	Name       string          `db:"name"`
	EntityName string          `db:"entity_name"`
	Type       string          `db:"type"`
	Config     json.RawMessage `db:"config"`
	IsActive   bool            `db:"is_active"`
	CreatedBy  *string         `db:"created_by"`
	UpdatedBy  *string         `db:"updated_by"`
	CreatedAt  time.Time       `db:"created_at"`
	UpdatedAt  time.Time       `db:"updated_at"`
}

// Record is the persisted-row view returned by the admin read API: it carries the raw JSON config
// plus audit columns so the handler forwards them verbatim (camelCase via the json tags).
type Record struct {
	ID         string          `json:"id"`
	TenantID   string          `json:"tenantId"`
	Name       string          `json:"name"`
	EntityName string          `json:"entityName"`
	Type       string          `json:"type"`
	Config     json.RawMessage `json:"config"`
	IsActive   bool            `json:"isActive"`
	CreatedBy  *string         `json:"createdBy"`
	UpdatedBy  *string         `json:"updatedBy"`
	CreatedAt  time.Time       `json:"createdAt"`
	UpdatedAt  time.Time       `json:"updatedAt"`
	// DraftState is set only on a read over the builder's draft overlay (specs/063 S7-2): live | edited | new | deleted.
	DraftState string `json:"draftState,omitempty"`
}

// CreateInput is the validated payload to insert a new report. Config is the already-validated,
// JSON-marshalled report definition. CreatedBy is the actor user id (NULL when empty).
type CreateInput struct {
	TenantID   string
	Name       string
	EntityName string
	Type       Type
	Config     []byte // JSON (non-nil)
	IsActive   bool
	CreatedBy  string
}

// Create inserts a report and returns its generated id.
func (r *SQLRepository) Create(ctx context.Context, in CreateInput) (string, error) {
	id := uuid.NewString()
	if err := InsertWithIDExec(ctx, r.db, id, in, r.now()); err != nil {
		return "", fmt.Errorf("report.SQLRepository.Create: %w", err)
	}
	return id, nil
}

// ErrReportNotFound is returned by Get/Update/Delete for an unknown or cross-tenant id.
var ErrReportNotFound = fmt.Errorf("report not found")

// Get returns one tenant-scoped, non-deleted report, or ErrReportNotFound.
func (r *SQLRepository) Get(ctx context.Context, tenantID, id string) (*Record, error) {
	const q = `SELECT id, tenant_id, name, entity_name, type, config, is_active,
		created_by, updated_by, created_at, updated_at
		FROM sys_reports
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	var row reportRow
	if err := r.db.GetContext(ctx, &row, q, tenantID, id); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrReportNotFound
		}
		return nil, fmt.Errorf("report.SQLRepository.Get: %w", err)
	}
	rec := rowToRecord(&row)
	return &rec, nil
}

// List returns every tenant-scoped, non-deleted report newest-first (admin list page).
func (r *SQLRepository) List(ctx context.Context, tenantID string) ([]Record, error) {
	const q = `SELECT id, tenant_id, name, entity_name, type, config, is_active,
		created_by, updated_by, created_at, updated_at
		FROM sys_reports
		WHERE tenant_id = ? AND deleted_at IS NULL
		ORDER BY created_at DESC, id DESC`
	var rows []reportRow
	if err := r.db.SelectContext(ctx, &rows, q, tenantID); err != nil {
		return nil, fmt.Errorf("report.SQLRepository.List: %w", err)
	}
	out := make([]Record, len(rows))
	for i := range rows {
		out[i] = rowToRecord(&rows[i])
	}
	return out, nil
}

// UpdateInput carries the validated full-replace update of a report's mutable fields. Every field
// is supplied (the service re-validates before calling), so the UPDATE sets them all.
type UpdateInput struct {
	Name       string
	EntityName string
	Type       Type
	Config     []byte // JSON (non-nil)
	IsActive   bool
	UpdatedBy  string
}

// Update applies the supplied fields to a tenant-scoped, non-deleted report. Returns
// ErrReportNotFound when no live row matches (unknown / cross-tenant / already-deleted id).
func (r *SQLRepository) Update(ctx context.Context, tenantID, id string, in UpdateInput) error {
	if err := UpdateExec(ctx, r.db, tenantID, id, in); err != nil {
		if errors.Is(err, ErrReportNotFound) {
			return ErrReportNotFound
		}
		return fmt.Errorf("report.SQLRepository.Update: %w", err)
	}
	return nil
}

// Delete soft-deletes a tenant-scoped report (stamps deleted_at), so a removed report keeps its
// history and never runs again. Returns ErrReportNotFound when no live row matched.
func (r *SQLRepository) Delete(ctx context.Context, tenantID, id string) error {
	if err := SoftDeleteExec(ctx, r.db, tenantID, id, r.now()); err != nil {
		if errors.Is(err, ErrReportNotFound) {
			return ErrReportNotFound
		}
		return fmt.Errorf("report.SQLRepository.Delete: %w", err)
	}
	return nil
}

// rowToRecord projects a persisted row to the admin read view (raw JSON config forwarded verbatim).
func rowToRecord(row *reportRow) Record {
	cfg := row.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage("null")
	}
	return Record{
		ID:         row.ID,
		TenantID:   row.TenantID,
		Name:       row.Name,
		EntityName: row.EntityName,
		Type:       row.Type,
		Config:     cfg,
		IsActive:   row.IsActive,
		CreatedBy:  row.CreatedBy,
		UpdatedBy:  row.UpdatedBy,
		CreatedAt:  row.CreatedAt,
		UpdatedAt:  row.UpdatedAt,
	}
}
