-- sys_named_credentials: the named-credential vault (specs/050-named-credentials, lát S1a). One row
-- = one way to call a partner system — base URL, auth type, the non-secret parameters of that type,
-- and the secret SEALED (AES-256-GCM, "v1:<keyId>:<b64>") under the master keyring
-- CREDENTIAL_ENC_KEY / CREDENTIAL_ENC_RETIRED_KEYS, which lives OUTSIDE the database.
--
--   * Write-only: no API ever returns secret_enc / prev_secret_enc / extra_headers_enc or the
--     plaintext; secret_hint keeps the last 4 characters when the secret is >= 16 long.
--   * Every envelope of a row is sealed under the SAME master key = key_id (precheck, reseal and
--     Doctor count rows per key_id), bound to AAD "tenant_id|id|slot" so an envelope copied onto
--     another row, tenant or slot never opens.
--   * secret_enc NULL = a "slot" waiting for the customer admin to enter the secret (needs_secret).
--     Status is derived (needs_secret / key_unavailable / grace / ready), never stored.
--   * name is unique per tenant among LIVE rows: name_live is NULL once deleted_at is set and a
--     UNIQUE index ignores NULLs — a deleted key frees its name (pack reinstall) while a double
--     click still gets 1062 (409 CREDENTIAL_NAME_TAKEN). The app never writes name_live (3105).
--   * Delete = soft delete + crypto-shred in ONE UPDATE (every *_enc, secret_hint, key_id to NULL);
--     the row stays only for the audit trail.
--   * Rules reference a key by id inside sys_workflows.actions JSON ({credentialId, path}) — no
--     foreign key (a dangling id is reported at send time as CREDENTIAL_NOT_FOUND).
--
-- Tenant-pinned like every fixed system table (tenant purge picks it up via tenant_id).
-- MySQL 8 / InnoDB / utf8mb4. Idempotent.
CREATE TABLE IF NOT EXISTS sys_named_credentials (
    id                 CHAR(36)     NOT NULL,
    tenant_id          CHAR(36)     NOT NULL,
    name               VARCHAR(64)  NOT NULL,             -- slug ^[a-z][a-z0-9_-]{0,63}$, immutable
    label              VARCHAR(150) NOT NULL,
    base_url           VARCHAR(500) NOT NULL,             -- https://host[:port][/prefix]; url_token has {secret}
    auth_type          VARCHAR(16)  NOT NULL,             -- header | basic | hmac | url_token (checked in Go)
    header_name        VARCHAR(100) NULL,                 -- type header; default Authorization
    header_prefix      VARCHAR(32)  NULL,                 -- type header; default "Bearer "
    username           VARCHAR(200) NULL,                 -- type basic (not secret)
    secret_enc         TEXT         NULL,                 -- v1:<keyId>:<b64>; NULL = slot waiting for its secret
    secret_hint        VARCHAR(8)   NULL,                 -- last 4 characters, only when the secret is >= 16 long
    prev_secret_enc    TEXT         NULL,                 -- hmac: previous key during its grace period
    prev_expires_at    TIMESTAMP(3) NULL,
    extra_headers_enc  TEXT         NULL,                 -- envelope of JSON {name: value}, <= 10 headers / 4KB
    extra_header_names JSON         NULL,                 -- names only, for display and audit
    key_id             VARCHAR(32)  NULL,                 -- master key sealing EVERY envelope of the row
    rotated_at         TIMESTAMP(3) NULL,
    created_by         CHAR(36)     NULL,
    updated_by         CHAR(36)     NULL,
    created_at         TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at         TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at         TIMESTAMP(3) NULL,
    name_live          VARCHAR(64) AS (IF(deleted_at IS NULL, name, NULL)) VIRTUAL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_named_cred_name (tenant_id, name_live),
    KEY idx_named_cred_tenant (tenant_id, deleted_at),
    KEY idx_named_cred_key (tenant_id, key_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
