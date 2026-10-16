package emaillog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/dynrepo"
)

// Column widths of sys_email_log (migration 000081). Every write clips INSIDE the repository: in
// strict mode an overlong value is "Data too long" and the row — the evidence that a mail failed —
// would silently vanish.
const (
	shortMax = 255
	errorMax = 2000
)

// clip cuts s to at most n RUNES: n is the column's character width (utf8mb4), so a cut
// on a rune boundary is always valid. A cut string ends with "…".
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// Repository is the sqlx-backed store of sys_email_log. Every statement pins tenant_id = ?.
type Repository struct {
	db *sqlx.DB
}

// NewRepository wires the repository over the shared sqlx pool.
func NewRepository(db *sqlx.DB) *Repository { return &Repository{db: db} }

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Insert writes one row. TenantID, ID, Source and Status are required (an untenanted row would be
// unreachable by every tenant-pinned read and could never be cleaned up with its tenant).
func (r *Repository) Insert(ctx context.Context, e Entry) error {
	if e.TenantID == "" || e.ID == "" || e.Source == "" || e.Status == "" {
		return fmt.Errorf("emaillog.Insert: id, tenant, source and status are required")
	}
	var finished any
	if e.Finished {
		finished = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO sys_email_log
		  (id, tenant_id, source, status, recipient_masked, subject_template, workflow_id, run_id, action_index,
		   action_hash, entity_name, record_id, user_id, job_id, attempts, last_error, resend_of, created_by, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.TenantID, e.Source, e.Status, clip(e.RecipientMasked, shortMax), nullStr(clip(e.SubjectTemplate, shortMax)),
		nullStr(e.WorkflowID), nullStr(e.RunID), e.ActionIndex, nullStr(e.ActionHash), nullStr(e.EntityName),
		nullStr(e.RecordID), nullStr(e.UserID), nullStr(e.JobID), e.Attempts, nullStr(clip(e.LastError, errorMax)),
		nullStr(e.ResendOf), nullStr(e.CreatedBy), finished)
	if err != nil {
		return fmt.Errorf("emaillog.Insert: %w", err)
	}
	return nil
}

// AttachJob records which sys_jobs row carries the delivery. Not CAS-guarded: a fast worker may
// finish before this lands and the job id is still worth keeping.
func (r *Repository) AttachJob(ctx context.Context, tenantID, id, jobID string) error {
	if _, err := r.db.ExecContext(ctx, `UPDATE sys_email_log SET job_id = ? WHERE tenant_id = ? AND id = ?`,
		jobID, tenantID, id); err != nil {
		return fmt.Errorf("emaillog.AttachJob: %w", err)
	}
	return nil
}

// The three state writers are CAS on status='queued' (specs/066 D-5): a worker that lost its lease
// and arrives late cannot flip a terminal row. The WHERE excludes the state the SET writes, so the
// answer does not depend on clientFoundRows.

// MarkAttempt records a failed attempt that still has retries left.
func (r *Repository) MarkAttempt(ctx context.Context, tenantID, id string, attempts int, lastErr string) error {
	if _, err := r.db.ExecContext(ctx, `UPDATE sys_email_log SET attempts = ?, last_error = ?
		WHERE tenant_id = ? AND id = ? AND status = 'queued'`,
		attempts, nullStr(clip(lastErr, errorMax)), tenantID, id); err != nil {
		return fmt.Errorf("emaillog.MarkAttempt: %w", err)
	}
	return nil
}

// MarkSent turns a queued row sent.
func (r *Repository) MarkSent(ctx context.Context, tenantID, id string, attempts int) error {
	if _, err := r.db.ExecContext(ctx, `UPDATE sys_email_log SET status = 'sent', attempts = ?, last_error = NULL, finished_at = ?
		WHERE tenant_id = ? AND id = ? AND status = 'queued'`,
		attempts, time.Now().UTC(), tenantID, id); err != nil {
		return fmt.Errorf("emaillog.MarkSent: %w", err)
	}
	return nil
}

// MarkFailed turns a queued row failed (the queue gave up, or the mail can never be sent).
func (r *Repository) MarkFailed(ctx context.Context, tenantID, id string, attempts int, lastErr string) error {
	if _, err := r.db.ExecContext(ctx, `UPDATE sys_email_log SET status = 'failed', attempts = ?, last_error = ?, finished_at = ?
		WHERE tenant_id = ? AND id = ? AND status = 'queued'`,
		attempts, nullStr(clip(lastErr, errorMax)), time.Now().UTC(), tenantID, id); err != nil {
		return fmt.Errorf("emaillog.MarkFailed: %w", err)
	}
	return nil
}

// ErrNotFound is "no such row for this tenant" — the same answer for another tenant's id and an id
// that never existed, so a probe learns nothing (specs/066 FR-036).
var ErrNotFound = errors.New("emaillog: entry not found")

// Get reads one row of the tenant, with the rule's current name (null once the rule is deleted).
func (r *Repository) Get(ctx context.Context, tenantID, id string) (*Row, error) {
	var row Row
	err := r.db.GetContext(ctx, &row, `
		SELECT l.id, l.created_at, l.finished_at, l.source, l.status, l.recipient_masked, l.subject_template,
		       l.workflow_id, w.name AS workflow_name, l.action_index, l.entity_name, l.record_id, l.job_id,
		       l.attempts, l.last_error, l.resend_of, l.resent_at, l.created_by, u.full_name AS created_by_name, l.action_hash
		FROM sys_email_log l
		LEFT JOIN sys_workflows w ON w.id = l.workflow_id AND w.tenant_id = l.tenant_id AND w.deleted_at IS NULL
		LEFT JOIN users u ON u.id = l.created_by AND u.tenant_id = l.tenant_id
		WHERE l.tenant_id = ? AND l.id = ?`, tenantID, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("emaillog.Get: %w", err)
	}
	return &row, nil
}

// LockResend takes the one-shot right to resend a failed row: it stamps resent_at only on a row of
// THIS tenant that is failed and not yet resent (specs/066 FR-033). false = somebody else got it, or
// the row is not resendable; the caller does not need to know which. resent_at moves NULL → at, so
// the affected-rows count is an honest answer whatever the driver's clientFoundRows says.
func (r *Repository) LockResend(ctx context.Context, tenantID, id string, at time.Time) (bool, error) {
	res, err := r.db.ExecContext(ctx, `UPDATE sys_email_log SET resent_at = ?
		WHERE tenant_id = ? AND id = ? AND status = 'failed' AND resent_at IS NULL`, at.UTC(), tenantID, id)
	if err != nil {
		return false, fmt.Errorf("emaillog.LockResend: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("emaillog.LockResend: %w", err)
	}
	return n == 1, nil
}

// ReleaseResend gives the right back when a resend took the lock but produced no new row to carry the
// retry (a lost log write plus a failed send) — otherwise the mail would be lost for good.
func (r *Repository) ReleaseResend(ctx context.Context, tenantID, id string) error {
	if _, err := r.db.ExecContext(ctx, `UPDATE sys_email_log SET resent_at = NULL
		WHERE tenant_id = ? AND id = ?`, tenantID, id); err != nil {
		return fmt.Errorf("emaillog.ReleaseResend: %w", err)
	}
	return nil
}

// Page size bounds of the listing (the audit-log's / jobs' house rule).
const (
	defaultLimit = 50
	maxLimit     = 200
)

// ClampLimit normalizes a requested page size to [1, 200], defaulting non-positive values to 50.
func ClampLimit(n int) int {
	if n <= 0 {
		return defaultLimit
	}
	return min(n, maxLimit)
}

// ponytail: idx_email_log_list has status and source after id, so a status/source filter scans the tenant's newest rows
// until the page fills (bounded by 90-day retention); add KEY (tenant_id, status, created_at DESC, id DESC) if a
// tenant's sent volume dwarfs its failures.
// The keyset is (created_at DESC, id DESC); the cursor label is minted by no other surface, so a
// /admin/jobs cursor replayed here is ErrInvalidCursor instead of a silently wrong page.
const (
	cursorSort = "sys_email_log.created_at"
	timeLayout = "2006-01-02 15:04:05.000"
)

// ListForTenant returns one page of the tenant's log, newest first, and the next page's cursor ("" on
// the last). Keyset: every page costs the rows it walks. workflow_name is the rule's CURRENT name
// (LEFT JOIN pinned to the same tenant; null once the rule is deleted).
func (r *Repository) ListForTenant(ctx context.Context, tenantID string, f Filter) ([]Row, string, error) {
	if tenantID == "" {
		return nil, "", fmt.Errorf("emaillog.ListForTenant: tenantID is required")
	}
	where := "l.tenant_id = ?"
	args := []any{tenantID}
	if f.Status != "" {
		where += " AND l.status = ?"
		args = append(args, f.Status)
	}
	if f.Source != "" {
		where += " AND l.source = ?"
		args = append(args, f.Source)
	}
	if strings.TrimSpace(f.After) != "" {
		isNull, at, id, err := dynrepo.DecodeCursor(f.After, cursorSort, "DESC")
		if err != nil {
			return nil, "", fmt.Errorf("emaillog.ListForTenant: %w", err)
		}
		if _, perr := time.Parse(timeLayout, at); isNull || perr != nil {
			return nil, "", fmt.Errorf("emaillog.ListForTenant: cursor boundary %q is not a created_at: %w", at, dynrepo.ErrInvalidCursor)
		}
		where += " AND (l.created_at < ? OR (l.created_at = ? AND l.id < ?))"
		args = append(args, at, at, id)
	}
	limit := ClampLimit(f.Limit)
	args = append(args, limit+1)
	q := listSQL(where)
	var rows []Row
	if err := r.db.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, "", fmt.Errorf("emaillog.ListForTenant: %w", err)
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		next = dynrepo.EncodeCursor(cursorSort, "DESC", false, last.CreatedAt.Format(timeLayout), last.ID)
	}
	return rows, next, nil
}

// listSQL is the listing statement for a where built from constant fragments (the hint pins
// idx_email_log_list; the EXPLAIN integration test runs this exact text).
func listSQL(where string) string {
	return `SELECT /*+ INDEX(l idx_email_log_list) */
		       l.id, l.created_at, l.finished_at, l.source, l.status, l.recipient_masked, l.subject_template,
		       l.workflow_id, w.name AS workflow_name, l.action_index, l.entity_name, l.record_id, l.job_id,
		       l.attempts, l.last_error, l.resend_of, l.resent_at, l.created_by, u.full_name AS created_by_name
		FROM sys_email_log l
		LEFT JOIN sys_workflows w ON w.id = l.workflow_id AND w.tenant_id = l.tenant_id AND w.deleted_at IS NULL
		LEFT JOIN users u ON u.id = l.created_by AND u.tenant_id = l.tenant_id
		WHERE ` + where + `
		ORDER BY l.created_at DESC, l.id DESC
		LIMIT ?`
}
