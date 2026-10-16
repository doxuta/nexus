package janitor

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// SQLSweeper runs the janitor's bounded DELETEs over sqlx with parameterized queries. Table names
// are static string literals (never interpolated); the cutoff and batch limit always travel as `?`.
// Every statement carries `LIMIT ?` so a single DELETE removes at most `limit` rows — bounding the
// table lock even against a multi-million-row backlog. These are cross-tenant SYSTEM tables, so
// (unlike record queries) the statements are intentionally NOT tenant-pinned — retention is global.
type SQLSweeper struct {
	db *sqlx.DB
}

// NewSQLSweeper wires the sweeper over an existing sqlx connection (the same pool the workflow /
// dynamic-record repositories use).
func NewSQLSweeper(db *sqlx.DB) *SQLSweeper {
	return &SQLSweeper{db: db}
}

// Ensure the concrete sweeper satisfies the janitor's consumer interface at compile time.
var _ Sweeper = (*SQLSweeper)(nil)

// DeleteWorkflowRunsFinishedBefore deletes up to limit sys_workflow_runs rows whose finished_at is
// strictly before cutoff, returning the number removed.
func (s *SQLSweeper) DeleteWorkflowRunsFinishedBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return s.deleteOlderThan(ctx, "sys_workflow_runs", "finished_at", cutoff, limit)
}

// DeleteMFAPendingExpiredBefore deletes up to limit mfa_pending_tokens rows whose expires_at is
// strictly before cutoff, returning the number removed.
func (s *SQLSweeper) DeleteMFAPendingExpiredBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return s.deleteOlderThan(ctx, "mfa_pending_tokens", "expires_at", cutoff, limit)
}

// DeletePasswordResetExpiredBefore deletes up to limit password_reset_tokens rows whose expires_at
// is strictly before cutoff, returning the number removed.
func (s *SQLSweeper) DeletePasswordResetExpiredBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return s.deleteOlderThan(ctx, "password_reset_tokens", "expires_at", cutoff, limit)
}

// DeleteSSOHandoffExpiredBefore deletes up to limit sso_handoff_tokens rows whose expires_at is
// strictly before cutoff, returning the number removed (SSO-FE-REDIRECT: the one-time handoff
// codes are 120s-TTL rows that would otherwise accumulate forever, mirrors mfa_pending_tokens).
func (s *SQLSweeper) DeleteSSOHandoffExpiredBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return s.deleteOlderThan(ctx, "sso_handoff_tokens", "expires_at", cutoff, limit)
}

// DeleteJobsFinishedBefore deletes up to limit sys_jobs rows whose finished_at is strictly before
// cutoff, returning the number removed (JOBS-1 retention). finished_at is set ONLY on done/failed
// rows — queued/running rows carry NULL and never match `finished_at < ?`, so no status filter is
// needed and in-flight work can never be swept.
func (s *SQLSweeper) DeleteJobsFinishedBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return s.deleteOlderThan(ctx, "sys_jobs", "finished_at", cutoff, limit)
}

// DeleteHookDeliveriesBefore deletes up to limit sys_hook_deliveries rows created strictly
// before cutoff (CONG-TIN-HIEU retention — security audit F1: the delivery log carries 2KB
// payload heads per signal and would otherwise grow unbounded; only the most recent 50 are
// ever surfaced, so aged rows have zero read value).
func (s *SQLSweeper) DeleteHookDeliveriesBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return s.deleteOlderThan(ctx, "sys_hook_deliveries", "created_at", cutoff, limit)
}

// DeleteIdempotencyKeysExpiredBefore deletes up to limit sys_idempotency_keys rows whose expires_at
// is strictly before cutoff (specs/047). Served by idx_idem_expires. expires_at is written by the
// application clock through the same driver, so comparing against the janitor's cutoff is symmetric.
func (s *SQLSweeper) DeleteIdempotencyKeysExpiredBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return s.deleteOlderThan(ctx, "sys_idempotency_keys", "expires_at", cutoff, limit)
}

// DeletePreviewSessionsExpiredBefore deletes up to limit sys_preview_sessions rows whose expires_at
// is strictly before cutoff (specs/064). A live session has expires_at in the future, so it can
// never match; the request path already refuses expired rows, so sweeping them changes nothing.
func (s *SQLSweeper) DeletePreviewSessionsExpiredBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return s.deleteOlderThan(ctx, "sys_preview_sessions", "expires_at", cutoff, limit)
}

// DeleteEmailLogBefore deletes up to limit sys_email_log rows created strictly before cutoff
// (specs/066). Cross-tenant by design, like every table here: retention is a global policy.
func (s *SQLSweeper) DeleteEmailLogBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return s.deleteOlderThan(ctx, "sys_email_log", "created_at", cutoff, limit)
}

// DeleteUsageDailyBefore deletes up to limit sys_usage_daily rows whose day is strictly before the
// calendar day of cutoff (specs/066 L4). `day` is a DATE: the cutoff is cut to its day so a row of
// the cutoff's own day is kept. Served by idx_usage_day. Cross-tenant by design, like every sweep here.
func (s *SQLSweeper) DeleteUsageDailyBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return s.deleteOlderThan(ctx, "sys_usage_daily", "day", cutoff.UTC().Truncate(24*time.Hour), limit)
}

// FailStaleWorkflowRunsStartedBefore turns up to limit 'running' sys_workflow_runs rows started
// before cutoff into 'failed' (#338). Cross-tenant by design, like every sweep here. The WHERE keeps
// status='running', so a closed row leaves the set and the batch loop ends; a row finished by the
// engine a moment ago no longer matches (CAS on the state the SET writes over).
func (s *SQLSweeper) FailStaleWorkflowRunsStartedBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit < 1 {
		limit = 1
	}
	// ponytail: cross-tenant sweep, no index leads with status/started_at → scans sys_workflow_runs (90d retention) hourly; add (status, started_at) past ~1M rows.
	res, err := s.db.ExecContext(ctx,
		`UPDATE sys_workflow_runs
		 SET status = 'failed', error = COALESCE(error, ?), finished_at = UTC_TIMESTAMP(3)
		 WHERE status = 'running' AND started_at < ? LIMIT ?`,
		"Lần chạy không kết thúc — máy chủ đã dừng giữa chừng", cutoff.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("janitor.SQLSweeper.FailStaleWorkflowRunsStartedBefore: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("janitor.SQLSweeper.FailStaleWorkflowRunsStartedBefore: rows affected: %w", err)
	}
	return n, nil
}

// deleteOlderThan runs one bounded DELETE. table/ageCol are trusted internal literals from the
// exported methods above (never user input); cutoff and limit are bound parameters. It returns
// RowsAffected so the janitor's batch loop can detect the final short batch.
func (s *SQLSweeper) deleteOlderThan(ctx context.Context, table, ageCol string, cutoff time.Time, limit int) (int64, error) {
	if limit < 1 {
		limit = 1
	}
	// #nosec G201 -- table/ageCol are compile-time constants from this package, not user input;
	// the only runtime values (cutoff, limit) are parameterized.
	q := fmt.Sprintf("DELETE FROM %s WHERE %s < ? LIMIT ?", table, ageCol)
	res, err := s.db.ExecContext(ctx, q, cutoff.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("janitor.SQLSweeper.deleteOlderThan(%s): %w", table, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("janitor.SQLSweeper.deleteOlderThan(%s): rows affected: %w", table, err)
	}
	return n, nil
}
