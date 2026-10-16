-- specs/066 L5 (M3): covering index for "how many distinct people signed in on day D" =
-- COUNT(DISTINCT user_id) WHERE tenant_id = ? AND created_at in [from, to). refresh_tokens gets one
-- row per login and per refresh and is never purged (000076), so without it the daily count scans
-- the tenant's whole history. In-place build, the table stays writable. Guarded, idempotent.
DROP PROCEDURE IF EXISTS _u94_rt_tenant_created_up;
CREATE PROCEDURE _u94_rt_tenant_created_up()
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.STATISTICS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'refresh_tokens' AND INDEX_NAME = 'idx_rt_tenant_created') THEN
        ALTER TABLE refresh_tokens
            ADD INDEX idx_rt_tenant_created (tenant_id, created_at, user_id), ALGORITHM=INPLACE, LOCK=NONE;
    END IF;
END;
CALL _u94_rt_tenant_created_up();
DROP PROCEDURE IF EXISTS _u94_rt_tenant_created_up;
