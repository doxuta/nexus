-- Reverse 000052: drop is_platform_operator. Guarded DROP COLUMN (idempotent, mirrors 000043 down).
DROP PROCEDURE IF EXISTS _u52_platform_operator_revert;
CREATE PROCEDURE _u52_platform_operator_revert()
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME   = 'users'
          AND COLUMN_NAME  = 'is_platform_operator'
    ) THEN
        ALTER TABLE users DROP COLUMN is_platform_operator;
    END IF;
END;
CALL _u52_platform_operator_revert();
DROP PROCEDURE IF EXISTS _u52_platform_operator_revert;
