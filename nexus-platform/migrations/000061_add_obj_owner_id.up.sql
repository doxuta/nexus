-- Giao việc & đổi chủ (TRỤ 2 mục 2, docs/NEXUS-MOT-XUONG-2026-07-29.md §3d mục 2): add a
-- SEPARATE `owner_id` column to every obj_* table instead of repurposing `created_by` — the
-- latter is documented and relied on as the record's IMMUTABLE creator (dynrepo/record.go
-- Record.CreatedBy doc comment; it feeds the workflow engine's "$owner" notification-recipient
-- sentinel), so mutating it on reassignment would silently corrupt every consumer that reads it
-- as "who made this record". owner_id starts equal to created_by (every INSERT path now writes
-- both — see dynrepo.InsertRecord/InsertRecordsAtomic/InsertRecords) and
-- services.RecordService.SetOwner (PUT /:entity/:id/owner) is the ONLY writer that ever changes
-- it afterward. rowscope.go's AccessOwner/AccessTeam predicates read
-- COALESCE(owner_id, created_by), so a row whose owner_id is still NULL when this migration (or
-- a Doctor generic-repair ADD COLUMN, which does not backfill) reaches it keeps EXACTLY today's
-- visibility — never less visible, so this migration cannot itself leak or hide data.
--
-- Same "no static table list" stored-procedure sweep as 000013_add_obj_version: obj_* tables
-- are created dynamically at runtime, discovered here from information_schema. Idempotent per
-- table (skips a table that already has owner_id) and the backfill UPDATE is idempotent too
-- (WHERE owner_id IS NULL). ddl/create_table.go's MandatoryColumns/MandatoryIndexes carries the
-- matching entries so every table created AFTER this migration gets owner_id + idx_owner_id
-- from birth, and the Doctor's A5 repair sweep (services/doctor_schema_checks.go) picks up any
-- table this migration might have missed. MySQL 8 / InnoDB / utf8mb4.
--
-- Head before this migration is v60 (000060_create_sys_record_notes).

DROP PROCEDURE IF EXISTS _addobjowner_add_owner_id_to_obj_tables;
CREATE PROCEDURE _addobjowner_add_owner_id_to_obj_tables()
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
        -- Skip the ADD COLUMN when the table already carries owner_id (idempotent re-run /
        -- post-migration table created between deploys).
        IF NOT EXISTS (
            SELECT 1 FROM information_schema.COLUMNS
                WHERE TABLE_SCHEMA = DATABASE()
                  AND TABLE_NAME = tname
                  AND COLUMN_NAME = 'owner_id'
        ) THEN
            SET @ddl = CONCAT('ALTER TABLE `', tname, '` ADD COLUMN `owner_id` CHAR(36) DEFAULT NULL, ADD INDEX `idx_owner_id` (`owner_id`)');
            PREPARE stmt FROM @ddl;
            EXECUTE stmt;
            DEALLOCATE PREPARE stmt;
        END IF;
        -- Backfill every existing row to be owned by its creator — IDENTICAL visibility to
        -- before this migration (created_by was the only signal rowFilter read). Idempotent
        -- (only touches rows still NULL), safe to re-run.
        SET @backfill = CONCAT('UPDATE `', tname, '` SET `owner_id` = `created_by` WHERE `owner_id` IS NULL');
        PREPARE bstmt FROM @backfill;
        EXECUTE bstmt;
        DEALLOCATE PREPARE bstmt;
    END LOOP;
    CLOSE cur;
END;
CALL _addobjowner_add_owner_id_to_obj_tables();
DROP PROCEDURE _addobjowner_add_owner_id_to_obj_tables;
