package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// maxErrorLen bounds what we persist into sys_jobs.last_error so a pathological error string
// (e.g. a dumped payload) can never bloat the TEXT column.
const maxErrorLen = 2000

// maxWorkerIDLen mirrors the sys_jobs.claimed_by VARCHAR(64) column. claimed_by IS the claim
// IDENTITY (the lost-lease guard is `WHERE claimed_by = ?`), so this truncation is only a
// last-resort belt-and-suspenders: the Manager already clamps the hostname so the full worker id
// stays ≤64 with its distinguishing "-<pid>-w<N>" suffix intact (truncating that suffix away would
// collapse the guard — see NewManager). A worker id that still overflows here is a bug upstream.
const maxWorkerIDLen = 64

// SQLStore is the sqlx-backed persistence of the queue. Every statement is parameterized; there
// is deliberately NO tenant filter on the queue operations themselves — the queue is a cross-
// tenant SYSTEM table (same intentional exception as the janitor / workflow scheduler): a job's
// tenant pinning lives in its row (tenant_id) and in what its handler does with the payload.
type SQLStore struct {
	db                 *sqlx.DB
	defaultMaxAttempts int
	// scrubTypes is the comma-joined set of job types whose payload is dropped on a terminal
	// transition (ScrubPayloadOnFinish). Empty = every payload is kept until the janitor sweeps.
	scrubTypes string
}

// NewSQLStore wires the store over the shared sqlx pool. defaultMaxAttempts (JOBS_MAX_ATTEMPTS)
// applies to jobs enqueued without an explicit MaxAttempts; non-positive values clamp to 5.
func NewSQLStore(db *sqlx.DB, defaultMaxAttempts int) *SQLStore {
	if defaultMaxAttempts < 1 {
		defaultMaxAttempts = 5
	}
	return &SQLStore{db: db, defaultMaxAttempts: defaultMaxAttempts}
}

// ScrubPayloadOnFinish makes the payload of the given job types NULL the moment a job of that
// type reaches a terminal state (done, failed, reaper-failed) — for payloads that carry personal
// data a delivery needs and nothing needs afterwards. Why (queue #95): an email_alert job carries
// the RENDERED recipient, possibly a field the admin flagged sensitive, and a finished row used to
// keep it for JOBS_RETENTION_DAYS in a table with no field-level ACL. The row itself stays
// (job_type, status, attempts, masked last_error) so the delivery history is not lost.
//
// A retry (Reschedule, the reaper's requeue) keeps the payload: the next attempt must still send.
// Opt-in per type, never global — workflow_timer's dedup probe (HasJobWithPayload) matches the
// payload of DONE rows, so scrubbing it would let an unrelated edit re-fire a timer. The scrub
// rides the same guarded UPDATE as the transition, so a lost claim scrubs nothing.
//
// Job types travel as one comma-joined bound parameter (FIND_IN_SET), hence the comma refusal.
// Call at boot, before the manager starts (not safe to change while workers run).
func (s *SQLStore) ScrubPayloadOnFinish(jobTypes ...string) error {
	for _, t := range jobTypes {
		if t == "" || strings.Contains(t, ",") {
			return fmt.Errorf("jobs.SQLStore.ScrubPayloadOnFinish: invalid job type %q", t)
		}
	}
	s.scrubTypes = strings.Join(jobTypes, ",")
	return nil
}

// Ensure the concrete store satisfies the manager's consumer interface at compile time.
var _ Store = (*SQLStore)(nil)

// Enqueue inserts one queued job and returns its id. Zero RunAt means "runnable now"; empty
// TenantID persists as NULL (system job); MaxAttempts <= 0 takes the store default.
func (s *SQLStore) Enqueue(ctx context.Context, j Job) (string, error) {
	return s.EnqueueExec(ctx, s.db, j)
}

// EnqueueExec is Enqueue on a caller-supplied executor — a pool or an open transaction. A producer
// that must not lose the wake-up (specs/043 S3c: the SLA breach job) inserts it in the SAME
// transaction as the state write that makes it due, so the two commit or roll back together.
func (s *SQLStore) EnqueueExec(ctx context.Context, ex sqlx.ExecerContext, j Job) (string, error) {
	if j.Type == "" {
		return "", fmt.Errorf("jobs.SQLStore.Enqueue: empty job type")
	}
	if len(j.Type) > 64 {
		return "", fmt.Errorf("jobs.SQLStore.Enqueue: job type %q exceeds 64 chars", j.Type)
	}
	id := uuid.NewString()
	runAt := j.RunAt
	if runAt.IsZero() {
		runAt = time.Now()
	}
	maxAttempts := j.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = s.defaultMaxAttempts
	}
	var tenant sql.NullString
	if j.TenantID != "" {
		tenant = sql.NullString{String: j.TenantID, Valid: true}
	}
	var payload any // nil → SQL NULL; []byte → JSON column
	if len(j.Payload) > 0 {
		payload = []byte(j.Payload)
	}
	_, err := ex.ExecContext(ctx, `
		INSERT INTO sys_jobs (id, tenant_id, job_type, payload, status, run_at, attempts, max_attempts)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?)`,
		id, tenant, j.Type, payload, StatusQueued, runAt.UTC(), maxAttempts)
	if err != nil {
		return "", fmt.Errorf("jobs.SQLStore.Enqueue: %w", err)
	}
	return id, nil
}

// dedupProbeSQL is HasJobWithPayload's statement. It SEEKS idx_sys_jobs_dedup (tenant_id, job_type,
// payload_hash) from migration 000067 (DAILY-QUEUE #83); before it, every probe walked all of the
// tenant's rows comparing JSON, and a miss (every new ticket) read the whole set.
//
// The hash predicate is written over `payload`, NOT over the payload_hash column: its left side is
// byte-identical to the generated column's definition, so MySQL substitutes the indexed column
// and seeks, while the statement never names a column that exists only from 000067 on. The binary
// therefore stays correct on a schema without the migration (a deploy whose migrate step failed
// and rolled the code back, a dev DB that lags main) — it just scans there, as it did before. A
// drift between this expression and the migration costs the seek, never the answer
// (TestJobsDedupProbeUsesHashIndex_Integration pins the seek, TestJobsHasJobWithPayload_Integration
// pins the answers). `payload = CAST(? AS JSON)` stays as the exact check, so a hash collision can
// never dedup a different payload. The payload is therefore bound twice: 3rd and last placeholder.
const dedupProbeSQL = `
	SELECT EXISTS(
		SELECT 1 FROM sys_jobs
		WHERE tenant_id = ? AND job_type = ?
		  AND UNHEX(SHA2(CAST(payload AS CHAR CHARACTER SET utf8mb4), 256))
		    = UNHEX(SHA2(CAST(CAST(? AS JSON) AS CHAR CHARACTER SET utf8mb4), 256))
		  AND status IN (?, ?, ?) AND payload = CAST(? AS JSON)
	)`

// HasJobWithPayload reports whether a non-terminated-as-retryable job of jobType in the tenant
// already carries exactly this payload — the TIMER-1 enqueue dedup probe. It matches status IN
// (queued, running, DONE) deliberately: a `done` row with the SAME payload means "this exact
// wake-up ALREADY FIRED", so an unrelated later update to the record (which re-emits after_update
// with an unchanged date → identical payload) must NOT re-park it and re-run the action (the HIGH
// bug: an SLA "escalate 2h after created_at" would re-escalate on every subsequent edit). `failed`
// is intentionally EXCLUDED so a job that exhausted its retry budget CAN be re-parked by a later
// mutation. Key order and whitespace are irrelevant: MySQL normalizes JSON on both sides before
// hashing/comparing. tenantID is required (tenant-pinned by contract).
//
// ponytail: the match is on MySQL's canonical TEXT of the JSON (that is what the hash covers),
// which is narrower than JSON value equality — 1 and 1.0 hash apart although `=` calls them equal.
// Harmless while the only producer is json.Marshal of a fixed struct (WorkflowTimerScheduler); a
// producer that renders one value two ways must normalize before it probes.
//
// Residual (documented, api-conventions §18.2): the `done` row is swept by the retention janitor
// after JOBS_RETENTION_DAYS (default 30), so a record edited MORE than that long after its timer
// fired would re-park and re-fire once. Bounded and rare.
func (s *SQLStore) HasJobWithPayload(ctx context.Context, jobType, tenantID string, payload []byte) (bool, error) {
	if jobType == "" || tenantID == "" {
		return false, fmt.Errorf("jobs.SQLStore.HasJobWithPayload: jobType and tenantID are required")
	}
	if len(payload) == 0 {
		return false, fmt.Errorf("jobs.SQLStore.HasJobWithPayload: empty payload")
	}
	var exists bool
	err := s.db.GetContext(ctx, &exists, dedupProbeSQL,
		tenantID, jobType, string(payload), StatusQueued, StatusRunning, StatusDone, string(payload))
	if err != nil {
		return false, fmt.Errorf("jobs.SQLStore.HasJobWithPayload: %w", err)
	}
	return exists, nil
}

// pendingForWorkflowSQL is HasPendingForWorkflow's statement. (status IN, job_type =) is a
// two-range seek of idx_sys_jobs_lane (000074), so it reads the type's PENDING rows only — never
// the done history kept for JOBS_RETENTION_DAYS (TestJobsPendingForWorkflowProbeSeeksLane_Integration).
// The optimizer can still prefer idx_sys_jobs_dedup (tenant_id, job_type, …) — an equality lookup
// that reads the tenant's WHOLE webhook history, done rows included (seen ~1 in 30 runs of that
// test on MySQL 8.0.46 after ANALYZE) — so the hint rules it out. It names the 000067 index, not
// the lane one, so a schema without 000074 still runs (on idx_sys_jobs_claim's status range).
// Under a heavy machine load it also picked idx_sys_jobs_tenant_list (000075: tenant_id,
// created_at …) — a tenant-wide scan of the same history — so that one is ruled out too (queue
// #329). Both go through the NO_INDEX optimizer hint, not IGNORE INDEX: an unknown name in the
// hint is a warning (3128) while in the index-hint syntax it is ERROR 1176, so code pushed before
// its migration still runs (TestJobsPendingProbeSurvivesMissingIndexes_Integration).
// ->> takes a literal path; workflowId is the top-level key every workflow-produced payload uses.
const pendingForWorkflowSQL = `
	SELECT EXISTS(
		SELECT /*+ NO_INDEX(sys_jobs idx_sys_jobs_dedup, idx_sys_jobs_tenant_list) */ 1 FROM sys_jobs
		WHERE status IN (?, ?) AND job_type = ? AND tenant_id = ?
		  AND payload->>'$.workflowId' = ?
	)`

// HasPendingForWorkflow reports whether a job of jobType parked by one workflow rule of the tenant
// is still queued or running — the scheduled-webhook back-pressure probe (review of #26: a
// scheduled rule is held while the deliveries of its previous firing are pending, so the queue
// cannot grow faster than it drains). done and failed rows do not count: the delivery is over.
// tenantID is required (tenant-pinned by contract).
func (s *SQLStore) HasPendingForWorkflow(ctx context.Context, jobType, tenantID, workflowID string) (bool, error) {
	if jobType == "" || tenantID == "" || workflowID == "" {
		return false, fmt.Errorf("jobs.SQLStore.HasPendingForWorkflow: jobType, tenantID and workflowID are required")
	}
	var pending bool
	if err := s.db.GetContext(ctx, &pending, pendingForWorkflowSQL,
		StatusQueued, StatusRunning, jobType, tenantID, workflowID); err != nil {
		return false, fmt.Errorf("jobs.SQLStore.HasPendingForWorkflow: %w", err)
	}
	return pending, nil
}

// failedBySlotSQL is FailedBySlot's statement: the same lane seek as pendingForWorkflowSQL, on the
// failed range. payload IS NOT NULL mirrors RequeueFinished — a row without it cannot be resent.
const failedBySlotSQL = `
	SELECT /*+ NO_INDEX(sys_jobs idx_sys_jobs_dedup, idx_sys_jobs_tenant_list) */
	       CAST(payload->>'$.actionIndex' AS SIGNED) AS slot, COUNT(*) AS n
	FROM sys_jobs
	WHERE status = ? AND job_type = ? AND tenant_id = ? AND payload IS NOT NULL
	  AND payload->>'$.workflowId' = ?
	  AND COALESCE(payload->>'$.credentialId', '') = ''
	GROUP BY slot`

// FailedBySlot counts, per action slot, the failed jobs of jobType parked by one workflow rule of the
// tenant that name no vault key and still hold their payload — the deliveries "Gửi lại"
// (RequeueFinished) could still send. It reads the slot-keyed payload of outbound_webhook
// ({workflowId, actionIndex, credentialId?}); Doctor C10 (specs/050 S3b) warns with it, because once
// a slot names a key the job handler fails such a job for good. tenantID is required.
func (s *SQLStore) FailedBySlot(ctx context.Context, jobType, tenantID, workflowID string) (map[int]int, error) {
	if jobType == "" || tenantID == "" || workflowID == "" {
		return nil, fmt.Errorf("jobs.SQLStore.FailedBySlot: jobType, tenantID and workflowID are required")
	}
	var rows []struct {
		Slot int `db:"slot"`
		N    int `db:"n"`
	}
	if err := s.db.SelectContext(ctx, &rows, failedBySlotSQL, StatusFailed, jobType, tenantID, workflowID); err != nil {
		return nil, fmt.Errorf("jobs.SQLStore.FailedBySlot: %w", err)
	}
	out := make(map[int]int, len(rows))
	for _, r := range rows {
		out[r.Slot] = r.N
	}
	return out, nil
}

// jobRow is the scan shape of a claimable row.
type jobRow struct {
	ID          string         `db:"id"`
	TenantID    sql.NullString `db:"tenant_id"`
	Type        string         `db:"job_type"`
	Payload     []byte         `db:"payload"`
	Attempts    int            `db:"attempts"`
	MaxAttempts int            `db:"max_attempts"`
}

// ClaimNext atomically claims the oldest due queued job the lane may take for workerID and returns
// it, or (nil,nil) when nothing is due. The claim is a short transaction:
//
//	SELECT ... WHERE status='queued' AND run_at <= now [AND lane predicate] ORDER BY run_at, id
//	  LIMIT 1 FOR UPDATE SKIP LOCKED     -- MySQL 8: rows locked by a concurrent claim are SKIPPED
//	UPDATE ... SET status='running', claimed_at, claimed_by, attempts=attempts+1
//
// FOR UPDATE SKIP LOCKED guarantees N concurrent workers never receive the same row and never
// block on each other's claim. attempts is incremented HERE (claim time), so a job that crashes
// the process still burns budget when reaped and reclaimed. The ORDER BY tie-break (id) rides the
// implicit PK suffix of the secondary index — no filesort (B1 discipline; see claimSelect).
func (s *SQLStore) ClaimNext(ctx context.Context, workerID string, now time.Time, lane Lane) (*Job, error) {
	if len(workerID) > maxWorkerIDLen {
		workerID = workerID[:maxWorkerIDLen]
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("jobs.SQLStore.ClaimNext: begin: %w", err)
	}
	// Rollback is a harmless no-op after a successful Commit.
	defer func() { _ = tx.Rollback() }()

	var row jobRow
	query, args := claimSelect(lane, now)
	err = tx.GetContext(ctx, &row, query, args...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // nothing due (or everything due is locked by a concurrent claim)
	}
	if err != nil {
		return nil, fmt.Errorf("jobs.SQLStore.ClaimNext: select: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE sys_jobs
		SET status = ?, claimed_at = ?, claimed_by = ?, attempts = attempts + 1
		WHERE id = ?`,
		StatusRunning, now.UTC(), workerID, row.ID); err != nil {
		return nil, fmt.Errorf("jobs.SQLStore.ClaimNext: update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("jobs.SQLStore.ClaimNext: commit: %w", err)
	}

	return &Job{
		ID:          row.ID,
		TenantID:    row.TenantID.String,
		Type:        row.Type,
		Payload:     row.Payload,
		Status:      StatusRunning,
		Attempts:    row.Attempts + 1, // reflects the claim we just committed
		MaxAttempts: row.MaxAttempts,
		ClaimedBy:   workerID,
	}, nil
}

// claimSelect builds ClaimNext's row-locking SELECT for one lane. Only placeholders are added to
// the text; every value — the job types included — is a bound argument.
//
//	dedicated (Only)  … AND job_type = ?         seeks idx_sys_jobs_lane (status, job_type, run_at)
//	shared (Exclude)  … AND job_type NOT IN (…)  rides idx_sys_jobs_claim (status, run_at)
//	zero Lane         no type predicate           rides idx_sys_jobs_claim, as before lanes
//
// Both stop at the first unlocked row they may take: the index order IS the ORDER BY, so LIMIT 1
// never sorts (TestJobsLaneClaimsSeek_Integration pins both plans). The non-dedicated claims FORCE
// idx_sys_jobs_claim: NOT IN is a range on idx_sys_jobs_lane too, and a plan that took it would
// sort — reading and row-locking every queued row of the shared types, parked timers included —
// before LIMIT 1. The hint names an index that exists since 000046; the dedicated claim names none,
// so it still runs (scanning) on a schema without 000074. The shared lane still steps
// over — and, under REPEATABLE READ, briefly row-locks — due rows of an excluded type that sit
// ahead of its first own row; a racing dedicated claim SKIPs those for the few ms of this tx.
// ponytail: bounded by the dedicated lane's DUE backlog (a backing-off row is not due); the
// upgrade, if that backlog ever grows large, is one claim per shared type over idx_sys_jobs_lane.
func claimSelect(lane Lane, now time.Time) (string, []any) {
	hint := " FORCE INDEX (idx_sys_jobs_claim)"
	where := "status = ? AND run_at <= ?"
	args := []any{StatusQueued, now.UTC()}
	switch {
	case lane.Only != "":
		hint = ""
		where += " AND job_type = ?"
		args = append(args, lane.Only)
	case len(lane.Exclude) > 0:
		where += " AND job_type NOT IN (?" + strings.Repeat(", ?", len(lane.Exclude)-1) + ")"
		for _, t := range lane.Exclude {
			args = append(args, t)
		}
	}
	return `
		SELECT id, tenant_id, job_type, payload, attempts, max_attempts
		FROM sys_jobs` + hint + `
		WHERE ` + where + `
		ORDER BY run_at, id
		LIMIT 1
		FOR UPDATE SKIP LOCKED`, args
}

// finish applies a terminal/requeue transition GUARDED by the caller's claim: the UPDATE matches
// only `status='running' AND claimed_by=workerID`, so a worker whose lease expired (job reaped
// and possibly reclaimed by someone else) cannot stomp the new owner's state. It reports whether
// the transition applied.
func (s *SQLStore) finish(ctx context.Context, query, id, workerID string, args ...any) (bool, error) {
	if len(workerID) > maxWorkerIDLen {
		workerID = workerID[:maxWorkerIDLen]
	}
	res, err := s.db.ExecContext(ctx, query, append(args, id, StatusRunning, workerID)...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n > 0, nil
}

// MarkDone finishes a claimed job as done. It returns applied=false (no error) when the claim was
// lost to the reaper meanwhile — the re-execution owns the row now. last_error is deliberately
// left as-is: on a job that succeeded after retries it documents the last failure (attempts > 1
// tells the story). The payload of a ScrubPayloadOnFinish type is dropped in the same UPDATE.
func (s *SQLStore) MarkDone(ctx context.Context, id, workerID string, finishedAt time.Time) (bool, error) {
	applied, err := s.finish(ctx, `
		UPDATE sys_jobs SET status = ?, finished_at = ?,
			payload = IF(FIND_IN_SET(job_type, ?) > 0, NULL, payload)
		WHERE id = ? AND status = ? AND claimed_by = ?`,
		id, workerID, StatusDone, finishedAt.UTC(), s.scrubTypes)
	if err != nil {
		return false, fmt.Errorf("jobs.SQLStore.MarkDone: %w", err)
	}
	return applied, nil
}

// Reschedule puts a failed-attempt job back in the queue for runAt (now + backoff), recording the
// error. Claim ownership is released (claimed_at/claimed_by cleared). Same lost-claim contract as
// MarkDone.
func (s *SQLStore) Reschedule(ctx context.Context, id, workerID string, runAt time.Time, lastError string) (bool, error) {
	applied, err := s.finish(ctx, `
		UPDATE sys_jobs SET status = ?, run_at = ?, last_error = ?, claimed_at = NULL, claimed_by = NULL
		WHERE id = ? AND status = ? AND claimed_by = ?`,
		id, workerID, StatusQueued, runAt.UTC(), clampError(lastError))
	if err != nil {
		return false, fmt.Errorf("jobs.SQLStore.Reschedule: %w", err)
	}
	return applied, nil
}

// Postpone requeues a claimed job for runAt and GIVES BACK the attempt ClaimNext just added
// (attempts = GREATEST(attempts-1, 0)): a tenant under maintenance is not a failure, so it must not
// burn the retry budget (specs/068 D-14 — Reschedule keeps the attempt, which would drain it).
// last_error is left as it was. Same lost-claim contract as Reschedule.
func (s *SQLStore) Postpone(ctx context.Context, id, workerID string, runAt time.Time) (bool, error) {
	applied, err := s.finish(ctx, `
		UPDATE sys_jobs SET status = ?, run_at = ?, attempts = GREATEST(attempts - 1, 0),
			claimed_at = NULL, claimed_by = NULL
		WHERE id = ? AND status = ? AND claimed_by = ?`,
		id, workerID, StatusQueued, runAt.UTC())
	if err != nil {
		return false, fmt.Errorf("jobs.SQLStore.Postpone: %w", err)
	}
	return applied, nil
}

// MarkFailed finishes a claimed job as terminally failed (budget exhausted or unregistered type),
// recording the error. Same lost-claim contract and payload scrub as MarkDone.
func (s *SQLStore) MarkFailed(ctx context.Context, id, workerID, lastError string, finishedAt time.Time) (bool, error) {
	applied, err := s.finish(ctx, `
		UPDATE sys_jobs SET status = ?, finished_at = ?, last_error = ?,
			payload = IF(FIND_IN_SET(job_type, ?) > 0, NULL, payload)
		WHERE id = ? AND status = ? AND claimed_by = ?`,
		id, workerID, StatusFailed, finishedAt.UTC(), clampError(lastError), s.scrubTypes)
	if err != nil {
		return false, fmt.Errorf("jobs.SQLStore.MarkFailed: %w", err)
	}
	return applied, nil
}

// reapedError is what a lease-expired job carries in last_error after the reaper touches it.
const reapedError = "jobs: worker lease expired (worker crashed or job outlived JOBS_LEASE_SEC)"

// ReapStuck recovers jobs stuck in 'running' with claimed_at older than cutoff (now - lease): a
// crashed worker never finished them. Jobs with attempt budget left go back to 'queued' (runnable
// immediately — their run_at is already past); jobs at/over budget are terminally failed. It
// returns the per-outcome counts. The fail pass runs FIRST so an over-budget row can never slip
// into the requeue pass.
func (s *SQLStore) ReapStuck(ctx context.Context, cutoff time.Time) (requeued, failed int64, err error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE sys_jobs SET status = ?, finished_at = ?, last_error = ?,
			payload = IF(FIND_IN_SET(job_type, ?) > 0, NULL, payload)
		WHERE status = ? AND claimed_at < ? AND attempts >= max_attempts`,
		StatusFailed, time.Now().UTC(), reapedError, s.scrubTypes, StatusRunning, cutoff.UTC())
	if err != nil {
		return 0, 0, fmt.Errorf("jobs.SQLStore.ReapStuck: fail pass: %w", err)
	}
	if failed, err = res.RowsAffected(); err != nil {
		return 0, 0, fmt.Errorf("jobs.SQLStore.ReapStuck: fail pass rows: %w", err)
	}

	res, err = s.db.ExecContext(ctx, `
		UPDATE sys_jobs SET status = ?, claimed_at = NULL, claimed_by = NULL, last_error = ?
		WHERE status = ? AND claimed_at < ?`,
		StatusQueued, reapedError, StatusRunning, cutoff.UTC())
	if err != nil {
		return 0, failed, fmt.Errorf("jobs.SQLStore.ReapStuck: requeue pass: %w", err)
	}
	if requeued, err = res.RowsAffected(); err != nil {
		return 0, failed, fmt.Errorf("jobs.SQLStore.ReapStuck: requeue pass rows: %w", err)
	}
	return requeued, failed, nil
}

// clampError bounds an error string to maxErrorLen bytes for the last_error column.
func clampError(s string) string {
	if len(s) > maxErrorLen {
		return s[:maxErrorLen]
	}
	return s
}
