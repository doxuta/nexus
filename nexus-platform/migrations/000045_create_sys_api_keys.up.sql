-- sys_api_keys: machine identity (APIKEY-1, production-ops slice c). An API key authenticates a
-- SERVER (not a human) as a (tenant_id, role_id) principal: the composite Bearer verifier maps a
-- presented `nxk_...` credential to auth.Claims{TenantID, RoleID, IsAdmin:false} and the ENTIRE
-- existing authorization stack (RequireTenant → per-action ACL → field-ACL → tenant-pinned repos)
-- applies unchanged. IsAdmin is hard-floored to false — a key can never reach /admin/*.
--
-- Hash-only storage, mirroring sso_handoff_tokens / mfa_pending_tokens / password_reset_tokens:
--   * key_hash = SHA-256 hex of the FULL raw key; the raw key is shown ONCE at creation and never
--     persisted — a DB leak yields no usable credential. UNIQUE(key_hash) makes the verify lookup
--     unambiguous and constant-work (indexed point read).
--   * key_prefix = the public, non-secret identification slice (first chars of the random part,
--     displayed as `nxk_<prefix>…` in the admin list) so an admin can tell keys apart / match a
--     leaked prefix to a row WITHOUT the secret. Indexed for support lookups; NOT unique (72-bit
--     prefix collisions are harmless — identity rides on key_hash alone).
--
-- Revocation is SOFT (revoked_at) so the row stays for audit; expiry is optional (NULL = never).
-- last_used_at is TIMESTAMP(3), written by a throttled write-behind (services.APIKeyService), not
-- per-request. No hard FK on role_id/tenant_id — logical refs validated at the service layer
-- (same convention as 000035 sys_views / 000042 sys_apps). MySQL 8 / InnoDB / utf8mb4. Idempotent.
CREATE TABLE IF NOT EXISTS sys_api_keys (
    id           CHAR(36)     NOT NULL,
    tenant_id    CHAR(36)     NOT NULL,
    role_id      CHAR(36)     NOT NULL,              -- the (in-tenant) role whose grants the key acts with
    name         VARCHAR(100) NOT NULL,              -- admin-facing label ("CI importer", "Zapier bridge")
    key_prefix   VARCHAR(16)  NOT NULL,              -- public identification slice (never the secret)
    key_hash     CHAR(64)     NOT NULL,              -- SHA-256 hex of the FULL raw key (nxk_...)
    created_by   CHAR(36)     DEFAULT NULL,          -- admin user who issued the key
    last_used_at TIMESTAMP(3) NULL DEFAULT NULL,     -- throttled write-behind touch on verify
    expires_at   TIMESTAMP(3) NULL DEFAULT NULL,     -- NULL = no expiry
    revoked_at   TIMESTAMP(3) NULL DEFAULT NULL,     -- soft revoke; row kept for audit
    created_at   TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at   TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_sys_api_keys_hash (key_hash),
    KEY idx_sys_api_keys_tenant (tenant_id, revoked_at),
    KEY idx_sys_api_keys_prefix (key_prefix)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
