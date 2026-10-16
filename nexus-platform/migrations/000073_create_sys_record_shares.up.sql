-- sys_record_shares: manual, per-record READ shares (DAILY-QUEUE #34 record-manual-share-a,
-- specs/051-record-manual-share). One row = "user_id may VIEW record_id of entity_name in tenant_id",
-- granted by shared_by. It is one extra OR term on the A5 row filter (dynrepo/rowscope.go) for the
-- owner/team levels, emitted ONLY on the read paths — a share widens which rows a role's existing
-- VIEW grant reaches; it never grants an action and never widens a write (Q4 2026-09-05: v1 shares
-- are read-only, so there is deliberately NO can_edit column — adding one later is additive).
--
--   * UNIQUE (tenant_id, entity_name, record_id, user_id): one row per (record, grantee). Revoking
--     soft-deletes it; sharing again with the same person revives THAT row (upsert), so the unique
--     key never collides with a tombstone.
--   * KEY idx_record_shares_grantee (tenant_id, user_id, entity_name, deleted_at, record_id) serves
--     the row-filter subquery `record_id FROM … WHERE tenant_id=? AND entity_name=? AND user_id=?
--     AND deleted_at IS NULL` as a covering index lookup.
--   * entity_name stays IN the predicate on purpose: record ids are UUIDs, but matching on
--     (entity_name, record_id) makes a mis-attributed share fail closed instead of relying on id
--     uniqueness across tables.
--   * No hard FK (same rationale as sys_record_notes / sys_files): the row lifecycle is decoupled
--     from the obj_* row and the users row; every query pins tenant_id in the application layer.
--
-- Tenant-pinned like every fixed system table. MySQL 8 / InnoDB / utf8mb4. Idempotent.
CREATE TABLE IF NOT EXISTS sys_record_shares (
    id          CHAR(36)    NOT NULL,
    tenant_id   CHAR(36)    NOT NULL,
    entity_name VARCHAR(64) NOT NULL,                -- canonical (sanitized) entity name
    record_id   CHAR(36)    NOT NULL,                -- the shared obj_<entity_name> row
    user_id     CHAR(36)    NOT NULL,                -- grantee: may VIEW the record
    shared_by   CHAR(36)    NOT NULL,                -- verified caller who (last) granted it
    created_by  CHAR(36)    NOT NULL,                -- first granter (standard transactional column)
    created_at  TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at  TIMESTAMP   NULL DEFAULT NULL,       -- revoked
    PRIMARY KEY (id),
    UNIQUE KEY uq_record_shares (tenant_id, entity_name, record_id, user_id),
    KEY idx_record_shares_grantee (tenant_id, user_id, entity_name, deleted_at, record_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
