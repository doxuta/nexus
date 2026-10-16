-- specs/065-customer-owner-tier lát B: the customer-owner capability. `can_manage_users` marks a
-- NON-admin user who may manage the other accounts of their own tenant through /org-admin/*
-- (middleware.RequireUserManager reads this row FRESH on every request, so turning it off bites on the
-- very next request). CHECK chk_users_tier makes "admin AND owner" unrepresentable: an owner is
-- is_admin=0 by construction, so every /admin/* route stays 403 for them by default.
--
-- Every existing row gets 0 => the CHECK holds, no data is updated, no one's behaviour changes. The
-- column is writable only through PUT /admin/users/:id/user-manager (operator-only) or the owner-tier
-- conversion of a later slice.
--
-- ONE ALTER statement, guarded by information_schema (MySQL 8 has no ADD COLUMN IF NOT EXISTS): DDL
-- auto-commits, so a single statement cannot leave a half-applied (dirty) migration.
DROP PROCEDURE IF EXISTS _can_manage_users_apply;
CREATE PROCEDURE _can_manage_users_apply()
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'users' AND COLUMN_NAME = 'can_manage_users'
    ) THEN
        ALTER TABLE users
            ADD COLUMN can_manage_users TINYINT(1) NOT NULL DEFAULT 0,
            ADD CONSTRAINT chk_users_tier CHECK (NOT (is_admin = 1 AND can_manage_users = 1));
    END IF;
END;
CALL _can_manage_users_apply();
DROP PROCEDURE IF EXISTS _can_manage_users_apply;
