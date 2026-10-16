-- E9 AI-Builder M4 (docs/e9-ai-builder-nl-metadata.md §12, MED-1 reviewer finding) — composite
-- index for the daily-budget hot path.
--
-- SQLRunRepository.TokensSince (internal/ai/runs.go) runs
--   SELECT COALESCE(SUM(...)) FROM sys_ai_runs WHERE tenant_id = ? AND created_at >= ?
-- on EVERY gated chat turn (BudgetGuard.Acquire step 3). The single-column idx_tenant (000036)
-- narrows to the tenant's rows but still scans/sorts by created_at for the range predicate; a
-- composite (tenant_id, created_at) index lets MySQL seek straight to the day boundary within the
-- tenant's rows, keeping the per-turn budget check O(rows-today) instead of O(rows-tenant-ever).
--
-- MySQL 8 does not support `ADD INDEX IF NOT EXISTS`, so this is wrapped in the same
-- stored-procedure existence guard used by 000026/000019 (idempotent re-run safe).
DROP PROCEDURE IF EXISTS _airuns39_apply;
CREATE PROCEDURE _airuns39_apply()
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.STATISTICS
        WHERE TABLE_SCHEMA = DATABASE()
          AND TABLE_NAME   = 'sys_ai_runs'
          AND INDEX_NAME   = 'idx_tenant_created'
    ) THEN
        ALTER TABLE sys_ai_runs
            ADD INDEX idx_tenant_created (tenant_id, created_at);
    END IF;
END;
CALL _airuns39_apply();
DROP PROCEDURE IF EXISTS _airuns39_apply;
