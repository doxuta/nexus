-- Reversal of 000039_sys_ai_runs_budget_index.up.sql — drops the composite budget-query index.
-- idx_tenant (single-column, from 000036) is untouched; it existed before v39 and is not owned by
-- this migration.
--
-- MySQL 8 does NOT support `ALTER TABLE ... DROP INDEX IF EXISTS` (verified: rejected with a
-- syntax error, unlike MariaDB) — same idempotency constraint documented in 000019/000026 — so the
-- drop is guarded by the same information_schema existence check inside a stored procedure.
DROP PROCEDURE IF EXISTS _airuns39_revert;
CREATE PROCEDURE _airuns39_revert()
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.STATISTICS
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME   = 'sys_ai_runs'
          AND INDEX_NAME   = 'idx_tenant_created'
    ) THEN
        ALTER TABLE sys_ai_runs DROP INDEX idx_tenant_created;
    END IF;
END;
CALL _airuns39_revert();
DROP PROCEDURE IF EXISTS _airuns39_revert;
