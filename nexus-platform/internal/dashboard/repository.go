package dashboard

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

// SQLRepository persists and reads sys_dashboards over sqlx with parameterized queries. It mirrors
// the report repository style: identifiers are static, values always travel as `?` placeholders,
// and tenant_id pins every statement (multi-tenant invariant).
type SQLRepository struct {
	db  *sqlx.DB
	now func() time.Time // injectable clock; defaults to time.Now
}

// NewSQLRepository wires the dashboard repository over an existing sqlx connection.
func NewSQLRepository(db *sqlx.DB) *SQLRepository {
	return &SQLRepository{db: db, now: time.Now}
}

// dashboardRow is the persisted sys_dashboards row. Widgets is kept as raw JSON so the repo
// forwards it verbatim to the admin read view; the service decodes it at run time.
type dashboardRow struct {
	ID          string          `db:"id"`
	TenantID    string          `db:"tenant_id"`
	Name        string          `db:"name"`
	Description *string         `db:"description"`
	Widgets     json.RawMessage `db:"widgets"`
	CreatedBy   *string         `db:"created_by"`
	CreatedAt   time.Time       `db:"created_at"`
	UpdatedAt   time.Time       `db:"updated_at"`
}

// Record is the persisted-row view returned by the admin read API: it carries the raw JSON widgets
// plus audit columns so the handler forwards them verbatim (camelCase via the json tags).
type Record struct {
	ID          string          `json:"id"`
	TenantID    string          `json:"tenantId"`
	Name        string          `json:"name"`
	Description *string         `json:"description"`
	Widgets     json.RawMessage `json:"widgets"`
	CreatedBy   *string         `json:"createdBy"`
	CreatedAt   time.Time       `json:"createdAt"`
	UpdatedAt   time.Time       `json:"updatedAt"`
	// DraftState is set only on a read over the builder's draft overlay (specs/063 S7-2): live | edited | new | deleted.
	DraftState string `json:"draftState,omitempty"`
}

// CreateInput is the validated payload to insert a new dashboard. Widgets is the already-validated,
// JSON-marshalled widget array. CreatedBy is the actor user id (NULL when empty).
type CreateInput struct {
	TenantID    string
	Name        string
	Description string
	Widgets     []byte // JSON array (non-nil; marshalled by service)
	CreatedBy   string
}

// UpdateInput carries the validated full-replace payload for a dashboard's mutable fields.
type UpdateInput struct {
	Name        string
	Description string
	Widgets     []byte // JSON array (non-nil; marshalled by service)
}

// ErrDashboardNotFound is returned by Get/Update/Delete for an unknown or cross-tenant id.
var ErrDashboardNotFound = fmt.Errorf("dashboard not found")

// Create inserts a dashboard and returns its generated id.
func (r *SQLRepository) Create(ctx context.Context, in CreateInput) (string, error) {
	id := uuid.NewString()
	if err := InsertWithIDExec(ctx, r.db, id, in, r.now()); err != nil {
		return "", fmt.Errorf("dashboard.SQLRepository.Create: %w", err)
	}
	return id, nil
}

// Get returns one tenant-scoped, non-deleted dashboard, or ErrDashboardNotFound.
func (r *SQLRepository) Get(ctx context.Context, tenantID, id string) (*Record, error) {
	const q = `SELECT id, tenant_id, name, description, widgets, created_by, created_at, updated_at
		FROM sys_dashboards
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	var row dashboardRow
	if err := r.db.GetContext(ctx, &row, q, tenantID, id); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrDashboardNotFound
		}
		return nil, fmt.Errorf("dashboard.SQLRepository.Get: %w", err)
	}
	rec := rowToRecord(&row)
	return &rec, nil
}

// List returns every tenant-scoped, non-deleted dashboard newest-first (admin list page).
func (r *SQLRepository) List(ctx context.Context, tenantID string) ([]Record, error) {
	const q = `SELECT id, tenant_id, name, description, widgets, created_by, created_at, updated_at
		FROM sys_dashboards
		WHERE tenant_id = ? AND deleted_at IS NULL
		ORDER BY created_at DESC, id DESC`
	var rows []dashboardRow
	if err := r.db.SelectContext(ctx, &rows, q, tenantID); err != nil {
		return nil, fmt.Errorf("dashboard.SQLRepository.List: %w", err)
	}
	out := make([]Record, len(rows))
	for i := range rows {
		out[i] = rowToRecord(&rows[i])
	}
	return out, nil
}

// Update applies the supplied fields to a tenant-scoped, non-deleted dashboard. Returns
// ErrDashboardNotFound when no live row matches (unknown / cross-tenant / already-deleted id).
func (r *SQLRepository) Update(ctx context.Context, tenantID, id string, in UpdateInput) error {
	if err := UpdateExec(ctx, r.db, tenantID, id, in); err != nil {
		if errors.Is(err, ErrDashboardNotFound) {
			return ErrDashboardNotFound
		}
		return fmt.Errorf("dashboard.SQLRepository.Update: %w", err)
	}
	return nil
}

// Delete soft-deletes a tenant-scoped dashboard (stamps deleted_at). Returns ErrDashboardNotFound
// when no live row matched. App-container cascade (../specs/010-app-container/spec.md INV-4): any
// sys_app_nav_items row referencing this dashboard is removed in the SAME transaction (SoftDeleteExec
// runs appmeta.PurgeNavForRef on the tx), so an app sidebar never keeps a dead tab.
func (r *SQLRepository) Delete(ctx context.Context, tenantID, id string) error {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dashboard.SQLRepository.Delete: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if err := SoftDeleteExec(ctx, tx, tenantID, id, r.now()); err != nil {
		if errors.Is(err, ErrDashboardNotFound) {
			return ErrDashboardNotFound
		}
		return fmt.Errorf("dashboard.SQLRepository.Delete: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("dashboard.SQLRepository.Delete: commit: %w", err)
	}
	return nil
}

// rowToRecord projects a persisted row to the admin read view (raw JSON widgets forwarded verbatim).
func rowToRecord(row *dashboardRow) Record {
	widgets := row.Widgets
	if len(widgets) == 0 {
		widgets = json.RawMessage("[]")
	}
	return Record{
		ID:          row.ID,
		TenantID:    row.TenantID,
		Name:        row.Name,
		Description: row.Description,
		Widgets:     widgets,
		CreatedBy:   row.CreatedBy,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}
}
