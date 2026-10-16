package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ErrLayoutNotFound is returned when no layout matches the lookup criteria (id + tenant_id).
var ErrLayoutNotFound = errors.New("layout not found")

// LayoutRepository provides tenant-scoped access to sys_layouts.
// It uses GORM over the fixed-schema system table. All methods pin tenant_id to prevent
// cross-tenant data access (IDOR guard spec §5).
type LayoutRepository struct {
	db *gorm.DB
}

// NewLayoutRepository wires a layout repository over an existing GORM connection.
func NewLayoutRepository(db *gorm.DB) *LayoutRepository {
	return &LayoutRepository{db: db}
}

// GetByEntity returns all live (non-deleted) layout rows for a given (tenant, entity).
// The caller receives raw LayoutDefinition rows; the LayoutJSON field holds the stored JSON.
func (r *LayoutRepository) GetByEntity(ctx context.Context, tenantID, entityName string) ([]LayoutDefinition, error) {
	var layouts []LayoutDefinition
	if err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND entity_name = ?", tenantID, entityName).
		Find(&layouts).Error; err != nil {
		return nil, fmt.Errorf("layout.GetByEntity: %w", err)
	}
	return layouts, nil
}

// GetByID returns the layout for a given id, tenant-pinned. Returns ErrLayoutNotFound when
// no row matches (either id unknown or belongs to a different tenant — identical response so
// the caller cannot distinguish "not found" from "belongs to another tenant": spec §5 IDOR).
func (r *LayoutRepository) GetByID(ctx context.Context, tenantID, id string) (*LayoutDefinition, error) {
	var layout LayoutDefinition
	err := r.db.WithContext(ctx).
		Where("id = ? AND tenant_id = ?", id, tenantID).
		First(&layout).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrLayoutNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("layout.GetByID: %w", err)
	}
	return &layout, nil
}

// Upsert inserts a layout or updates the layout_json when the unique key
// (tenant_id, entity_name, view_type) already exists. This is a race-safe atomic upsert:
// the DB-level UNIQUE constraint + ON DUPLICATE KEY UPDATE prevents concurrent duplicates.
func (r *LayoutRepository) Upsert(ctx context.Context, layout *LayoutDefinition) error {
	// Raw INSERT ... ON DUPLICATE KEY UPDATE through the shared statement (UpsertLayoutExec): GORM's
	// Save() upserts by PK, not by the compound unique key uq_sys_layouts_tenant_entity_view.
	if err := UpsertLayoutExec(ctx, r.db.WithContext(ctx).Statement.ConnPool, layout, time.Time{}); err != nil {
		return fmt.Errorf("layout.Upsert: %w", err)
	}
	// On the ON DUPLICATE KEY UPDATE (re-save) path MySQL keeps the EXISTING row's id and
	// discards the freshly-minted layout.ID. Read back the canonical persisted id so the caller
	// (and the POST response) get a by-id handle that actually resolves on PUT/DELETE — without
	// this, a re-save returns a ghost id that 404s. The compound unique key guarantees exactly
	// one live row per (tenant, entity, view), so this selects deterministically.
	var persisted LayoutDefinition
	if err := r.db.WithContext(ctx).
		Select("id").
		Where("tenant_id = ? AND entity_name = ? AND view_type = ?",
			layout.TenantID, layout.EntityName, layout.ViewType).
		First(&persisted).Error; err != nil {
		return fmt.Errorf("layout.Upsert: read back persisted id: %w", err)
	}
	layout.ID = persisted.ID
	return nil
}

// UpdateByID updates only the layout_json for the layout identified by (id, tenant_id,
// deleted_at IS NULL). Returns ErrLayoutNotFound when 0 rows are affected (id unknown,
// cross-tenant id, or soft-deleted row). The tenant re-pin in the WHERE clause is the
// primary IDOR guard: even if the caller passes a forged id belonging to tenant B, the
// tenant_id = ? clause makes the UPDATE affect 0 rows for tenant A's principal.
func (r *LayoutRepository) UpdateByID(ctx context.Context, tenantID, id, layoutJSON string) error {
	res := r.db.WithContext(ctx).
		Model(&LayoutDefinition{}).
		Where("id = ? AND tenant_id = ? AND deleted_at IS NULL", id, tenantID).
		Update("layout_json", layoutJSON)
	if res.Error != nil {
		return fmt.Errorf("layout.UpdateByID: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrLayoutNotFound
	}
	return nil
}

// HardDelete permanently removes the layout row (no soft-delete) identified by
// (id, tenant_id). Returns ErrLayoutNotFound when 0 rows are affected. Hard-delete is
// required by spec D4: the UNIQUE constraint cannot be partial (MySQL 8 does not support
// filtered unique indexes), so keeping soft-deleted rows would block future inserts via
// ON DUPLICATE KEY UPDATE. The revert-to-default semantic is "delete the custom row;
// next GET falls back to generateDefault".
func (r *LayoutRepository) HardDelete(ctx context.Context, tenantID, id string) error {
	n, err := DeleteLayoutExec(ctx, r.db.WithContext(ctx).Statement.ConnPool, tenantID, id)
	if err != nil {
		return fmt.Errorf("layout.HardDelete: %w", err)
	}
	if n == 0 {
		return ErrLayoutNotFound
	}
	return nil
}

// Execer is the one capability the layout *Exec statements need: satisfied by *sqlx.Tx, *sqlx.DB,
// *sql.Tx and the gorm.ConnPool of a GORM handle. The repository calls them on its pool; the draft
// layer's publish (specs/063-draft-publish S7-3) calls the SAME statements on its transaction.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// UpsertLayoutExec inserts a layout or updates its layout_json when the unique key
// (tenant_id, entity_name, view_type) already exists — atomic at DB level. A zero `at` stamps NOW();
// a non-zero `at` stamps updated_at EXPLICITLY (the ON UPDATE clause of the column would otherwise
// override it): publish passes the instant the draft was last saved, so a field born between that
// save and the publish still lands in "Mới thêm" and one the builder left off the form stays off.
func UpsertLayoutExec(ctx context.Context, ex Execer, l *LayoutDefinition, at time.Time) error {
	var ts sql.NullTime
	if !at.IsZero() {
		ts = sql.NullTime{Time: at, Valid: true}
	}
	const q = `INSERT INTO sys_layouts (id, tenant_id, entity_name, view_type, layout_json, created_at, updated_at)
	        VALUES (?, ?, ?, ?, ?, COALESCE(?, NOW()), COALESCE(?, NOW()))
	        ON DUPLICATE KEY UPDATE layout_json = VALUES(layout_json), updated_at = COALESCE(?, NOW())`
	if _, err := ex.ExecContext(ctx, q, l.ID, l.TenantID, l.EntityName, l.ViewType, l.LayoutJSON, ts, ts, ts); err != nil {
		return fmt.Errorf("metadata.UpsertLayoutExec: %w", err)
	}
	return nil
}

// DeleteLayoutExec hard-deletes the layout row (id, tenant_id) and reports how many rows went
// (0 = unknown id or another tenant's). Hard delete on purpose (spec D4): the UNIQUE key cannot be
// partial, so a soft-deleted row would block the next upsert.
func DeleteLayoutExec(ctx context.Context, ex Execer, tenantID, id string) (int64, error) {
	res, err := ex.ExecContext(ctx, `DELETE FROM sys_layouts WHERE id = ? AND tenant_id = ?`, id, tenantID)
	if err != nil {
		return 0, fmt.Errorf("metadata.DeleteLayoutExec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("metadata.DeleteLayoutExec: rows affected: %w", err)
	}
	return n, nil
}

// DeleteLayoutByViewExec is DeleteLayoutExec keyed by (entity, view) — what a publish knows of a
// layout draft ("entity:view"), not the row id.
func DeleteLayoutByViewExec(ctx context.Context, ex Execer, tenantID, entityName, viewType string) (int64, error) {
	res, err := ex.ExecContext(ctx, `DELETE FROM sys_layouts WHERE tenant_id = ? AND entity_name = ? AND view_type = ?`, tenantID, entityName, viewType)
	if err != nil {
		return 0, fmt.Errorf("metadata.DeleteLayoutByViewExec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("metadata.DeleteLayoutByViewExec: rows affected: %w", err)
	}
	return n, nil
}
