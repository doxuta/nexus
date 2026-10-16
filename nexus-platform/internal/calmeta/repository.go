package calmeta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// mysqlDuplicateEntry is MySQL's ER_DUP_ENTRY — a unique-index violation
// (uq_sys_business_calendars_tenant_name), folded to ErrCalendarDuplicateName.
const mysqlDuplicateEntry = 1062

// SQLRepository persists sys_business_calendars over sqlx with parameterized queries,
// mirroring the appmeta/dashboard repository style: identifiers are static, values always
// travel as `?` placeholders, and tenant_id pins every statement.
type SQLRepository struct {
	db  *sqlx.DB
	now func() time.Time // injectable clock; defaults to time.Now
}

// NewSQLRepository wires the calendar repository over an existing sqlx connection.
func NewSQLRepository(db *sqlx.DB) *SQLRepository {
	return &SQLRepository{db: db, now: time.Now}
}

// calendarSelect is the shared projection for every calendar read.
const calendarSelect = `SELECT id, tenant_id, name, timezone, hours_json, holidays_json,
		is_default, created_by, updated_by, created_at, updated_at
	FROM sys_business_calendars`

// calendarRow is the scan target for calendarSelect. JSON columns land in plain []byte
// (NULL-tolerant — holidays_json is nullable; json.RawMessage is not a supported NULL scan
// destination), then rowToCalendar projects to the wire struct.
type calendarRow struct {
	ID        string    `db:"id"`
	TenantID  string    `db:"tenant_id"`
	Name      string    `db:"name"`
	Timezone  string    `db:"timezone"`
	Hours     []byte    `db:"hours_json"`
	Holidays  []byte    `db:"holidays_json"`
	IsDefault bool      `db:"is_default"`
	CreatedBy *string   `db:"created_by"`
	UpdatedBy *string   `db:"updated_by"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// rowToCalendar projects a persisted row to the wire struct, normalizing empty JSON
// columns to a stable shape (NULL holidays → []).
func rowToCalendar(row *calendarRow) BusinessCalendar {
	cal := BusinessCalendar{
		ID: row.ID, TenantID: row.TenantID, Name: row.Name, Timezone: row.Timezone,
		Hours: json.RawMessage(row.Hours), Holidays: json.RawMessage(row.Holidays),
		IsDefault: row.IsDefault, CreatedBy: row.CreatedBy, UpdatedBy: row.UpdatedBy,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	normalize(&cal)
	return cal
}

// CreateInput is the validated payload to insert a calendar. Hours is the service-marshalled
// hours_json (non-nil); Holidays may be nil (stored as SQL NULL).
type CreateInput struct {
	TenantID  string
	Name      string
	Timezone  string
	Hours     []byte
	Holidays  []byte
	IsDefault bool
	CreatedBy string
}

// UpdateInput carries the validated full-replace payload for a calendar's mutable fields.
type UpdateInput struct {
	Name      string
	Timezone  string
	Hours     []byte
	Holidays  []byte
	IsDefault bool
	UpdatedBy string
}

// nullable maps "" (after trim) to SQL NULL (appmeta convention).
func nullable(s string) sql.NullString {
	if strings.TrimSpace(s) == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// nullableJSON maps a nil/empty byte slice to SQL NULL (holidays_json is nullable).
func nullableJSON(b []byte) any {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return string(b)
}

// isDuplicateEntry reports whether err is a MySQL ER_DUP_ENTRY (1062).
func isDuplicateEntry(err error) bool {
	var me *gomysql.MySQLError
	return errors.As(err, &me) && me.Number == mysqlDuplicateEntry
}

// normalize gives empty JSON columns a stable wire shape (NULL holidays → []).
func normalize(c *BusinessCalendar) {
	if len(c.Holidays) == 0 {
		c.Holidays = json.RawMessage("[]")
	}
	if len(c.Hours) == 0 {
		c.Hours = json.RawMessage("{}")
	}
}

// Create inserts a calendar and returns its generated id. When IsDefault is set, the
// tenant's previous default flag is cleared in the SAME transaction (≤1 default per tenant).
// A (tenant, name) collision returns ErrCalendarDuplicateName.
func (r *SQLRepository) Create(ctx context.Context, in CreateInput) (string, error) {
	id := uuid.NewString()
	now := r.now().UTC()
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("calmeta.SQLRepository.Create: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	if in.IsDefault {
		if err := clearDefault(ctx, tx, in.TenantID, ""); err != nil {
			return "", fmt.Errorf("calmeta.SQLRepository.Create: %w", err)
		}
	}
	const q = `INSERT INTO sys_business_calendars
		(id, tenant_id, name, timezone, hours_json, holidays_json, is_default,
		 created_by, updated_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := tx.ExecContext(ctx, q,
		id, in.TenantID, in.Name, in.Timezone, string(in.Hours), nullableJSON(in.Holidays),
		in.IsDefault, nullable(in.CreatedBy), nullable(in.CreatedBy), now, now,
	); err != nil {
		if isDuplicateEntry(err) {
			return "", ErrCalendarDuplicateName
		}
		return "", fmt.Errorf("calmeta.SQLRepository.Create: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("calmeta.SQLRepository.Create: commit: %w", err)
	}
	return id, nil
}

// clearDefault drops the is_default flag on every live calendar of the tenant except
// exceptID ("" = no exception). Runs inside the caller's transaction.
func clearDefault(ctx context.Context, tx *sqlx.Tx, tenantID, exceptID string) error {
	const q = `UPDATE sys_business_calendars SET is_default = 0
		WHERE tenant_id = ? AND is_default = 1 AND id <> ? AND deleted_at IS NULL`
	if _, err := tx.ExecContext(ctx, q, tenantID, exceptID); err != nil {
		return fmt.Errorf("clear default: %w", err)
	}
	return nil
}

// GetByID returns one tenant-scoped, non-deleted calendar, or ErrCalendarNotFound.
func (r *SQLRepository) GetByID(ctx context.Context, tenantID, id string) (*BusinessCalendar, error) {
	const q = calendarSelect + ` WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	var row calendarRow
	if err := r.db.GetContext(ctx, &row, q, tenantID, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCalendarNotFound
		}
		return nil, fmt.Errorf("calmeta.SQLRepository.GetByID: %w", err)
	}
	cal := rowToCalendar(&row)
	return &cal, nil
}

// GetByName returns one tenant-scoped, non-deleted calendar by name, or ErrCalendarNotFound.
func (r *SQLRepository) GetByName(ctx context.Context, tenantID, name string) (*BusinessCalendar, error) {
	const q = calendarSelect + ` WHERE tenant_id = ? AND name = ? AND deleted_at IS NULL`
	var row calendarRow
	if err := r.db.GetContext(ctx, &row, q, tenantID, name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCalendarNotFound
		}
		return nil, fmt.Errorf("calmeta.SQLRepository.GetByName: %w", err)
	}
	cal := rowToCalendar(&row)
	return &cal, nil
}

// GetDefault returns the tenant's default calendar, or ErrCalendarNotFound when none is
// flagged. This is the SLA/timer engine's lookup (next slice, 1b): not-found there means
// "fall back to wall-clock arithmetic".
func (r *SQLRepository) GetDefault(ctx context.Context, tenantID string) (*BusinessCalendar, error) {
	const q = calendarSelect + ` WHERE tenant_id = ? AND is_default = 1 AND deleted_at IS NULL LIMIT 1`
	var row calendarRow
	if err := r.db.GetContext(ctx, &row, q, tenantID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCalendarNotFound
		}
		return nil, fmt.Errorf("calmeta.SQLRepository.GetDefault: %w", err)
	}
	cal := rowToCalendar(&row)
	return &cal, nil
}

// List returns every tenant-scoped, non-deleted calendar, default first then by name.
// Non-nil slice always (empty must marshal as [] not null).
func (r *SQLRepository) List(ctx context.Context, tenantID string) ([]BusinessCalendar, error) {
	const q = calendarSelect + ` WHERE tenant_id = ? AND deleted_at IS NULL
		ORDER BY is_default DESC, name ASC, id ASC`
	var rows []calendarRow
	if err := r.db.SelectContext(ctx, &rows, q, tenantID); err != nil {
		return nil, fmt.Errorf("calmeta.SQLRepository.List: %w", err)
	}
	cals := make([]BusinessCalendar, len(rows))
	for i := range rows {
		cals[i] = rowToCalendar(&rows[i])
	}
	return cals, nil
}

// Update applies the mutable fields to a tenant-scoped, non-deleted calendar. When
// IsDefault is set, other calendars' default flags are cleared in the SAME transaction.
// Returns ErrCalendarNotFound when no live row matches, ErrCalendarDuplicateName when the
// new name collides.
func (r *SQLRepository) Update(ctx context.Context, tenantID, id string, in UpdateInput) error {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("calmeta.SQLRepository.Update: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if in.IsDefault {
		if err := clearDefault(ctx, tx, tenantID, id); err != nil {
			return fmt.Errorf("calmeta.SQLRepository.Update: %w", err)
		}
	}
	const q = `UPDATE sys_business_calendars
		SET name = ?, timezone = ?, hours_json = ?, holidays_json = ?, is_default = ?, updated_by = ?
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	res, err := tx.ExecContext(ctx, q,
		in.Name, in.Timezone, string(in.Hours), nullableJSON(in.Holidays),
		in.IsDefault, nullable(in.UpdatedBy),
		tenantID, id,
	)
	if err != nil {
		if isDuplicateEntry(err) {
			return ErrCalendarDuplicateName
		}
		return fmt.Errorf("calmeta.SQLRepository.Update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("calmeta.SQLRepository.Update: rows affected: %w", err)
	}
	if n == 0 {
		return ErrCalendarNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("calmeta.SQLRepository.Update: commit: %w", err)
	}
	return nil
}

// calendarUsagesSQL lists the live rules of one tenant that pin calendar id, by either door: the
// date_offset timer or any action's calendarId (set_deadline). Disabled rules count — re-enabling
// one must not meet a dangling pin. `condition` is a reserved word, hence the backticks.
// ponytail: one JSON scan of the tenant's rules per delete (rare admin action); no reference
// table to keep in sync. Index only if a tenant grows past ~5k rules.
const calendarUsagesSQL = "SELECT DISTINCT w.id AS workflow_id, w.name AS workflow_name\n" +
	"FROM sys_workflows w\n" +
	"LEFT JOIN JSON_TABLE(w.actions, '$[*]' COLUMNS (cid VARCHAR(64) PATH '$.calendarId')) AS jt ON TRUE\n" +
	"WHERE w.tenant_id = ? AND w.deleted_at IS NULL\n" +
	"  AND (jt.cid = ? OR w.`condition` ->> '$.timer.calendarId' = ?)\n" +
	"ORDER BY w.name, w.id"

// Usages lists the tenant's live rules that pin calendar id (empty = free to delete).
func (r *SQLRepository) Usages(ctx context.Context, tenantID, id string) ([]Usage, error) {
	var out []Usage
	if err := r.db.SelectContext(ctx, &out, calendarUsagesSQL, tenantID, id, id); err != nil {
		return nil, fmt.Errorf("calmeta.SQLRepository.Usages: %w", err)
	}
	return out, nil
}

// SoftDelete soft-deletes a tenant-scoped calendar. The name is tombstone-renamed
// ("<name>__deleted_<id8>") so uq_sys_business_calendars_tenant_name frees the original
// name for a future recreate — the same unique-slot discipline as sys_apps (the service
// caps names at 80 runes, so name + the 18-char suffix always fits VARCHAR(100)).
func (r *SQLRepository) SoftDelete(ctx context.Context, tenantID, id string) error {
	const q = `UPDATE sys_business_calendars
		SET deleted_at = ?, is_default = 0, name = CONCAT(name, '__deleted_', LEFT(id, 8))
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	res, err := r.db.ExecContext(ctx, q, r.now().UTC(), tenantID, id)
	if err != nil {
		return fmt.Errorf("calmeta.SQLRepository.SoftDelete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("calmeta.SQLRepository.SoftDelete: rows affected: %w", err)
	}
	if n == 0 {
		return ErrCalendarNotFound
	}
	return nil
}
