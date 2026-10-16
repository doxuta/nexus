-- specs/068-tenant-backup-restore slice B: per-tenant write freeze. A deadline column, NOT tenants.status
-- (status is enforced at login/refresh; a crashed restore would lock the customer out for good).
-- maintenance_until: the freeze lapses by itself when the instant passes. NULL = not frozen.
-- Idempotent: every statement guarded. NO DELIMITER (precedent 000070).
DROP PROCEDURE IF EXISTS _tmaint_up;
CREATE PROCEDURE _tmaint_up()
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'tenants' AND COLUMN_NAME = 'maintenance_until') THEN
        ALTER TABLE tenants ADD COLUMN maintenance_until DATETIME(3) NULL DEFAULT NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'tenants' AND COLUMN_NAME = 'maintenance_reason') THEN
        ALTER TABLE tenants ADD COLUMN maintenance_reason VARCHAR(200) NULL DEFAULT NULL;
    END IF;
END;
CALL _tmaint_up();
DROP PROCEDURE _tmaint_up;
