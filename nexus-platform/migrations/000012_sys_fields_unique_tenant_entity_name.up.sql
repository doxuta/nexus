-- RM-25 metadata hardening (spec: docs/_planner/backend-audit-roadmap.md §RM-25).
-- Tighten sys_fields uniqueness to (tenant_id, entity_id, name) so a duplicate field row
-- can never be persisted — the last-resort serializer behind the AddField advisory lock and
-- the DDL 1061/1062 → 409 classification. MySQL 8 / InnoDB / utf8mb4.
-- Head before this migration is v11 (000011_db_hardening_fk_indexes).
--
-- WHY (tenant_id, entity_id, name) and NOT just (entity_id, name):
--   entity_id already implies a tenant (a sys_entities row belongs to exactly one tenant),
--   so (entity_id, name) would be sufficient for correctness. We include tenant_id as the
--   LEADING column to (a) mirror the denormalized-tenant defense-in-depth used everywhere
--   else (RM-18: every sys_* uniqueness/lookup is tenant-pinned), and (b) keep the index
--   useful as a tenant-scoped covering prefix. A field's denormalized tenant_id always equals
--   its parent entity's tenant_id (enforced at write time), so this never rejects a legitimate
--   row that (entity_id, name) would have allowed.
--
-- SOFT-DELETE SEMANTICS (intentional, must stay consistent with AddField's dup-check):
--   AddField rejects re-adding a soft-deleted field name via ListFieldsIncludingDeleted
--   (§7.2 B-1). The unique index therefore covers BOTH active AND soft-deleted rows (it has
--   no deleted_at predicate — MySQL unique indexes are unconditional). That is DELIBERATE and
--   NHẤT QUÁN with the app-level rule: a name that was added then soft-deleted is still
--   "taken" and must be restored, not re-created. A consequence is that you cannot hard-create
--   a second row with a name whose soft-deleted row still exists — which is exactly the
--   behaviour the dup-check already enforces at the application layer.

-- ─────────────────────────────────────────────────────────────────────────────
-- Orphan guard (pattern mirrors 000011 RM-21): fail LOUD before the ALTER if the data
-- already contains a duplicate (tenant_id, entity_id, name) group — the ADD UNIQUE would
-- otherwise fail with an opaque 1062 listing one offending value. We COUNT the duplicate
-- groups and raise a readable SIGNAL when any exist; we NEVER auto-delete rows.
-- ─────────────────────────────────────────────────────────────────────────────
DROP PROCEDURE IF EXISTS _rm25_assert_no_dup_fields;
CREATE PROCEDURE _rm25_assert_no_dup_fields()
BEGIN
    DECLARE dup_groups INT DEFAULT 0;
    SELECT COUNT(*) INTO dup_groups FROM (
        SELECT 1
            FROM sys_fields
            GROUP BY tenant_id, entity_id, name
            HAVING COUNT(*) > 1
    ) d;
    IF dup_groups > 0 THEN
        -- MESSAGE_TEXT is capped at 128 bytes by MySQL (ER_DATA_TOO_LONG, 1648); keep it short.
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'RM-25 ABORT: duplicate (tenant_id,entity_id,name) in sys_fields (incl soft-deleted). Fix data manually; rows NOT deleted.';
    END IF;
END;
CALL _rm25_assert_no_dup_fields();
DROP PROCEDURE _rm25_assert_no_dup_fields;

ALTER TABLE sys_fields
    ADD UNIQUE KEY uq_sys_fields_tenant_entity_name (tenant_id, entity_id, name);
