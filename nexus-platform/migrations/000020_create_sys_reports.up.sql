-- EPIC 6 M2 — Saved-report-as-metadata: sys_reports (spec: docs/ENGINE-COVERAGE-SPEC.md §EPIC 6,
-- benchmark Salesforce summary/matrix report + Metabase saved-questions). One row per report a
-- Nexus admin saves against a dynamic entity: "chạy <type> trên <entity_name> với <config>". The
-- run endpoint (GET /api/v1/reports/:id/run) executes the saved config THROUGH the existing
-- aggregate (summary/matrix) or list (tabular) engine — sys_reports stores only the definition,
-- never materialized data, so a report always reflects live, tenant + A5 row-scoped records.
--
-- Multi-tenant invariant: every query against sys_reports MUST filter tenant_id (rules: đa tổ
-- chức). entity_name carries the dynamic entity the report targets ('accounts', ...). type is an
-- ENUM over the three report shapes. config is a single JSON document describing the report:
--   tabular:  {"columns":["name","amount","status"], "filters":{...}, "sort":{"field":"amount","dir":"desc"}}
--   summary:  {"groupBy":"status", "metric":"sum", "field":"amount", "filters":{...}}
--   matrix:   {"groupBy":"status", "metric":"sum", "field":"amount", ...}  (M2 runs matrix as a
--             grouped summary; a second pivot axis is deferred — see service notes)
-- The shape of config is validated at the service layer (not by the DB) against the entity's live
-- metadata, so a report can never reference a field that no longer exists at RUN time.
-- is_active gates whether the report is listed/runnable (a draft/archived report stays dormant).
-- audit cols: created_at / updated_at (TIMESTAMP(3)), created_by / updated_by (actor user id),
-- deleted_at for soft-delete (a removed report keeps its history, never runs again).
-- No hard FK on tenant_id / entity_name by design (entity_name is a logical reference into the
-- metadata registry, validated at the service layer, not a physical table). MySQL 8 / InnoDB.
CREATE TABLE IF NOT EXISTS sys_reports (
    id          CHAR(36)     NOT NULL,
    tenant_id   CHAR(36)     NOT NULL,
    name        VARCHAR(150) NOT NULL,                                       -- human label of the report
    entity_name VARCHAR(100) NOT NULL,                                       -- dynamic entity the report targets
    type        ENUM('tabular','summary','matrix') NOT NULL,                 -- report shape
    config      JSON         NOT NULL,                                       -- columns/filters/groupBy/metric/field/sort
    is_active   TINYINT(1)   NOT NULL DEFAULT 1,                             -- only active reports are listed/runnable
    created_by  CHAR(36)     DEFAULT NULL,                                   -- actor who created the report
    updated_by  CHAR(36)     DEFAULT NULL,                                   -- actor of the last update
    created_at  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at  TIMESTAMP(3) DEFAULT NULL,                                   -- soft-delete marker
    PRIMARY KEY (id),
    -- Admin list page reads "non-deleted reports of (tenant)" newest-first; the run/get path
    -- reads one (tenant, id). tenant_id leads every access path → composite covers both.
    KEY idx_report_tenant (tenant_id, deleted_at),
    -- Reports-of-an-entity lookup (a builder filtering the list by entity) is an index range scan.
    KEY idx_report_entity (tenant_id, entity_name, deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
