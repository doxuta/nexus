-- Reverse 000092: drops the freeze columns (a freeze in flight simply lapses). Idempotent.
DROP PROCEDURE IF EXISTS _tmaint_down;
CREATE PROCEDURE _tmaint_down()
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'tenants' AND COLUMN_NAME = 'maintenance_reason') THEN
        ALTER TABLE tenants DROP COLUMN maintenance_reason;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'tenants' AND COLUMN_NAME = 'maintenance_until') THEN
        ALTER TABLE tenants DROP COLUMN maintenance_until;
    END IF;
END;
CALL _tmaint_down();
DROP PROCEDURE _tmaint_down;
