-- specs/043-bpm-process-engine — Migration C `approval_process_versions` (lát S10
-- `bpm-process-versions`, plan.md §"Migration C"). Head before this migration is v87
-- (000087_workflow_runs_running_status). MySQL 8 / InnoDB / utf8mb4. Idempotent: every DDL is
-- guarded (information_schema / IF NOT EXISTS) and the backfill only touches processes that have
-- no version 1 yet — re-running it after a failed or forced run changes nothing twice.
--
-- WHAT IT ADDS
--   sys_approval_process_versions — one IMMUTABLE row per published definition of a process:
--       definition JSON = {name, entityName, trigger, criteria, steps, graph, outcome}. The
--       sys_approval_processes row stays the denormalised copy of the ACTIVE version, so the hot
--       path (ListActiveProcesses, idx_ap_lookup) and the pack export read exactly what they read
--       before. Every index leads with tenant_id (Điều II); no hard FK (precedent 000021).
--   sys_approval_processes  + version INT UNSIGNED NOT NULL DEFAULT 1   — the active version number
--                           + draft JSON, draft_updated_by, draft_updated_at
--                                                         — the unpublished edit (never read by the
--                                                           engine: a draft starts no approval).
--   sys_approval_instances  + process_version INT UNSIGNED NULL — the version an instance runs under
--                                                         (NULL = armed before this migration; the
--                                                         pinned process_snapshot is unchanged and
--                                                         stays the authority for what it executes).
--                           + idx_ai_version — "how many pending instances still run version N".
--
-- DATA
--   One version-1 row for every live process, built from its current columns. A soft-deleted process
--   gets none (it can no longer be edited or published). Instances already in flight keep
--   process_version NULL: they were armed under an unknown, possibly already-edited definition, so
--   "version 1" would be a claim nobody can prove — they count as "running an older version" (S11).
--
-- ROLLBACK — read before running the down file: roll the BINARY back first (a binary of S10 or
-- later writes version rows on every process save and publish).

DROP PROCEDURE IF EXISTS _bpm_c_columns;
CREATE PROCEDURE _bpm_c_columns()
BEGIN
    -- One ALTER per table: DDL is atomic in MySQL 8, so a table has all of its columns or none,
    -- and one sentinel column is a sound guard. Appended last (no AFTER) = INSTANT.
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_processes' AND COLUMN_NAME = 'version') THEN
        ALTER TABLE sys_approval_processes
            ADD COLUMN version          INT UNSIGNED NOT NULL DEFAULT 1,
            ADD COLUMN draft            JSON         NULL DEFAULT NULL,
            ADD COLUMN draft_updated_by CHAR(36)     NULL DEFAULT NULL,
            ADD COLUMN draft_updated_at TIMESTAMP(3) NULL DEFAULT NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_instances' AND COLUMN_NAME = 'process_version') THEN
        ALTER TABLE sys_approval_instances
            ADD COLUMN process_version INT UNSIGNED NULL DEFAULT NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.STATISTICS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_instances' AND INDEX_NAME = 'idx_ai_version') THEN
        ALTER TABLE sys_approval_instances
            ADD KEY idx_ai_version (tenant_id, process_id, status, process_version);
    END IF;
END;
CALL _bpm_c_columns();
DROP PROCEDURE IF EXISTS _bpm_c_columns;

CREATE TABLE IF NOT EXISTS sys_approval_process_versions (
    id         CHAR(36)     NOT NULL,
    tenant_id  CHAR(36)     NOT NULL,
    process_id CHAR(36)     NOT NULL,                                   -- sys_approval_processes.id
    version    INT UNSIGNED NOT NULL,                                   -- 1, 2, 3 … per process
    definition JSON         NOT NULL,                                   -- {name, entityName, trigger, criteria, steps, graph, outcome}
    note       VARCHAR(500) NULL DEFAULT NULL,                          -- the publisher's note
    created_by CHAR(36)     NULL DEFAULT NULL,                          -- who published it
    created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_by CHAR(36)     NULL DEFAULT NULL,
    updated_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at TIMESTAMP(3) NULL DEFAULT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_apv (tenant_id, process_id, version),
    KEY idx_apv_process (tenant_id, process_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Version 1 of every live process that has none yet, from its current columns.
INSERT INTO sys_approval_process_versions
    (id, tenant_id, process_id, version, definition, note, created_by, created_at, updated_by, updated_at)
SELECT UUID(), p.tenant_id, p.id, 1,
       JSON_OBJECT('name', p.name, 'entityName', p.entity_name, 'trigger', p.trigger_event,
                   'criteria', p.criteria, 'steps', p.steps, 'graph', p.graph, 'outcome', p.outcome),
       NULL, p.created_by, p.created_at, p.created_by, p.created_at
FROM sys_approval_processes p
WHERE p.deleted_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM sys_approval_process_versions v
                  WHERE v.tenant_id = p.tenant_id AND v.process_id = p.id AND v.version = 1);
