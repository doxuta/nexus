-- specs/066 L4 (M2): per-day usage counters of a tenant (rule runs, mail, outbound/inbound webhooks).
-- One row per (tenant, calendar day in APP_TIMEZONE, metric, dim). The roller overwrites today and
-- yesterday and back-fills the 28 days before them once after boot, so a day's figure outlives the
-- retention janitor that deletes the source rows (sys_workflow_runs / sys_jobs / sys_hook_deliveries).
-- A statistics table written by the system: no deleted_at / created_by (precedent sys_workflow_runs).
CREATE TABLE IF NOT EXISTS sys_usage_daily (
    id         CHAR(36)     NOT NULL PRIMARY KEY,
    tenant_id  CHAR(36)     NOT NULL,
    day        DATE         NOT NULL,
    metric     VARCHAR(48)  NOT NULL,
    dim        VARCHAR(100) NOT NULL DEFAULT '',
    value      BIGINT       NOT NULL,
    created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    UNIQUE KEY uq_usage_daily (tenant_id, day, metric, dim),
    KEY idx_usage_day (day)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- The inbound-hook counters read sys_hook_deliveries by tenant + day; its only index leads with
-- (tenant_id, hook_id). Covering (status rides along). In-place build, the table stays writable.
-- NO `DELIMITER` — one statement for the multi-statement driver (precedent 000070/000091).
DROP PROCEDURE IF EXISTS _usage_daily_up;
CREATE PROCEDURE _usage_daily_up()
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.STATISTICS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_hook_deliveries' AND INDEX_NAME = 'idx_hookdel_tenant_created') THEN
        ALTER TABLE sys_hook_deliveries
            ADD KEY idx_hookdel_tenant_created (tenant_id, created_at, status), ALGORITHM=INPLACE, LOCK=NONE;
    END IF;
END;
CALL _usage_daily_up();
DROP PROCEDURE IF EXISTS _usage_daily_up;
