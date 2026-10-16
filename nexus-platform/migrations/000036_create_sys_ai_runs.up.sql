-- E9 AI-Builder M1 — sys_ai_runs (docs/e9-ai-builder-nl-metadata.md §5).
--
-- Append-only ledger: ONE row per POST /admin/ai/builder/chat turn. It is the factory's audit +
-- token-cost record for every LLM call the conversational AI Builder makes on behalf of a tenant.
-- There is deliberately NO draft table (a draft is transient — it lives only in the response until
-- the human applies it through the existing /admin/entities + /admin/fields APIs) and NO session
-- table (MVP holds the conversation history client-side and replays it each turn).
--
-- prompt_hash is sha256(last user message) — the raw prompt is NEVER stored. A user's message can
-- contain customer business data (the whole point of the discovery chat), so persisting it verbatim
-- would turn an audit ledger into a data-at-rest liability. The hash lets ops correlate/deduplicate
-- turns and prove a given prompt was seen without holding its content.
--
-- outcome enumerates how the turn ended: 'ok' (final assistant text), 'invalid_draft' (a proposal
-- failed spec validation and the agent could not recover), 'gateway_error' (9router returned non-2xx
-- / transport failure), 'timeout' (the per-turn deadline elapsed), 'max_steps' (the tool-loop hit
-- AI_MAX_STEPS and returned a partial draft). Every terminal path writes a row — a failed/timed-out
-- turn still costs latency (and possibly tokens), which is exactly what the factory needs to see.
--
-- Multi-tenant invariant: tenant_id pins every read of this table (idx_tenant leads the cost/usage
-- queries); idx_created_at backs time-window rollups. MySQL 8 / InnoDB, utf8mb4.
CREATE TABLE IF NOT EXISTS sys_ai_runs (
    id             CHAR(36)     NOT NULL,
    tenant_id      CHAR(36)     NOT NULL,                       -- owning tenant (from JWT, never client body)
    user_id        CHAR(36)     NOT NULL,                       -- verified actor who invoked the turn
    feature        VARCHAR(40)  NOT NULL,                       -- 'ai_builder.chat'
    model          VARCHAR(80)  NOT NULL,                       -- e.g. 'cc/claude-sonnet-4-6'
    prompt_hash    CHAR(64)     NOT NULL,                       -- sha256(last user msg) hex — raw prompt NEVER stored
    prompt_tokens  INT          DEFAULT NULL,                   -- from gateway usage (nullable: absent when the call errored pre-response)
    output_tokens  INT          DEFAULT NULL,
    tool_calls     INT          NOT NULL DEFAULT 0,             -- number of model-initiated tool executions in the turn
    outcome        VARCHAR(24)  NOT NULL,                       -- 'ok'|'invalid_draft'|'gateway_error'|'timeout'|'max_steps'
    latency_ms     INT          DEFAULT NULL,                   -- wall-clock of the whole turn
    created_at     TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    KEY idx_tenant (tenant_id),
    KEY idx_created_at (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
