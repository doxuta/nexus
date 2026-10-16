-- SESSION-LIST-KILL (DAILY-QUEUE #44/#45, ../specs/055-session-list-kill/spec.md): login sessions.
--
-- refresh_tokens (mig 000003) knew only id/user_id/tenant_id/token_hash/expires_at/revoked_at/
-- created_at, so the engine could not say which device a login came from, and /profile could only
-- offer "log out everywhere". This adds:
--
--   session_id    CHAR(36)     — the device chain: minted at login (password, MFA step 2, SSO),
--                                copied onto every rotated successor by /auth/refresh, carried in
--                                the access token as the `sid` claim. Rotation revokes before it
--                                re-issues, so a chain has exactly ONE live row at any moment.
--   user_agent    VARCHAR(255) — the login request's User-Agent (cut at 255 bytes of valid UTF-8).
--   ip            VARCHAR(64)  — the client IP ALREADY MASKED to /24 (IPv4) or /48 (IPv6); the
--                                full address never lands here (it lives in the access log).
--   last_used_at  TIMESTAMP    — the login / latest refresh of the chain.
--   revoke_reason VARCHAR(16)  — NULL = an ordinary revoke (rotation, logout, W1, password change);
--                                'ended' = the SESSION was ended (DELETE /auth/sessions/:id, admin
--                                end-session, logout-all): its live access tokens are refused at
--                                once and the E5 reuse alarm ignores a replay of that token.
--
-- Indexes:
--   idx_rt_user_live (tenant_id, user_id, revoked_at)     — list / end a user's sessions touching
--                                                          only the live rows, however long the
--                                                          rotated-away history grows (a refresh
--                                                          every access TTL, rows never purged).
--   idx_rt_session   (tenant_id, session_id, revoke_reason) — "is this session ended?" is one probe
--                                                          (the auth stage asks it per request,
--                                                          behind a 30 s cache).
--
-- Backfill: every LIVE row becomes its own session (session_id = id, last_used_at = created_at), so
-- chains issued before this migration show up in the list and continue under the new model at their
-- next refresh. Revoked rows are left NULL — nothing reads a dead row's session.
--
-- MySQL 8 has no ADD COLUMN / ADD INDEX IF NOT EXISTS, so each step is guarded through
-- information_schema inside a throwaway procedure (pattern of 000043). Idempotent.

DROP PROCEDURE IF EXISTS _u76_rt_sessions_apply;
CREATE PROCEDURE _u76_rt_sessions_apply()
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
                   WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'refresh_tokens' AND COLUMN_NAME = 'session_id') THEN
        ALTER TABLE refresh_tokens ADD COLUMN session_id CHAR(36) NULL DEFAULT NULL AFTER tenant_id;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
                   WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'refresh_tokens' AND COLUMN_NAME = 'user_agent') THEN
        ALTER TABLE refresh_tokens ADD COLUMN user_agent VARCHAR(255) NULL DEFAULT NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
                   WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'refresh_tokens' AND COLUMN_NAME = 'ip') THEN
        ALTER TABLE refresh_tokens ADD COLUMN ip VARCHAR(64) NULL DEFAULT NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
                   WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'refresh_tokens' AND COLUMN_NAME = 'last_used_at') THEN
        ALTER TABLE refresh_tokens ADD COLUMN last_used_at TIMESTAMP NULL DEFAULT NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
                   WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'refresh_tokens' AND COLUMN_NAME = 'revoke_reason') THEN
        ALTER TABLE refresh_tokens ADD COLUMN revoke_reason VARCHAR(16) NULL DEFAULT NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.STATISTICS
                   WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'refresh_tokens' AND INDEX_NAME = 'idx_rt_user_live') THEN
        ALTER TABLE refresh_tokens ADD INDEX idx_rt_user_live (tenant_id, user_id, revoked_at);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.STATISTICS
                   WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'refresh_tokens' AND INDEX_NAME = 'idx_rt_session') THEN
        ALTER TABLE refresh_tokens ADD INDEX idx_rt_session (tenant_id, session_id, revoke_reason);
    END IF;
END;
CALL _u76_rt_sessions_apply();
DROP PROCEDURE IF EXISTS _u76_rt_sessions_apply;

UPDATE refresh_tokens
   SET session_id = id, last_used_at = created_at
 WHERE session_id IS NULL AND revoked_at IS NULL;
