// Package appmeta is the storage layer of the App container (../specs/010-app-container/spec.md).
// It persists sys_apps (the app shells shown in the sidebar App Switcher) and
// sys_app_nav_items (the ordered, polymorphic nav references into tenant-global
// entities/dashboards). An app is a LENS, never an ownership container: nav rows hold
// logical ref_ids (sys_entities.id | sys_dashboards.id) with no hard FK, validated at the
// service layer and cascade-cleaned by PurgeNavForRef inside the owning delete transactions
// (INV-4).
//
// Multi-tenant invariant (INV-5): every repository query filters tenant_id.
package appmeta

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Nav item_type values (v1). report/link are deliberately v2.
const (
	ItemTypeEntity    = "entity"
	ItemTypeDashboard = "dashboard"
)

// ErrAppNotFound is returned by Get/Update/SoftDelete for an unknown or cross-tenant id/name.
var ErrAppNotFound = fmt.Errorf("app not found")

// ErrAppDuplicateName is returned by Create when the (tenant_id, name) slug slot is taken
// (MySQL 1062 on uq_sys_apps_tenant_name) — the service maps it to 409 APP_ALREADY_EXISTS.
var ErrAppDuplicateName = fmt.Errorf("app name already exists")

// App is a persisted sys_apps row. NavCount is computed (COUNT of the app's nav items) for the
// admin list/detail views; it is not a physical column. json tags give the camelCase wire shape.
type App struct {
	ID          string  `db:"id" json:"id"`
	TenantID    string  `db:"tenant_id" json:"tenantId"`
	Name        string  `db:"name" json:"name"`
	Label       string  `db:"label" json:"label"`
	Description *string `db:"description" json:"description"`
	Icon        *string `db:"icon" json:"icon"`
	Color       *string `db:"color" json:"color"`
	IsDefault   bool    `db:"is_default" json:"isDefault"`
	SortOrder   int     `db:"sort_order" json:"sortOrder"`
	NavCount    int     `db:"nav_count" json:"navCount"`
	// Options is the raw sys_apps.options JSON; only homeDashboardId is read (see HomeDashboardID).
	Options *string `db:"options" json:"-"`
	// HomeDashboardID is the dashboard the app's home page leads with ("" = none). Derived from
	// Options after every read (fillHome) — never a physical column.
	HomeDashboardID string `db:"-" json:"homeDashboardId"`
	// HomeQueueIDs are the work queues the app's home shows (explicit pick). Empty = derive from the
	// app's menu tables. Stale ids (queue deleted since) are tolerated by readers.
	HomeQueueIDs []string `db:"-" json:"homeQueueIds"`
	// DefaultRoleIDs are the roles for whom this app is the one they land in at sign-in (explicit
	// per-role default). Empty = fall back to the tenant isDefault / only-app rule.
	DefaultRoleIDs []string `db:"-" json:"defaultRoleIds"`
	// DefaultForCaller is set on GET /apps only: the caller's role is in DefaultRoleIDs.
	DefaultForCaller bool      `db:"-" json:"defaultForCaller,omitempty"`
	CreatedBy        *string   `db:"created_by" json:"createdBy"`
	UpdatedBy        *string   `db:"updated_by" json:"updatedBy"`
	CreatedAt        time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt        time.Time `db:"updated_at" json:"updatedAt"`
	// DraftState is live|edited when the builder's draft overlay was applied to this read (never a
	// column; omitted on the customer's live reads) - specs/063-draft-publish S7-4.
	DraftState string `db:"-" json:"draftState,omitempty"`
}

// NavItem is a persisted sys_app_nav_items row: one ordered reference from an app to a
// tenant-global entity or dashboard. Label NULL = "use the referenced object's own label".
type NavItem struct {
	ID        string    `db:"id" json:"id"`
	TenantID  string    `db:"tenant_id" json:"tenantId"`
	AppID     string    `db:"app_id" json:"appId"`
	ItemType  string    `db:"item_type" json:"itemType"`
	RefID     string    `db:"ref_id" json:"refId"`
	Label     *string   `db:"label" json:"label"`
	SortOrder int       `db:"sort_order" json:"sortOrder"`
	CreatedAt time.Time `db:"created_at" json:"createdAt"`
}

// ExecerContext is the minimal executor PurgeNavForRef needs. It is satisfied by *sqlx.DB,
// *sqlx.Tx, *sql.DB, *sql.Tx AND gorm's ConnPool (tx.Statement.ConnPool inside a GORM
// transaction), so the ONE purge statement can run inside whichever transaction owns the
// referenced object's delete.
type ExecerContext interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// PurgeNavForRef deletes every nav row referencing (itemType, refID) in the tenant — the
// INV-4 orphan cascade, shared by ALL delete paths of referenced objects (entity soft-delete
// in metadata.Repository.SoftDeleteEntity's tx, dashboard soft-delete in
// dashboard.SQLRepository.Delete's tx). Root-caused here once so no future delete path can
// reimplement it differently. Zero rows affected is fine (the ref was in no app's nav).
func PurgeNavForRef(ctx context.Context, ex ExecerContext, tenantID, itemType, refID string) error {
	const q = `DELETE FROM sys_app_nav_items WHERE tenant_id = ? AND item_type = ? AND ref_id = ?`
	if _, err := ex.ExecContext(ctx, q, tenantID, itemType, refID); err != nil {
		return fmt.Errorf("appmeta.PurgeNavForRef: %w", err)
	}
	// A deleted dashboard must also stop being an app's home page (same shared cascade, one place).
	if itemType == ItemTypeDashboard {
		const home = `UPDATE sys_apps SET options = JSON_REMOVE(options, '$.homeDashboardId')
			WHERE tenant_id = ? AND options IS NOT NULL
			  AND JSON_UNQUOTE(JSON_EXTRACT(options, '$.homeDashboardId')) = ?`
		if _, err := ex.ExecContext(ctx, home, tenantID, refID); err != nil {
			return fmt.Errorf("appmeta.PurgeNavForRef: clear home dashboard: %w", err)
		}
	}
	return nil
}

// fillHome derives the options-backed fields (home dashboard, home queues, per-role default) from
// the raw options JSON. A malformed/foreign options blob yields the empty values - options is v2
// headroom shared with other keys, never an error source.
func fillHome(a *App) {
	a.HomeDashboardID, a.HomeQueueIDs, a.DefaultRoleIDs = "", []string{}, []string{}
	if a.Options == nil {
		return
	}
	var o struct {
		HomeDashboardID string   `json:"homeDashboardId"`
		HomeQueueIDs    []string `json:"homeQueueIds"`
		DefaultRoleIDs  []string `json:"defaultRoleIds"`
	}
	if json.Unmarshal([]byte(*a.Options), &o) == nil {
		a.HomeDashboardID = o.HomeDashboardID
		if o.HomeQueueIDs != nil {
			a.HomeQueueIDs = o.HomeQueueIDs
		}
		if o.DefaultRoleIDs != nil {
			a.DefaultRoleIDs = o.DefaultRoleIDs
		}
	}
}
