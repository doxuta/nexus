-- Reverse of 000036: drop the AI-run ledger. Idempotent (IF EXISTS) so a repeated down never errors.
DROP TABLE IF EXISTS sys_ai_runs;
