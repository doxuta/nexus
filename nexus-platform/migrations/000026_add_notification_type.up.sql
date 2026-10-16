-- EPIC 4 / FE-P0 — add `type` column to sys_notifications so the FE notification inbox
-- can render different icons per notification category (workflow, bpm, system, etc.).
--
-- MySQL 8 (unlike MariaDB) does NOT support `ADD COLUMN IF NOT EXISTS`, so the column add
-- is wrapped in a stored-procedure existence guard (same pattern as 000019_workflow_scheduled).
--
-- Head before this migration is v25 (000025_nn_relation_unique). Reverts cleanly to v25.
-- MySQL 8 / InnoDB / utf8mb4.

DROP PROCEDURE IF EXISTS _notif26_apply;
CREATE PROCEDURE _notif26_apply()
BEGIN
    -- Add `type` column for FE notification categorisation.
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME   = 'sys_notifications'
          AND COLUMN_NAME  = 'type'
    ) THEN
        ALTER TABLE sys_notifications
            ADD COLUMN type VARCHAR(40) NULL DEFAULT NULL AFTER message;
    END IF;

    -- Ensure the inbox hot-path index exists (tenant_id, user_id, is_read, created_at).
    -- This index already exists in 000017 but guard it idempotently in case the DB was
    -- set up via a partial run.
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.STATISTICS
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME   = 'sys_notifications'
          AND INDEX_NAME   = 'idx_notif_inbox'
    ) THEN
        ALTER TABLE sys_notifications
            ADD INDEX idx_notif_inbox (tenant_id, user_id, is_read, created_at);
    END IF;
END;
CALL _notif26_apply();
DROP PROCEDURE IF EXISTS _notif26_apply;
