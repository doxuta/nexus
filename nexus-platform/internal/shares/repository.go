// Package shares stores the manual per-record READ shares (DAILY-QUEUE #34 record-manual-share-a,
// specs/051-record-manual-share): "user X may view record R of entity E". The row filter that makes
// a share visible lives in dynrepo/rowscope.go; this package only creates, lists and revokes rows,
// behind Service, which authorizes every call through services.RecordService.ResolveShareTarget.
//
// Safety: EVERY query pins tenant_id + entity_name + record_id (rules: đa tổ chức); values always
// travel as `?` placeholders; identifiers are static.
package shares

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// ErrShareNotFound: no LIVE share of this record with this user in the tenant (never shared, or
// already revoked) — one sentinel for every miss, so a probe learns nothing more.
var ErrShareNotFound = errors.New("shares: share not found")

// Share is a live sys_record_shares row.
type Share struct {
	ID         string    `db:"id"`
	TenantID   string    `db:"tenant_id"`
	EntityName string    `db:"entity_name"`
	RecordID   string    `db:"record_id"`
	UserID     string    `db:"user_id"` // "" for a team share
	TeamID     string    `db:"team_id"` // "" for a person share (DAILY-QUEUE #224)
	SharedBy   string    `db:"shared_by"`
	CreatedAt  time.Time `db:"created_at"`
}

// Repository runs sys_record_shares DML over sqlx (same shape as notes.Repository).
type Repository struct {
	db *sqlx.DB
}

// NewRepository wires a share repository over an existing sqlx connection.
func NewRepository(db *sqlx.DB) *Repository { return &Repository{db: db} }

// user_id / team_id are NULL on the other kind of share; COALESCE keeps Share a plain-string struct.
const shareColumns = "id, tenant_id, entity_name, record_id, COALESCE(user_id, '') AS user_id, COALESCE(team_id, '') AS team_id, shared_by, created_at"

// Upsert grants (or re-grants) a share to ONE grantee — a person (userID) or a team (teamID), exactly
// one non-empty — and returns the live row plus whether the call CHANGED the
// table (a new row, or a revoked one revived) — false when the grantee already held a live share,
// which is left exactly as it was (original sharer and date kept). The caller skips its audit row on
// false (DAILY-QUEUE #226: a re-share must not repeat a History line).
//
// Two statements instead of INSERT … ON DUPLICATE KEY UPDATE on purpose: production forces the DSN's
// clientFoundRows (specs/057), under which a no-op ON DUPLICATE KEY reports 1 — the same as an
// insert — so RowsAffected could not tell them apart. Revive first (matches only a tombstone), then
// INSERT IGNORE (0 rows when the UNIQUE key already holds a live row); each is atomic, and two racing
// callers still produce exactly one winner. id/created_by are only used when the row is new.
func (r *Repository) Upsert(ctx context.Context, id, tenantID, entity, recordID, userID, teamID, sharedBy string) (*Share, bool, error) {
	// grantee_id is the generated COALESCE(user_id, team_id) the UNIQUE key is built on (migration 000090).
	grantee := userID
	if grantee == "" {
		grantee = teamID
	}
	if grantee == "" || (userID != "" && teamID != "") {
		return nil, false, fmt.Errorf("shares.Upsert: exactly one of userID / teamID is required")
	}
	const revive = `UPDATE sys_record_shares
	                SET shared_by = ?, created_at = CURRENT_TIMESTAMP, deleted_at = NULL
	                WHERE tenant_id = ? AND entity_name = ? AND record_id = ? AND grantee_id = ? AND deleted_at IS NOT NULL`
	const insert = `INSERT IGNORE INTO sys_record_shares (id, tenant_id, entity_name, record_id, user_id, team_id, shared_by, created_by)
	                VALUES (?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?)`
	changed, err := r.rowsAffected(ctx, revive, sharedBy, tenantID, entity, recordID, grantee)
	if err != nil {
		return nil, false, fmt.Errorf("shares.Upsert: revive: %w", err)
	}
	if !changed {
		if changed, err = r.rowsAffected(ctx, insert, id, tenantID, entity, recordID, userID, teamID, sharedBy, sharedBy); err != nil {
			return nil, false, fmt.Errorf("shares.Upsert: insert: %w", err)
		}
	}
	var s Share
	const get = `SELECT ` + shareColumns + ` FROM sys_record_shares
	             WHERE tenant_id = ? AND entity_name = ? AND record_id = ? AND grantee_id = ? AND deleted_at IS NULL`
	if err := r.db.GetContext(ctx, &s, get, tenantID, entity, recordID, grantee); err != nil {
		return nil, false, fmt.Errorf("shares.Upsert: read back: %w", err)
	}
	return &s, changed, nil
}

// rowsAffected runs one statement and reports whether it touched a row.
func (r *Repository) rowsAffected(ctx context.Context, q string, args ...any) (bool, error) {
	res, err := r.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n > 0, nil
}

// ListByRecord returns the live shares of one record, oldest first. A record is shared with a
// handful of people, so there is no pagination.
// ponytail: unpaginated; add LIMIT/cursor if a record ever carries hundreds of shares.
func (r *Repository) ListByRecord(ctx context.Context, tenantID, entity, recordID string) ([]Share, error) {
	const q = `SELECT ` + shareColumns + ` FROM sys_record_shares
	           WHERE tenant_id = ? AND entity_name = ? AND record_id = ? AND deleted_at IS NULL
	           ORDER BY created_at, id`
	out := []Share{}
	if err := r.db.SelectContext(ctx, &out, q, tenantID, entity, recordID); err != nil {
		return nil, fmt.Errorf("shares.ListByRecord: %w", err)
	}
	return out, nil
}

// Revoke soft-deletes the live share of (record, grantee) — the grantee is a person's or a team's id
// (UUIDs never collide, grantee_id spans both) — and reports which kind it was so the caller can audit
// it. ErrShareNotFound when none is live.
func (r *Repository) Revoke(ctx context.Context, tenantID, entity, recordID, granteeID string) (team bool, err error) {
	var sh Share
	const get = `SELECT ` + shareColumns + ` FROM sys_record_shares
	             WHERE tenant_id = ? AND entity_name = ? AND record_id = ? AND grantee_id = ? AND deleted_at IS NULL`
	if err := r.db.GetContext(ctx, &sh, get, tenantID, entity, recordID, granteeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrShareNotFound
		}
		return false, fmt.Errorf("shares.Revoke: read: %w", err)
	}
	// The guard `deleted_at IS NULL` keeps the CAS honest under clientFoundRows (a concurrent revoke
	// that got there first leaves 0 matching rows).
	const q = `UPDATE sys_record_shares SET deleted_at = ?
	           WHERE tenant_id = ? AND entity_name = ? AND record_id = ? AND grantee_id = ? AND deleted_at IS NULL`
	res, err := r.db.ExecContext(ctx, q, time.Now().UTC(), tenantID, entity, recordID, granteeID)
	if err != nil {
		return false, fmt.Errorf("shares.Revoke: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("shares.Revoke: rows affected: %w", err)
	}
	if n == 0 {
		return false, ErrShareNotFound
	}
	return sh.TeamID != "", nil
}
