-- AUTH-HARDENING (prod-ops slice b, loop/STATE.md): persistent account lockout.
-- users gains
--   failed_login_count INT NOT NULL DEFAULT 0   — consecutive wrong-password attempts
--   locked_until       TIMESTAMP(3) NULL        — NULL = not locked; future = login refused
--
-- Written by internal/auth (RecordFailedLogin / ResetLoginFailures) and read by
-- services.AuthService.Login: a wrong password on an EXISTING user increments the counter
-- atomically; crossing AUTH_LOCKOUT_MAX_FAILURES (default 10) stamps
-- locked_until = now + AUTH_LOCKOUT_DURATION_MIN (default 15). While locked_until is in the
-- future, login answers the SAME generic 401 INVALID_CREDENTIALS as a wrong password
-- (anti-enumeration — a locked account is indistinguishable from a bad guess). A successful
-- password login resets both columns. Unknown tenant/user attempts never write (no row).
--
-- DEFAULT 0 / NULL backfills every existing row as "never failed, not locked", so no account
-- changes behavior from this migration alone.
--
-- MySQL 8 does NOT support ADD COLUMN IF NOT EXISTS, so the ALTERs are wrapped in a
-- stored-procedure information_schema existence guard (same pattern as
-- 000040_sso_trust_idp_mfa). Idempotent per rules/database.md. Head before this migration
-- is v42 (000042_create_sys_apps). MySQL 8 / InnoDB / utf8mb4.

DROP PROCEDURE IF EXISTS _u43_user_lockout_apply;
CREATE PROCEDURE _u43_user_lockout_apply()
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME   = 'users'
          AND COLUMN_NAME  = 'failed_login_count'
    ) THEN
        ALTER TABLE users
            ADD COLUMN failed_login_count INT NOT NULL DEFAULT 0;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME   = 'users'
          AND COLUMN_NAME  = 'locked_until'
    ) THEN
        ALTER TABLE users
            ADD COLUMN locked_until TIMESTAMP(3) NULL DEFAULT NULL;
    END IF;
END;
CALL _u43_user_lockout_apply();
DROP PROCEDURE IF EXISTS _u43_user_lockout_apply;
