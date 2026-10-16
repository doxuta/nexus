-- Reverse of 000059: drop the feature discriminator. Sessions collapse back into one undivided
-- pool (the pre-F9-5 behavior). Idempotent via the same stored-procedure existence guard.
DROP PROCEDURE IF EXISTS _d59_ai_session_feature_revert;
CREATE PROCEDURE _d59_ai_session_feature_revert()
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME   = 'sys_ai_sessions'
          AND COLUMN_NAME  = 'feature'
    ) THEN
        ALTER TABLE sys_ai_sessions DROP COLUMN feature;
    END IF;
END;
CALL _d59_ai_session_feature_revert();
DROP PROCEDURE IF EXISTS _d59_ai_session_feature_revert;
