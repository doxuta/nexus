-- specs/043-bpm-process-engine — Migration A `bpm_runtime_v2` (lát S3a `bpm-seats-dual-write`,
-- plan.md §"Migration A"). Head before this migration is v69 (000069_create_sys_idempotency_keys).
-- MySQL 8 / InnoDB / utf8mb4. Idempotent: every DDL is guarded (information_schema / IF NOT EXISTS)
-- and every data statement only touches rows it has not already handled — re-running it after a
-- failed or forced run changes nothing twice.
--
-- WHAT IT ADDS
--   sys_approval_processes  + graph JSON, options JSON      — graph-shaped definitions and process
--                                                             options (written from S3a2; NULL here).
--   sys_approval_instances  + seq INT UNSIGNED NOT NULL DEFAULT 0
--                                                           — the CAS counter of D-4. Bumped by EVERY
--                                                             state write from this slice on (the
--                                                             S3a binary), although authority stays
--                                                             the old current_step + escalated CAS
--                                                             until S3b: a rolling S3a→S3b deploy runs
--                                                             both binaries, and a step the S3a binary
--                                                             moved without bumping seq would let an
--                                                             S3b decision on the old seq win — two
--                                                             steps for one click.
--                          + context JSON, last_error VARCHAR(500),
--                            sla_paused_at TIMESTAMP(3), sla_remaining_seconds INT UNSIGNED
--                                                           — run state of later slices (NULL here).
--   sys_bpm_tasks                                           — WORK SEATS: who holds the step an
--                                                             instance stands on, per visit. The unit a
--                                                             parallel step (S4), a work inbox (S5)
--                                                             and a pool claim will lock on. This slice
--                                                             WRITES seats beside every instance write
--                                                             (same transaction) and READS none: the
--                                                             inbox still reads current_approver_*.
--   Every sys_bpm_tasks index leads with tenant_id (Điều II) — including the breach scan, unlike the
--   cross-tenant idx_ai_sla of 000021. No hard FK (precedent 000021: instance/record ids are logical
--   references). A seat is ended by its status, never soft-deleted; deleted_at exists because every
--   transactional table carries it. Tenant purge finds the table by its tenant_id column
--   (TenantProvisionService.tenantScopedTables) — nothing to register.
--
-- DATA — order matters: pins first, seats second (a seat's node id is read off the pin).
--   (a) Every PENDING instance with no pin (armed before 000065) is pinned from its process row
--       WITHOUT the deleted_at filter. Until now such an instance decided through the LIVE process,
--       and GetProcess filters deleted_at — so soft-deleting the process made it undecidable. The pin
--       is marked "backfilled": true and every step in it gets slaClock "wall": that instance was
--       timed on the wall clock and finishes on it (D-9), even if the live process was saved after
--       S1 with slaClock "business". An instance whose process row is gone altogether stays unpinned.
--   (b) Every PENDING instance with a projected approver gets ONE open seat (entry_seq 0 = the visit
--       current at seq 0):
--         node_id   = the id its PIN gives the current step, else s{current_step+1} (a pin written
--                     before S0 has no ids; after S0 ids survive reorders, so the position can name
--                     the wrong step — the same answer bpm.NormalizeSteps gives on the read side);
--         due_at    = sla_due_at;
--         due_basis = what S1 recorded, in bpm.Instance.CurrentSLABasis order: the newest history
--                     entry carrying slaBasis, else the pin's firstDueBasis, else 'wall'
--                     (NULL when there is no deadline);
--         breach_notified_at = set iff the instance is already escalated (the exact instant lives in
--                     the history; updated_at stands in for it);
--         created_at = the instance's updated_at — its last state write: the step entry or the
--                     delegation that handed it to the current holder (off by the SLA after an
--                     escalation).
--       An instance with NO projected approver gets no seat: the read-side fallback of D-6 owns it,
--       exactly like an instance a pre-S3a binary arms during a rolling deploy.
--
-- ROLLBACK — read before running the down file
--   * Roll the BINARY back first. The S3a binary writes seq and sys_bpm_tasks on every approval
--     write: on the pre-A schema the arming path fails fail-open (logged, no instance — records
--     silently stop entering approval, the same trap 000065 warns about), and submit / decide /
--     recall / delegate return errors.
--   * A binary older than S3a on THIS schema keeps working, but moves instances without touching
--     seats or seq: seats go stale (the next S3a write closes every open seat of the instance and
--     re-opens the right one) — the read path of S3a2 must not trust a seat whose node / holder no
--     longer matches its instance (specs/043 spec.md "Nợ còn mở" #21).
--   * The down file DROPS every seat — after parallel steps (S4) or pool claims (S5) that is
--     destructive state, not a mirror. It refuses to drop graph/options while a live process
--     uses them (like 000064's outcome guard). It does NOT un-pin the instances (a) pinned: that
--     pin is the only record of which chain they executed (000065's header), and a binary of any
--     age decides correctly through it — un-pinning would hand them back to the live process,
--     deleted-process failure included.

DROP PROCEDURE IF EXISTS _bpm_a_columns;
CREATE PROCEDURE _bpm_a_columns()
BEGIN
    -- One ALTER per table: DDL is atomic in MySQL 8, so a table has all of its columns or none,
    -- and one sentinel column is a sound guard. Appended last (no AFTER) = INSTANT.
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_processes' AND COLUMN_NAME = 'graph') THEN
        ALTER TABLE sys_approval_processes
            ADD COLUMN graph   JSON NULL DEFAULT NULL,
            ADD COLUMN options JSON NULL DEFAULT NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_instances' AND COLUMN_NAME = 'seq') THEN
        ALTER TABLE sys_approval_instances
            ADD COLUMN seq                   INT UNSIGNED NOT NULL DEFAULT 0,
            ADD COLUMN context               JSON         NULL DEFAULT NULL,
            ADD COLUMN last_error            VARCHAR(500) NULL DEFAULT NULL,
            ADD COLUMN sla_paused_at         TIMESTAMP(3) NULL DEFAULT NULL,
            ADD COLUMN sla_remaining_seconds INT UNSIGNED NULL DEFAULT NULL;
    END IF;
END;
CALL _bpm_a_columns();
DROP PROCEDURE IF EXISTS _bpm_a_columns;

CREATE TABLE IF NOT EXISTS sys_bpm_tasks (
    id                 CHAR(36)     NOT NULL,
    tenant_id          CHAR(36)     NOT NULL,
    instance_id        CHAR(36)     NOT NULL,                          -- sys_approval_instances.id
    process_id         CHAR(36)     NOT NULL,                          -- sys_approval_processes.id
    entity_name        VARCHAR(100) NOT NULL,                          -- record under approval
    record_id          CHAR(36)     NOT NULL,
    node_id            VARCHAR(64)  NOT NULL,                          -- stable step id (S0)
    entry_seq          INT UNSIGNED NOT NULL DEFAULT 0,                -- instance seq on entering the step = the visit
    kind               VARCHAR(16)  NOT NULL DEFAULT 'approval',       -- approval | task | rework
    assignee_type      VARCHAR(16)  NOT NULL,                          -- user | role | team | queue
    assignee_id        CHAR(36)     NOT NULL,
    claimed_by         CHAR(36)     NULL DEFAULT NULL,                 -- pool claim (S5)
    claimed_at         TIMESTAMP(3) NULL DEFAULT NULL,
    status             VARCHAR(16)  NOT NULL DEFAULT 'open',           -- open | done | cancelled
    outcome            VARCHAR(16)  NULL DEFAULT NULL,                 -- approved | rejected | reassigned | recalled | …
    decided_by         CHAR(36)     NULL DEFAULT NULL,                 -- who acted on a 'done' seat
    comment            MEDIUMTEXT   NULL,                              -- their note: uncapped upstream, TEXT would 1406
    due_at             TIMESTAMP(3) NULL DEFAULT NULL,                 -- this visit's deadline
    due_basis          VARCHAR(24)  NULL DEFAULT NULL,                 -- business | wall | wall_no_calendar | wall_calendar_error
    breach_notified_at TIMESTAMP(3) NULL DEFAULT NULL,                 -- breach actioned (per seat from S3c)
    delegated_from     CHAR(36)     NULL DEFAULT NULL,                 -- user who handed this seat on
    version            INT UNSIGNED NOT NULL DEFAULT 0,                -- seat CAS (S3b)
    created_by         CHAR(36)     NULL DEFAULT NULL,
    created_at         TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at         TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    completed_at       TIMESTAMP(3) NULL DEFAULT NULL,
    deleted_at         TIMESTAMP(3) NULL DEFAULT NULL,
    PRIMARY KEY (id),
    KEY idx_bt_inbox   (tenant_id, status, assignee_type, assignee_id, created_at), -- "my work" (S3a2/S5)
    KEY idx_bt_claimed (tenant_id, status, claimed_by),                             -- claimed pool work (S5)
    KEY idx_bt_visit   (tenant_id, instance_id, node_id, entry_seq),                -- seats of a visit; every dual write
    KEY idx_bt_record  (tenant_id, entity_name, record_id),                         -- a record's seats
    KEY idx_bt_breach  (tenant_id, status, breach_notified_at, due_at),             -- breach scan, tenant-pinned (S3b/S3c)
    KEY idx_bt_done    (tenant_id, decided_by, completed_at),                       -- "done by me" (S5)
    KEY idx_bt_process (tenant_id, process_id, status)                              -- impact / repin (S10/S11)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- (a) Pin every pending pre-000065 instance from its process row — deleted or not. Assigning
-- updated_at to itself keeps ON UPDATE from stamping the migration time over its last state write.
UPDATE sys_approval_instances i
    JOIN sys_approval_processes p ON p.tenant_id = i.tenant_id AND p.id = i.process_id
SET i.process_snapshot = IF(p.outcome IS NULL,
        JSON_OBJECT('steps', p.steps, 'backfilled', CAST('true' AS JSON)),
        JSON_OBJECT('steps', p.steps, 'outcome', p.outcome, 'backfilled', CAST('true' AS JSON))),
    i.updated_at = i.updated_at
WHERE i.status = 'pending' AND i.process_snapshot IS NULL;

-- (a') ...on the wall clock: slaClock "wall" on every step of a pin (a) wrote. A loop over the step
-- index because MySQL 8 has no ordered JSON_ARRAYAGG to map an array in place.
DROP PROCEDURE IF EXISTS _bpm_a_wall_clock;
CREATE PROCEDURE _bpm_a_wall_clock()
BEGIN
    DECLARE i INT DEFAULT 0;
    DECLARE n INT DEFAULT 0;
    SELECT COALESCE(MAX(JSON_LENGTH(process_snapshot, '$.steps')), 0) INTO n
        FROM sys_approval_instances
        WHERE JSON_EXTRACT(process_snapshot, '$.backfilled') = CAST('true' AS JSON);
    WHILE i < n DO
        UPDATE sys_approval_instances
            SET process_snapshot = JSON_SET(process_snapshot, CONCAT('$.steps[', i, '].slaClock'), 'wall'),
                updated_at = updated_at
            WHERE JSON_EXTRACT(process_snapshot, '$.backfilled') = CAST('true' AS JSON)
              AND JSON_LENGTH(process_snapshot, '$.steps') > i;
        SET i = i + 1;
    END WHILE;
END;
CALL _bpm_a_wall_clock();
DROP PROCEDURE IF EXISTS _bpm_a_wall_clock;

-- (b) One open seat per pending instance that has a projected approver and no seat yet.
INSERT INTO sys_bpm_tasks
    (id, tenant_id, instance_id, process_id, entity_name, record_id, node_id, entry_seq, kind,
     assignee_type, assignee_id, status, due_at, due_basis, breach_notified_at, version,
     created_by, created_at, updated_at)
SELECT UUID(), i.tenant_id, i.id, i.process_id, i.entity_name, i.record_id,
       COALESCE(JSON_UNQUOTE(JSON_EXTRACT(i.process_snapshot, CONCAT('$.steps[', i.current_step, '].id'))),
                CONCAT('s', i.current_step + 1)),
       0, 'approval',
       i.current_approver_type, i.current_approver_id, 'open', i.sla_due_at,
       CASE WHEN i.sla_due_at IS NULL THEN NULL ELSE COALESCE(
           (SELECT h.basis FROM JSON_TABLE(i.history, '$[*]' COLUMNS (
                n FOR ORDINALITY, basis VARCHAR(24) PATH '$.slaBasis')) h
            WHERE h.basis IS NOT NULL AND h.basis <> '' ORDER BY h.n DESC LIMIT 1),
           NULLIF(JSON_UNQUOTE(JSON_EXTRACT(i.process_snapshot, '$.firstDueBasis')), ''),
           'wall') END,
       CASE WHEN i.escalated = 1 THEN i.updated_at END,
       0, NULL, i.updated_at, i.updated_at
FROM sys_approval_instances i
WHERE i.status = 'pending'
  AND i.current_approver_type IS NOT NULL AND i.current_approver_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM sys_bpm_tasks t WHERE t.tenant_id = i.tenant_id AND t.instance_id = i.id);
