-- RM-28 Slice B — DOWN. Mirror of the up sweep (docs/rm28-pivot-uniqueness.md §2.2 "Down"):
-- reverse the demote by trying to re-add a physical UNIQUE index on every field the up sweep
-- (or the runtime DDL post Slice B — AddUniqueGencolNonUnique/BuildCreateTable) currently marks
-- `is_unique` with a non-unique idx_ index. MySQL 8 / InnoDB. Reverts head v51 → v50.
--
-- ⚠️ PRACTICALLY IRREVERSIBLE — READ BEFORE RUNNING. Between the up migration and any down, the
-- pivot may have accepted values from MULTIPLE co-tenants sharing the same obj_* table that
-- would have collided under the OLD single shared UNIQUE index (that is the entire point of
-- RM-28: decoupling them). Re-adding `UNIQUE(tenant_id, <f>)` can therefore legitimately 1062 on
-- data that was perfectly valid under the pivot model. This down migration does NOT delete any
-- row and does NOT silently drop the conflict under the rug — it is a GUARDED PER-FIELD
-- procedure: each field is attempted independently, a 1062 is logged to sys_unique_demote_log
-- (action='revert_failed_duplicate') and the loop CONTINUES to the next field — NEVER a single
-- multi-statement ADD UNIQUE that dirties the schema (partial revert) on the FIRST conflict.
--
-- MANUAL RUNBOOK for a field that fails to revert: (1) identify the offending
-- (tenant_id, value) collisions — e.g. `SELECT tenant_id, <f>, COUNT(*) FROM obj_<e> WHERE
-- deleted_at IS NULL GROUP BY tenant_id, <f> HAVING COUNT(*) > 1` per declaring tenant is no
-- longer even meaningful post-decoupling (a collision can now legitimately span co-tenants who
-- never shared enforcement) — inspect sys_unique_index for which tenant(s) actually declared the
-- field unique, then dedupe THEIR rows by hand; (2) re-run this down migration (idempotent — a
-- field already reverted, or with no matching idx_ candidate, is a no-op); or (3) force
-- `ALTER TABLE obj_<e> DROP KEY idx_obj_<e>_<f>, ADD UNIQUE KEY uq_obj_<e>_<f> (tenant_id, <f>)`
-- by hand after accepting data loss/rejection for the losing rows.
--
-- CANDIDATE SET: driven by CURRENT sys_fields metadata (is_unique=1, field_type <> 'relation',
-- not soft-deleted) — NOT by "was this field demoted by the up migration". A field created AFTER
-- the up migration ran (via AddField/CreateEntity post Slice B) was NEVER uq_ to begin with, but
-- from the DB's post-down perspective it should look identical to a pre-Slice-B unique field —
-- so it is correctly included here too. (The Go runtime code itself is unaffected by this DB-only
-- down migration; a code rollback must accompany it for the two to stay consistent — see the
-- "practically irreversible" framing above.)
CREATE TABLE IF NOT EXISTS sys_unique_demote_log (
    id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    table_name    VARCHAR(80)  NOT NULL,
    field_name    VARCHAR(64)  NOT NULL,
    action        VARCHAR(32)  NOT NULL,
    missing_count INT          NOT NULL DEFAULT 0,
    checked_at    TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    KEY idx_sudl_table_field (table_name, field_name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP PROCEDURE IF EXISTS _rm28b_revert_unique_indexes;
CREATE PROCEDURE _rm28b_revert_unique_indexes()
BEGIN
    DECLARE done INT DEFAULT 0;
    DECLARE tname VARCHAR(64);
    DECLARE cname VARCHAR(64);

    -- Candidate: every LIVE plain-unique (non-relation) field whose obj_* table currently has a
    -- non-unique idx_obj_<table>_<field> index and does NOT already have uq_obj_<table>_<field>
    -- (idempotent — a field already reverted, or one this migration never demoted, is a no-op).
    DECLARE cur CURSOR FOR
        SELECT DISTINCT CONCAT('obj_', se.name), sf.name
            FROM sys_fields sf
            JOIN sys_entities se ON se.id = sf.entity_id
            WHERE sf.is_unique = 1
              AND sf.field_type <> 'relation'
              AND sf.deleted_at IS NULL
              AND se.deleted_at IS NULL
              AND EXISTS (
                  SELECT 1 FROM information_schema.STATISTICS s
                  WHERE s.TABLE_SCHEMA = DATABASE()
                    AND s.TABLE_NAME = CONCAT('obj_', se.name)
                    AND s.INDEX_NAME = CONCAT('idx_obj_', se.name, '_', sf.name)
                    AND s.NON_UNIQUE = 1
              )
              AND NOT EXISTS (
                  SELECT 1 FROM information_schema.STATISTICS s2
                  WHERE s2.TABLE_SCHEMA = DATABASE()
                    AND s2.TABLE_NAME = CONCAT('obj_', se.name)
                    AND s2.INDEX_NAME = CONCAT('uq_obj_', se.name, '_', sf.name)
              );
    DECLARE CONTINUE HANDLER FOR NOT FOUND SET done = 1;

    OPEN cur;
    revert_loop: LOOP
        FETCH cur INTO tname, cname;
        IF done = 1 THEN
            LEAVE revert_loop;
        END IF;

        SET @revert_ok = 1;
        BEGIN
            -- Guarded attempt: a 1062 here means the data is no longer representable under a
            -- single shared UNIQUE index (the exact decoupling RM-28 achieved) — swallow it,
            -- log it, and move to the NEXT field. Never let one field's conflict abort the batch.
            DECLARE CONTINUE HANDLER FOR 1062 SET @revert_ok = 0;
            SET @alter_sql = CONCAT(
                'ALTER TABLE `', tname, '` ',
                'DROP KEY `idx_', tname, '_', cname, '`, ',
                'ADD UNIQUE KEY `uq_', tname, '_', cname, '` (`tenant_id`, `', cname, '`), ',
                'ALGORITHM=INPLACE, LOCK=NONE'
            );
            PREPARE alter_stmt FROM @alter_sql;
            EXECUTE alter_stmt;
            DEALLOCATE PREPARE alter_stmt;
        END;

        IF @revert_ok = 1 THEN
            INSERT INTO sys_unique_demote_log (table_name, field_name, action, missing_count)
                VALUES (tname, cname, 'reverted', 0);
        ELSE
            INSERT INTO sys_unique_demote_log (table_name, field_name, action, missing_count)
                VALUES (tname, cname, 'revert_failed_duplicate', -1);
        END IF;
    END LOOP;
    CLOSE cur;
END;
CALL _rm28b_revert_unique_indexes();
DROP PROCEDURE _rm28b_revert_unique_indexes;
