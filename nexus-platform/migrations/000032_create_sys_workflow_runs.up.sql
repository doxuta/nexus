-- W6 — Workflow run history (docs/nexus-ux-revolution-plan.md §3.4 "Run history").
--
-- sys_workflow_runs: one row per (event × workflow) execution recorded by the ASYNC engine
-- (workflow.AsyncDispatcher) and one row per claimed scheduled firing (workflow.Scheduler).
-- Recording happens INSIDE the existing async execution path (worker goroutine, after the
-- triggering mutation already committed) so it adds ZERO sync latency to record writes, and it
-- is fail-open: a failed INSERT here is logged and the executor continues (never crashes,
-- never blocks the rule).
--
--   status:  success = condition matched, every action ran clean
--            failed  = an action (or the scheduled scan) errored — `error` carries the first error
--            skipped = condition evaluated false (event rules) / no record matched (scheduled)
--   record_id: the triggering record (NULL for scheduled firings, which span many records)
--   actions_result: per-action outcome JSON [{type, field?, ok, error?}] (NULL when skipped);
--                   for scheduled firings a summary {scanned, matched, failed}
--   trigger_type: after_create | after_update | after_delete | scheduled (VARCHAR not ENUM so a
--                 future trigger needs no ALTER on what is an append-heavy table)
--
-- started_at/finished_at are DATETIME(3) (ms precision) set by the app in UTC — a run is an
-- interval measured by the engine, not a row-write timestamp.
--
-- Indexes: the runs API reads newest-first per workflow (tenant_id, workflow_id, started_at) and
-- newest-first per tenant (tenant_id, started_at) — both are backward index scans, O(limit).
--
-- RETENTION (known debt, alongside TOKEN-JANITOR — eng-buildout-plan.md): no janitor in this
-- slice; the table grows with every mutation on entities that carry rules (including skipped
-- runs). A retention sweep (e.g. delete runs older than N days) is a follow-up slice.
--
-- Tenant-pinned; every query filters tenant_id (rules/database.md). MySQL 8 / InnoDB / utf8mb4.
-- Idempotent (CREATE TABLE IF NOT EXISTS) per rules/database.md.

CREATE TABLE IF NOT EXISTS sys_workflow_runs (
    id             CHAR(36)     NOT NULL PRIMARY KEY,
    tenant_id      CHAR(36)     NOT NULL,
    workflow_id    CHAR(36)     NOT NULL,
    trigger_type   VARCHAR(32)  NOT NULL,
    record_id      CHAR(36)     DEFAULT NULL,              -- NULL for scheduled firings
    status         ENUM('success','failed','skipped') NOT NULL,
    error          TEXT         DEFAULT NULL,               -- first error (failed runs)
    actions_result JSON         DEFAULT NULL,               -- per-action outcome / scheduled summary
    started_at     DATETIME(3)  NOT NULL,
    finished_at    DATETIME(3)  NOT NULL,
    INDEX idx_wfr_tenant_wf_started (tenant_id, workflow_id, started_at),
    INDEX idx_wfr_tenant_started    (tenant_id, started_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
