package notify

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// Audit status values written to security_alerts.status (migration 000009).
const (
	statusPending = "pending"
	statusSent    = "sent"
	statusFailed  = "failed"
)

// AlertRepository is the dispatcher's consumer-side view of the security_alerts audit
// table. The dispatcher inserts a pending row when it picks up an event and flips the
// status to sent/failed after the send attempt. Defining the interface here (not in the
// concrete repo) lets unit tests substitute a fake without a database.
//
// Multi-tenant invariant: every method takes tenantID and the implementation filters on
// it — an alert row can never be read or mutated across tenants.
type AlertRepository interface {
	// InsertPending records a new pending audit row and returns its generated id.
	InsertPending(ctx context.Context, evt SecurityEvent) (string, error)
	// MarkSent stamps status=sent and sent_at for the row, scoped to tenant_id.
	MarkSent(ctx context.Context, tenantID, id string) error
	// MarkFailed stamps status=failed and stores the (truncated) error, scoped to tenant_id.
	MarkFailed(ctx context.Context, tenantID, id, sendErr string) error
}

// SQLAlertRepository persists audit rows in the fixed security_alerts system table over
// sqlx with parameterized queries. It mirrors the dynrepo style: identifiers are static
// (a fixed table), values always travel as `?` placeholders, and tenant_id pins every
// statement.
type SQLAlertRepository struct {
	db  *sqlx.DB
	now func() time.Time // injectable clock; defaults to time.Now
}

// NewSQLAlertRepository wires the audit repository over an existing sqlx connection.
func NewSQLAlertRepository(db *sqlx.DB) *SQLAlertRepository {
	return &SQLAlertRepository{db: db, now: time.Now}
}

// InsertPending writes a pending audit row for the event and returns the generated id.
// recipient snapshots evt.Email so the audit stays accurate if the user later changes
// their address. created_at uses the repository clock, not evt.At (which is the
// originating event's time, recorded only in business context).
func (r *SQLAlertRepository) InsertPending(ctx context.Context, evt SecurityEvent) (string, error) {
	id := uuid.NewString()
	const q = `INSERT INTO security_alerts
		(id, tenant_id, user_id, alert_type, channel, recipient, status, created_at)
		VALUES (?, ?, ?, ?, 'email', ?, ?, ?)`
	if _, err := r.db.ExecContext(ctx, q,
		id, evt.TenantID, evt.UserID, evt.Type, evt.Email, statusPending, r.now().UTC(),
	); err != nil {
		return "", fmt.Errorf("notify.SQLAlertRepository.InsertPending: %w", err)
	}
	return id, nil
}

// MarkSent flips a pending row to status=sent and stamps sent_at. The UPDATE is pinned to
// (tenant_id, id) so no cross-tenant row can ever be touched.
func (r *SQLAlertRepository) MarkSent(ctx context.Context, tenantID, id string) error {
	const q = `UPDATE security_alerts SET status = ?, sent_at = ?, error = NULL
		WHERE tenant_id = ? AND id = ?`
	if _, err := r.db.ExecContext(ctx, q, statusSent, r.now().UTC(), tenantID, id); err != nil {
		return fmt.Errorf("notify.SQLAlertRepository.MarkSent: %w", err)
	}
	return nil
}

// MarkFailed flips a row to status=failed and stores a truncated error string. The error
// is capped so a verbose SMTP failure cannot overflow the TEXT column or bloat the audit.
func (r *SQLAlertRepository) MarkFailed(ctx context.Context, tenantID, id, sendErr string) error {
	const maxErr = 1000
	if len(sendErr) > maxErr {
		sendErr = sendErr[:maxErr]
	}
	const q = `UPDATE security_alerts SET status = ?, error = ?
		WHERE tenant_id = ? AND id = ?`
	if _, err := r.db.ExecContext(ctx, q, statusFailed, sql.NullString{String: sendErr, Valid: sendErr != ""}, tenantID, id); err != nil {
		return fmt.Errorf("notify.SQLAlertRepository.MarkFailed: %w", err)
	}
	return nil
}
