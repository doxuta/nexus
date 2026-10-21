-- Reverse 000100: drop sys_ai_messages.settled_json. MCP threads then replay every earlier proposal again,
-- settled or not (the behaviour before issue #42).
DROP PROCEDURE IF EXISTS _aimsg100_revert;
CREATE PROCEDURE _aimsg100_revert()
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_ai_messages' AND COLUMN_NAME = 'settled_json'
    ) THEN
        ALTER TABLE sys_ai_messages DROP COLUMN settled_json;
    END IF;
END;
CALL _aimsg100_revert();
DROP PROCEDURE IF EXISTS _aimsg100_revert;
