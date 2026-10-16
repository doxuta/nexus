-- Reversal of 000033_sys_fields_order_description.up.sql — drops field_order + description.
-- Guarded via information_schema (MySQL 8 has no DROP COLUMN IF EXISTS) so a partial apply
-- reverts cleanly. Reverts to v32.

DROP PROCEDURE IF EXISTS _w5_fields33_revert;
CREATE PROCEDURE _w5_fields33_revert()
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME   = 'sys_fields'
          AND COLUMN_NAME  = 'description'
    ) THEN
        ALTER TABLE sys_fields DROP COLUMN description;
    END IF;

    IF EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME   = 'sys_fields'
          AND COLUMN_NAME  = 'field_order'
    ) THEN
        ALTER TABLE sys_fields DROP COLUMN field_order;
    END IF;
END;
CALL _w5_fields33_revert();
DROP PROCEDURE IF EXISTS _w5_fields33_revert;
