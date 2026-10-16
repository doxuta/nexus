package tenantbackup

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"gorm.io/gorm"
)

// Sentinels the service maps to API errors.
var (
	ErrNotFound       = errors.New("tenantbackup: backup not found")
	ErrTenantNotFound = errors.New("tenantbackup: tenant not found")
	ErrInProgress     = errors.New("tenantbackup: an operation is already running for this tenant")
	ErrSlotTaken      = errors.New("tenantbackup: this schedule slot already has a backup")
)

const mysqlDuplicateEntry = 1062

// TenantInfo is the slice of the tenants row a backup needs. Unscoped: a retired customer is
// still backed up and downloaded (FR-020).
type TenantInfo struct {
	ID      string
	Slug    string
	Name    string
	Retired bool
}

// Cursor is the keyset position of the backup list: ORDER BY created_at DESC, id DESC, so the
// next page is "strictly before (created_at, id)".
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// Encode renders the cursor as an opaque URL-safe string.
func (c Cursor) Encode() string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(c.CreatedAt.UnixMicro(), 10) + "|" + c.ID))
}

// DecodeCursor parses Cursor.Encode's output; anything else is BACKUP_CURSOR_INVALID.
func DecodeCursor(s string) (*Cursor, error) {
	bad := &Error{Code: "BACKUP_CURSOR_INVALID", Msg: "Con trỏ phân trang không hợp lệ"}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, bad
	}
	us, id, ok := strings.Cut(string(raw), "|")
	if !ok || !uuidRe.MatchString(id) {
		return nil, bad
	}
	n, err := strconv.ParseInt(us, 10, 64)
	if err != nil {
		return nil, bad
	}
	return &Cursor{CreatedAt: time.UnixMicro(n), ID: id}, nil
}

// ReadyFields are the values written when a backup finishes.
type ReadyFields struct {
	SizeBytes     int64
	SHA256        string
	SchemaVersion int64
	RowCount      int64
	TableCount    int
	FileCount     int
	BlobBytes     int64
	Warnings      []Warning
}

// StaleRow is what the reaper reads across tenants: ids and the file key, never customer data.
type StaleRow struct {
	ID         string
	TenantID   string
	StorageKey string
}

// Store is the persistence port of Service (fakeable in unit tests).
type Store interface {
	TenantInfo(ctx context.Context, tenantID string) (*TenantInfo, error)
	CheckSchema(ctx context.Context) error
	CurrentSchema(ctx context.Context) (version int64, dirty bool, err error)
	Estimate(ctx context.Context, tenantID string) (int64, error)
	Insert(ctx context.Context, b *Backup) error
	Get(ctx context.Context, tenantID, id string) (*Backup, error)
	List(ctx context.Context, tenantID string, limit int, after *Cursor) ([]Backup, error)
	// RestoreRunning reports whether a restore is in flight for the tenant (it owns the write freeze).
	RestoreRunning(ctx context.Context, tenantID string) (bool, error)
	Heartbeat(ctx context.Context, tenantID, id string) (int64, error)
	MarkReady(ctx context.Context, tenantID, id string, r ReadyFields) (int64, error)
	MarkFailed(ctx context.Context, tenantID, id, code, detail string) (int64, error)
	ListStale(ctx context.Context, olderThan time.Duration) ([]StaleRow, error)
	OperatorLabel(ctx context.Context, actorTenantID, userID string) string
	PersonLabels(ctx context.Context, pairs [][2]string) map[[2]string]string
}

// GormStore implements Store: GORM for the catalogue (a fixed system table), sqlx for everything
// read off the dynamic tables.
type GormStore struct {
	db    *gorm.DB
	sdb   *sqlx.DB
	shape func(ctx context.Context) error
}

// NewGormStore wires the store. shape is the purge's schema-shape guard
// (services.TenantProvisionService.CheckSchemaShape), passed as a func because this package does
// not import services; nil skips that step.
func NewGormStore(db *gorm.DB, sdb *sqlx.DB, shape func(ctx context.Context) error) *GormStore {
	return &GormStore{db: db, sdb: sdb, shape: shape}
}

// TenantInfo reads the tenants row, retired or not.
func (r *GormStore) TenantInfo(ctx context.Context, tenantID string) (*TenantInfo, error) {
	if !uuidRe.MatchString(tenantID) {
		return nil, ErrTenantNotFound
	}
	// branding is deliberately NOT read here: it is NULL for most customers (a *json.RawMessage
	// cannot Scan a NULL) and the file takes it from the snapshot (sqlxSnapshot.Branding) so it is
	// consistent with every table.
	var row struct {
		ID        string     `db:"id"`
		Slug      string     `db:"slug"`
		Name      string     `db:"name"`
		DeletedAt *time.Time `db:"deleted_at"`
	}
	if err := r.sdb.GetContext(ctx, &row, "SELECT id, slug, name, deleted_at FROM tenants WHERE id = ?", tenantID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrTenantNotFound
		}
		return nil, fmt.Errorf("tenantbackup.GormStore.TenantInfo: %w", err)
	}
	return &TenantInfo{ID: row.ID, Slug: row.Slug, Name: row.Name, Retired: row.DeletedAt != nil}, nil
}

// CheckSchema is policy.CheckSchema plus the purge's shape guard.
func (r *GormStore) CheckSchema(ctx context.Context) error {
	if err := CheckSchema(ctx, r.sdb); err != nil {
		return err
	}
	if r.shape != nil {
		if err := r.shape(ctx); err != nil {
			return fmt.Errorf("tenantbackup.GormStore.CheckSchema: shape: %w", err)
		}
	}
	return nil
}

// CurrentSchema reads the migration version and its dirty flag.
func (r *GormStore) CurrentSchema(ctx context.Context) (int64, bool, error) {
	var row struct {
		Version int64 `db:"version"`
		Dirty   bool  `db:"dirty"`
	}
	if err := r.sdb.GetContext(ctx, &row, "SELECT version, dirty FROM schema_migrations LIMIT 1"); err != nil {
		return 0, false, fmt.Errorf("tenantbackup.GormStore.CurrentSchema: %w", err)
	}
	return row.Version, row.Dirty, nil
}

// bytesPerRowEstimate is the rough size of one exported row in the zip (JSONL, deflated); the
// estimate only has to stop a backup that cannot possibly fit, not predict the size.
const bytesPerRowEstimate = 1024

// Estimate is the upper-ish bound of a backup's size: attachment bytes (sys_files.size) plus rows
// × 1 KiB (FR-070). Counting is one indexed COUNT per exported table.
func (r *GormStore) Estimate(ctx context.Context, tenantID string) (int64, error) {
	var blob int64
	if err := r.sdb.GetContext(ctx, &blob, "SELECT COALESCE(SUM(size), 0) FROM sys_files WHERE tenant_id = ?", tenantID); err != nil {
		return 0, fmt.Errorf("tenantbackup.GormStore.Estimate: files: %w", err)
	}
	refs, err := exportTables(ctx, r.sdb)
	if err != nil {
		return 0, err
	}
	var rows int64
	for _, ref := range refs {
		q, err := ref.countQuery()
		if err != nil {
			return 0, err
		}
		var n int64
		if err := r.sdb.GetContext(ctx, &n, q, tenantID); err != nil {
			return 0, fmt.Errorf("tenantbackup.GormStore.Estimate: count %s: %w", ref.Table, err)
		}
		rows += n
	}
	return blob + rows*bytesPerRowEstimate, nil
}

// Insert creates the catalogue row. 1062 on uq_tbk_oplock means another operation is running for
// this customer (the database ruled, not an app lock) => ErrInProgress.
func (r *GormStore) Insert(ctx context.Context, b *Backup) error {
	if err := r.db.WithContext(ctx).Create(b).Error; err != nil {
		var me *gomysql.MySQLError
		if errors.As(err, &me) && me.Number == mysqlDuplicateEntry && strings.Contains(me.Message, "uq_tbk_oplock") {
			return ErrInProgress
		}
		if errors.As(err, &me) && me.Number == mysqlDuplicateEntry && strings.Contains(me.Message, "uq_tbk_slot") {
			return ErrSlotTaken
		}
		return fmt.Errorf("tenantbackup.GormStore.Insert: %w", err)
	}
	return nil
}

// Get reads one backup pinned to (id, tenant_id) — another customer's id under this customer's
// path is simply not found (FR-003).
func (r *GormStore) Get(ctx context.Context, tenantID, id string) (*Backup, error) {
	var b Backup
	err := r.db.WithContext(ctx).
		Where("id = ? AND tenant_id = ? AND deleted_at IS NULL", id, tenantID).Take(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.Get: %w", err)
	}
	return &b, nil
}

// List returns up to limit rows newest first. The keyset predicate runs in the SAME direction as
// the sort (created_at DESC, id DESC => "strictly before"), so a page boundary never repeats or
// skips a row. Callers ask for limit+1 to learn whether another page exists.
func (r *GormStore) List(ctx context.Context, tenantID string, limit int, after *Cursor) ([]Backup, error) {
	q := r.db.WithContext(ctx).Where("tenant_id = ? AND deleted_at IS NULL", tenantID)
	if after != nil {
		q = q.Where("(created_at < ? OR (created_at = ? AND id < ?))", after.CreatedAt, after.CreatedAt, after.ID)
	}
	var out []Backup
	if err := q.Order("created_at DESC, id DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.List: %w", err)
	}
	return out, nil
}

// Heartbeat bumps updated_at of a RUNNING row. RowsAffected 0 means the row is gone or no longer
// running (customer purged, reaper got there) and the caller must stop.
func (r *GormStore) Heartbeat(ctx context.Context, tenantID, id string) (int64, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE sys_tenant_backups SET updated_at = CURRENT_TIMESTAMP(3) WHERE id = ? AND tenant_id = ? AND status = ?",
		id, tenantID, StatusRunning)
	if res.Error != nil {
		return 0, fmt.Errorf("tenantbackup.GormStore.Heartbeat: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// MarkReady finishes a running backup. The WHERE excludes the state it writes (status = running),
// so a second finisher or a reaped row reports 0 rows.
func (r *GormStore) MarkReady(ctx context.Context, tenantID, id string, f ReadyFields) (int64, error) {
	var warnings any
	if len(f.Warnings) > 0 {
		b, err := json.Marshal(f.Warnings)
		if err != nil {
			return 0, fmt.Errorf("tenantbackup.GormStore.MarkReady: warnings: %w", err)
		}
		warnings = string(b)
	}
	res := r.db.WithContext(ctx).Exec(`UPDATE sys_tenant_backups SET status = ?, size_bytes = ?, sha256 = ?,
		schema_version = ?, row_count = ?, table_count = ?, file_count = ?, blob_bytes = ?, warnings = ?,
		finished_at = CURRENT_TIMESTAMP(3)
		WHERE id = ? AND tenant_id = ? AND status = ?`,
		StatusReady, f.SizeBytes, f.SHA256, f.SchemaVersion, f.RowCount, f.TableCount, f.FileCount, f.BlobBytes,
		warnings, id, tenantID, StatusRunning)
	if res.Error != nil {
		return 0, fmt.Errorf("tenantbackup.GormStore.MarkReady: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// MarkFailed fails a running backup (table name + code only in detail — never a value).
func (r *GormStore) MarkFailed(ctx context.Context, tenantID, id, code, detail string) (int64, error) {
	if len(detail) > 500 {
		detail = detail[:500]
	}
	res := r.db.WithContext(ctx).Exec(`UPDATE sys_tenant_backups SET status = ?, error_code = ?, error_detail = ?,
		finished_at = CURRENT_TIMESTAMP(3) WHERE id = ? AND tenant_id = ? AND status = ?`,
		StatusFailed, code, detail, id, tenantID, StatusRunning)
	if res.Error != nil {
		return 0, fmt.Errorf("tenantbackup.GormStore.MarkFailed: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// ListStale lists running backups whose heartbeat is older than the threshold. It is the one
// cross-tenant read of this package and reads only id / tenant_id / storage_key (the same
// precedent as the jobs reaper): it must see every tenant's abandoned rows to fail them.
func (r *GormStore) ListStale(ctx context.Context, olderThan time.Duration) ([]StaleRow, error) {
	var rows []struct {
		ID         string  `gorm:"column:id"`
		TenantID   string  `gorm:"column:tenant_id"`
		StorageKey *string `gorm:"column:storage_key"`
	}
	err := r.db.WithContext(ctx).Raw(`SELECT id, tenant_id, storage_key FROM sys_tenant_backups
		WHERE op_lock IS NOT NULL AND status = ? AND restore_status IS NULL AND deleted_at IS NULL
		  AND updated_at < NOW(3) - INTERVAL ? SECOND`, StatusRunning, int64(olderThan.Seconds())).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("tenantbackup.GormStore.ListStale: %w", err)
	}
	out := make([]StaleRow, 0, len(rows))
	for _, x := range rows {
		s := StaleRow{ID: x.ID, TenantID: x.TenantID}
		if x.StorageKey != nil {
			s.StorageKey = *x.StorageKey
		}
		out = append(out, s)
	}
	return out, nil
}

// RestoreRunning is true while a catalogue row of the tenant has restore_status = 'running'.
func (r *GormStore) RestoreRunning(ctx context.Context, tenantID string) (bool, error) {
	var n int64
	err := r.db.WithContext(ctx).Table("sys_tenant_backups").
		Where("tenant_id = ? AND restore_status = ? AND deleted_at IS NULL", tenantID, "running").Count(&n).Error
	if err != nil {
		return false, fmt.Errorf("tenantbackup.GormStore.RestoreRunning: %w", err)
	}
	return n > 0, nil
}

const genericOperatorLabel = "Người dựng"

// OperatorLabel is "Họ tên (Tên xưởng)" for the operator who acted from the workshop tenant:
// the full name (blank => "Người dựng") and the workshop's name. No email, no id (FR-009/D-25).
// A lookup failure degrades to the generic label — never fails the operation.
func (r *GormStore) OperatorLabel(ctx context.Context, actorTenantID, userID string) string {
	if actorTenantID == "" || userID == "" {
		return genericOperatorLabel
	}
	var row struct {
		FullName string `db:"full_name"`
		Tenant   string `db:"tenant"`
	}
	err := r.sdb.GetContext(ctx, &row, `SELECT u.full_name AS full_name, t.name AS tenant
		FROM users u JOIN tenants t ON t.id = u.tenant_id WHERE u.tenant_id = ? AND u.id = ?`, actorTenantID, userID)
	if err != nil {
		return genericOperatorLabel
	}
	name := strings.TrimSpace(row.FullName)
	if name == "" {
		name = genericOperatorLabel
	}
	return name + " (" + row.Tenant + ")"
}

// PersonLabels resolves several (actorTenantID, userID) pairs, one lookup per distinct pair.
func (r *GormStore) PersonLabels(ctx context.Context, pairs [][2]string) map[[2]string]string {
	out := make(map[[2]string]string, len(pairs))
	for _, p := range pairs {
		if _, done := out[p]; done {
			continue
		}
		out[p] = r.OperatorLabel(ctx, p[0], p[1])
	}
	return out
}
