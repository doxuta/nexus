-- ENG-AUTH2 #1: forgot/reset-password backend (docs/eng-buildout-plan.md §"#1").
--
-- password_reset_tokens stores the SHA-256 hash of each issued reset token (never the raw
-- value, mirroring refresh_tokens/000003) plus its owning tenant + user, expiry, and
-- single-use consumption marker (used_at).
--
-- UNIQUE(token_hash) is a MUST-FIX (adversarial verify #1, item 2): a hash collision must be a
-- DB error (rejected insert), never a silent cross-tenant write into an existing row. Reset-
-- password requests carry {token,newPassword} with NO tenant, so the consuming query resolves
-- the owning tenant FROM the row (FindByHash is tenant-agnostic) rather than trusting a
-- client-supplied tenant; the uniqueness of token_hash is what makes that lookup unambiguous.
--
-- MySQL 8 / InnoDB / utf8mb4. Idempotent (CREATE TABLE IF NOT EXISTS) per rules/database.md.

CREATE TABLE IF NOT EXISTS password_reset_tokens (
    id         CHAR(36)  NOT NULL PRIMARY KEY,
    tenant_id  CHAR(36)  NOT NULL,
    user_id    CHAR(36)  NOT NULL,
    token_hash CHAR(64)  NOT NULL,               -- SHA-256 hex digest of the raw reset token
    expires_at TIMESTAMP NOT NULL,
    used_at    TIMESTAMP NULL DEFAULT NULL,       -- single-use marker; NULL = still usable
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uq_prt_token_hash (token_hash),
    INDEX idx_prt_tenant (tenant_id),
    INDEX idx_prt_user (user_id),
    CONSTRAINT fk_prt_user   FOREIGN KEY (user_id)   REFERENCES users (id)   ON DELETE CASCADE,
    CONSTRAINT fk_prt_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
