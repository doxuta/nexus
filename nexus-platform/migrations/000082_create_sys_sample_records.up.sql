-- specs/064-preview-as-role-sample-data S9-4 (DAILY-QUEUE #396): the sample-data mark. One row per
-- record the builder (or the generator / a pack) seeded as SAMPLE data, written in the SAME
-- transaction as the obj_<entity> row (dynrepo.insertSampleMark) so a sample row is never unmarked.
-- A side table on purpose: no DDL on obj_*, no per-entity column, rows created before this table
-- existed are simply not marked.
--
-- entity_name is the sanitized entity name (same convention as sys_record_shares). No foreign keys
-- (like sys_record_shares / sys_files): the obj_ table is dynamic. deleted_at is reserved for an
-- "un-mark" action that is not built yet.
CREATE TABLE IF NOT EXISTS sys_sample_records (
    id          CHAR(36)    NOT NULL,
    tenant_id   CHAR(36)    NOT NULL,
    entity_name VARCHAR(64) NOT NULL,
    record_id   CHAR(36)    NOT NULL,
    batch_id    CHAR(36)    NOT NULL,
    source      VARCHAR(16) NOT NULL,
    created_by  CHAR(36)    NOT NULL,
    created_at  TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at  TIMESTAMP   NULL DEFAULT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_sample_record (tenant_id, entity_name, record_id),
    KEY idx_sample_batch (tenant_id, batch_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
