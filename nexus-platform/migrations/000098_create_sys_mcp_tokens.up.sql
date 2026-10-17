-- sys_mcp_tokens: scoped credentials for the MCP endpoint (issue #3). A builder issues one so an
-- outside agent (Claude Code, Claude Desktop) can call the builder agent's read and propose tools at
-- /mcp, and nothing else: the auth middleware of /api/v1 refuses the token, and /mcp has no tool that
-- applies, publishes or writes data. Every call runs as the issuing builder, and every proposal lands
-- in ONE assistant thread of that builder (session_id → sys_ai_sessions, feature 'builder'), where a
-- human applies it to drafts and publishes it.
--
-- Hash-only storage, as sys_api_keys: token_hash = SHA-256 hex of the FULL raw `nxm_…` token, shown
-- once at issue and never stored; token_prefix = the public identification slice shown in the list.
-- expires_at is required and at most 8 h after issue (services.MCPTokenService); revocation is soft
-- (revoked_at, the row stays). last_used_at is written on each verified request. No hard FK on
-- tenant_id / user_id / session_id: logical references checked in the service, like sys_api_keys.
-- MySQL 8 / InnoDB / utf8mb4. Idempotent.
CREATE TABLE IF NOT EXISTS sys_mcp_tokens (
    id           CHAR(36)     NOT NULL,
    tenant_id    CHAR(36)     NOT NULL,
    user_id      CHAR(36)     NOT NULL,              -- the builder who issued it; every call runs as them
    session_id   CHAR(36)     NOT NULL,              -- sys_ai_sessions.id: the thread proposals land in
    token_prefix VARCHAR(16)  NOT NULL,              -- public identification slice (never the secret)
    token_hash   CHAR(64)     NOT NULL,              -- SHA-256 hex of the full raw token (nxm_...)
    expires_at   TIMESTAMP(3) NOT NULL,
    revoked_at   TIMESTAMP(3) NULL DEFAULT NULL,
    last_used_at TIMESTAMP(3) NULL DEFAULT NULL,
    created_at   TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at   TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_sys_mcp_tokens_hash (token_hash),
    KEY idx_sys_mcp_tokens_user (tenant_id, user_id, revoked_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
