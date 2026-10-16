-- EPIC 6 Dashboard M2 — sys_dashboards: stores dashboard definitions as metadata rows. Each row
-- represents a dashboard a Nexus admin saves; it holds an ordered array of widget configs in the
-- widgets JSON column. Widgets reference a dynamic entity + an aggregate query (metric / field /
-- groupBy / filters) — the run endpoint executes each widget through the SAME aggregate engine as
-- a live aggregate request (GET /api/v1/:entity?aggregate=...), so a dashboard always reflects
-- live, tenant + A5 row-scoped records and never stores materialized data.
--
-- Multi-tenant invariant: every query against sys_dashboards MUST filter tenant_id. No hard FK on
-- tenant_id by design (logical ref, validated at service layer). MySQL 8 / InnoDB.
--
-- widgets JSON column schema (array of widget objects):
--   [{"id":"...", "title":"...", "type":"kpi|bar|line|table",
--     "entity":"...", "metric":"count|sum|avg|min|max",
--     "field":"...", "groupBy":"...", "filters":[{"field":...,"op":...,"values":[...]}]}]
--
-- type is a render hint stored verbatim — the backend does not branch on it; the frontend uses it.
-- groupBy presence/absence distinguishes scalar (KPI) from grouped (bar/line/table) results.
-- audit cols: created_at / updated_at (TIMESTAMP(3)), created_by (actor user id — nullable),
-- deleted_at for soft-delete (a removed dashboard is never run again; history is kept).
CREATE TABLE IF NOT EXISTS sys_dashboards (
    id          CHAR(36)     NOT NULL,
    tenant_id   CHAR(36)     NOT NULL,
    name        VARCHAR(150) NOT NULL,                                       -- human label of the dashboard
    description TEXT         DEFAULT NULL,                                   -- optional longer description
    widgets     JSON         NOT NULL,                                       -- ordered array of widget configs
    created_by  CHAR(36)     DEFAULT NULL,                                   -- actor who created the dashboard
    created_at  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at  TIMESTAMP(3) DEFAULT NULL,                                   -- soft-delete marker
    PRIMARY KEY (id),
    -- Admin list page reads "non-deleted dashboards of (tenant)" newest-first; the get/run path
    -- reads one (tenant, id). tenant_id leads every access path → composite covers both.
    KEY idx_dashboard_tenant (tenant_id, deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
