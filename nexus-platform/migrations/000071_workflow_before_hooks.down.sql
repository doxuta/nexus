-- DOWN of 000071: revert the specs/048 before-hooks ENUM widening (v71 → v70). MySQL 8 / InnoDB.
--
-- Shrinking an ENUM is only safe when NO row still carries a removed value — otherwise MySQL
-- silently coerces those rows to '' (empty string), corrupting them. Same orphan-guard discipline as
-- the 000019 / 000048 downs: ABORT LOUDLY (SIGNAL) while any before_save/before_delete rule (live or
-- soft-deleted — both would be coerced) or any 'blocked' run exists. Deleting those rows to get past
-- the guard on a real database is IRREVERSIBLE — ask the Founder first (campaign §6).

DROP PROCEDURE IF EXISTS _wf71_guard_before_hooks;
CREATE PROCEDURE _wf71_guard_before_hooks()
BEGIN
    DECLARE n INT DEFAULT 0;
    DECLARE r INT DEFAULT 0;
    SELECT COUNT(*) INTO n FROM sys_workflows WHERE trigger_event IN ('before_save','before_delete');
    SELECT COUNT(*) INTO r FROM sys_workflow_runs WHERE status = 'blocked';
    IF n > 0 OR r > 0 THEN
        SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT =
            'WF71 ABORT: before_save/before_delete workflows or blocked runs still exist; delete them before down-migrating';
    END IF;
END;
CALL _wf71_guard_before_hooks();
DROP PROCEDURE _wf71_guard_before_hooks;

-- Safe now: no row carries a removed value (orphan-guard passed), so the shrink coerces nothing.
-- Re-applying the same shrink is a no-op so this is naturally idempotent.
ALTER TABLE sys_workflows
    MODIFY COLUMN trigger_event
        ENUM('after_create','after_update','after_delete','scheduled','date_offset') NOT NULL;

ALTER TABLE sys_workflow_runs
    MODIFY COLUMN status ENUM('success','failed','skipped') NOT NULL;
