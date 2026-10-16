package project

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"nexus/platform/internal/auth"
)

// ErrNotFound is returned by Get / TenantRow when there is no live row.
var ErrNotFound = errors.New("project: not found")

// Repository reads and writes the project registry. GORM, like every other fixed system table.
type Repository struct {
	db *gorm.DB
}

// NewRepository wires the repository over the shared GORM handle.
func NewRepository(db *gorm.DB) *Repository { return &Repository{db: db} }

// TenantRow is the slice of a live (not retired) tenant the registry needs.
type TenantRow struct {
	ID        string
	Slug      string
	Name      string
	Status    string
	CreatedAt time.Time
}

// TenantRow returns one tenant that is not retired (soft-deleted). A suspended tenant is returned —
// the registry keeps showing it, with its status. ErrNotFound for an unknown or retired id.
func (r *Repository) TenantRow(ctx context.Context, tenantID string) (*TenantRow, error) {
	var t auth.Tenant
	err := r.db.WithContext(ctx).Where("id = ?", tenantID).Take(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("project.Repository.TenantRow: %w", err)
	}
	return &TenantRow{ID: t.ID, Slug: t.Slug, Name: t.Name, Status: t.Status, CreatedAt: t.CreatedAt}, nil
}

// ListByTenants returns the profiles of exactly the tenants asked for, keyed by tenant id. A tenant
// without a profile is absent from the map (the service reads that as building/unknown).
func (r *Repository) ListByTenants(ctx context.Context, tenantIDs []string) (map[string]Profile, error) {
	out := map[string]Profile{}
	if len(tenantIDs) == 0 {
		return out, nil
	}
	var rows []Profile
	if err := r.db.WithContext(ctx).Where("tenant_id IN ?", tenantIDs).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("project.Repository.ListByTenants: %w", err)
	}
	for _, p := range rows {
		out[p.TenantID] = p
	}
	return out, nil
}

// Get returns one tenant's profile or ErrNotFound.
func (r *Repository) Get(ctx context.Context, tenantID string) (*Profile, error) {
	var p Profile
	err := r.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Take(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("project.Repository.Get: %w", err)
	}
	return &p, nil
}

// Upsert inserts p, or — when the tenant already has a profile (uq_project_profiles_tenant) —
// overwrites ONLY the listed columns, so a PATCH of the industry can never clobber a health
// snapshot written by a concurrent health-check (and vice versa).
func (r *Repository) Upsert(ctx context.Context, p *Profile, updateCols ...string) error {
	// deleted_at is always cleared: a soft-deleted row would hide from Get while still owning the
	// unique tenant key, so the upsert would silently write into a row nobody can read.
	cols := append([]string{"updated_at", "deleted_at"}, updateCols...)
	err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "tenant_id"}},
		DoUpdates: clause.AssignmentColumns(cols),
	}).Create(p).Error
	if err != nil {
		return fmt.Errorf("project.Repository.Upsert: %w", err)
	}
	return nil
}

// CreateInTx inserts a profile inside the caller's transaction (project creation writes the tenant,
// its owner, its calendar and this row atomically).
func (r *Repository) CreateInTx(ctx context.Context, tx *gorm.DB, p *Profile) error {
	if err := tx.WithContext(ctx).Create(p).Error; err != nil {
		return fmt.Errorf("project.Repository.CreateInTx: %w", err)
	}
	return nil
}

type countRow struct {
	TenantID string     `gorm:"column:tenant_id"`
	N        int        `gorm:"column:n"`
	M        int        `gorm:"column:m"`
	At       *time.Time `gorm:"column:at"`
}

// countQuery is one fixed `GROUP BY tenant_id` statement. The statements are literals (table names
// are never built from input); the id list is the only parameter.
type countQuery struct {
	sql string
	set func(c *Counts, row countRow)
}

var countQueries = []countQuery{
	{"SELECT tenant_id, COUNT(*) AS n FROM sys_apps WHERE tenant_id IN ? AND deleted_at IS NULL GROUP BY tenant_id",
		func(c *Counts, r countRow) { c.Apps = r.N }},
	{"SELECT tenant_id, COUNT(*) AS n FROM sys_entities WHERE tenant_id IN ? AND deleted_at IS NULL GROUP BY tenant_id",
		func(c *Counts, r countRow) { c.Entities = r.N }},
	// sys_layouts only holds layouts somebody customised (the default is generated on read).
	{"SELECT tenant_id, COUNT(*) AS n FROM sys_layouts WHERE tenant_id IN ? AND deleted_at IS NULL GROUP BY tenant_id",
		func(c *Counts, r countRow) { c.CustomLayouts = r.N }},
	{"SELECT tenant_id, COUNT(*) AS n FROM sys_workflows WHERE tenant_id IN ? AND deleted_at IS NULL GROUP BY tenant_id",
		func(c *Counts, r countRow) { c.Workflows = r.N }},
	{"SELECT tenant_id, COUNT(*) AS n FROM sys_approval_processes WHERE tenant_id IN ? AND deleted_at IS NULL GROUP BY tenant_id",
		func(c *Counts, r countRow) { c.Approvals = r.N }},
	// acl_roles has no deleted_at.
	{"SELECT tenant_id, COUNT(*) AS n FROM acl_roles WHERE tenant_id IN ? GROUP BY tenant_id",
		func(c *Counts, r countRow) { c.Roles = r.N }},
	{"SELECT tenant_id, COUNT(*) AS n, CAST(COALESCE(SUM(is_admin = 0), 0) AS SIGNED) AS m FROM users " +
		"WHERE tenant_id IN ? AND status = 'active' AND deleted_at IS NULL GROUP BY tenant_id",
		func(c *Counts, r countRow) { c.Users = r.N; c.StaffUsers = r.M }},
	{"SELECT tenant_id, COUNT(*) AS n FROM sys_reports WHERE tenant_id IN ? AND deleted_at IS NULL GROUP BY tenant_id",
		func(c *Counts, r countRow) { c.Reports = r.N }},
	{"SELECT tenant_id, COUNT(*) AS n FROM sys_dashboards WHERE tenant_id IN ? AND deleted_at IS NULL GROUP BY tenant_id",
		func(c *Counts, r countRow) { c.Dashboards = r.N }},
}

// Signals returns, for exactly the listed tenants, the build-step counts, the branding flag and the
// last time anybody used the workspace. It runs a FIXED number of statements (9 counts + 1 last
// active + 1 branded) whatever the number of tenants — FR-027. countQueries holds 9 grouped
// counts (users answers two numbers), so with last-active that is the plan's "10 GROUP BY".
func (r *Repository) Signals(ctx context.Context, tenantIDs []string) (map[string]Signals, error) {
	out := make(map[string]Signals, len(tenantIDs))
	if len(tenantIDs) == 0 {
		return out, nil
	}
	for _, id := range tenantIDs {
		out[id] = Signals{}
	}
	db := r.db.WithContext(ctx)

	for _, q := range countQueries {
		var rows []countRow
		if err := db.Raw(q.sql, tenantIDs).Scan(&rows).Error; err != nil {
			return nil, fmt.Errorf("project.Repository.Signals: counts: %w", err)
		}
		for _, row := range rows {
			if s, ok := out[row.TenantID]; ok {
				q.set(&s.Counts, row)
				out[row.TenantID] = s
			}
		}
	}

	// Last activity = the latest refresh of any live login session of the workspace.
	var active []countRow
	if err := db.Raw("SELECT tenant_id, MAX(last_used_at) AS at FROM refresh_tokens "+
		"WHERE tenant_id IN ? AND revoked_at IS NULL GROUP BY tenant_id", tenantIDs).Scan(&active).Error; err != nil {
		return nil, fmt.Errorf("project.Repository.Signals: last active: %w", err)
	}
	for _, row := range active {
		if s, ok := out[row.TenantID]; ok {
			s.LastActiveAt = row.At
			out[row.TenantID] = s
		}
	}

	// Backup signal (specs/068 T100): last ready backup + whether the latest finished attempt failed. Two grouped
	// statements, fixed whatever the number of tenants.
	var ready []countRow
	if err := db.Raw("SELECT tenant_id, MAX(finished_at) AS at FROM sys_tenant_backups "+
		"WHERE tenant_id IN ? AND status = 'ready' AND deleted_at IS NULL GROUP BY tenant_id", tenantIDs).Scan(&ready).Error; err != nil {
		return nil, fmt.Errorf("project.Repository.Signals: backup ready: %w", err)
	}
	for _, row := range ready {
		if s, ok := out[row.TenantID]; ok {
			s.Backup.LastReadyAt = row.At
			out[row.TenantID] = s
		}
	}
	var failed []countRow
	if err := db.Raw("SELECT b.tenant_id FROM sys_tenant_backups b WHERE b.tenant_id IN ? AND b.deleted_at IS NULL AND b.status = 'failed' "+
		"AND NOT EXISTS (SELECT 1 FROM sys_tenant_backups n WHERE n.tenant_id = b.tenant_id AND n.deleted_at IS NULL "+
		"AND n.status <> 'running' AND (n.created_at > b.created_at OR (n.created_at = b.created_at AND n.id > b.id)))",
		tenantIDs).Scan(&failed).Error; err != nil {
		return nil, fmt.Errorf("project.Repository.Signals: backup failed: %w", err)
	}
	for _, row := range failed {
		if s, ok := out[row.TenantID]; ok {
			s.Backup.LastFailed = true
			out[row.TenantID] = s
		}
	}

	// Branded = a branding document is set (SQL NULL or JSON null both mean "default Nexus").
	var branded []struct {
		ID string `gorm:"column:id"`
		B  bool   `gorm:"column:branded"`
	}
	if err := db.Raw("SELECT id, (branding IS NOT NULL AND JSON_TYPE(branding) <> 'NULL') AS branded "+
		"FROM tenants WHERE id IN ?", tenantIDs).Scan(&branded).Error; err != nil {
		return nil, fmt.Errorf("project.Repository.Signals: branded: %w", err)
	}
	for _, row := range branded {
		if s, ok := out[row.ID]; ok {
			s.Branded = row.B
			out[row.ID] = s
		}
	}
	return out, nil
}
