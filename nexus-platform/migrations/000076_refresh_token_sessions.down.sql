-- SESSION-LIST-KILL — revert 000076: drop the session indexes and columns of refresh_tokens.
-- Guarded through information_schema so the down is idempotent. Dropping the columns discards the
-- device labels and the 'ended' markers; the code that reads them is rolled back with the schema,
-- and every refresh token itself stays exactly as usable as before (token_hash / revoked_at /
-- expires_at are untouched).

DROP PROCEDURE IF EXISTS _u76_rt_sessions_revert;
CREATE PROCEDURE _u76_rt_sessions_revert()
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.STATISTICS
               WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'refresh_tokens' AND INDEX_NAME = 'idx_rt_session') THEN
        ALTER TABLE refresh_tokens DROP INDEX idx_rt_session;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.STATISTICS
               WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'refresh_tokens' AND INDEX_NAME = 'idx_rt_user_live') THEN
        ALTER TABLE refresh_tokens DROP INDEX idx_rt_user_live;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
               WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'refresh_tokens' AND COLUMN_NAME = 'revoke_reason') THEN
        ALTER TABLE refresh_tokens DROP COLUMN revoke_reason;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
               WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'refresh_tokens' AND COLUMN_NAME = 'last_used_at') THEN
        ALTER TABLE refresh_tokens DROP COLUMN last_used_at;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
               WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'refresh_tokens' AND COLUMN_NAME = 'ip') THEN
        ALTER TABLE refresh_tokens DROP COLUMN ip;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
               WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'refresh_tokens' AND COLUMN_NAME = 'user_agent') THEN
        ALTER TABLE refresh_tokens DROP COLUMN user_agent;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
               WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'refresh_tokens' AND COLUMN_NAME = 'session_id') THEN
        ALTER TABLE refresh_tokens DROP COLUMN session_id;
    END IF;
END;
CALL _u76_rt_sessions_revert();
DROP PROCEDURE IF EXISTS _u76_rt_sessions_revert;
