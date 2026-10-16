-- ENG-AUTH2 #7: MFA / TOTP (RFC 6238) — enrollment, backup codes, two-step login pending
-- token (docs/eng-buildout-plan.md §"#7").
--
-- user_mfa: one row per enrolled user. secret_enc is the AES-256-GCM SEALED (reversible, NOT
-- hashed — internal/crypto/aesgcm.go) base32 TOTP seed; it must be reversible because the
-- verify algorithm needs the raw seed back (a one-way hash cannot support that, unlike a
-- password). enabled=0 while a secret has been generated but not yet confirmed with a first
-- code (POST /auth/mfa/enroll); flips to 1 once activated (POST /auth/mfa/activate).
-- last_used_step is the RFC 6238 30s time-step of the most recently ACCEPTED code — NOT
-- wall-clock "now" — the replay guard: resubmitting a code whose step is <= last_used_step is
-- rejected via an atomic conditional UPDATE even though it can still be inside the +-1 step
-- skew window that would otherwise revalidate it. UNIQUE(tenant_id,user_id): exactly one MFA
-- configuration per user.
--
-- mfa_backup_codes: up to 10 single-use recovery codes generated at activation time. Only the
-- SHA-256 hash is stored (unlike the TOTP seed, a backup code is a bearer credential consumed
-- once and never needs to be reversed — same pattern as refresh_tokens/password_reset_tokens).
-- used_at is the single-use marker (NULL = still usable), flipped by an atomic conditional
-- UPDATE (CAS) exactly like password_reset_tokens.used_at.
--
-- mfa_pending_tokens: the short-lived (5 min), single-use, OPAQUE bridge between "password
-- verified" and "MFA code verified" in the two-step login flow. It is DELIBERATELY its own
-- table and NOT a JWT/refresh token: reusing the refresh-token store (or minting a real JWT)
-- would make the pending credential independently redeemable at POST /auth/refresh, bypassing
-- the second factor entirely (docs/eng-buildout-plan.md §"#7" MUST-FIX). Mirrors
-- password_reset_tokens exactly: only the SHA-256 hash is persisted, UNIQUE(token_hash) so a
-- hash collision is a DB error rather than a silent cross-tenant hit, and consumed via the
-- same single-use conditional UPDATE.
--
-- All three tables are tenant-pinned; every query filters tenant_id (rules/database.md).
-- MySQL 8 / InnoDB / utf8mb4. Idempotent (CREATE TABLE IF NOT EXISTS) per rules/database.md.

CREATE TABLE IF NOT EXISTS user_mfa (
    id             CHAR(36)   NOT NULL PRIMARY KEY,
    tenant_id      CHAR(36)   NOT NULL,
    user_id        CHAR(36)   NOT NULL,
    secret_enc     TEXT       NOT NULL,               -- AES-256-GCM sealed TOTP seed (reversible)
    enabled        TINYINT(1) NOT NULL DEFAULT 0,
    last_used_step BIGINT     NOT NULL DEFAULT 0,      -- RFC 6238 time-step of last ACCEPTED code (replay guard)
    created_at     TIMESTAMP  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMP  NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uq_user_mfa_tenant_user (tenant_id, user_id),
    INDEX idx_user_mfa_tenant (tenant_id),
    CONSTRAINT fk_user_mfa_user   FOREIGN KEY (user_id)   REFERENCES users (id)   ON DELETE CASCADE,
    CONSTRAINT fk_user_mfa_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS mfa_backup_codes (
    id         CHAR(36)  NOT NULL PRIMARY KEY,
    tenant_id  CHAR(36)  NOT NULL,
    user_id    CHAR(36)  NOT NULL,
    code_hash  CHAR(64)  NOT NULL,               -- SHA-256 hex digest of the raw backup code
    used_at    TIMESTAMP NULL DEFAULT NULL,       -- single-use marker; NULL = still usable
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_mfa_backup_tenant_user (tenant_id, user_id),
    CONSTRAINT fk_mfa_backup_user   FOREIGN KEY (user_id)   REFERENCES users (id)   ON DELETE CASCADE,
    CONSTRAINT fk_mfa_backup_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS mfa_pending_tokens (
    id         CHAR(36)  NOT NULL PRIMARY KEY,
    tenant_id  CHAR(36)  NOT NULL,
    user_id    CHAR(36)  NOT NULL,
    token_hash CHAR(64)  NOT NULL,               -- SHA-256 hex digest of the raw opaque pending token
    expires_at TIMESTAMP NOT NULL,
    used_at    TIMESTAMP NULL DEFAULT NULL,       -- single-use marker; NULL = still usable
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uq_mfa_pending_token_hash (token_hash),
    INDEX idx_mfa_pending_tenant (tenant_id),
    INDEX idx_mfa_pending_user (user_id),
    CONSTRAINT fk_mfa_pending_user   FOREIGN KEY (user_id)   REFERENCES users (id)   ON DELETE CASCADE,
    CONSTRAINT fk_mfa_pending_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
