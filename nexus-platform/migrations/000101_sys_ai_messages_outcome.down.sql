-- Reverse 000101: drop sys_ai_messages.outcome. A reopened session then shows a declined turn as an empty
-- answer again, and its next turn replays the declined prompt (the behaviour before issue #30 item 5).
DROP PROCEDURE IF EXISTS _aimsg101_revert;
CREATE PROCEDURE _aimsg101_revert()
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_ai_messages' AND COLUMN_NAME = 'outcome'
    ) THEN
        ALTER TABLE sys_ai_messages DROP COLUMN outcome;
    END IF;
END;
CALL _aimsg101_revert();
DROP PROCEDURE IF EXISTS _aimsg101_revert;
