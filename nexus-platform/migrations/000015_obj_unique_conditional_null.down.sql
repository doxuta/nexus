-- W5-E1 — DOWN: revert every obj_* unique generated column from the conditional-NULL form
-- back to the plain unconditional projection. Mirror of the up sweep. MySQL 8 / InnoDB.
-- Reverts head v15 → v14.
--
-- WARNING (data semantics): reverting reinstates the soft-delete unique-value SQUAT bug — a
-- soft-deleted row's value will again occupy its unique slot. More importantly, the rebuild's
-- index recompute will FAIL LOUD with 1062 if a LIVE row and a SOFT-DELETED row currently
-- share the same value (which the conditional form legitimately allowed: the deleted row
-- projected NULL, the live row the value). That is the correct fail-fast for a down: the data
-- is no longer representable under the old unconditional unique index, so the rollback must
-- not silently drop or merge rows. An operator hitting this must reconcile the duplicate
-- values by hand before rolling back.
--
-- IDEMPOTENT: only columns whose GENERATION_EXPRESSION currently references `deleted_at` (the
-- conditional form) are reverted; a column already in the plain form is skipped, so a re-run
-- is a no-op. Matches the up sweep's discovery (unique gen-col backed by uq_<table>_<col>).

DROP PROCEDURE IF EXISTS _w5e1_revert_unique_cols_unconditional;
CREATE PROCEDURE _w5e1_revert_unique_cols_unconditional()
BEGIN
    DECLARE done INT DEFAULT 0;
    DECLARE tname VARCHAR(64);
    DECLARE cname VARCHAR(64);
    DECLARE cur CURSOR FOR
        SELECT c.TABLE_NAME, c.COLUMN_NAME
            FROM information_schema.COLUMNS c
            JOIN information_schema.STATISTICS s
              ON s.TABLE_SCHEMA = c.TABLE_SCHEMA
             AND s.TABLE_NAME   = c.TABLE_NAME
             AND s.COLUMN_NAME  = c.COLUMN_NAME
             AND s.NON_UNIQUE   = 0
             AND s.INDEX_NAME   = CONCAT('uq_', c.TABLE_NAME, '_', c.COLUMN_NAME)
            WHERE c.TABLE_SCHEMA = DATABASE()
              AND c.TABLE_NAME LIKE 'obj\_%'
              AND c.EXTRA LIKE '%STORED GENERATED%'
              AND c.GENERATION_EXPRESSION LIKE '%deleted_at%'
            GROUP BY c.TABLE_NAME, c.COLUMN_NAME;
    DECLARE CONTINUE HANDLER FOR NOT FOUND SET done = 1;

    OPEN cur;
    read_loop: LOOP
        FETCH cur INTO tname, cname;
        IF done = 1 THEN
            LEAVE read_loop;
        END IF;
        SET @ddl = CONCAT(
            'ALTER TABLE `', tname, '` MODIFY COLUMN `', cname,
            '` VARCHAR(255) COLLATE utf8mb4_bin GENERATED ALWAYS AS (JSON_UNQUOTE(JSON_EXTRACT(`data`, ',
            QUOTE(CONCAT('$.', cname)),
            '))) STORED');
        PREPARE stmt FROM @ddl;
        EXECUTE stmt;
        DEALLOCATE PREPARE stmt;
    END LOOP;
    CLOSE cur;
END;
CALL _w5e1_revert_unique_cols_unconditional();
DROP PROCEDURE _w5e1_revert_unique_cols_unconditional;
