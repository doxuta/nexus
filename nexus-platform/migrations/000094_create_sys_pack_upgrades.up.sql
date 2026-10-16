-- pack-apply-upgrade (specs/069, Phase 2 of specs/011-pack-provenance): one row per upgrade run.
-- Doubles as the per-(tenant, pack) run lock: running_lock = 1 while running, NULL when finished.
-- A UNIQUE key over a nullable column admits any number of finished rows but only ONE running row.
-- No pack content and no secrets are stored here: results carry type/key/code only.
CREATE TABLE IF NOT EXISTS sys_pack_upgrades (
    id             CHAR(36)     NOT NULL PRIMARY KEY,
    tenant_id      CHAR(36)     NOT NULL,
    pack_slug      VARCHAR(150) NOT NULL,
    from_versions  VARCHAR(255) NOT NULL DEFAULT '',
    to_version     VARCHAR(50)  NOT NULL,
    source         VARCHAR(16)  NOT NULL,
    mode           VARCHAR(16)  NOT NULL DEFAULT 'live',
    plan_hash      CHAR(64)     NOT NULL,
    status         VARCHAR(16)  NOT NULL,
    running_lock   TINYINT(1)   NULL DEFAULT 1,
    decisions      JSON         NOT NULL,
    results        JSON         NULL,
    counts         JSON         NULL,
    created_by     CHAR(36)     NOT NULL,
    started_at     DATETIME(3)  NOT NULL,
    finished_at    DATETIME(3)  NULL,
    created_at     TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at     TIMESTAMP    NULL,
    UNIQUE KEY uq_packupg_running (tenant_id, pack_slug, running_lock),
    INDEX idx_packupg_tenant_slug_started (tenant_id, pack_slug, started_at),
    -- <=> is null-safe: with `running_lock = 1` a ('running', NULL) row evaluates to NULL and a
    -- CHECK only rejects FALSE, so it would slip through.
    CONSTRAINT chk_packupg_lock CHECK ((status = 'running' AND running_lock <=> 1)
                                    OR (status <> 'running' AND running_lock IS NULL))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
