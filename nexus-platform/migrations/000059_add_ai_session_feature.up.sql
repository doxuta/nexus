-- EASE audit F9-5 — sys_ai_sessions.feature: which AI surface owns the thread.
--
-- Both the admin AI Builder (/admin/ai/builder/*) and the every-user ⌘K assistant
-- (/assistant/ask) persist their conversations in the ONE sys_ai_sessions store. Without a
-- feature discriminator an admin's ⌘K ask-data threads leaked into the Builder session rail
-- (and there was no way to list/resume assistant threads at all). This column splits the two
-- surfaces: 'builder' (default) | 'assistant'. Every session read/write now pins
-- (tenant_id, user_id, feature) — a thread belongs to exactly one surface.
--
-- Backfill IS the default: every pre-existing row (including old ⌘K threads, accepted) becomes
-- 'builder', so nothing changes for current Builder users. No new index: the existing
-- idx_ai_sessions_tenant_user (tenant_id, user_id) already narrows to one user's handful of
-- sessions; feature is a residual filter over those few rows — a third index would be dead
-- weight on every insert.
--
-- MySQL 8 has no ADD COLUMN IF NOT EXISTS → stored-procedure existence guard, idempotent per
-- rules/database.md (same pattern as 000043_add_user_lockout / 000052_add_user_platform_operator).
-- Head before this migration is v58 (000058_add_hook_payment_match). MySQL 8 / InnoDB / utf8mb4.

DROP PROCEDURE IF EXISTS _u59_ai_session_feature_apply;
CREATE PROCEDURE _u59_ai_session_feature_apply()
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME   = 'sys_ai_sessions'
          AND COLUMN_NAME  = 'feature'
    ) THEN
        ALTER TABLE sys_ai_sessions
            ADD COLUMN feature VARCHAR(20) NOT NULL DEFAULT 'builder';
    END IF;
END;
CALL _u59_ai_session_feature_apply();
DROP PROCEDURE IF EXISTS _u59_ai_session_feature_apply;
