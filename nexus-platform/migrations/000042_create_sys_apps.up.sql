-- App container (docs/apps-concept-spec.md §2) — sys_apps + sys_app_nav_items.
--
-- An App is a "lens" (Salesforce Lightning App archetype): an ordered NAV over the tenant's
-- entities/dashboards, NOT an ownership container. Entities and dashboards stay tenant-global;
-- an app only REFERENCES them through sys_app_nav_items (join table), so one entity can appear
-- in many apps and deleting an app never touches data. There is deliberately NO app_id FK on
-- sys_entities (the Airtable-container dead-end).
--
-- INV-1: this migration is DDL-ONLY — zero seed rows. A tenant with 0 apps keeps today's
-- sidebar behaviour untouched (implicit "All Items").
-- INV-5: tenant_id on BOTH tables; every repository query filters it.
-- No hard FKs on tenant_id/app_id/ref_id — logical refs validated at the service layer
-- (same convention as 000035 sys_views / 000024). ref_id is polymorphic
-- (sys_entities.id | sys_dashboards.id per item_type), cascade-cleaned in the owning
-- repo transactions (INV-4), not by the database.
CREATE TABLE IF NOT EXISTS sys_apps (
    id          CHAR(36)     NOT NULL,
    tenant_id   CHAR(36)     NOT NULL,
    name        VARCHAR(100) NOT NULL,            -- slug (a-z0-9_), identity for ?app=<name>
    label       VARCHAR(100) NOT NULL,            -- display name
    description TEXT         DEFAULT NULL,
    icon        VARCHAR(50)  DEFAULT NULL,        -- lucide icon name (same pattern as sys_entities.options.icon)
    color       VARCHAR(20)  DEFAULT NULL,        -- brand tone token
    is_default  TINYINT(1)   NOT NULL DEFAULT 0,  -- app opened after login (≤1/tenant, enforced in service tx)
    sort_order  INT          NOT NULL DEFAULT 0,  -- position in the App Switcher
    options     JSON         DEFAULT NULL,        -- v2 headroom: headerColor, logoUrl, landingNavItemId
    created_by  CHAR(36)     DEFAULT NULL,
    updated_by  CHAR(36)     DEFAULT NULL,
    created_at  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at  TIMESTAMP(3) DEFAULT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_sys_apps_tenant_name (tenant_id, name),
    KEY idx_sys_apps_lookup (tenant_id, deleted_at, sort_order)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS sys_app_nav_items (
    id         CHAR(36)    NOT NULL,
    tenant_id  CHAR(36)    NOT NULL,
    app_id     CHAR(36)    NOT NULL,
    item_type  VARCHAR(20) NOT NULL,              -- v1: 'entity' | 'dashboard'
    ref_id     CHAR(36)    NOT NULL,              -- sys_entities.id | sys_dashboards.id (logical ref, no hard FK)
    label      VARCHAR(100) DEFAULT NULL,         -- tab-label override (NULL = use the entity/dashboard label)
    sort_order INT         NOT NULL DEFAULT 0,
    created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_app_nav_item (app_id, item_type, ref_id),
    KEY idx_app_nav (tenant_id, app_id, sort_order)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
