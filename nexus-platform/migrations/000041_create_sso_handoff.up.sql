-- sso_handoff_tokens: the short-lived (default 120s), single-use, OPAQUE bridge between a
-- successful SSO /callback identity resolution and the frontend /exchange XHR that mints the
-- session (SSO-FE-REDIRECT). The browser is redirected (302) to the frontend carrying ONLY this
-- opaque code in the URL; the actual session tokens are minted at exchange time and never travel
-- through the URL, browser history, Referer, or server logs.
--
-- DELIBERATELY its own table, mirroring mfa_pending_tokens / password_reset_tokens exactly:
--   * only the SHA-256 hex digest of the raw code is persisted (a DB leak never yields a usable
--     code), UNIQUE(token_hash) so a hash collision is a DB error not a silent cross-tenant hit;
--   * single-use via the same atomic conditional UPDATE (used_at IS NULL AND expires_at > now);
--   * carries only (tenant_id, user_id) — the identity SSO already resolved. Session issuance
--     (and the local-MFA gate) is re-run at exchange from this row, so a handoff code is NOT a
--     session and cannot be redeemed at /auth/refresh.
--
-- Tenant-pinned like every fixed system table. MySQL 8 / InnoDB / utf8mb4. Idempotent.
CREATE TABLE IF NOT EXISTS sso_handoff_tokens (
    id          CHAR(36)  NOT NULL PRIMARY KEY,
    tenant_id   CHAR(36)  NOT NULL,
    user_id     CHAR(36)  NOT NULL,
    provider_id CHAR(36)  NOT NULL,              -- provider resolved at callback; re-loaded at exchange to honor trust_idp_mfa
    token_hash  CHAR(64)  NOT NULL,              -- SHA-256 hex digest of the raw opaque handoff code
    expires_at TIMESTAMP NOT NULL,
    used_at    TIMESTAMP NULL DEFAULT NULL,       -- single-use marker; NULL = still usable
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uq_sso_handoff_token_hash (token_hash),
    KEY idx_sso_handoff_expires (expires_at)      -- janitor purge of expired rows
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
