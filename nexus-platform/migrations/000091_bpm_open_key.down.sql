-- Reverse specs/043 Migration B `bpm_open_key`. ROLL THE BINARY BACK FIRST: a binary of S9a or
-- later inserts through lane / open_key and reads uq_ai_open as "already waiting".
--
-- What is lost: the lane column (every row goes back to being an approval — there is no
-- independent-lane row before S12b), the open-key uniqueness, and the idempotency columns S9b
-- fills. No approval row is touched. Refused (BPM-B ABORT) while an 'independent' lane row exists:
-- dropping `lane` would silently turn a tracking instance into a second approval of its record.
-- Idempotent: every statement is guarded. NO `DELIMITER` (precedent 000070).
DROP PROCEDURE IF EXISTS _bpm_b_down;
CREATE PROCEDURE _bpm_b_down()
BEGIN
    DECLARE n INT DEFAULT 0;
    IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_instances' AND COLUMN_NAME = 'lane') THEN
        SELECT COUNT(*) INTO n FROM sys_approval_instances WHERE lane <> 'exclusive';
        IF n > 0 THEN
            SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT =
                'BPM-B ABORT: independent-lane approval instances exist; settle or delete them before down-migrating';
        END IF;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.STATISTICS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_instances' AND INDEX_NAME = 'uq_ai_idem') THEN
        ALTER TABLE sys_approval_instances DROP KEY uq_ai_idem;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.STATISTICS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_instances' AND INDEX_NAME = 'uq_ai_open') THEN
        ALTER TABLE sys_approval_instances DROP KEY uq_ai_open;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_instances' AND COLUMN_NAME = 'open_key') THEN
        ALTER TABLE sys_approval_instances DROP COLUMN open_key;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_instances' AND COLUMN_NAME = 'lane') THEN
        ALTER TABLE sys_approval_instances
            DROP COLUMN lane, DROP COLUMN idempotency_key, DROP COLUMN request_hash;
    END IF;
END;
CALL _bpm_b_down();
DROP PROCEDURE IF EXISTS _bpm_b_down;
