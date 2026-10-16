-- DOWN of 000019: revert sys_workflows scheduler support (v19 → v17). MySQL 8 / InnoDB.
--
-- Order matters: drop the index and the two added columns, then shrink the ENUM back to the
-- three M1 event triggers. Shrinking the ENUM is only safe when NO row still carries the value
-- being removed ('scheduled') — otherwise MySQL would silently coerce those rows to '' (empty
-- string), corrupting them. So we ABORT LOUDLY (SIGNAL) when any 'scheduled' rule still exists,
-- forcing the operator to delete/repoint them first rather than losing data on a blind down.
-- The SIGNAL message is kept under 128 bytes (ER_DATA_TOO_LONG cap on SET MESSAGE_TEXT).
--
-- IDEMPOTENCY NOTE: MySQL 8 has no `DROP INDEX IF EXISTS` / `DROP COLUMN IF EXISTS` (MariaDB-only),
-- so every drop is guarded by an information_schema existence check inside a stored procedure — a
-- partial down re-run then never errors on an already-removed object.

-- Orphan-guard first: refuse to proceed while scheduled rules exist (the ENUM shrink below would
-- corrupt them). Done before any drop so a refused down leaves the schema fully intact.
DROP PROCEDURE IF EXISTS _wf19_guard_scheduled;
CREATE PROCEDURE _wf19_guard_scheduled()
BEGIN
    DECLARE n INT DEFAULT 0;
    SELECT COUNT(*) INTO n FROM sys_workflows WHERE trigger_event = 'scheduled';
    IF n > 0 THEN
        SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT =
            'WF19 ABORT: scheduled workflows still exist; delete/repoint before down-migrating';
    END IF;
END;
CALL _wf19_guard_scheduled();
DROP PROCEDURE _wf19_guard_scheduled;

-- Idempotent drops (information_schema-guarded): a partial down re-run never errors.
DROP PROCEDURE IF EXISTS _wf19_revert;
CREATE PROCEDURE _wf19_revert()
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.STATISTICS
        WHERE table_schema = DATABASE()
          AND table_name = 'sys_workflows'
          AND index_name = 'idx_wf_scheduled'
    ) THEN
        ALTER TABLE sys_workflows DROP INDEX idx_wf_scheduled;
    END IF;

    IF EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE table_schema = DATABASE()
          AND table_name = 'sys_workflows'
          AND column_name = 'last_run_at'
    ) THEN
        ALTER TABLE sys_workflows DROP COLUMN last_run_at;
    END IF;

    IF EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE table_schema = DATABASE()
          AND table_name = 'sys_workflows'
          AND column_name = 'interval_seconds'
    ) THEN
        ALTER TABLE sys_workflows DROP COLUMN interval_seconds;
    END IF;
END;
CALL _wf19_revert();
DROP PROCEDURE _wf19_revert;

-- Safe now: no row carries 'scheduled' (orphan-guard passed), so dropping it from the ENUM
-- coerces nothing. Re-applying the same shrink is a no-op so this is naturally idempotent.
ALTER TABLE sys_workflows
    MODIFY COLUMN trigger_event
        ENUM('after_create','after_update','after_delete') NOT NULL;
