-- Reverse APPROVAL-OUTCOME. Dropping the column loses any configured write-back, so warn loudly
-- rather than silently turning every approval back into a dead-end note.
--
-- NO `DELIMITER` (golang-migrate splits on ';') — same proc-as-one-statement pattern as 000019.
DROP PROCEDURE IF EXISTS _ap64_guard_outcome;
CREATE PROCEDURE _ap64_guard_outcome()
BEGIN
    DECLARE n INT DEFAULT 0;
    SELECT COUNT(*) INTO n FROM sys_approval_processes
        WHERE outcome IS NOT NULL AND deleted_at IS NULL;
    IF n > 0 THEN
        SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT =
            'AP64 ABORT: approval processes with a configured outcome still exist; clear them before down-migrating';
    END IF;
END;
CALL _ap64_guard_outcome();
DROP PROCEDURE _ap64_guard_outcome;

ALTER TABLE sys_approval_processes DROP COLUMN outcome;
