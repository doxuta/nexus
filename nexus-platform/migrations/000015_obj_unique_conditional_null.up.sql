-- W5-E1 — soft-delete unique-value SQUAT fix (spec: kanban t_6739fdfe / Wave-5 W5-E1).
-- Rebuild EVERY existing obj_* unique generated column into the conditional-NULL form so a
-- soft-deleted row no longer occupies its unique slot. MySQL 8 / InnoDB / utf8mb4.
-- Head before this migration is v14 (000014_create_audit_log).
--
-- THE BUG: a unique field is materialized as
--     `<f>` VARCHAR(255) COLLATE utf8mb4_bin GENERATED ALWAYS AS
--            (JSON_UNQUOTE(JSON_EXTRACT(`data`,'$.<f>'))) STORED
-- backed by UNIQUE KEY uq_obj_<entity>_<f> (tenant_id, <f>). MySQL unique indexes are
-- UNCONDITIONAL (no deleted_at predicate) and MySQL 8 has no partial indexes, so a
-- soft-deleted record's value SQUATS the slot forever: re-creating the same email/sku after
-- a soft-delete trips 1062 → 409 RECORD_DUPLICATE against an invisible row. The CRM
-- delete-customer → re-add-same-email flow is broken.
--
-- THE FIX: the generated value must be the field value ONLY while the row is live, NULL once
-- soft-deleted —
--     GENERATED ALWAYS AS (CASE WHEN `deleted_at` IS NULL
--         THEN JSON_UNQUOTE(JSON_EXTRACT(`data`,'$.<f>')) ELSE NULL END) STORED
-- MySQL unique indexes treat each NULL as distinct, so deleted rows stop colliding while two
-- LIVE rows with the same value still collide (both project the same non-NULL value). The raw
-- value stays in `data` JSON untouched, so a restore recomputes the original value. We do NOT
-- switch to (tenant_id, <f>, deleted_at): two live rows both have deleted_at=NULL and NULLs
-- are distinct there, which would silently LOSE live uniqueness.
--
-- WHY a stored-procedure sweep (and NOT a fixed ALTER list): obj_* tables are created
-- dynamically at runtime by the entity engine (ddl.BuildCreateTable). There is no static set
-- of table/column names to enumerate, so we discover the unique generated columns from
-- information_schema at migration time. The matching ddl/create_table.go + ddl/alter.go change
-- makes EVERY unique column created AFTER this migration carry the conditional form from birth;
-- this sweep upgrades the ones that already exist. golang-migrate runs CREATE PROCEDURE bodies
-- intact (verified by the 000011/000012/000013 procedures).
--
-- IDEMPOTENT: a column whose GENERATION_EXPRESSION already references `deleted_at` (the
-- conditional form, or a column created post-migration) is skipped, so a re-run is a no-op.
-- We rebuild via ALTER ... MODIFY COLUMN, which recomputes the STORED values and rebuilds the
-- existing unique index in place — the index is NOT dropped/recreated (its name and shape are
-- unchanged). If two LIVE rows somehow already share a value, the MODIFY's index rebuild fails
-- LOUD with 1062 rather than silently corrupting — that is the correct fail-fast.

DROP PROCEDURE IF EXISTS _w5e1_make_unique_cols_conditional;
CREATE PROCEDURE _w5e1_make_unique_cols_conditional()
BEGIN
    DECLARE done INT DEFAULT 0;
    DECLARE tname VARCHAR(64);
    DECLARE cname VARCHAR(64);
    -- Cursor over every STORED generated column on an obj_* base table that
    --   (a) is backed by a tenant-scoped UNIQUE index named uq_<table>_<col> (the unique-field
    --       naming convention from ddl.BuildCreateTable / BuildAddUniqueColumn), AND
    --   (b) does NOT already use the conditional-NULL form (GENERATION_EXPRESSION has no
    --       `deleted_at` reference) — so a re-run / post-migration column is skipped.
    -- Joining through STATISTICS on the uq_ index name guarantees we only touch UNIQUE
    -- generated columns and never the relation `<f>_id` (idx_) or indexed (idx_) gen-cols.
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
              AND c.GENERATION_EXPRESSION <> ''
              AND c.GENERATION_EXPRESSION NOT LIKE '%deleted_at%'
            GROUP BY c.TABLE_NAME, c.COLUMN_NAME;
    DECLARE CONTINUE HANDLER FOR NOT FOUND SET done = 1;

    OPEN cur;
    read_loop: LOOP
        FETCH cur INTO tname, cname;
        IF done = 1 THEN
            LEAVE read_loop;
        END IF;
        -- Rebuild the column expression to the conditional-NULL form. Column type/collation are
        -- pinned to the canonical unique-column shape (VARCHAR(255) COLLATE utf8mb4_bin) so the
        -- MODIFY is exactly the create-time definition plus the CASE wrapper.
        SET @ddl = CONCAT(
            'ALTER TABLE `', tname, '` MODIFY COLUMN `', cname,
            '` VARCHAR(255) COLLATE utf8mb4_bin GENERATED ALWAYS AS (CASE WHEN `deleted_at` IS NULL THEN JSON_UNQUOTE(JSON_EXTRACT(`data`, ',
            QUOTE(CONCAT('$.', cname)),
            ')) ELSE NULL END) STORED');
        PREPARE stmt FROM @ddl;
        EXECUTE stmt;
        DEALLOCATE PREPARE stmt;
    END LOOP;
    CLOSE cur;
END;
CALL _w5e1_make_unique_cols_conditional();
DROP PROCEDURE _w5e1_make_unique_cols_conditional;
