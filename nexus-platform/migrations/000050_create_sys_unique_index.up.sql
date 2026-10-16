-- sys_unique_index: the per-tenant uniqueness PIVOT table (RM-28 Slice A, expand phase).
--
-- WHY IT EXISTS. obj_<entity> tables are named by ENTITY, not by tenant, so two tenants that
-- register an entity with the same name (e.g. "accounts") SHARE one physical table (rows kept
-- apart only by tenant_id). A field's `UNIQUE(tenant_id, <gencol>)` index is therefore ONE
-- physical object on the shared table: tenant A declaring a field unique forces the constraint
-- onto tenant B's rows of the same entity-name too (cross-tenant coupling). This pivot moves
-- uniqueness OUT of the shared physical index and into per-tenant DATA: a tenant that declares a
-- field unique gets pivot rows (the real UNIQUE constraint lives here); a tenant that does not
-- declare it gets zero pivot rows and therefore zero constraint. Uniqueness becomes opt-in DATA
-- per tenant (Salesforce MT_Indexes shape) — the correct model for MySQL 8, which has no partial
-- unique index (unlike Postgres).
--
-- Slice A is ADDITIVE ONLY: this migration just CREATES the table. It does NOT backfill existing
-- rows (A2) and does NOT touch any obj_* unique index (Slice B). During Slice A the OLD obj_*
-- unique index still enforces uniqueness, so the app dual-writes the pivot as belt-and-suspenders.
--
-- COLUMN NOTES.
--   * value COLLATE utf8mb4_bin — BYTE-DISTINCT, matching the generated column's collation EXACTLY
--     (the gencol is VARCHAR(255) utf8mb4_bin). value is always READ BACK from the gencol in-tx
--     (never computed app-side), so it inherits the gencol's byte semantics / JSON coercion /
--     255-truncation and can never diverge from what the old index enforced.
--   * record_id — lets the app DELETE/UPDATE a record's pivot rows BY RECORD (not by value), which
--     is stale-safe under REPEATABLE READ. It is deliberately NOT part of uq_pivot.
--   * uq_pivot (tenant_id, entity_name, field_name, value) is the uniqueness constraint: two
--     DIFFERENT records of the same (tenant, entity, field) holding the same value → 1062.
--
-- INDEX-KEY LENGTH. uq_pivot bytes = tenant_id CHAR(36) 144 + entity_name VARCHAR(64) 256 +
-- field_name VARCHAR(64) 256 + value VARCHAR(255) utf8mb4_bin 1020 = 1676 B < 3072 B (InnoDB
-- DYNAMIC row format, declared explicitly). MySQL 8 / InnoDB / utf8mb4. Idempotent.
CREATE TABLE IF NOT EXISTS sys_unique_index (
    id           CHAR(36)      NOT NULL,
    tenant_id    CHAR(36)      NOT NULL,
    entity_name  VARCHAR(64)   NOT NULL,                          -- sanitized identifier ≤26; 64 is safe headroom
    field_name   VARCHAR(64)   NOT NULL,                          -- sanitized field/gencol name
    value        VARCHAR(255)  COLLATE utf8mb4_bin NOT NULL,      -- byte-exact copy of the gencol value
    record_id    CHAR(36)      NOT NULL,                          -- owning obj_* record (delete/update by record, not value)
    created_at   TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    UNIQUE KEY uq_pivot (tenant_id, entity_name, field_name, value),
    KEY idx_pivot_record (tenant_id, entity_name, record_id)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
