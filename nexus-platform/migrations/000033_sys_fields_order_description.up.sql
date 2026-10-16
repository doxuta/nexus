-- W5 Builder core v2 (docs/nexus-ux-revolution-plan.md §3.2) — sys_fields gains:
--   field_order  INT NOT NULL DEFAULT 0   — admin-controlled display/order position. The
--                Studio Data Model builder drags fields into an order and that order becomes
--                the default ordering of GET /metadata/fields/:entity (and therefore of the
--                generated default form/list layouts).
--   description  VARCHAR(500) NULL        — per-field admin description / help text shown in
--                the builder inspector (NOT the per-layout-cell helpText, which lives in
--                sys_layouts JSON).
--
-- BACKFILL (runs exactly once, inside the column-add guard): existing rows are numbered
-- deterministically per (tenant_id, entity_id) in gaps of 10 — ROW_NUMBER() ordered by
-- created_at, then name, then id. created_at is TIMESTAMP (1s resolution) and every field of
-- an entity-create lands in ONE transaction (identical created_at), so name/id tie-breaks make
-- the backfill deterministic across runs. Soft-deleted rows are numbered too (harmless; keeps
-- the window function total and repeatable).
--
-- MySQL 8 does NOT support ADD COLUMN IF NOT EXISTS, so the ALTERs are wrapped in a
-- stored-procedure existence guard (same pattern as 000026_add_notification_type).
-- Head before this migration is v32 (000032_create_sys_workflow_runs).
-- MySQL 8 / InnoDB / utf8mb4.

DROP PROCEDURE IF EXISTS _w5_fields33_apply;
CREATE PROCEDURE _w5_fields33_apply()
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME   = 'sys_fields'
          AND COLUMN_NAME  = 'field_order'
    ) THEN
        ALTER TABLE sys_fields
            ADD COLUMN field_order INT NOT NULL DEFAULT 0 AFTER options;

        -- One-time deterministic backfill: 10, 20, 30… per (tenant_id, entity_id).
        UPDATE sys_fields sf
        JOIN (
            SELECT id,
                   ROW_NUMBER() OVER (
                       PARTITION BY tenant_id, entity_id
                       ORDER BY created_at ASC, name ASC, id ASC
                   ) AS rn
            FROM sys_fields
        ) x ON x.id = sf.id
        SET sf.field_order = x.rn * 10;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME   = 'sys_fields'
          AND COLUMN_NAME  = 'description'
    ) THEN
        ALTER TABLE sys_fields
            ADD COLUMN description VARCHAR(500) NULL DEFAULT NULL AFTER field_order;
    END IF;
END;
CALL _w5_fields33_apply();
DROP PROCEDURE IF EXISTS _w5_fields33_apply;
