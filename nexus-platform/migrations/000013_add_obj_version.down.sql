-- RM-24 Optimistic Locking — DOWN: drop the `version` column from every obj_* table.
-- Mirror of the up sweep: discover obj_* tables from information_schema and DROP COLUMN
-- `version` where present (idempotent — skip tables that never got it). MySQL 8 / InnoDB.
-- Reverts head v13 → v12.

DROP PROCEDURE IF EXISTS _rm24_drop_version_from_obj_tables;
CREATE PROCEDURE _rm24_drop_version_from_obj_tables()
BEGIN
    DECLARE done INT DEFAULT 0;
    DECLARE tname VARCHAR(64);
    DECLARE cur CURSOR FOR
        SELECT TABLE_NAME
            FROM information_schema.TABLES
            WHERE TABLE_SCHEMA = DATABASE()
              AND TABLE_TYPE = 'BASE TABLE'
              AND TABLE_NAME LIKE 'obj\_%';
    DECLARE CONTINUE HANDLER FOR NOT FOUND SET done = 1;

    OPEN cur;
    read_loop: LOOP
        FETCH cur INTO tname;
        IF done = 1 THEN
            LEAVE read_loop;
        END IF;
        IF EXISTS (
            SELECT 1 FROM information_schema.COLUMNS
                WHERE TABLE_SCHEMA = DATABASE()
                  AND TABLE_NAME = tname
                  AND COLUMN_NAME = 'version'
        ) THEN
            SET @ddl = CONCAT('ALTER TABLE `', tname, '` DROP COLUMN `version`');
            PREPARE stmt FROM @ddl;
            EXECUTE stmt;
            DEALLOCATE PREPARE stmt;
        END IF;
    END LOOP;
    CLOSE cur;
END;
CALL _rm24_drop_version_from_obj_tables();
DROP PROCEDURE _rm24_drop_version_from_obj_tables;
