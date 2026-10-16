-- DOWN of 000048: revert the 'date_offset' timer trigger (v48 → v47). MySQL 8 / InnoDB.
--
-- Shrinking the ENUM is only safe when NO row still carries 'date_offset' — otherwise MySQL
-- would silently coerce those rows to '' (empty string), corrupting them. Same orphan-guard
-- discipline as the 000019 down: ABORT LOUDLY (SIGNAL) while date_offset rules exist, forcing
-- the operator to delete/repoint them first rather than losing data on a blind down.
-- (Parked 'workflow_timer' sys_jobs rows need no cleanup: their handler no-ops on a rule whose
-- trigger is no longer date_offset, and the jobs janitor sweeps them once finished.)

DROP PROCEDURE IF EXISTS _wf48_guard_date_offset;
CREATE PROCEDURE _wf48_guard_date_offset()
BEGIN
    DECLARE n INT DEFAULT 0;
    SELECT COUNT(*) INTO n FROM sys_workflows WHERE trigger_event = 'date_offset';
    IF n > 0 THEN
        SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT =
            'WF48 ABORT: date_offset workflows still exist; delete/repoint before down-migrating';
    END IF;
END;
CALL _wf48_guard_date_offset();
DROP PROCEDURE _wf48_guard_date_offset;

-- Safe now: no row carries 'date_offset' (orphan-guard passed), so dropping it from the ENUM
-- coerces nothing. Re-applying the same shrink is a no-op so this is naturally idempotent.
ALTER TABLE sys_workflows
    MODIFY COLUMN trigger_event
        ENUM('after_create','after_update','after_delete','scheduled') NOT NULL;
