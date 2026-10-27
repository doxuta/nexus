-- Issue #30 item 5: how an assistant turn ended, so a reopened session says it as the live turn did — a turn
-- the model declined reads "the model declined" instead of an empty answer, a turn that ran out of steps keeps
-- its "not finished" note. The values are sys_ai_runs.outcome's for a turn that is stored: 'ok', 'max_steps',
-- 'refused'. NULL = not recorded: a user message, a row written before this migration, or a row no model
-- wrote (the MCP thread's). Written only by ai.SessionStore's turn write (AppendTurn / AppendAssistantTurn).
--
-- ONE ALTER statement, guarded by information_schema (MySQL 8 has no ADD COLUMN IF NOT EXISTS): DDL
-- auto-commits, so a single statement cannot leave a half-applied (dirty) migration.
DROP PROCEDURE IF EXISTS _aimsg101_apply;
CREATE PROCEDURE _aimsg101_apply()
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_ai_messages' AND COLUMN_NAME = 'outcome'
    ) THEN
        ALTER TABLE sys_ai_messages ADD COLUMN outcome VARCHAR(24) NULL DEFAULT NULL AFTER settled_json;
    END IF;
END;
CALL _aimsg101_apply();
DROP PROCEDURE IF EXISTS _aimsg101_apply;
