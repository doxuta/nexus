package appmeta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// mysqlDuplicateEntry is MySQL's ER_DUP_ENTRY — a unique-index violation
// (uq_sys_apps_tenant_name), folded to ErrAppDuplicateName.
const mysqlDuplicateEntry = 1062

// SQLRepository persists sys_apps + sys_app_nav_items over sqlx with parameterized queries,
// mirroring the dashboard repository style: identifiers are static, values always travel as `?`
// placeholders, and tenant_id pins every statement (INV-5).
type SQLRepository struct {
	db  *sqlx.DB
	now func() time.Time // injectable clock; defaults to time.Now
}

// NewSQLRepository wires the app repository over an existing sqlx connection.
func NewSQLRepository(db *sqlx.DB) *SQLRepository {
	return &SQLRepository{db: db, now: time.Now}
}

// appSelect is the shared projection for every app read. nav_count is a correlated COUNT over
// the app's nav items (tenant-pinned on both sides) so the admin list/detail carry it for free.
const appSelect = `SELECT a.id, a.tenant_id, a.name, a.label, a.description, a.icon, a.color,
		a.is_default, a.sort_order, a.options, a.created_by, a.updated_by, a.created_at, a.updated_at,
		(SELECT COUNT(*) FROM sys_app_nav_items n
			WHERE n.tenant_id = a.tenant_id AND n.app_id = a.id) AS nav_count
	FROM sys_apps a`

// CreateInput is the validated payload to insert a new app. Name is the already-sanitized slug.
type CreateInput struct {
	TenantID    string
	Name        string
	Label       string
	Description string
	Icon        string
	Color       string
	IsDefault   bool
	SortOrder   int
	CreatedBy   string
}

// UpdateInput carries the validated full-replace payload for an app's mutable fields.
// Name (the slug) is immutable — it is the deep-link identity (?app=<name>).
type UpdateInput struct {
	Label       string
	Description string
	Icon        string
	Color       string
	IsDefault   bool
	SortOrder   int
	UpdatedBy   string
	// HomeDashboardID: nil = leave options untouched; "" = clear; otherwise set (the service has
	// already validated it against the tenant's dashboards).
	HomeDashboardID *string
	// HomeQueueIDs / DefaultRoleIDs: nil = leave untouched; empty = clear; otherwise replace (the
	// service has validated each id against the tenant).
	HomeQueueIDs   *[]string
	DefaultRoleIDs *[]string
}

// NavItemInput is one validated nav reference for ReplaceNav. Slice order IS the nav order.
type NavItemInput struct {
	ItemType string
	RefID    string
	Label    string // "" → NULL (use the referenced object's own label)
}

// nullable maps "" (after trim) to SQL NULL, mirroring the dashboard repo's convention.
func nullable(s string) sql.NullString {
	if strings.TrimSpace(s) == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// isDuplicateEntry reports whether err is a MySQL ER_DUP_ENTRY (1062).
func isDuplicateEntry(err error) bool {
	var me *gomysql.MySQLError
	return errors.As(err, &me) && me.Number == mysqlDuplicateEntry
}

// Create inserts an app and returns its generated id. When IsDefault is set, the tenant's
// previous default flag is cleared in the SAME transaction (≤1 default per tenant). A
// (tenant, name) slug collision returns ErrAppDuplicateName.
func (r *SQLRepository) Create(ctx context.Context, in CreateInput) (string, error) {
	id := uuid.NewString()
	now := r.now().UTC()
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("appmeta.SQLRepository.Create: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	if in.IsDefault {
		if err := ClearDefaultExec(ctx, tx, in.TenantID, ""); err != nil {
			return "", fmt.Errorf("appmeta.SQLRepository.Create: %w", err)
		}
	}
	const q = `INSERT INTO sys_apps
		(id, tenant_id, name, label, description, icon, color, is_default, sort_order,
		 created_by, updated_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := tx.ExecContext(ctx, q,
		id, in.TenantID, in.Name, in.Label, nullable(in.Description), nullable(in.Icon),
		nullable(in.Color), in.IsDefault, in.SortOrder,
		nullable(in.CreatedBy), nullable(in.CreatedBy), now, now,
	); err != nil {
		if isDuplicateEntry(err) {
			return "", ErrAppDuplicateName
		}
		return "", fmt.Errorf("appmeta.SQLRepository.Create: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("appmeta.SQLRepository.Create: commit: %w", err)
	}
	return id, nil
}

// GetByID returns one tenant-scoped, non-deleted app, or ErrAppNotFound.
func (r *SQLRepository) GetByID(ctx context.Context, tenantID, id string) (*App, error) {
	const q = appSelect + ` WHERE a.tenant_id = ? AND a.id = ? AND a.deleted_at IS NULL`
	var app App
	if err := r.db.GetContext(ctx, &app, q, tenantID, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAppNotFound
		}
		return nil, fmt.Errorf("appmeta.SQLRepository.GetByID: %w", err)
	}
	fillHome(&app)
	return &app, nil
}

// GetByName returns one tenant-scoped, non-deleted app by slug, or ErrAppNotFound.
func (r *SQLRepository) GetByName(ctx context.Context, tenantID, name string) (*App, error) {
	const q = appSelect + ` WHERE a.tenant_id = ? AND a.name = ? AND a.deleted_at IS NULL`
	var app App
	if err := r.db.GetContext(ctx, &app, q, tenantID, name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAppNotFound
		}
		return nil, fmt.Errorf("appmeta.SQLRepository.GetByName: %w", err)
	}
	fillHome(&app)
	return &app, nil
}

// List returns every tenant-scoped, non-deleted app in App Switcher order. Non-nil slice
// always (empty must marshal as [] not null — same contract as ListNav).
func (r *SQLRepository) List(ctx context.Context, tenantID string) ([]App, error) {
	const q = appSelect + ` WHERE a.tenant_id = ? AND a.deleted_at IS NULL
		ORDER BY a.sort_order ASC, a.created_at ASC, a.id ASC`
	apps := make([]App, 0, 8)
	if err := r.db.SelectContext(ctx, &apps, q, tenantID); err != nil {
		return nil, fmt.Errorf("appmeta.SQLRepository.List: %w", err)
	}
	for i := range apps {
		fillHome(&apps[i])
	}
	return apps, nil
}

// Update applies the mutable fields to a tenant-scoped, non-deleted app. When IsDefault is
// set, other apps' default flags are cleared in the SAME transaction. Returns ErrAppNotFound
// when no live row matches (unknown / cross-tenant / already-deleted id).
func (r *SQLRepository) Update(ctx context.Context, tenantID, id string, in UpdateInput) error {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("appmeta.SQLRepository.Update: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if in.IsDefault {
		if err := ClearDefaultExec(ctx, tx, tenantID, id); err != nil {
			return fmt.Errorf("appmeta.SQLRepository.Update: %w", err)
		}
	}
	if err := UpdateExec(ctx, tx, tenantID, id, in); err != nil {
		if errors.Is(err, ErrAppNotFound) {
			return ErrAppNotFound
		}
		return fmt.Errorf("appmeta.SQLRepository.Update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("appmeta.SQLRepository.Update: commit: %w", err)
	}
	return nil
}

// SoftDelete soft-deletes a tenant-scoped app AND hard-deletes its nav rows in the SAME
// transaction (spec §3 DELETE /admin/apps/:id). The slug is tombstone-renamed
// ("<name>__deleted_<id8>") so uq_sys_apps_tenant_name frees the original name for a future
// recreate — the same unique-slot discipline as entity/relationship soft-deletes (the service
// caps slugs at 60 chars, so name + the 18-char suffix always fits VARCHAR(100)).
func (r *SQLRepository) SoftDelete(ctx context.Context, tenantID, id string) error {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("appmeta.SQLRepository.SoftDelete: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := SoftDeleteExec(ctx, tx, tenantID, id, r.now().UTC()); err != nil {
		return fmt.Errorf("appmeta.SQLRepository.SoftDelete: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("appmeta.SQLRepository.SoftDelete: commit: %w", err)
	}
	return nil
}

// Restore brings a soft-deleted app back under its own id with the given slug and properties (the
// undo of an applied-at-once delete, specs/063 S7-7b). The menu stays empty: the delete purged it.
// restored=false: no deleted row has that id. A slug a live app took meanwhile is ErrAppDuplicateName.
func (r *SQLRepository) Restore(ctx context.Context, tenantID, id, name string, in UpdateInput) (bool, error) {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("appmeta.SQLRepository.Restore: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if in.IsDefault {
		if err := ClearDefaultExec(ctx, tx, tenantID, id); err != nil {
			return false, fmt.Errorf("appmeta.SQLRepository.Restore: %w", err)
		}
	}
	ok, err := RestoreExec(ctx, tx, tenantID, id, name, in)
	if err != nil {
		if isDuplicateEntry(err) {
			return false, ErrAppDuplicateName
		}
		return false, fmt.Errorf("appmeta.SQLRepository.Restore: %w", err)
	}
	if !ok {
		return false, nil
	}
	if err := ApplyOptionsExec(ctx, tx, tenantID, id, in); err != nil {
		return false, fmt.Errorf("appmeta.SQLRepository.Restore: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("appmeta.SQLRepository.Restore: commit: %w", err)
	}
	return true, nil
}

// ReplaceNav swaps the app's entire nav for the given ordered items (replace-all semantics,
// spec §3 PUT /admin/apps/:id/nav): delete-then-insert in one transaction, sort_order = slice
// index. The caller (service) has already validated every ref against the tenant.
func (r *SQLRepository) ReplaceNav(ctx context.Context, tenantID, appID string, items []NavItemInput) error {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("appmeta.SQLRepository.ReplaceNav: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := ReplaceNavExec(ctx, tx, tenantID, appID, items, r.now()); err != nil {
		return fmt.Errorf("appmeta.SQLRepository.ReplaceNav: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("appmeta.SQLRepository.ReplaceNav: commit: %w", err)
	}
	return nil
}

// ListNav returns the app's nav rows in nav order. The slice is always non-nil so an empty
// nav marshals as JSON [] (a nil slice marshals as null and crashes FE .map consumers).
func (r *SQLRepository) ListNav(ctx context.Context, tenantID, appID string) ([]NavItem, error) {
	const q = `SELECT id, tenant_id, app_id, item_type, ref_id, label, sort_order, created_at
		FROM sys_app_nav_items
		WHERE tenant_id = ? AND app_id = ?
		ORDER BY sort_order ASC, created_at ASC, id ASC`
	items := make([]NavItem, 0, 8)
	if err := r.db.SelectContext(ctx, &items, q, tenantID, appID); err != nil {
		return nil, fmt.Errorf("appmeta.SQLRepository.ListNav: %w", err)
	}
	return items, nil
}

// PurgeNavRef removes every nav row referencing (itemType, refID) — the standalone entry to
// the shared INV-4 cascade for callers that do not carry their own transaction.
func (r *SQLRepository) PurgeNavRef(ctx context.Context, tenantID, itemType, refID string) error {
	if err := PurgeNavForRef(ctx, r.db, tenantID, itemType, refID); err != nil {
		return fmt.Errorf("appmeta.SQLRepository.PurgeNavRef: %w", err)
	}
	return nil
}
