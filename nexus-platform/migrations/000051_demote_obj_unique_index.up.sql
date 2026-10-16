-- RM-28 Slice B — CONTRACT phase (docs/rm28-pivot-uniqueness.md §2.2 phase 3): demote every
-- plain `is_unique` field's physical index on a shared obj_* table from UNIQUE to non-unique.
-- MySQL 8 / InnoDB / utf8mb4. Head before this migration is v50 (000050_create_sys_unique_index).
--
-- THE PROBLEM (recap, §1.1/§1.2): obj_<entity> tables are named by ENTITY, not by tenant, so two
-- tenants that register an entity with the same name SHARE one physical table. A field's
-- `UNIQUE(tenant_id, <gencol>)` index is therefore ONE physical object on the shared table —
-- tenant A declaring a field unique couples the constraint onto tenant B's rows of the same
-- entity-name too, even though B never opted in. Slice A/A2 (migrations 000050 + app-layer
-- dual-write) built a per-tenant pivot table (sys_unique_index) that carries the REAL uniqueness
-- constraint as tenant-scoped DATA instead of a shared physical index, while leaving the old
-- obj_* UNIQUE index in place as a belt-and-suspenders backstop. THIS migration removes that
-- backstop for every field the pivot has already fully caught up with, so uniqueness becomes
-- SOLELY the pivot's job (§1.4 — app-enforced, backstopped by Doctor D1_PIVOT_RECONCILE).
--
-- WHAT DOES NOT CHANGE: the generated column itself (STORED, VARCHAR(255) COLLATE utf8mb4_bin,
-- CASE-guarded against deleted_at) stays byte-identical — it is dual-purpose (§1.2): sort,
-- filter-eq, and CSV-import dedup probe all read it. Only the INDEX changes shape, from
-- `UNIQUE KEY uq_obj_<e>_<f> (tenant_id, <f>)` to `KEY idx_obj_<e>_<f> (tenant_id, <f>)` — same
-- leading columns, so it still serves every existing IN-probe/sort/filter query plan.
--
-- COVERAGE GUARD (critical, do NOT skip — §2.2 "Cổng pha 3"): a field is demoted ONLY when the
-- pivot (sys_unique_index) already has a matching row for EVERY live obj_<e> row (of every
-- tenant that has declared this field unique) — i.e. zero live rows would lose enforcement the
-- moment the physical index stops enforcing it. A field failing this check is left UNTOUCHED
-- (still physically UNIQUE) and logged to sys_unique_demote_log — never silently demoted with a
-- coverage gap. The coverage subquery is scoped to DECLARING TENANTS ONLY (tenants whose own
-- sys_fields row for this (entity,field) has is_unique=1): a co-tenant sharing the same obj_*
-- table who never declared the field unique has no uniqueness to preserve, so its live rows
-- (even ones with duplicate values under this JSON key) must NOT block the demote — counting
-- them would mean a field could NEVER be demoted for tenant A just because tenant B (with zero
-- enforcement obligation) happens to have duplicate data. This is the whole point of RM-28:
-- decoupling tenant A's constraint from tenant B's data.
--
-- PRECONDITION (operator runbook, not enforced by this migration — the coverage guard above is
-- the automated backstop if skipped): run Doctor check D1_PIVOT_RECONCILE fix-all (backfill) for
-- every tenant BEFORE running this migration, so the pivot is caught up and the coverage guard
-- passes for as many fields as possible. A field a fresh backfill still can't fully cover (an
-- intra-tenant duplicate the operator hasn't dealt with yet) will simply stay UNIQUE — safe,
-- re-runnable: fix the duplicate, re-run this migration (or Doctor's own repair, once flipped).
--
-- CANDIDATE-SET SCOPING (correctness bug caught during build — see report for full detail):
-- the sweep does NOT touch every `uq_obj_*` index blindly. It is driven by information_schema
-- (safe, real, already-existing identifiers — mirrors migration 000015's discovery pattern)
-- filtered down to ONLY plain `is_unique=1, field_type <> 'relation'` fields via an EXISTS
-- against sys_fields/sys_entities. This EXCLUDES two classes that also produce a `uq_obj_*`
-- name and would otherwise be silently (and unsafely) touched by a naive sweep:
--   (a) a 1-1 relation's cardinality index (`uq_obj_<e>_<f>_id`, BuildAddOneToOneRelationColumn)
--       — this is a SEPARATE physical constraint, NEVER pivot-backed (sys_unique_index only
--       tracks plain unique fields — see dynrepo/pivot.go's doc comment), so demoting it would
--       remove 1-1 exclusivity enforcement with NOTHING to replace it. Out of RM-28's scope
--       entirely; not retired, not touched, no coverage check even attempted.
--   (b) an ORPHANED `uq_obj_*` index with no live owning field row at all (DeleteField residue
--       on a shared table, Doctor A3's pre-existing class) — there is no sys_fields row to even
--       compute a coverage set from, so it is left for A3/manual handling, not auto-demoted.
--
-- IDEMPOTENT + RE-RUNNABLE: once a field is demoted its physical index is literally no longer
-- named `uq_obj_*` (it's `idx_obj_*`), so a re-run's information_schema-driven cursor naturally
-- no longer selects it — no separate "already demoted" check needed. A field the coverage guard
-- skipped is picked up again on every re-run and re-evaluated fresh.
--
-- FAIL LOUD (§2.2 "Contract ALTER"): `ALGORITHM=INPLACE, LOCK=NONE` is declared explicitly on
-- the demote ALTER so MySQL refuses (loud, transactionless-DDL-safe error) instead of silently
-- falling back to a COPY algorithm that would lock the shared table under production traffic.
-- Dropping a UNIQUE secondary index and adding a non-unique secondary index are both
-- INPLACE-eligible operations in InnoDB — this should always succeed, but if a future MySQL
-- version's optimizer disagrees, failing loud here is exactly the desired outcome.
--
-- AUDIT TRAIL: sys_unique_demote_log is a plain operational log (not tenant data — a demote
-- decision is about a SHARED table/field, not scoped to one tenant, so it deliberately has no
-- tenant_id/UUID-per-rules/database.md, which governs tenant transactional tables). It records
-- every field the sweep considered and what happened, so an operator can `SELECT * FROM
-- sys_unique_demote_log WHERE action = 'skipped_uncovered'` to find exactly which fields still
-- need data cleanup + a re-run.
CREATE TABLE IF NOT EXISTS sys_unique_demote_log (
    id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    table_name    VARCHAR(80)  NOT NULL,
    field_name    VARCHAR(64)  NOT NULL,
    action        VARCHAR(32)  NOT NULL,  -- 'demoted' | 'skipped_uncovered'
    missing_count INT          NOT NULL DEFAULT 0,
    checked_at    TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    KEY idx_sudl_table_field (table_name, field_name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP PROCEDURE IF EXISTS _rm28b_demote_unique_indexes;
CREATE PROCEDURE _rm28b_demote_unique_indexes()
BEGIN
    DECLARE done INT DEFAULT 0;
    DECLARE tname VARCHAR(64);
    DECLARE cname VARCHAR(64);
    DECLARE entity_name VARCHAR(64);

    -- Discovery cursor: every STORED generated column on an obj_* table backed by a
    -- tenant-scoped UNIQUE index named uq_<table>_<column> (mirrors 000015's join shape) AND
    -- currently owned by a LIVE plain-unique (non-relation) sys_fields row of SOME tenant —
    -- the candidate-set scoping documented above.
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
              AND EXISTS (
                  SELECT 1 FROM sys_fields sf
                    JOIN sys_entities se ON se.id = sf.entity_id
                  WHERE se.name = SUBSTRING(c.TABLE_NAME, 5)
                    AND sf.name = c.COLUMN_NAME
                    AND sf.is_unique = 1
                    AND sf.field_type <> 'relation'
                    AND sf.deleted_at IS NULL
                    AND se.deleted_at IS NULL
              )
            GROUP BY c.TABLE_NAME, c.COLUMN_NAME;
    DECLARE CONTINUE HANDLER FOR NOT FOUND SET done = 1;

    OPEN cur;
    read_loop: LOOP
        FETCH cur INTO tname, cname;
        IF done = 1 THEN
            LEAVE read_loop;
        END IF;
        SET entity_name = SUBSTRING(tname, 5);

        -- Coverage guard: any live row of a DECLARING tenant (is_unique=1 for this entity+field,
        -- not soft-deleted) missing a matching pivot row (same tenant/entity/field/record_id AND
        -- CURRENT value) blocks the demote for THIS field. Non-declaring co-tenants' rows are
        -- deliberately excluded from the count (see header) — decoupling them is RM-28's point.
        SET @cov_sql = CONCAT(
            'SELECT COUNT(*) INTO @v_missing FROM `', tname, '` o ',
            'WHERE o.`deleted_at` IS NULL AND o.`', cname, '` IS NOT NULL ',
            'AND o.`tenant_id` IN (',
                'SELECT se2.`tenant_id` FROM `sys_fields` sf2 JOIN `sys_entities` se2 ON se2.`id` = sf2.`entity_id` ',
                'WHERE se2.`name` = ', QUOTE(entity_name), ' AND sf2.`name` = ', QUOTE(cname), ' ',
                'AND sf2.`is_unique` = 1 AND sf2.`deleted_at` IS NULL AND se2.`deleted_at` IS NULL',
            ') ',
            'AND NOT EXISTS (SELECT 1 FROM `sys_unique_index` p WHERE p.`tenant_id` = o.`tenant_id` ',
                'AND p.`entity_name` = ', QUOTE(entity_name), ' AND p.`field_name` = ', QUOTE(cname), ' ',
                'AND p.`record_id` = o.`id` AND p.`value` = o.`', cname, '`)'
        );
        PREPARE cov_stmt FROM @cov_sql;
        EXECUTE cov_stmt;
        DEALLOCATE PREPARE cov_stmt;

        IF @v_missing = 0 THEN
            SET @alter_sql = CONCAT(
                'ALTER TABLE `', tname, '` ',
                'DROP KEY `uq_', tname, '_', cname, '`, ',
                'ADD KEY `idx_', tname, '_', cname, '` (`tenant_id`, `', cname, '`), ',
                'ALGORITHM=INPLACE, LOCK=NONE'
            );
            PREPARE alter_stmt FROM @alter_sql;
            EXECUTE alter_stmt;
            DEALLOCATE PREPARE alter_stmt;
            INSERT INTO sys_unique_demote_log (table_name, field_name, action, missing_count)
                VALUES (tname, cname, 'demoted', 0);
        ELSE
            INSERT INTO sys_unique_demote_log (table_name, field_name, action, missing_count)
                VALUES (tname, cname, 'skipped_uncovered', @v_missing);
        END IF;
    END LOOP;
    CLOSE cur;
END;
CALL _rm28b_demote_unique_indexes();
DROP PROCEDURE _rm28b_demote_unique_indexes;
