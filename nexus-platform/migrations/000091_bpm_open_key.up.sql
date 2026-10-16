-- specs/043-bpm-process-engine — Migration B `bpm_open_key` (lát S9a `bpm-open-key-durable-arm`,
-- plan.md §"Migration B"). Head before this migration is v90 (000090_record_shares_team).
-- MySQL 8 / InnoDB / utf8mb4. Idempotent: every DDL is guarded (information_schema), so a re-run
-- after a failed or forced run changes nothing twice.
--
-- WHAT IT ADDS (sys_approval_instances)
--   lane            VARCHAR(16) NOT NULL DEFAULT 'exclusive'
--                     'exclusive' = an approval (at most ONE open per record, across every process);
--                     'independent' = a tracking lane (S12b: at most one per process per record).
--                     Every row that exists today is an approval, hence the default.
--   open_key        VARCHAR(180) GENERATED ALWAYS AS (CASE WHEN status='pending' THEN
--                     CONCAT(entity_name,'/',record_id,'/',IF(lane='exclusive','x',process_id)) END) STORED
--                     NULL once the instance settles, so a finished approval never blocks the next.
--   uq_ai_open      UNIQUE (tenant_id, open_key)  — the database, not a check-then-insert, says
--                     "this record is already waiting". NULLs do not collide.
--   idempotency_key VARCHAR(128) NULL, request_hash CHAR(64) NULL, UNIQUE uq_ai_idem
--                     (tenant_id, idempotency_key) — read from S9b (a repeated submit call).
--
-- PRE-CHECK — this migration REFUSES (SIGNAL 'BPM-B ABORT') when a tenant already holds two PENDING
-- instances of one record (the old start-up race): it never deletes or cancels data. Nexus withdraws
-- one of each pair by hand (the Doctor check D4_APPROVAL_DUPLICATE_PENDING lists them) and re-runs.
-- Run it on a copy first; adding a STORED column rebuilds the table (ALGORITHM=COPY) — run it off-peak.
--
-- NO `DELIMITER` — the procedure is one statement for the multi-statement driver (precedent 000070).
DROP PROCEDURE IF EXISTS _bpm_b_up;
CREATE PROCEDURE _bpm_b_up()
BEGIN
    DECLARE dup INT DEFAULT 0;
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_instances' AND COLUMN_NAME = 'open_key') THEN
        SELECT COUNT(*) INTO dup FROM (
            SELECT 1 FROM sys_approval_instances WHERE status = 'pending'
            GROUP BY tenant_id, entity_name, record_id HAVING COUNT(*) > 1) d;
        IF dup > 0 THEN
            SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT =
                'BPM-B ABORT: records with 2 PENDING approvals exist; withdraw one of each (Doctor D4) and re-run';
        END IF;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_instances' AND COLUMN_NAME = 'lane') THEN
        ALTER TABLE sys_approval_instances
            ADD COLUMN lane            VARCHAR(16)  NOT NULL DEFAULT 'exclusive',
            ADD COLUMN idempotency_key VARCHAR(128) NULL DEFAULT NULL,
            ADD COLUMN request_hash    CHAR(64)     NULL DEFAULT NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_instances' AND COLUMN_NAME = 'open_key') THEN
        ALTER TABLE sys_approval_instances
            ADD COLUMN open_key VARCHAR(180) GENERATED ALWAYS AS (CASE WHEN status = 'pending'
                THEN CONCAT(entity_name, '/', record_id, '/', IF(lane = 'exclusive', 'x', process_id)) END) STORED;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.STATISTICS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_instances' AND INDEX_NAME = 'uq_ai_open') THEN
        ALTER TABLE sys_approval_instances ADD UNIQUE KEY uq_ai_open (tenant_id, open_key);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.STATISTICS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_instances' AND INDEX_NAME = 'uq_ai_idem') THEN
        ALTER TABLE sys_approval_instances ADD UNIQUE KEY uq_ai_idem (tenant_id, idempotency_key);
    END IF;
END;
CALL _bpm_b_up();
DROP PROCEDURE IF EXISTS _bpm_b_up;
