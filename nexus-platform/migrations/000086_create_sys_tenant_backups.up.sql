-- sys_tenant_backups: the catalogue of per-customer backups (specs/068-tenant-backup-restore, slice A2).
-- One row = one zip on TENANT_BACKUP_DIR (<tenant>/<id>.zip) holding ONE customer's data. The same row
-- doubles as the restore record of a `pre_restore` backup (restore_* columns, slice D); the columns are
-- here from the start so the table is created once.
--
--   * op_lock is a VIRTUAL generated column = tenant_id while the row is a LIVE running operation
--     (a backup writing, or a restore in flight) and NULL otherwise. UNIQUE ignores NULLs, so the
--     DATABASE — not an app lock, which is lost across replicas — guarantees one running operation
--     per customer; the loser gets 1062 on uq_tbk_oplock => 409 BACKUP_IN_PROGRESS. The app never
--     writes op_lock (3105), the GORM struct does not declare it.
--   * uq_tbk_slot (tenant_id, schedule_slot): slice E's "one scheduled backup per day / per schema
--     version" without a lock; NULL slots (manual backups) never collide.
--   * updated_at is the HEARTBEAT: a running backup bumps it; a row with a live op_lock and no
--     heartbeat for 30 minutes is reaped to failed (BACKUP_ABANDONED).
--   * error_detail is table name + code only, never a data value.
--
-- Tenant-pinned like every fixed system table (tenant purge deletes by tenant_id). No FK (sys_*
-- convention). MySQL 8 / InnoDB / utf8mb4. Idempotent.
CREATE TABLE IF NOT EXISTS sys_tenant_backups (
    id                   CHAR(36)     NOT NULL,
    tenant_id            CHAR(36)     NOT NULL,                  -- the customer backed up
    kind                 VARCHAR(16)  NOT NULL,                  -- manual | scheduled | pre_restore
    status               VARCHAR(16)  NOT NULL DEFAULT 'running',-- running | ready | failed
    note                 VARCHAR(200) NULL,
    storage_key          VARCHAR(255) NULL,                      -- <tenant>/<id>.zip on TENANT_BACKUP_DIR
    size_bytes           BIGINT       NULL,
    sha256               CHAR(64)     NULL,                      -- the whole zip
    format_version       INT          NOT NULL,
    schema_version       BIGINT       NOT NULL,                  -- MAX(schema_migrations.version) in the snapshot
    row_count            BIGINT       NULL,
    table_count          INT          NULL,
    file_count           INT          NULL,
    blob_bytes           BIGINT       NULL,
    warnings             JSON         NULL,                      -- [{code,count,sample[<=10]}]
    error_code           VARCHAR(64)  NULL,
    error_detail         VARCHAR(500) NULL,
    schedule_slot        VARCHAR(32)  NULL,                      -- 'd:2026-09-30' | 'v:77' (slice E)
    restore_source_id    CHAR(36)     NULL,                      -- only kind = pre_restore (slice D)
    restore_status       VARCHAR(16)  NULL,                      -- running | done | failed
    restore_step         VARCHAR(16)  NULL,                      -- lock | prebackup | apply | doctor
    restore_committed_at TIMESTAMP(3) NULL,
    restore_report       JSON         NULL,
    restore_finished_at  TIMESTAMP(3) NULL,
    actor_tenant_id      CHAR(36)     NULL,                      -- the OPERATOR's tenant (the workshop)
    created_by           CHAR(36)     NULL,                      -- NULL = schedule
    created_at           TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at           TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    finished_at          TIMESTAMP(3) NULL,
    deleted_at           TIMESTAMP(3) NULL,
    op_lock              CHAR(36) AS (IF(deleted_at IS NULL AND (status = 'running' OR restore_status = 'running'),
                                         tenant_id, NULL)) VIRTUAL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_tbk_oplock (op_lock),
    UNIQUE KEY uq_tbk_slot (tenant_id, schedule_slot),
    KEY idx_tbk_tenant_created (tenant_id, created_at),
    CONSTRAINT chk_tbk_kind CHECK (kind IN ('manual','scheduled','pre_restore')),
    CONSTRAINT chk_tbk_status CHECK (status IN ('running','ready','failed')),
    CONSTRAINT chk_tbk_restore_status CHECK (restore_status IS NULL OR restore_status IN ('running','done','failed')),
    CONSTRAINT chk_tbk_restore_step CHECK (restore_step IS NULL OR restore_step IN ('lock','prebackup','apply','doctor'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
