-- W9b-BE — Saved views: sys_views (docs/_archive/OPUS-HANDOFF-PLAYBOOK.md §5.3 "W9b-BE").
--
-- One row per saved list-view configuration a user keeps against a dynamic entity. A view is a
-- pure UI preset — the filters/sort/columns/density the FE re-applies through the SAME ACL'd list
-- endpoint (GET /api/v1/:entity). sys_views stores only the definition, never data, so applying a
-- view always yields live, tenant + A5 row-scoped records; the view CRUD itself is NOT a data-read
-- gate.
--
-- Ownership / scope model (validated at the service layer, docs/saved-views.md §Business Logic):
--   * user_id NULL  → SHARED (tenant-wide): visible to every tenant member; only an admin may
--                     create/update/delete it.
--   * user_id != NULL → PERSONAL: visible + mutable only by that user (another user's personal
--                     view is treated as not-found — no cross-user leak).
-- The list read (GET /api/v1/views?entity=…) returns the union: shared ∪ the caller's own personal
-- views for that entity.
--
-- Multi-tenant invariant: every query against sys_views MUST filter tenant_id (rules: đa tổ chức).
-- entity_name carries the dynamic entity the view targets ('accounts', ...) — a logical reference
-- into the metadata registry, validated at the service layer (no physical FK, mirroring
-- sys_reports). config_json is an opaque FE-defined document {filters, sort, columns, density, …};
-- its shape is owned by the FE and stored verbatim (the server only checks it is valid JSON).
-- audit cols: created_at / updated_at (TIMESTAMP(3)), created_by / updated_by (actor user id),
-- deleted_at for soft-delete (a removed view keeps its history). MySQL 8 / InnoDB.
CREATE TABLE IF NOT EXISTS sys_views (
    id          CHAR(36)     NOT NULL,
    tenant_id   CHAR(36)     NOT NULL,
    user_id     CHAR(36)     DEFAULT NULL,                                   -- NULL = shared/tenant-wide; else personal owner
    entity_name VARCHAR(100) NOT NULL,                                       -- dynamic entity the view targets
    name        VARCHAR(150) NOT NULL,                                       -- human label of the view
    config_json JSON         NOT NULL,                                       -- FE-defined preset (filters/sort/columns/density)
    created_by  CHAR(36)     DEFAULT NULL,                                   -- actor who created the view
    updated_by  CHAR(36)     DEFAULT NULL,                                   -- actor of the last update
    created_at  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at  TIMESTAMP(3) DEFAULT NULL,                                   -- soft-delete marker
    PRIMARY KEY (id),
    -- The list page reads "non-deleted views of (tenant, entity)" and then filters shared∪own in
    -- SQL; tenant_id + entity_name lead every access path → composite covers the list and the
    -- per-id get (which still leads with tenant_id). deleted_at trails so the IS-NULL live filter
    -- rides the index.
    KEY idx_views_lookup (tenant_id, entity_name, deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
