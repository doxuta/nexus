// Package janitor is the retention sweeper for cross-tenant SYSTEM tables that grow unbounded:
// sys_workflow_runs (one row per event×rule, incl. skipped), mfa_pending_tokens,
// password_reset_tokens and sso_handoff_tokens (short-lived credentials), sys_jobs (finished
// background jobs), sys_hook_deliveries (inbound-hook delivery log) and sys_idempotency_keys
// (expired replay claims), sys_email_log (tenant email log) — Sweep is the authoritative list. A single goroutine ticks every
// Interval and, per sweep, deletes rows past their retention cutoff in BOUNDED batches so a huge
// backlog can never lock a table for long.
//
// TENANT-AGNOSTIC BY DESIGN: retention is a global operational policy on system tables, not
// business data — so unlike the record paths, these DELETEs are deliberately NOT tenant-pinned
// (the same intentional exception the workflow scheduler's ListDueScheduled and the BPM SLA scan
// make). No PII is logged: each sweep logs only per-table deleted counts.
package janitor

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Sweeper is the janitor's consumer-side view of the delete operations, one per table. Each method
// deletes at most `limit` rows older than `cutoff` and returns how many it removed, so the janitor
// can loop until a batch comes back short. Satisfied by *SQLSweeper; a fake drives the unit tests.
type Sweeper interface {
	// DeleteWorkflowRunsFinishedBefore removes up to limit sys_workflow_runs rows whose finished_at
	// is strictly before cutoff.
	DeleteWorkflowRunsFinishedBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	// DeleteMFAPendingExpiredBefore removes up to limit mfa_pending_tokens rows whose expires_at is
	// strictly before cutoff.
	DeleteMFAPendingExpiredBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	// DeletePasswordResetExpiredBefore removes up to limit password_reset_tokens rows whose
	// expires_at is strictly before cutoff.
	DeletePasswordResetExpiredBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	// DeleteSSOHandoffExpiredBefore removes up to limit sso_handoff_tokens rows whose expires_at
	// is strictly before cutoff (SSO-FE-REDIRECT one-time handoff codes).
	DeleteSSOHandoffExpiredBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	// DeleteJobsFinishedBefore removes up to limit sys_jobs rows whose finished_at is strictly
	// before cutoff (JOBS-1 retention). Only done/failed rows carry a finished_at — queued and
	// running rows have NULL and are never swept.
	DeleteJobsFinishedBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	// DeleteHookDeliveriesBefore removes up to limit sys_hook_deliveries rows created strictly
	// before cutoff (CONG-TIN-HIEU delivery-log retention — shares RunsRetention).
	DeleteHookDeliveriesBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	// DeleteIdempotencyKeysExpiredBefore removes up to limit sys_idempotency_keys rows whose
	// expires_at is strictly before cutoff (specs/047 — Idempotency-Key claims past their 24 h
	// replay window; the request path already treats them as absent).
	DeleteIdempotencyKeysExpiredBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	// DeletePreviewSessionsExpiredBefore removes up to limit sys_preview_sessions rows whose
	// expires_at is strictly before cutoff (specs/064 — "preview as role" trail rows; shares
	// RunsRetention, the audit log keeps the long-term record).
	DeletePreviewSessionsExpiredBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	// DeleteEmailLogBefore removes up to limit sys_email_log rows created strictly before cutoff
	// (specs/066 — the tenant's email log; shares RunsRetention).
	DeleteEmailLogBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	// DeleteUsageDailyBefore removes up to limit sys_usage_daily rows whose DAY is strictly before
	// cutoff (specs/066 L4 — the tenant's per-day usage figures; own USAGE_RETENTION_DAYS, 2 years).
	DeleteUsageDailyBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	// FailStaleWorkflowRunsStartedBefore closes up to limit sys_workflow_runs rows still 'running'
	// whose started_at is strictly before cutoff: status failed, with a reason (#338 — the server
	// died between writing the row and finishing the run). It closes, never deletes.
	FailStaleWorkflowRunsStartedBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
}

// staleRunAfter is how long a run may stay 'running' before the janitor calls it dead. The longest
// legitimate run is one rule's actions over one record (seconds) or one scheduled scan (maxScan
// records) — an hour is far past both. ponytail: a constant; a config knob if a customer's scheduled
// scan ever legitimately runs longer.
const staleRunAfter = time.Hour

// defaultBatchSize bounds how many rows one DELETE statement removes, so a single statement can
// never take a long table lock even when the backlog is millions of rows. The sweep loops until a
// batch returns fewer than this many rows (nothing left to delete this cycle).
const defaultBatchSize = 1000

// Janitor is the background retention sweeper. Start launches one ticker goroutine; Stop drains it
// gracefully. It shares the workflow scheduler's lifecycle contract (idempotent Start/Stop, panic-
// recovering ticks, injectable clock for tests).
type Janitor struct {
	sweeper        Sweeper
	interval       time.Duration
	runsRetention  time.Duration
	tokenGrace     time.Duration
	jobsRetention  time.Duration
	usageRetention time.Duration
	batchSize      int
	now            func() time.Time // injectable clock (tests); defaults to time.Now
	logger         *zap.Logger

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}
}

// Config configures a Janitor. Interval is the tick cadence (0 is caller's business — a disabled
// janitor should not be constructed/started). RunsRetention/TokenGrace/JobsRetention are the age
// thresholds.
type Config struct {
	Interval      time.Duration
	RunsRetention time.Duration
	TokenGrace    time.Duration
	JobsRetention time.Duration // finished (done/failed) sys_jobs rows (JOBS_RETENTION_DAYS)
	// UsageRetention is the age of the sys_usage_daily rows swept (USAGE_RETENTION_DAYS, default 730 d).
	UsageRetention time.Duration
}

// New builds (does NOT start) a janitor. A nil sweeper makes every sweep a no-op (janitor disabled).
// A nil logger is replaced with a no-op. RunsRetention/TokenGrace <= 0 are clamped to a safe
// non-zero minimum so a misconfig can never delete rows "older than now" (i.e. everything).
func New(cfg Config, sweeper Sweeper, logger *zap.Logger) *Janitor {
	if logger == nil {
		logger = zap.NewNop()
	}
	runsRetention := cfg.RunsRetention
	if runsRetention <= 0 {
		runsRetention = 90 * 24 * time.Hour
	}
	tokenGrace := cfg.TokenGrace
	if tokenGrace <= 0 {
		tokenGrace = 24 * time.Hour
	}
	jobsRetention := cfg.JobsRetention
	if jobsRetention <= 0 {
		jobsRetention = 30 * 24 * time.Hour
	}
	usageRetention := cfg.UsageRetention
	if usageRetention <= 0 {
		usageRetention = 730 * 24 * time.Hour
	}
	return &Janitor{
		usageRetention: usageRetention,
		sweeper:        sweeper,
		interval:       cfg.Interval,
		runsRetention:  runsRetention,
		tokenGrace:     tokenGrace,
		jobsRetention:  jobsRetention,
		batchSize:      defaultBatchSize,
		now:            time.Now,
		logger:         logger,
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
	}
}

// Start launches the ticker goroutine (idempotent). The first sweep fires after one Interval so
// boot does not race the rest of wiring. The goroutine runs until Stop.
func (j *Janitor) Start() {
	j.startOnce.Do(func() { go j.run() })
}

// run is the ticker loop. Each tick is wrapped in sweepOnce (which recovers panics) so one bad
// sweep can never kill the goroutine. It exits when stop is closed, then closes done for Stop.
func (j *Janitor) run() {
	defer close(j.done)
	interval := j.interval
	if interval < time.Second {
		interval = time.Hour // defensive: a sub-second interval would be a busy loop
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-j.stop:
			return
		case <-ticker.C:
			j.sweepOnce(context.Background())
		}
	}
}

// sweepOnce runs one full sweep, recovering from any panic so the goroutine survives.
func (j *Janitor) sweepOnce(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			j.logger.Error("janitor: recovered panic during sweep", zap.Any("panic", r))
		}
	}()
	if j.sweeper == nil {
		return
	}
	j.Sweep(ctx, j.now().UTC())
}

// Sweep performs one deterministic retention pass at time `now` and logs per-table deleted counts.
// Exported so a test (and a future ops "run now" hook) can drive a single pass without the ticker.
// Each table is swept independently: an error on one is logged and the others still run.
func (j *Janitor) Sweep(ctx context.Context, now time.Time) {
	runsCutoff := now.Add(-j.runsRetention)
	tokenCutoff := now.Add(-j.tokenGrace)
	jobsCutoff := now.Add(-j.jobsRetention)

	runs := j.deleteBatched(ctx, "sys_workflow_runs", runsCutoff, j.sweeper.DeleteWorkflowRunsFinishedBefore)
	// Close the runs the server died on BEFORE they would age into the retention delete above next sweep.
	staleRuns := j.deleteBatched(ctx, "sys_workflow_runs(stale running)", now.Add(-staleRunAfter), j.sweeper.FailStaleWorkflowRunsStartedBefore)
	mfa := j.deleteBatched(ctx, "mfa_pending_tokens", tokenCutoff, j.sweeper.DeleteMFAPendingExpiredBefore)
	resets := j.deleteBatched(ctx, "password_reset_tokens", tokenCutoff, j.sweeper.DeletePasswordResetExpiredBefore)
	handoffs := j.deleteBatched(ctx, "sso_handoff_tokens", tokenCutoff, j.sweeper.DeleteSSOHandoffExpiredBefore)
	jobs := j.deleteBatched(ctx, "sys_jobs", jobsCutoff, j.sweeper.DeleteJobsFinishedBefore)
	hookDeliveries := j.deleteBatched(ctx, "sys_hook_deliveries", runsCutoff, j.sweeper.DeleteHookDeliveriesBefore)
	// expires_at already marks the end of the replay window — no grace on top of it.
	idemKeys := j.deleteBatched(ctx, "sys_idempotency_keys", now, j.sweeper.DeleteIdempotencyKeysExpiredBefore)

	previewSessions := j.deleteBatched(ctx, "sys_preview_sessions", runsCutoff, j.sweeper.DeletePreviewSessionsExpiredBefore)

	emailLog := j.deleteBatched(ctx, "sys_email_log", runsCutoff, j.sweeper.DeleteEmailLogBefore)

	usageDaily := j.deleteBatched(ctx, "sys_usage_daily", now.Add(-j.usageRetention), j.sweeper.DeleteUsageDailyBefore)

	j.logger.Info("janitor: retention sweep complete",
		zap.Int64("sys_email_log_deleted", emailLog),
		zap.Int64("sys_usage_daily_deleted", usageDaily),
		zap.Int64("sys_workflow_runs_deleted", runs),
		zap.Int64("sys_workflow_runs_stale_failed", staleRuns),
		zap.Int64("mfa_pending_tokens_deleted", mfa),
		zap.Int64("password_reset_tokens_deleted", resets),
		zap.Int64("sso_handoff_tokens_deleted", handoffs),
		zap.Int64("sys_jobs_deleted", jobs),
		zap.Int64("sys_hook_deliveries_deleted", hookDeliveries),
		zap.Int64("sys_idempotency_keys_deleted", idemKeys),
		zap.Int64("sys_preview_sessions_deleted", previewSessions))
}

// deleteBatched deletes rows older than cutoff in bounded batches, looping until a batch returns
// fewer than batchSize rows (nothing left this cycle). It returns the total deleted. A delete error
// is logged with the table name and stops THIS table's loop (the total so far is still returned) —
// a transient DB error must not crash the janitor. The loop is also bounded by a hard iteration cap
// as a belt-and-braces guard against a sweeper that never shrinks a batch.
func (j *Janitor) deleteBatched(ctx context.Context, table string, cutoff time.Time,
	del func(ctx context.Context, cutoff time.Time, limit int) (int64, error)) int64 {
	var total int64
	// maxIterations caps the loop so a misbehaving sweeper (always returning a full batch) cannot
	// spin forever; at batchSize=1000 this still permits deleting up to 10M rows per table per sweep.
	const maxIterations = 10_000
	for i := 0; i < maxIterations; i++ {
		n, err := del(ctx, cutoff, j.batchSize)
		total += n
		if err != nil {
			j.logger.Warn("janitor: delete batch failed (fail-open)",
				zap.String("table", table), zap.Int64("deleted_before_error", total), zap.Error(err))
			return total
		}
		if n < int64(j.batchSize) {
			break // last (short) batch — done with this table
		}
	}
	return total
}

// Stop signals the ticker goroutine to exit and waits (up to the context deadline) for the in-flight
// sweep to finish. Idempotent and safe from graceful-shutdown handlers. Returns ctx.Err() if the
// deadline passes before the goroutine exits.
func (j *Janitor) Stop(ctx context.Context) error {
	j.stopOnce.Do(func() { close(j.stop) })
	select {
	case <-j.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
