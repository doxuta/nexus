-- Reverse 000097. BEFORE running this, DELETE /admin/tenants/:id/owner-tier for every converted
-- tenant (specs/065 lát D): otherwise the remaining owners are staff users holding the all-data role
-- but no longer able to manage users, and nobody inside the tenant can administer it.
DROP PROCEDURE IF EXISTS _can_manage_users_revert;
CREATE PROCEDURE _can_manage_users_revert()
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'users' AND COLUMN_NAME = 'can_manage_users'
    ) THEN
        ALTER TABLE users DROP CHECK chk_users_tier, DROP COLUMN can_manage_users;
    END IF;
END;
CALL _can_manage_users_revert();
DROP PROCEDURE IF EXISTS _can_manage_users_revert;
