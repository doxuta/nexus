-- Reverse 000099: drop the prompt-cache columns of sys_ai_runs. prompt_tokens keeps the cached input
-- it already counted; only the breakdown is lost.
DROP PROCEDURE IF EXISTS _airuns99_revert;
CREATE PROCEDURE _airuns99_revert()
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_ai_runs' AND COLUMN_NAME = 'cache_read_tokens'
    ) THEN
        ALTER TABLE sys_ai_runs DROP COLUMN cache_read_tokens, DROP COLUMN cache_write_tokens;
    END IF;
END;
CALL _airuns99_revert();
DROP PROCEDURE IF EXISTS _airuns99_revert;
