-- Reverse APPROVAL-REQUEST's trigger widening.
--
-- Narrowing an ENUM is destructive when rows hold the value being removed: MySQL would silently
-- truncate every 'manual' process to '' and those processes would then arm on NOTHING — invisible
-- breakage rather than a loud failure. So refuse while any such row exists and say what to do.
--
-- NO `DELIMITER` — golang-migrate does not support it (it splits on ';'). The proc body is one
-- statement to the driver; CALL then DROP, exactly like migration 000019's guard.
DROP PROCEDURE IF EXISTS _ap63_guard_manual;
CREATE PROCEDURE _ap63_guard_manual()
BEGIN
    DECLARE n INT DEFAULT 0;
    SELECT COUNT(*) INTO n FROM sys_approval_processes
        WHERE trigger_event = 'manual' AND deleted_at IS NULL;
    IF n > 0 THEN
        SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT =
            'AP63 ABORT: manual approval processes still exist; delete/repoint before down-migrating';
    END IF;
END;
CALL _ap63_guard_manual();
DROP PROCEDURE _ap63_guard_manual;

ALTER TABLE sys_approval_processes
    MODIFY COLUMN trigger_event ENUM('after_create','after_update') NOT NULL;
