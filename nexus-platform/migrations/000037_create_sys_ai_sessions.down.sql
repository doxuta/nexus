-- Reverse of 000037: drop the AI-Builder session store. Child (messages) first, then the parent
-- (sessions). Idempotent (IF EXISTS) so a repeated down never errors. sys_ai_runs is a SEPARATE
-- ledger (mig 000036) and is intentionally NOT touched here.
DROP TABLE IF EXISTS sys_ai_messages;
DROP TABLE IF EXISTS sys_ai_sessions;
