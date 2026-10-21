-- Issue #42: which proposal cards of an assistant turn the builder has settled in the Studio — applied,
-- discarded or undone. An MCP thread replays its earlier proposals into the draft of every new tool call,
-- so an agent may build on them as within one chat turn; a settled card must not be among them. Applied,
-- it is in the live schema (or the builder's drafts) that the validators read anyway; discarded or undone,
-- it is nothing. A card whose apply failed stays pending (the Studio offers it again).
--
-- settled_json is a JSON array of the indexes of draft_json's items that are settled; NULL = none. Written
-- only by ai.SessionStore.SettleProposal (POST /admin/ai/builder/sessions/:id/settled).
--
-- ONE ALTER statement, guarded by information_schema (MySQL 8 has no ADD COLUMN IF NOT EXISTS): DDL
-- auto-commits, so a single statement cannot leave a half-applied (dirty) migration.
DROP PROCEDURE IF EXISTS _aimsg100_apply;
CREATE PROCEDURE _aimsg100_apply()
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_ai_messages' AND COLUMN_NAME = 'settled_json'
    ) THEN
        ALTER TABLE sys_ai_messages ADD COLUMN settled_json JSON DEFAULT NULL AFTER draft_json;
    END IF;
END;
CALL _aimsg100_apply();
DROP PROCEDURE IF EXISTS _aimsg100_apply;
