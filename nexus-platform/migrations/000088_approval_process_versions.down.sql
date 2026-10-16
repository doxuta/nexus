-- Reverse specs/043 Migration C `approval_process_versions`. ROLL THE BINARY BACK FIRST: a binary
-- of S10 or later reads sys_approval_process_versions and the version / draft columns on every
-- process read and write.
--
-- What is lost:
--   * sys_approval_process_versions — the history of published definitions. The sys_approval_processes
--     row keeps the ACTIVE one (it is the denormalised copy), so nothing that RUNS is lost; the
--     older versions are.
--   * sys_approval_processes.version / draft* — refused below while a live process still holds an
--     unpublished draft: dropping the column would erase unpublished work without a word.
--   * sys_approval_instances.process_version and idx_ai_version. The pinned process_snapshot (the
--     authority for what an instance executes) is untouched.
--
-- Idempotent: every statement is guarded, so a re-run after a partial failure is safe. NO
-- `DELIMITER` — same proc-as-one-statement pattern as 000070.
DROP PROCEDURE IF EXISTS _bpm_c_down;
CREATE PROCEDURE _bpm_c_down()
BEGIN
    DECLARE n INT DEFAULT 0;
    IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_processes' AND COLUMN_NAME = 'draft') THEN
        SELECT COUNT(*) INTO n FROM sys_approval_processes WHERE deleted_at IS NULL AND draft IS NOT NULL;
        IF n > 0 THEN
            SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT =
                'BPM-C ABORT: approval processes hold unpublished drafts; publish or discard them before down-migrating';
        END IF;
        ALTER TABLE sys_approval_processes
            DROP COLUMN version,
            DROP COLUMN draft,
            DROP COLUMN draft_updated_by,
            DROP COLUMN draft_updated_at;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.STATISTICS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_instances' AND INDEX_NAME = 'idx_ai_version') THEN
        ALTER TABLE sys_approval_instances DROP KEY idx_ai_version;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_instances' AND COLUMN_NAME = 'process_version') THEN
        ALTER TABLE sys_approval_instances DROP COLUMN process_version;
    END IF;
END;
CALL _bpm_c_down();
DROP PROCEDURE IF EXISTS _bpm_c_down;

DROP TABLE IF EXISTS sys_approval_process_versions;
