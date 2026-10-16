-- Reverse specs/043 Migration A `bpm_runtime_v2`. ROLL THE BINARY BACK FIRST: a binary of S3a or
-- later writes seq and sys_bpm_tasks on every approval write, so on the pre-A schema new records
-- silently stop entering approval (the arming path is fail-open) and every decision errors.
--
-- What is lost:
--   * sys_bpm_tasks — every work seat. In S3a a seat only mirrors its instance (the instance's
--     history holds the same decisions), but once parallel steps (S4), pool claims (S5) or
--     rework seats (S8) exist, the seats ARE the state and this drop destroys it.
--   * sys_approval_instances.seq / context / last_error / sla_paused_at / sla_remaining_seconds.
--   * sys_approval_processes.graph / options — refused below while a live process uses them,
--     like 000064's outcome guard: dropping them would silently erase process definitions.
--
-- What is deliberately KEPT: the pins the up file wrote for pending pre-000065 instances
-- ("backfilled": true). plan.md first said to reset them to NULL; that would hand those instances
-- back to the live process — a soft-deleted process makes them undecidable again, a mid-flight
-- edit re-wires them (#8) — and throw away the only record of which chain they executed
-- (000065's header). Every binary back to S0 decides through the pin (it ignores the extra key).
--
-- Idempotent: every statement is guarded, so a re-run after a partial failure is safe.
-- NO `DELIMITER` — same proc-as-one-statement pattern as 000064.
DROP PROCEDURE IF EXISTS _bpm_a_down;
CREATE PROCEDURE _bpm_a_down()
BEGIN
    DECLARE n INT DEFAULT 0;
    IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_processes' AND COLUMN_NAME = 'graph') THEN
        SELECT COUNT(*) INTO n FROM sys_approval_processes
            WHERE deleted_at IS NULL AND (graph IS NOT NULL OR options IS NOT NULL);
        IF n > 0 THEN
            SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT =
                'BPM-A ABORT: approval processes store a graph/options definition; migrate them back to steps before down-migrating';
        END IF;
        ALTER TABLE sys_approval_processes DROP COLUMN graph, DROP COLUMN options;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_instances' AND COLUMN_NAME = 'seq') THEN
        ALTER TABLE sys_approval_instances
            DROP COLUMN seq,
            DROP COLUMN context,
            DROP COLUMN last_error,
            DROP COLUMN sla_paused_at,
            DROP COLUMN sla_remaining_seconds;
    END IF;
END;
CALL _bpm_a_down();
DROP PROCEDURE IF EXISTS _bpm_a_down;

DROP TABLE IF EXISTS sys_bpm_tasks;
