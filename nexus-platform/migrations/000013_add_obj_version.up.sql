-- RM-24 Optimistic Locking — add a per-record `version` column to every dynamic obj_* table
-- so an If-Match-guarded UPDATE can detect a lost-update and return 409 (spec:
-- docs/optimistic-lock-spec.md §2-4). MySQL 8 / InnoDB / utf8mb4.
-- Head before this migration is v12 (000012_sys_fields_unique_tenant_entity_name).
--
-- WHY a stored-procedure sweep (and NOT a fixed ADD COLUMN list): obj_* tables are created
-- dynamically at runtime by the entity engine (ddl.BuildCreateTable). There is no static set
-- of table names a migration could enumerate, so we discover them from information_schema at
-- migration time and ADD the column to each. The matching ddl/create_table.go change makes
-- EVERY obj_* table created AFTER this migration carry `version` from birth; this sweep covers
-- the ones that already exist. golang-migrate runs stored-procedure bodies intact (verified
-- by the 000011/000012 orphan-guard procedures), and the integration harness applies each
-- migration file as one multi-statement Exec, so the CREATE PROCEDURE ... CALL ... DROP shape
-- works under both runners.
--
-- IDEMPOTENT per table: the sweep checks information_schema.COLUMNS before each ADD COLUMN so a
-- table that somehow already has `version` (e.g. created post-migration, then a re-run) is
-- skipped rather than failing with ER_DUP_FIELDNAME (1060). The column is NOT NULL DEFAULT 0,
-- so existing rows backfill to version 0 (the opt-in baseline FE sends as the first If-Match).
--
-- NO index on `version`: it is only ever read/written alongside the PRIMARY (tenant_id, id)
-- predicate in UpdateRecord, so a standalone index would be dead weight (spec §2).

DROP PROCEDURE IF EXISTS _rm24_add_version_to_obj_tables;
CREATE PROCEDURE _rm24_add_version_to_obj_tables()
BEGIN
    DECLARE done INT DEFAULT 0;
    DECLARE tname VARCHAR(64);
    -- Cursor over every base table in the CURRENT schema whose name starts with `obj_`.
    -- LIKE 'obj\_%' escapes the underscore so it matches the literal prefix, not any char.
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
        -- Skip if the table already carries a `version` column (idempotent re-run / post-migration table).
        IF NOT EXISTS (
            SELECT 1 FROM information_schema.COLUMNS
                WHERE TABLE_SCHEMA = DATABASE()
                  AND TABLE_NAME = tname
                  AND COLUMN_NAME = 'version'
        ) THEN
            SET @ddl = CONCAT('ALTER TABLE `', tname, '` ADD COLUMN `version` INT NOT NULL DEFAULT 0');
            PREPARE stmt FROM @ddl;
            EXECUTE stmt;
            DEALLOCATE PREPARE stmt;
        END IF;
    END LOOP;
    CLOSE cur;
END;
CALL _rm24_add_version_to_obj_tables();
DROP PROCEDURE _rm24_add_version_to_obj_tables;
