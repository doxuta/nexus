-- AUTH-HARDENING (prod-ops slice b) — revert: drop the persistent-lockout columns.
-- Guarded (IF EXISTS via information_schema) so the down is idempotent per rules/database.md.
-- Dropping the columns also discards any in-flight lock state — acceptable for a rollback:
-- the code path that reads them is rolled back together with the schema.

DROP PROCEDURE IF EXISTS _u43_user_lockout_revert;
CREATE PROCEDURE _u43_user_lockout_revert()
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME   = 'users'
          AND COLUMN_NAME  = 'locked_until'
    ) THEN
        ALTER TABLE users DROP COLUMN locked_until;
    END IF;

    IF EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME   = 'users'
          AND COLUMN_NAME  = 'failed_login_count'
    ) THEN
        ALTER TABLE users DROP COLUMN failed_login_count;
    END IF;
END;
CALL _u43_user_lockout_revert();
DROP PROCEDURE IF EXISTS _u43_user_lockout_revert;
