-- Reverts 000092: the usage figures go with their table; the hook-delivery index is dropped
-- (inbound-hook counters then fall back to the (tenant_id, hook_id) index — correct, slower).
DROP PROCEDURE IF EXISTS _usage_daily_down;
CREATE PROCEDURE _usage_daily_down()
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.STATISTICS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_hook_deliveries' AND INDEX_NAME = 'idx_hookdel_tenant_created') THEN
        ALTER TABLE sys_hook_deliveries DROP KEY idx_hookdel_tenant_created;
    END IF;
END;
CALL _usage_daily_down();
DROP PROCEDURE IF EXISTS _usage_daily_down;
DROP TABLE IF EXISTS sys_usage_daily;
