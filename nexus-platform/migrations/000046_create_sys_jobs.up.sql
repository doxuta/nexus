-- sys_jobs: the durable job queue (JOBS-1, engine-depth primitive #0 — legacy-upgrade-notes #4).
-- One row per unit of background work. Replaces fire-and-forget goroutines for work that must
-- survive a crash: a worker CLAIMS a queued row atomically (SELECT ... FOR UPDATE SKIP LOCKED,
-- MySQL 8), runs the registered handler for job_type, and finishes the row as done/failed or
-- reschedules it (status back to 'queued', run_at pushed by capped exponential backoff). A REAPER
-- requeues 'running' rows whose claimed_at is older than the worker lease (crashed worker), so no
-- job is ever lost in a wedged 'running' state. This table is the substrate the timer/deadline
-- engine (parked row + wake-up) and campaign send will stand on.
--
--   * tenant_id NULL = system job (e.g. transport-level email delivery); tenant jobs pin it.
--   * status ∈ queued | running | done | failed. Claimable = queued AND run_at <= now.
--   * run_at TIMESTAMP(3): scheduled execution time — supports delayed jobs and retry backoff.
--     ponytail: TIMESTAMP tops out at 2038; if the timer engine ever parks decade-scale
--     deadlines here, migrate run_at to DATETIME(3) then.
--   * attempts is incremented AT CLAIM TIME (not on failure) so a job that hard-crashes the
--     process still burns its budget when the reaper requeues it — no infinite crash loop.
--   * finished_at is set ONLY on done/failed → the retention janitor deletes on finished_at
--     alone (rows still queued/running have NULL and are never swept).
--
-- Indexes: (status, run_at) serves the claim scan (ORDER BY run_at, id rides the implicit PK
-- suffix of the secondary index — no filesort); (status, claimed_at) serves the reaper;
-- (tenant_id) serves per-tenant ops lookups; (finished_at) serves the janitor's bounded DELETE.
-- MySQL 8 / InnoDB / utf8mb4. Idempotent.
CREATE TABLE IF NOT EXISTS sys_jobs (
    id           CHAR(36)     NOT NULL,
    tenant_id    CHAR(36)     DEFAULT NULL,          -- NULL = system job
    job_type     VARCHAR(64)  NOT NULL,              -- handler registry key
    payload      JSON         DEFAULT NULL,          -- handler-defined input
    status       VARCHAR(16)  NOT NULL DEFAULT 'queued',
    run_at       TIMESTAMP(3) NOT NULL,              -- claimable when <= now (delay/backoff)
    attempts     INT          NOT NULL DEFAULT 0,    -- incremented at claim
    max_attempts INT          NOT NULL DEFAULT 5,
    last_error   TEXT         NULL,                  -- last failure (kept even if a retry succeeds)
    claimed_at   TIMESTAMP(3) NULL DEFAULT NULL,     -- lease start; reaper requeues stale claims
    claimed_by   VARCHAR(64)  DEFAULT NULL,          -- worker id (host-pid-wN)
    finished_at  TIMESTAMP(3) NULL DEFAULT NULL,     -- set on done/failed only
    created_at   TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at   TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_sys_jobs_claim (status, run_at),
    KEY idx_sys_jobs_reaper (status, claimed_at),
    KEY idx_sys_jobs_tenant (tenant_id),
    KEY idx_sys_jobs_finished (finished_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
