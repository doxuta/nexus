-- A1 auth schema (spec: docs/auth-security.md §2 A1).
-- Replaces the "client-declared tenant via X-Tenant-Id" anti-pattern with a real
-- auth boundary: signed-JWT login resolves {tenant slug, email, password} -> tenant_id,
-- and refresh tokens are stored hashed (never raw). MySQL 8 / InnoDB / utf8mb4.

-- tenants: lets login {tenant, email, password} resolve a slug -> tenant id.
CREATE TABLE IF NOT EXISTS tenants (
    id         CHAR(36)     NOT NULL PRIMARY KEY,
    slug       VARCHAR(64)  NOT NULL,
    name       VARCHAR(150) NOT NULL,
    status     VARCHAR(20)  NOT NULL DEFAULT 'active',
    created_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uq_tenants_slug (slug)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- users: one identity per (tenant_id, email). password_hash is bcrypt; role_id is a
-- forward-reference to acl_roles (introduced in A4) so it stays a plain nullable column
-- with no FK until that table exists.
CREATE TABLE IF NOT EXISTS users (
    id            CHAR(36)     NOT NULL PRIMARY KEY,
    tenant_id     CHAR(36)     NOT NULL,
    email         VARCHAR(255) NOT NULL,
    password_hash VARCHAR(255) NOT NULL,
    full_name     VARCHAR(150) DEFAULT NULL,
    status        VARCHAR(20)  NOT NULL DEFAULT 'active',  -- active/disabled/invited
    is_admin      TINYINT(1)   NOT NULL DEFAULT 0,         -- super-gate for /admin/* (A4 swaps to role)
    role_id       CHAR(36)     DEFAULT NULL,               -- FK acl_roles (A4)
    created_at    TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    deleted_at    TIMESTAMP    NULL DEFAULT NULL,
    UNIQUE KEY uq_users_tenant_email (tenant_id, email),
    INDEX idx_users_tenant (tenant_id),
    INDEX idx_users_deleted (deleted_at),
    CONSTRAINT fk_users_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- refresh_tokens: stores the SHA-256 hash of each refresh token (never the raw value),
-- supports rotation (insert new + revoke old) and revocation (logout / password change).
CREATE TABLE IF NOT EXISTS refresh_tokens (
    id         CHAR(36)  NOT NULL PRIMARY KEY,
    user_id    CHAR(36)  NOT NULL,
    tenant_id  CHAR(36)  NOT NULL,
    token_hash CHAR(64)  NOT NULL,               -- SHA-256 hex digest of the refresh token
    expires_at TIMESTAMP NOT NULL,
    revoked_at TIMESTAMP NULL DEFAULT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_rt_user (user_id),
    INDEX idx_rt_hash (token_hash),
    INDEX idx_rt_tenant (tenant_id),
    CONSTRAINT fk_rt_user   FOREIGN KEY (user_id)   REFERENCES users (id)   ON DELETE CASCADE,
    CONSTRAINT fk_rt_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Seed: the demo tenant only, matching the tenant_id 000002's reference data already uses.
-- A tenant row carries no authority on its own — it is a namespace, not a credential.
INSERT INTO tenants (id, slug, name, status) VALUES
    ('00000000-0000-0000-0000-000000000001', 'demo', 'Demo Tenant', 'active')
    ON DUPLICATE KEY UPDATE slug = slug;

-- SEED ADMIN REMOVED (issue #95). This file used to also INSERT
-- 'admin@demo.local' with is_admin=1 and the bcrypt hash of "ChangeMe!123" — a password
-- committed to source, in a public-ish repo, applied by EVERY `migrate up` against a fresh
-- database. That includes the first ./scripts/deploy.sh on a new VPS: the site comes up on a
-- public HTTPS domain with a super-admin whose credential anyone who has seen this repo knows.
-- GO-LIVE-CHECKLIST.md §5 mitigated it with a MANUAL step that necessarily runs AFTER the site
-- is already live, and the window reopened on every rebuild-from-scratch.
--
-- Deleted rather than gated: SQL cannot read an env var, so a "SEED_DEMO_ADMIN"-style flag would
-- have to be a marker table, and a gate that can be switched on is a gate that gets left on. The
-- one supported way to create the first admin is now the operator tool, which takes the password
-- from the environment and never writes one into source:
--
--   docker compose -f deploy/nexus/compose.prod.yml --env-file deploy/nexus/.env.prod run --rm \
--     -e BOOTSTRAP_TENANT=acme -e BOOTSTRAP_ADMIN_EMAIL=admin@acme.com \
--     -e BOOTSTRAP_ADMIN_PASSWORD='a-strong-password' --entrypoint /app/bootstrap-admin nexus
--
-- Editing an already-applied migration is normally forbidden here (.claude/rules/database.md).
-- It is correct in this one case: golang-migrate stores no checksum, so this edit is invisible to
-- every database that already ran 000003 and changes ONLY what a fresh install creates. Existing
-- databases are handled by the forward migration 000066_remove_demo_admin_seed, which removes the
-- account only where its password is still the committed one.
