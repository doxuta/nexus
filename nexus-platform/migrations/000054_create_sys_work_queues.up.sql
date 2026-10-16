-- VIEC-HOM-NAY (docs/work-queues.md §2): role-based work queues — pure metadata composing the
-- existing list/aggregate/filter engine (incl. Wave 3.6 relative tokens in filters JSON).
--   role_id NULL = every user sees the queue; else only users carrying that role (admins see all).
--   action_verb is a v1 allowlist (open | approve) — deep-links only, no action framework.
CREATE TABLE IF NOT EXISTS sys_work_queues (
    id          CHAR(36)     NOT NULL PRIMARY KEY,
    tenant_id   CHAR(36)     NOT NULL,
    name        VARCHAR(64)  NOT NULL,
    label       VARCHAR(150) NOT NULL,
    entity_name VARCHAR(64)  NOT NULL,
    role_id     CHAR(36)     NULL,
    filters     JSON         NOT NULL,
    sort_field  VARCHAR(64)  NULL,
    sort_order  VARCHAR(4)   NOT NULL DEFAULT 'asc',
    action_verb VARCHAR(16)  NOT NULL DEFAULT 'open',
    position    INT          NOT NULL DEFAULT 0,
    is_active   TINYINT(1)   NOT NULL DEFAULT 1,
    created_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at  TIMESTAMP    NULL DEFAULT NULL,
    UNIQUE KEY uq_workqueue (tenant_id, name),
    INDEX idx_workqueue_tenant (tenant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
