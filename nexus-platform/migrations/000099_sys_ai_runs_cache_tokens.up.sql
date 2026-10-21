-- Issue #30 item 8: the AI ledger keeps the prompt-cache part of every turn's input. Providers bill a
-- call's input in three parts — uncached, read from the prompt cache, written to it — and the cached
-- bulk is most of a builder call (~70 KB of persona and tool schemas). prompt_tokens holds ALL of it
-- (internal/ai.Usage normalizes the gateway's OpenAI or Anthropic usage shape so each token counts
-- once); these two columns say how much of prompt_tokens was a cache read and a cache write, so the
-- cost per app can be priced at each part's rate instead of read as a floor.
--
-- NULL = none reported, like the other token columns: every row before this migration, a turn that
-- never reached the gateway, and a gateway that reports no cache figures.
--
-- ONE ALTER statement, guarded by information_schema (MySQL 8 has no ADD COLUMN IF NOT EXISTS): DDL
-- auto-commits, so a single statement cannot leave a half-applied (dirty) migration.
DROP PROCEDURE IF EXISTS _airuns99_apply;
CREATE PROCEDURE _airuns99_apply()
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_ai_runs' AND COLUMN_NAME = 'cache_read_tokens'
    ) THEN
        ALTER TABLE sys_ai_runs
            ADD COLUMN cache_read_tokens  INT DEFAULT NULL AFTER output_tokens,
            ADD COLUMN cache_write_tokens INT DEFAULT NULL AFTER cache_read_tokens;
    END IF;
END;
CALL _airuns99_apply();
DROP PROCEDURE IF EXISTS _airuns99_apply;
