-- EPIC 4 M2 — Workflow scheduler + email_alert/outbound_webhook actions (spec:
-- docs/ENGINE-COVERAGE-SPEC.md §EPIC 4; benchmark Salesforce time-based-workflow + outbound-message
-- + SugarCRM scheduler). This migration extends sys_workflows (created in 000016) so a rule can
-- fire on a SCHEDULE (a background ticker scans due rules, evaluates the condition against the
-- entity's records, and runs the actions) — the M1 after_create/after_update/after_delete events
-- were all record-mutation triggered; 'scheduled' is the first time-based trigger.
--
-- Head before this migration is v17 (000017_create_sys_notifications). Reverts cleanly to v17.
-- MySQL 8 / InnoDB / utf8mb4.
--
-- Three changes, all idempotent so a re-run never errors:
--   1. trigger_event ENUM gains 'scheduled'. ENUM grows by appending the new value at the END so
--      the integer ordinals of the existing values (after_create=1 … after_delete=3) are unchanged
--      — appending is the only safe ENUM mutation (re-ordering would silently re-map stored rows).
--      Re-applying the same MODIFY is a no-op so this statement is naturally idempotent.
--   2. interval_seconds: per-rule cadence. A scheduled rule fires at most once per interval_seconds
--      (the scheduler claims a run via an atomic compare-and-swap on last_run_at, see below). 0 =
--      "every tick the scheduler runs" (gated only by last_run_at being strictly in the past). It
--      is NULL/ignored for the three event triggers. UNSIGNED so a negative cadence is impossible.
--   3. last_run_at: the idempotency clock. The scheduler claims a due rule with
--        UPDATE ... SET last_run_at = now WHERE id = ? AND (last_run_at IS NULL OR last_run_at <= ?)
--      — an atomic CAS, so two scheduler ticks (or two server instances) can never both fire the
--      same rule in the same window (no double-fire). NULL = never run (fires on the first due tick).
--
-- An index on (tenant_id, trigger_event, is_active, last_run_at) makes the scheduler's "scheduled,
-- active, non-deleted, due" scan an index range scan instead of a full table sweep.
--
-- IDEMPOTENCY NOTE: MySQL 8 (unlike MariaDB) does NOT support `ADD COLUMN IF NOT EXISTS` / `ADD
-- INDEX IF NOT EXISTS`. To keep a re-run (e.g. after a partial apply) from erroring on an already-
-- present column/index, every ADD is wrapped in an information_schema existence guard inside a
-- stored procedure — the same pattern the rest of the migration suite uses.

ALTER TABLE sys_workflows
    MODIFY COLUMN trigger_event
        ENUM('after_create','after_update','after_delete','scheduled') NOT NULL;

DROP PROCEDURE IF EXISTS _wf19_apply;
CREATE PROCEDURE _wf19_apply()
BEGIN
    -- interval_seconds: per-rule cadence (defaults 0 = fire whenever due).
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE table_schema = DATABASE()
          AND table_name = 'sys_workflows'
          AND column_name = 'interval_seconds'
    ) THEN
        ALTER TABLE sys_workflows
            ADD COLUMN interval_seconds INT UNSIGNED NOT NULL DEFAULT 0
                AFTER is_active;
    END IF;

    -- last_run_at: the CAS idempotency clock (NULL = never run).
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE table_schema = DATABASE()
          AND table_name = 'sys_workflows'
          AND column_name = 'last_run_at'
    ) THEN
        ALTER TABLE sys_workflows
            ADD COLUMN last_run_at TIMESTAMP(3) NULL DEFAULT NULL
                AFTER interval_seconds;
    END IF;

    -- Scheduler hot-path index: "active, non-deleted, scheduled rules of a tenant that are due".
    -- last_run_at is the trailing column so the range predicate (last_run_at <= now) is covered.
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.STATISTICS
        WHERE table_schema = DATABASE()
          AND table_name = 'sys_workflows'
          AND index_name = 'idx_wf_scheduled'
    ) THEN
        ALTER TABLE sys_workflows
            ADD INDEX idx_wf_scheduled (tenant_id, trigger_event, is_active, last_run_at);
    END IF;
END;
CALL _wf19_apply();
DROP PROCEDURE _wf19_apply;
