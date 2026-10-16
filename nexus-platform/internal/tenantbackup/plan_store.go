package tenantbackup

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// The read side of the restore preview on MySQL. Every query is tenant-pinned (a child without a
// tenant_id goes through its parent) and read-only.

var _ PlanStore = (*GormStore)(nil)

// OperationRunning reads the generated op_lock column: set while a backup or restore of this
// customer is running.
func (r *GormStore) OperationRunning(ctx context.Context, tenantID string) (bool, error) {
	var n int
	if err := r.sdb.GetContext(ctx, &n,
		"SELECT COUNT(*) FROM sys_tenant_backups WHERE tenant_id = ? AND op_lock IS NOT NULL", tenantID); err != nil {
		return false, fmt.Errorf("tenantbackup.GormStore.OperationRunning: %w", err)
	}
	return n > 0, nil
}

// ExportTables lists the tables a backup of the live schema would hold, and every foreign key.
func (r *GormStore) ExportTables(ctx context.Context) ([]TableRef, []FK, error) {
	refs, err := exportTables(ctx, r.sdb)
	if err != nil {
		return nil, nil, err
	}
	_, fks, err := LoadSchema(ctx, r.sdb)
	if err != nil {
		return nil, nil, err
	}
	return refs, fks, nil
}

// TableColumns returns the native columns of those of `names` that exist in this schema.
func (r *GormStore) TableColumns(ctx context.Context, names []string) (map[string][]Column, error) {
	out := make(map[string][]Column, len(names))
	if len(names) == 0 {
		return out, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
	args := make([]any, len(names))
	for i, n := range names {
		args[i] = n
	}
	var have []string
	// #nosec G202 -- marks is only "?,?,..."; the names are bound arguments.
	if err := r.sdb.SelectContext(ctx, &have,
		"SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME IN ("+marks+")", args...); err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.TableColumns: %w", err)
	}
	for _, t := range have {
		cols, err := NativeColumns(ctx, r.sdb, t)
		if err != nil {
			return nil, err
		}
		out[t] = cols
	}
	return out, nil
}

// CountRows counts one table's rows of one customer.
func (r *GormStore) CountRows(ctx context.Context, ref TableRef, tenantID string) (int64, error) {
	q, err := ref.countQuery()
	if err != nil {
		return 0, err
	}
	var n int64
	if err := r.sdb.GetContext(ctx, &n, q, tenantID); err != nil {
		return 0, fmt.Errorf("tenantbackup.GormStore.CountRows %s: %w", ref.Table, err)
	}
	return n, nil
}

// EntityLabels maps obj_ table name -> the entity's current label.
func (r *GormStore) EntityLabels(ctx context.Context, tenantID string) (map[string]string, error) {
	var rows []struct {
		Table string `db:"table_name"`
		Label string `db:"label"`
	}
	if err := r.sdb.SelectContext(ctx, &rows,
		"SELECT table_name, label FROM sys_entities WHERE tenant_id = ? AND deleted_at IS NULL", tenantID); err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.EntityLabels: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, x := range rows {
		out[x.Table] = x.Label
	}
	return out, nil
}

// LiveFiles maps sys_files id -> size for one customer.
func (r *GormStore) LiveFiles(ctx context.Context, tenantID string) (map[string]int64, error) {
	var rows []struct {
		ID   string `db:"id"`
		Size int64  `db:"size"`
	}
	if err := r.sdb.SelectContext(ctx, &rows, "SELECT id, size FROM sys_files WHERE tenant_id = ?", tenantID); err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.LiveFiles: %w", err)
	}
	out := make(map[string]int64, len(rows))
	for _, x := range rows {
		out[x.ID] = x.Size
	}
	return out, nil
}

// PendingJobs counts the customer's queued background jobs.
func (r *GormStore) PendingJobs(ctx context.Context, tenantID string) (int64, error) {
	var n int64
	if err := r.sdb.GetContext(ctx, &n, "SELECT COUNT(*) FROM sys_jobs WHERE tenant_id = ? AND status = 'queued'", tenantID); err != nil {
		return 0, fmt.Errorf("tenantbackup.GormStore.PendingJobs: %w", err)
	}
	return n, nil
}

// AccessNow reads the live AccessSnapshot of one customer.
func (r *GormStore) AccessNow(ctx context.Context, tenantID string) (*AccessSnapshot, error) {
	a := NewAccessSnapshot()
	wrap := func(what string, err error) error {
		return fmt.Errorf("tenantbackup.GormStore.AccessNow %s: %w", what, err)
	}

	var ids []string
	if err := r.sdb.SelectContext(ctx, &ids, "SELECT id FROM acl_roles WHERE tenant_id = ?", tenantID); err != nil {
		return nil, wrap("roles", err)
	}
	for _, id := range ids {
		a.AddRole(id)
	}
	var grants []struct {
		Role, Entity, Action, Level string
	}
	if err := r.sdb.SelectContext(ctx, &grants, `SELECT a.role_id AS role, a.entity_name AS entity, a.action AS action, a.access_level AS level
		FROM acl_role_actions a JOIN acl_roles p ON p.id = a.role_id WHERE p.tenant_id = ?`, tenantID); err != nil {
		return nil, wrap("grants", err)
	}
	for _, g := range grants {
		a.AddGrant(g.Role, g.Entity, g.Action, g.Level)
	}
	var facl []struct {
		Role   string `db:"role_id"`
		Entity string `db:"entity_name"`
		Field  string `db:"field_name"`
		View   bool   `db:"can_view"`
		Edit   bool   `db:"can_edit"`
	}
	if err := r.sdb.SelectContext(ctx, &facl,
		"SELECT role_id, entity_name, field_name, can_view, can_edit FROM sys_field_acl WHERE tenant_id = ?", tenantID); err != nil {
		return nil, wrap("field acl", err)
	}
	for _, f := range facl {
		a.AddFieldACL(f.Role, f.Entity, f.Field, f.View, f.Edit)
	}
	var shares []struct {
		ID      string         `db:"id"`
		User    sql.NullString `db:"user_id"`
		Deleted bool           `db:"deleted"`
	}
	if err := r.sdb.SelectContext(ctx, &shares,
		"SELECT id, user_id, deleted_at IS NOT NULL AS deleted FROM sys_record_shares WHERE tenant_id = ?", tenantID); err != nil {
		return nil, wrap("shares", err)
	}
	for _, s := range shares {
		a.AddShare(s.ID, s.User.String, s.Deleted)
	}
	var members []struct {
		Team string `db:"team_id"`
		User string `db:"user_id"`
	}
	if err := r.sdb.SelectContext(ctx, &members, `SELECT m.team_id, m.user_id FROM team_memberships m
		JOIN teams t ON t.id = m.team_id WHERE t.tenant_id = ?`, tenantID); err != nil {
		return nil, wrap("memberships", err)
	}
	for _, m := range members {
		a.AddMember(m.Team, m.User)
	}
	var fields []struct {
		ID      string `db:"id"`
		Options []byte `db:"options"`
		Deleted bool   `db:"deleted"`
	}
	if err := r.sdb.SelectContext(ctx, &fields,
		"SELECT id, options, deleted_at IS NOT NULL AS deleted FROM sys_fields WHERE tenant_id = ?", tenantID); err != nil {
		return nil, wrap("fields", err)
	}
	for _, f := range fields {
		a.AddField(f.ID, f.Options, f.Deleted)
	}
	var wfs []struct {
		ID      string `db:"id"`
		Active  bool   `db:"is_active"`
		Deleted bool   `db:"deleted"`
		Actions []byte `db:"actions"`
	}
	if err := r.sdb.SelectContext(ctx, &wfs,
		"SELECT id, is_active, deleted_at IS NOT NULL AS deleted, actions FROM sys_workflows WHERE tenant_id = ?", tenantID); err != nil {
		return nil, wrap("workflows", err)
	}
	for _, w := range wfs {
		a.AddWorkflow(w.ID, w.Active, w.Deleted, w.Actions)
	}
	var users []struct {
		ID      string         `db:"id"`
		Role    sql.NullString `db:"role_id"`
		Status  string         `db:"status"`
		Deleted bool           `db:"deleted"`
	}
	if err := r.sdb.SelectContext(ctx, &users,
		"SELECT id, role_id, status, deleted_at IS NOT NULL AS deleted FROM users WHERE tenant_id = ?", tenantID); err != nil {
		return nil, wrap("users", err)
	}
	for _, u := range users {
		a.AddUser(u.ID, u.Role.String, u.Status, u.Deleted)
	}
	var keys []sql.NullString
	if err := r.sdb.SelectContext(ctx, &keys,
		"SELECT role_id FROM sys_api_keys WHERE tenant_id = ? AND revoked_at IS NULL", tenantID); err != nil {
		return nil, wrap("api keys", err)
	}
	for _, k := range keys {
		a.AddAPIKey(k.String)
	}
	var creds []struct {
		ID   string `db:"id"`
		Name string `db:"name"`
	}
	if err := r.sdb.SelectContext(ctx, &creds,
		"SELECT id, name FROM sys_named_credentials WHERE tenant_id = ? AND deleted_at IS NULL", tenantID); err != nil {
		return nil, wrap("credentials", err)
	}
	for _, c := range creds {
		a.CredNames[c.ID] = c.Name
	}
	return a, nil
}
