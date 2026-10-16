-- E9 AI-Builder M3 — sys_ai_sessions + sys_ai_messages (docs/e9-ai-builder-nl-metadata.md §11).
--
-- Server-saved conversation sessions so a builder can resume a modelling chat after a reload
-- (M2 held the history client-side and lost it on refresh). A "session" is just a named thread of
-- one builder's conversation — it is NOT a project silo: entities still land in the one shared
-- tenant model (Nexus = 1 tenant = 1 app/customer).
--
-- ⚠️ TWO STORES, TWO PURPOSES (spec §11):
--   * sys_ai_sessions / sys_ai_messages  → store the RAW conversation text. This is the tenant's
--     OWN data (like any obj_* record), scoped per (tenant_id, user_id), and it MUST be raw so the
--     chat can be resumed and its draft-cards re-rendered.
--   * sys_ai_runs (mig 000036)           → the audit/cost LEDGER. It stays HASH-ONLY (prompt_hash),
--     never raw, and is UNCHANGED by this migration. Different purpose (prove-a-turn-happened +
--     token cost), different retention profile — do not conflate the two.
--
-- Multi-tenant + per-user invariant: EVERY read/write of these tables filters tenant_id AND (for
-- sessions) user_id — a session belongs to exactly one builder; another user's/tenant's session is
-- invisible (a guessed id resolves to not-found, never a 403 that would leak existence, spec §7).
--
-- Idempotent (CREATE TABLE IF NOT EXISTS) per rules/database.md. MySQL 8 / InnoDB / utf8mb4.

CREATE TABLE IF NOT EXISTS sys_ai_sessions (
    id          CHAR(36)     NOT NULL,
    tenant_id   CHAR(36)     NOT NULL,                                   -- owning tenant (from JWT, never client body)
    user_id     CHAR(36)     NOT NULL,                                   -- owning builder — session is per (tenant, user)
    title       VARCHAR(200) NOT NULL DEFAULT '',                        -- auto-derived from the first user message (rune-truncated, no LLM)
    created_at  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),      -- bumped on every appended message (drives newest-first list)
    deleted_at  TIMESTAMP(3) DEFAULT NULL,                               -- soft-delete marker (a removed session keeps its rows)
    PRIMARY KEY (id),
    -- The session list reads "non-deleted sessions of (tenant, user)" → tenant_id + user_id lead the
    -- primary access path; the per-id get also leads with tenant_id + user_id (ownership guard).
    KEY idx_ai_sessions_tenant_user (tenant_id, user_id),
    -- Backs the newest-first ordering of a user's session list.
    KEY idx_ai_sessions_updated (updated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS sys_ai_messages (
    id          CHAR(36)     NOT NULL,
    session_id  CHAR(36)     NOT NULL,                                   -- parent session (logical ref; ownership checked via sys_ai_sessions)
    tenant_id   CHAR(36)     NOT NULL,                                   -- denormalized for defense-in-depth tenant filtering
    seq         INT          NOT NULL,                                   -- 0-based turn order within the session (append-only)
    role        VARCHAR(16)  NOT NULL,                                   -- 'user' | 'assistant' (server-written only — never a client role)
    content     MEDIUMTEXT   DEFAULT NULL,                               -- raw message text (may be empty for a max_steps assistant turn)
    draft_json  JSON         DEFAULT NULL,                               -- assistant turn's proposed draft[] (NULL = no draft) → re-rendered on resume
    created_at  TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    -- Every message read is "the messages of one session, in turn order" → (session_id, seq) covers it.
    KEY idx_ai_messages_session_seq (session_id, seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
