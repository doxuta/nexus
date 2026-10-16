-- CONG-TIN-HIEU (docs/inbound-hooks.md): per-tenant HMAC-signed inbound webhooks.
CREATE TABLE IF NOT EXISTS sys_inbound_hooks (
    id          CHAR(36)     NOT NULL PRIMARY KEY,
    tenant_id   CHAR(36)     NOT NULL,
    name        VARCHAR(64)  NOT NULL,
    entity_name VARCHAR(64)  NOT NULL,
    secret      VARCHAR(128) NOT NULL,
    mapping     JSON         NOT NULL,
    match_field VARCHAR(64)  DEFAULT NULL,
    is_active   TINYINT(1)   NOT NULL DEFAULT 1,
    created_by  CHAR(36)     NOT NULL,
    created_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at  TIMESTAMP    NULL DEFAULT NULL,
    UNIQUE KEY uq_hook (tenant_id, name),
    INDEX idx_tenant (tenant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS sys_hook_deliveries (
    id           CHAR(36)      NOT NULL PRIMARY KEY,
    tenant_id    CHAR(36)      NOT NULL,
    hook_id      CHAR(36)      NOT NULL,
    status       VARCHAR(16)   NOT NULL,
    record_id    CHAR(36)      DEFAULT NULL,
    error        VARCHAR(500)  DEFAULT NULL,
    payload_head VARCHAR(2048) DEFAULT NULL,
    created_at   TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_hook (tenant_id, hook_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
