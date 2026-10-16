-- Giao việc & đổi chủ — DOWN: drop `owner_id` (+ its index `idx_owner_id`) from every obj_*
-- table. Mirror of the up sweep. Reverts head v61 → v60.

DROP PROCEDURE IF EXISTS _addobjowner_drop_owner_id_from_obj_tables;
CREATE PROCEDURE _addobjowner_drop_owner_id_from_obj_tables()
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
                  AND COLUMN_NAME = 'owner_id'
        ) THEN
            SET @ddl = CONCAT('ALTER TABLE `', tname, '` DROP INDEX `idx_owner_id`, DROP COLUMN `owner_id`');
            PREPARE stmt FROM @ddl;
            EXECUTE stmt;
            DEALLOCATE PREPARE stmt;
        END IF;
    END LOOP;
    CLOSE cur;
END;
CALL _addobjowner_drop_owner_id_from_obj_tables();
DROP PROCEDURE _addobjowner_drop_owner_id_from_obj_tables;
