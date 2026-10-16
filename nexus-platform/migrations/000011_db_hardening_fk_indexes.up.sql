-- RM-21 + RM-22 + RM-23 DB hardening (spec: docs/_planner/backend-audit-roadmap.md §4b).
-- Three additive, independent schema changes bundled into one migration. MySQL 8 / InnoDB / utf8mb4.
-- Head before this migration is v10 (000010_tenant_id_not_null).
--
-- RM-21  FK sys_fields.entity_id -> sys_entities.id (ON DELETE CASCADE).
-- RM-22  Composite indexes on refresh_tokens covering the auth hot paths.
-- RM-23  Index on team_sets.primary_team_id (the rule "every FK-like *_id is indexed").
--
-- sys_relationships is intentionally SKIPPED for RM-21: that table references entities by
-- NAME (parent_entity_name / child_entity_name VARCHAR columns, see migrations/000001:38-51),
-- NOT by an entity_id surrogate. There is no entity-id column to constrain, so no FK applies.

-- ─────────────────────────────────────────────────────────────────────────────
-- RM-21 — FK sys_fields.entity_id -> sys_entities.id
-- ─────────────────────────────────────────────────────────────────────────────
-- Orphan guard: a field whose entity_id has no matching sys_entities row would make the
-- ADD CONSTRAINT fail with ER_NO_REFERENCED_ROW_2 (1452). We force that failure to be LOUD
-- and EXPLICIT (a clear SIGNAL message) BEFORE attempting the ALTER, so the operator sees
-- "found orphan sys_fields rows" rather than an opaque FK error — and we NEVER auto-delete
-- the orphans. Soft-deleted entities are fine: the sys_entities row still physically exists
-- (deleted_at is set, the PK row is present), so the FK resolves and CASCADE only fires on a
-- real hard-delete.
--
-- A stored procedure runs the COUNT and raises a SIGNAL (SQLSTATE 45000, errno 1644) when any
-- orphan exists, so the migration fails loud HERE with a readable message rather than at the
-- ALTER with an opaque 1452. When clean, the procedure is a no-op and the ALTER proceeds.
DROP PROCEDURE IF EXISTS _rm21_assert_no_orphans;
CREATE PROCEDURE _rm21_assert_no_orphans()
BEGIN
    DECLARE orphan_count INT DEFAULT 0;
    SELECT COUNT(*) INTO orphan_count
        FROM sys_fields f
        LEFT JOIN sys_entities e ON e.id = f.entity_id
        WHERE e.id IS NULL;
    IF orphan_count > 0 THEN
        SIGNAL SQLSTATE '45000'
            SET MESSAGE_TEXT = 'RM-21 ABORT: orphan sys_fields rows exist (entity_id with no sys_entities). Fix data manually; migration will NOT delete them.';
    END IF;
END;
CALL _rm21_assert_no_orphans();
DROP PROCEDURE _rm21_assert_no_orphans;

ALTER TABLE sys_fields
    ADD CONSTRAINT fk_sys_fields_entity
        FOREIGN KEY (entity_id) REFERENCES sys_entities (id) ON DELETE CASCADE;

-- ─────────────────────────────────────────────────────────────────────────────
-- RM-22 — composite indexes on refresh_tokens (auth hot path)
-- ─────────────────────────────────────────────────────────────────────────────
-- idx_rt_hash_tenant covers FindLiveByHash:
--   WHERE tenant_id = ? AND token_hash = ? AND revoked_at IS NULL AND expires_at > ?
-- The two equality predicates (token_hash, tenant_id) are the leading composite keys, so the
-- lookup is an index range/ref instead of relying on the single-column idx_rt_hash + filter.
-- idx_rt_user_active covers RevokeAllForUser:
--   WHERE tenant_id = ? AND user_id = ? AND revoked_at IS NULL
-- ordering (user_id, tenant_id, revoked_at) lets the engine skip already-revoked rows at the
-- index level rather than fetching every user row and filtering revoked_at row-by-row.
ALTER TABLE refresh_tokens
    ADD INDEX idx_rt_hash_tenant (token_hash, tenant_id),
    ADD INDEX idx_rt_user_active (user_id, tenant_id, revoked_at);

-- ─────────────────────────────────────────────────────────────────────────────
-- RM-23 — index team_sets.primary_team_id
-- ─────────────────────────────────────────────────────────────────────────────
-- primary_team_id is a NULL-able soft pointer to the owning team (no hard FK by design,
-- see migrations/000006:54-64). Indexing it removes the full-scan on admin team-set filters.
ALTER TABLE team_sets
    ADD INDEX idx_ts_primary_team (primary_team_id);
