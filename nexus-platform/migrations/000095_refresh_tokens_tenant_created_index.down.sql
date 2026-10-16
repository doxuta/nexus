-- specs/066 L5 (M3) revert: drop the sign-in counting index (guarded, idempotent).
DROP PROCEDURE IF EXISTS _u94_rt_tenant_created_down;
CREATE PROCEDURE _u94_rt_tenant_created_down()
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.STATISTICS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'refresh_tokens' AND INDEX_NAME = 'idx_rt_tenant_created') THEN
        ALTER TABLE refresh_tokens DROP INDEX idx_rt_tenant_created;
    END IF;
END;
CALL _u94_rt_tenant_created_down();
DROP PROCEDURE IF EXISTS _u94_rt_tenant_created_down;
