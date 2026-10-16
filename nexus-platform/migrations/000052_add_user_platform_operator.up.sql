-- TENANT-PROVISION (TOTAL-UPGRADE 2.1, docs/tenant-provisioning-spec.md §2.3): a NEW authority
-- tier above tenant-admin. `is_platform_operator` gates the operator-only provisioning routes
-- (POST/GET /admin/tenants) that mint a NEW customer tenant + its first admin.
--
-- WHY a separate column and not a JWT claim: the gate reads this row FRESH on every request
-- (middleware.RequirePlatformOperator), so revoking the flag takes effect on the next request
-- with zero token-stack changes and no stale-claim window.
--
-- HARD RULE (the #95 known-credential lesson): this column is grantable ONLY out-of-band —
-- cmd/bootstrap-admin (BOOTSTRAP_PLATFORM_OPERATOR=true) or direct SQL. It is absent from every
-- API write path (not in CreateUserInput/UpdateUserInput; the struct field is json:"-"), so no
-- operator can self-grant or grant another via the API. This migration SEEDS no operator.
--
-- DEFAULT 0 backfills every existing row as "not an operator", so this migration alone changes
-- no one's behavior. MySQL 8 has no ADD COLUMN IF NOT EXISTS → stored-procedure existence guard,
-- idempotent per rules/database.md (same pattern as 000043_add_user_lockout). Head before this
-- migration is v51 (000051_demote_obj_unique_index). MySQL 8 / InnoDB / utf8mb4.

DROP PROCEDURE IF EXISTS _u52_platform_operator_apply;
CREATE PROCEDURE _u52_platform_operator_apply()
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME   = 'users'
          AND COLUMN_NAME  = 'is_platform_operator'
    ) THEN
        ALTER TABLE users
            ADD COLUMN is_platform_operator TINYINT(1) NOT NULL DEFAULT 0;
    END IF;
END;
CALL _u52_platform_operator_apply();
DROP PROCEDURE IF EXISTS _u52_platform_operator_apply;
