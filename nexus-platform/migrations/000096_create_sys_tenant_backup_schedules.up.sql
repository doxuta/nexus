-- sys_tenant_backup_schedules: per-customer automatic-backup setting (specs/068-tenant-backup-restore, slice E).
-- NO row = the default (enabled, keep the 7 newest scheduled backups): a customer is covered the moment it
-- exists, and a row appears only when the operator changes the setting (PUT …/backup-schedule is an upsert
-- on uq_tbs_tenant). Tenant-pinned like every fixed system table (the tenant purge deletes by tenant_id).
-- No FK (sys_* convention). MySQL 8 / InnoDB / utf8mb4. Idempotent.
CREATE TABLE IF NOT EXISTS sys_tenant_backup_schedules (
    id             CHAR(36)     NOT NULL,
    tenant_id      CHAR(36)     NOT NULL,
    enabled        TINYINT(1)   NOT NULL DEFAULT 1,
    keep_scheduled INT          NOT NULL DEFAULT 7,
    created_by     CHAR(36)     NULL,
    updated_by     CHAR(36)     NULL,
    created_at     TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at     TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at     TIMESTAMP(3) NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_tbs_tenant (tenant_id),
    CONSTRAINT chk_tbs_keep CHECK (keep_scheduled BETWEEN 1 AND 30)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
