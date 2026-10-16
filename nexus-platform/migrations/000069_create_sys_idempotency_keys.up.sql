-- sys_idempotency_keys: the Idempotency-Key claims of POST /:entity (DAILY-QUEUE #29/#30,
-- specs/047-idempotency-key-on-create). A client that lost the connection before seeing the 201
-- resends the SAME request with the SAME key; the claim row turns that resend into a replay of the
-- first outcome instead of a second record.
--
--   * UNIQUE (tenant_id, principal_id, idem_key) IS the lock: the claim is an autocommit INSERT, so
--     a concurrent duplicate gets 1062 at once (409 IDEMPOTENCY_IN_PROGRESS) instead of waiting on
--     a row lock. principal_id = the verified Claims.UserID (a user id, or an API key id).
--   * idem_key is ascii_bin: keys are opaque, case-sensitive tokens (the API accepts visible ASCII
--     only, 1-255 chars); a _ci collation would fold "Abc" and "abc" into one claim.
--   * request_hash = hex SHA-256 of entity + NUL + the re-marshalled JSON body (key-order and
--     whitespace insensitive) — same key with a different body is 422 IDEMPOTENCY_KEY_REUSED.
--   * No response body is stored: a replay re-checks that the record still exists in the tenant and
--     answers only its id, so this table never holds a copy of personal data.
--   * expires_at is written by the application clock and compared against it (request path and the
--     janitor), so the driver's time-zone handling is symmetric. DATETIME, not TIMESTAMP: no session
--     time-zone conversion at all.
--   * KEY (expires_at) serves the janitor's bounded DELETE ... WHERE expires_at < ? LIMIT ?.
--
-- Tenant-pinned like every fixed system table. MySQL 8 / InnoDB / utf8mb4. Idempotent.
CREATE TABLE IF NOT EXISTS sys_idempotency_keys (
    id           CHAR(36)     NOT NULL PRIMARY KEY,
    tenant_id    CHAR(36)     NOT NULL,
    principal_id VARCHAR(64)  NOT NULL,
    idem_key     VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    entity_name  VARCHAR(64)  NOT NULL,
    request_hash CHAR(64)     NOT NULL,
    status       VARCHAR(16)  NOT NULL,              -- processing | completed
    record_id    CHAR(36)     NULL DEFAULT NULL,     -- set when status = completed
    created_at   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    expires_at   DATETIME(3)  NOT NULL,
    UNIQUE KEY uq_idem_tenant_principal_key (tenant_id, principal_id, idem_key),
    KEY idx_idem_expires (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
