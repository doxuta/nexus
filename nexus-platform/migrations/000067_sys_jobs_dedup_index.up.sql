-- SYS-JOBS-DEDUP-INDEX (DAILY-QUEUE #83): the TIMER-1 enqueue dedup probe becomes a 1-row seek.
--
-- Every ticket create AND every ticket edit runs the probe twice since #11 made tk_tickets.due_date
-- always set (services.WorkflowTimerScheduler.ScheduleTimer → jobs.SQLStore.HasJobWithPayload):
--   WHERE tenant_id = ? AND job_type = ? AND status IN (queued, running, done) AND payload = CAST(? AS JSON)
-- The only usable index was idx_sys_jobs_tenant (tenant_id), so each probe walked EVERY row of the
-- tenant and compared JSON row by row — done rows included, a set that grows with retention. A
-- miss (the common case: a new ticket, a moved due date) reads the whole set.
--
-- The queue proposed a composite (tenant_id, job_type, status). Measured, it does not help:
-- workflow_timer is the ONLY tenant-pinned job type (email_alert is a tenant-NULL system job) and
-- `done` rows sit inside the probe's own status IN range, so the composite narrows away only the
-- `failed` rows. What discriminates is the payload, and a JSON column cannot be indexed. So:
--
--   * payload_hash — a VIRTUAL generated column, SHA-256 of MySQL's canonical text of the payload.
--     MySQL stores JSON in a normalized binary form (keys sorted, whitespace dropped), so the text
--     is the same for any two value-equal payloads the producer writes. VIRTUAL = no stored bytes
--     and no table rebuild (ADD COLUMN is INSTANT); only the index below materializes it. The
--     explicit CHARACTER SET pins the hashed bytes regardless of the session charset.
--   * idx_sys_jobs_dedup (tenant_id, job_type, payload_hash) — the probe's seek. The probe
--     (jobs.dedupProbeSQL) writes the SAME expression over `payload` rather than naming the
--     column, so the optimizer substitutes payload_hash and seeks here, and the binary still runs
--     (scanning, as before) on a schema without this migration. It keeps
--     `payload = CAST(? AS JSON)` as the exact check, so a hash collision can never dedup a
--     different payload.
--   * idx_sys_jobs_tenant (tenant_id) is dropped: it is the leftmost prefix of idx_sys_jobs_dedup,
--     which serves every per-tenant lookup it served (tenant purge's DELETE ... WHERE tenant_id = ?).
--
-- Measured 2026-09-29 on MySQL 8.0.46, 125,160 sys_jobs rows, 75,004 in one tenant (workflow_timer:
-- 45k done, 5k queued, 5k failed; 20k of another type), EXPLAIN ANALYZE of the probe:
--   before, idx_sys_jobs_tenant, a MISS              Index lookup, 75,004 rows read, 88-116 ms
--   queue's (tenant_id, job_type, status), a MISS    not chosen by the optimizer (it kept
--                                                     idx_sys_jobs_tenant); FORCEd: range scan,
--                                                     50,004 rows read, ~102 ms
--   this (tenant_id, job_type, payload_hash)
--     a MISS                                          Index lookup, 0 rows read, 0.005-0.015 ms
--     a HIT                                           Index lookup, 1 row read, 0.014-0.038 ms
-- The hit case was already cheap-ish before (the scan stops at the first match); the miss case is
-- the one every new ticket pays.
--
-- ponytail: matching on canonical TEXT is narrower than JSON value equality (1 and 1.0 hash apart,
-- although `=` calls them equal). Harmless: the only producer is json.Marshal of a fixed Go struct,
-- which always renders the same value the same way. A MySQL upgrade that changed how CAST(json AS
-- CHAR) prints a value would make parked pre-upgrade rows miss the probe (one double-park each);
-- strings and integers — all the timer payload holds — print the same in every 8.x.
ALTER TABLE sys_jobs
    ADD COLUMN payload_hash BINARY(32)
        GENERATED ALWAYS AS (UNHEX(SHA2(CAST(payload AS CHAR CHARACTER SET utf8mb4), 256))) VIRTUAL;

-- A separate statement: adding the column and its index together cannot run LOCK=NONE; apart,
-- the index build is in place and leaves the queue writable.
ALTER TABLE sys_jobs
    ADD KEY idx_sys_jobs_dedup (tenant_id, job_type, payload_hash),
    DROP KEY idx_sys_jobs_tenant;
