-- W8-BE-1 — Vertical-pack install ledger (docs/_archive/OPUS-HANDOFF-PLAYBOOK.md §5.2).
--
-- sys_pack_installs: one row per SUCCESSFUL install of a vertical pack into a tenant, written
-- fail-open by the installer (a failed insert here is logged and never breaks the install, which
-- has already committed its schema/data via auto-committing DDL). The Studio "Vertical library"
-- reads these (GET /admin/verticals/installs) to show "đã cài v1.1.0 ngày…". Re-installing the
-- same pack appends ANOTHER row (the ledger is an append-only history, not a per-slug upsert), so
-- the newest row per (tenant, slug) is the current version.
--
--   slug         pack manifest slug ("crm"/"clinic"; empty for a manifest-less uploaded pack)
--   version      pack manifest semver ("1.1.0"; empty for a manifest-less pack)
--   installed_at DATETIME(3) (ms precision), set by the app in UTC — the moment the install
--                completed, not a row-write timestamp
--   result_json  the InstallResult JSON (what was created vs. skipped per resource kind)
--
-- Index: the API reads newest-first per tenant (tenant_id, installed_at) — a backward index scan,
-- O(limit). Tenant-pinned; every query filters tenant_id (rules/database.md).
--
-- Head before this migration is v33 (000033_sys_fields_order_description).
-- Idempotent (CREATE TABLE IF NOT EXISTS). MySQL 8 / InnoDB / utf8mb4.

CREATE TABLE IF NOT EXISTS sys_pack_installs (
    id           CHAR(36)     NOT NULL PRIMARY KEY,
    tenant_id    CHAR(36)     NOT NULL,
    slug         VARCHAR(150) NOT NULL DEFAULT '',
    version      VARCHAR(50)  NOT NULL DEFAULT '',
    installed_at DATETIME(3)  NOT NULL,
    result_json  JSON         DEFAULT NULL,
    INDEX idx_pack_installs_tenant_installed (tenant_id, installed_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
