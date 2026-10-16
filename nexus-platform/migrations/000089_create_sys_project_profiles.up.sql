-- specs/062-customer-workspace L5 (DAILY-QUEUE #377): the project registry — one row per customer
-- workspace holding what the workshop knows ABOUT the project that the tenant row does not: its
-- industry, how it was created, the hand-set "Chạy thử" mark and the last health snapshot.
--
--   * tenant_id is the CUSTOMER the profile describes (uq_project_profiles_tenant: one profile per
--     customer). The rows are written by a builder of the workshop, never by the customer.
--   * There is deliberately NO delivered_at: "Đã giao" is the first numbered publish of the
--     draft/publish tier (sys_meta_versions), read at list time — a stored copy could only drift.
--   * Stage (building | trial | live | needs_attention) is DERIVED, never stored.
--   * health_json = {critical, warn, checks:[{checkId,severity,count}], appsNotReady:[{appId,name,reasons}],
--     ops?}. health_status is the tri-state the stage reads; `ops` never feeds it.
--   * No FK (sys_* convention). Backup class: KeepExport (tenantbackup/policy.go) — the phase of a
--     project must not rewind when its data is restored.
-- MySQL 8 / InnoDB / utf8mb4. Idempotent.
CREATE TABLE IF NOT EXISTS sys_project_profiles (
    id                CHAR(36)     NOT NULL,
    tenant_id         CHAR(36)     NOT NULL,
    industry          VARCHAR(64)  NULL DEFAULT NULL,
    origin            VARCHAR(16)  NULL DEFAULT NULL,            -- pack | blank | ai
    origin_pack       VARCHAR(64)  NULL DEFAULT NULL,
    trial_since       TIMESTAMP(3) NULL DEFAULT NULL,
    health_status     VARCHAR(16)  NULL DEFAULT NULL,            -- ok | warn | critical
    health_json       JSON         NULL,
    health_checked_at TIMESTAMP(3) NULL DEFAULT NULL,
    created_by        CHAR(36)     NOT NULL DEFAULT '',
    updated_by        CHAR(36)     NOT NULL DEFAULT '',
    created_at        TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at        TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at        TIMESTAMP(3) NULL DEFAULT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_project_profiles_tenant (tenant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
