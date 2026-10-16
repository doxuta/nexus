// Package notification provides the notification repository for reading from sys_notifications.
// Write paths live in the workflow and bpm packages (they insert rows as side effects of their
// engines). This package owns the READ surface: list, unread-count, mark-read, mark-all-read.
//
// Note: sys_notifications has no deleted_at column (migration 000017) — rows are permanent inbox
// items. There is no soft-delete on this table.
package notification

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// Notification is the read model returned by the list and mark-read APIs.
type Notification struct {
	ID         string     `db:"id"          json:"id"`
	TenantID   string     `db:"tenant_id"   json:"-"`
	UserID     string     `db:"user_id"     json:"-"`
	Type       *string    `db:"type"        json:"type,omitempty"`
	Title      *string    `db:"title"       json:"title,omitempty"`
	Message    string     `db:"message"     json:"message"`
	EntityName *string    `db:"entity_name" json:"entityName,omitempty"`
	RecordID   *string    `db:"record_id"   json:"recordId,omitempty"`
	IsRead     bool       `db:"is_read"     json:"isRead"`
	ReadAt     *time.Time `db:"read_at"     json:"readAt,omitempty"`
	CreatedAt  time.Time  `db:"created_at"  json:"createdAt"`

	// Href is the FE deep-link, computed (not a column). Clicking a notice must land ON the record
	// it is about, not on the generic inbox. Empty when there is nothing to point at.
	Href string `db:"-" json:"href,omitempty"`
}

// DeepLink returns the FE route this notification points at: the record it concerns when both
// entity and record are present, else the approvals inbox for approval/assignment types, else "".
// A notice you cannot click through to the thing it is about is half a notification.
func (n *Notification) DeepLink() string {
	if n.EntityName != nil && *n.EntityName != "" && n.RecordID != nil && *n.RecordID != "" {
		return "/" + *n.EntityName + "/" + *n.RecordID
	}
	if n.Type != nil && (*n.Type == "approval" || *n.Type == "assignment") {
		return "/approvals"
	}
	return ""
}

// SQLRepository reads from sys_notifications using sqlx parameterized queries.
// All methods are pinned to (tenant_id, user_id) — no cross-tenant, no cross-user reads.
type SQLRepository struct {
	db  *sqlx.DB
	now func() time.Time
}

// New wires the notification repository over an existing sqlx connection.
func New(db *sqlx.DB) *SQLRepository {
	return &SQLRepository{db: db, now: time.Now}
}

// ErrNotFound is returned when a notification id does not exist within (tenant, user) scope.
var ErrNotFound = fmt.Errorf("notification: not found")

// List returns the caller's notifications (newest first), optionally filtered to unread only.
// page / limit are 1-indexed offset pagination matching the standard API envelope.
// ORDER BY created_at DESC, id DESC (tie-break) — the idx_notif_inbox composite covers this.
func (r *SQLRepository) List(ctx context.Context, tenantID, userID string, unreadOnly bool, page, limit int) ([]Notification, int, error) {
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	offset := (page - 1) * limit

	// COUNT query — shares the same WHERE so total reflects the filter.
	countQ := `SELECT COUNT(*) FROM sys_notifications WHERE tenant_id = ? AND user_id = ?`
	listQ := `SELECT id, tenant_id, user_id, type, title, message, entity_name, record_id, is_read, read_at, created_at
		FROM sys_notifications
		WHERE tenant_id = ? AND user_id = ?`
	args := []any{tenantID, userID}

	if unreadOnly {
		countQ += " AND is_read = 0"
		listQ += " AND is_read = 0"
	}
	listQ += " ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?"

	var total int
	if err := r.db.GetContext(ctx, &total, countQ, args...); err != nil {
		return nil, 0, fmt.Errorf("notification.SQLRepository.List count: %w", err)
	}

	listArgs := append(args, limit, offset)
	var rows []Notification
	if err := r.db.SelectContext(ctx, &rows, listQ, listArgs...); err != nil {
		return nil, 0, fmt.Errorf("notification.SQLRepository.List: %w", err)
	}
	return rows, total, nil
}

// CountUnread returns the number of unread notifications for (tenant, user).
// Uses the idx_notif_inbox (tenant_id, user_id, is_read, created_at) index.
func (r *SQLRepository) CountUnread(ctx context.Context, tenantID, userID string) (int, error) {
	const q = `SELECT COUNT(*) FROM sys_notifications WHERE tenant_id = ? AND user_id = ? AND is_read = 0`
	var n int
	if err := r.db.GetContext(ctx, &n, q, tenantID, userID); err != nil {
		return 0, fmt.Errorf("notification.SQLRepository.CountUnread: %w", err)
	}
	return n, nil
}

// ExistsForUser checks whether (tenant_id, user_id, id) row exists (ignoring is_read state).
// Used by MarkRead to distinguish "not theirs / not found" (404) from "already read" (200, idempotent).
func (r *SQLRepository) ExistsForUser(ctx context.Context, tenantID, userID, id string) (bool, error) {
	const q = `SELECT 1 FROM sys_notifications WHERE tenant_id = ? AND user_id = ? AND id = ? LIMIT 1`
	var one int
	err := r.db.GetContext(ctx, &one, q, tenantID, userID, id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("notification.SQLRepository.ExistsForUser: %w", err)
	}
	return true, nil
}

// MarkRead marks a single notification as read (idempotent: already-read rows return nil).
// Caller MUST first check ExistsForUser to distinguish 404 from already-read.
func (r *SQLRepository) MarkRead(ctx context.Context, tenantID, userID, id string) error {
	const q = `UPDATE sys_notifications
		SET is_read = 1, read_at = ?
		WHERE tenant_id = ? AND user_id = ? AND id = ? AND is_read = 0`
	if _, err := r.db.ExecContext(ctx, q, r.now().UTC(), tenantID, userID, id); err != nil {
		return fmt.Errorf("notification.SQLRepository.MarkRead: %w", err)
	}
	return nil
}

// MarkAllRead marks every unread notification of the caller as read.
// Returns the number of rows updated (0 is valid — no unread).
func (r *SQLRepository) MarkAllRead(ctx context.Context, tenantID, userID string) (int, error) {
	const q = `UPDATE sys_notifications SET is_read = 1, read_at = ? WHERE tenant_id = ? AND user_id = ? AND is_read = 0`
	res, err := r.db.ExecContext(ctx, q, r.now().UTC(), tenantID, userID)
	if err != nil {
		return 0, fmt.Errorf("notification.SQLRepository.MarkAllRead: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("notification.SQLRepository.MarkAllRead rows affected: %w", err)
	}
	return int(n), nil
}
