-- ENG-AUTH2 #8: SSO / OIDC authorization-code login (docs/eng-buildout-plan.md §"#8").
--
-- sso_providers: one row per tenant-registered OIDC identity provider (Google/Azure/Okta/...).
-- client_secret_enc is the AES-256-GCM SEALED (reversible, NOT hashed — internal/crypto/aesgcm.go,
-- reused from ENG-AUTH2 #7) OAuth2 client secret; it must be reversible because the token-exchange
-- call needs the raw secret back to authenticate against the IdP. enabled gates BOTH admin
-- visibility and the public /start + /callback resolution (a disabled provider answers
-- SSO_PROVIDER_NOT_FOUND). jit_provisioning is the per-provider opt-in for auto-creating a local
-- user on first login from an unrecognized `sub` with no matching local email — defaults to 0
-- (off), the safe default (docs/eng-buildout-plan.md §"#8" CONFIRM WITH HUMAN item b).
-- UNIQUE(tenant_id, issuer): a tenant may register a given issuer at most once.
--
-- sso_identities: links one IdP-asserted subject (`sub`) under one provider to exactly one local
-- user, scoped to a tenant. UNIQUE(tenant_id, provider_id, subject) makes the callback resolution
-- lookup unambiguous. email is the IdP-asserted email AT LINK TIME (audit trail only) — every
-- subsequent login resolves strictly by (tenant_id, provider_id, subject), never re-consulted by
-- email (no implicit re-linking on email drift). ON DELETE CASCADE from sso_providers: removing a
-- provider revokes every session-origin tied to it. ON DELETE CASCADE from users: deleting a user
-- also removes their SSO links (mirrors user_mfa's fk_user_mfa_user).
--
-- Both tables are tenant-pinned; every query filters tenant_id (rules/database.md).
-- MySQL 8 / InnoDB / utf8mb4. Idempotent (CREATE TABLE IF NOT EXISTS) per rules/database.md.

CREATE TABLE IF NOT EXISTS sso_providers (
    id                CHAR(36)     NOT NULL PRIMARY KEY,
    tenant_id         CHAR(36)     NOT NULL,
    issuer            VARCHAR(512) NOT NULL,
    client_id         VARCHAR(255) NOT NULL,
    client_secret_enc TEXT         NOT NULL,          -- AES-256-GCM sealed OAuth2 client secret (reversible)
    display_name      VARCHAR(150) DEFAULT NULL,
    enabled           TINYINT(1)   NOT NULL DEFAULT 1,
    jit_provisioning  TINYINT(1)   NOT NULL DEFAULT 0, -- safe default: OFF
    created_at        TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uq_sso_providers_tenant_issuer (tenant_id, issuer),
    INDEX idx_sso_providers_tenant (tenant_id),
    CONSTRAINT fk_sso_providers_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS sso_identities (
    id          CHAR(36)     NOT NULL PRIMARY KEY,
    tenant_id   CHAR(36)     NOT NULL,
    provider_id CHAR(36)     NOT NULL,
    user_id     CHAR(36)     NOT NULL,
    subject     VARCHAR(255) NOT NULL,               -- OIDC `sub` claim
    email       VARCHAR(255) DEFAULT NULL,            -- IdP-asserted email AT LINK TIME (audit only)
    created_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uq_sso_identities_tenant_provider_subject (tenant_id, provider_id, subject),
    INDEX idx_sso_identities_tenant (tenant_id),
    INDEX idx_sso_identities_user (user_id),
    CONSTRAINT fk_sso_identities_provider FOREIGN KEY (provider_id) REFERENCES sso_providers (id) ON DELETE CASCADE,
    CONSTRAINT fk_sso_identities_user     FOREIGN KEY (user_id)     REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_sso_identities_tenant   FOREIGN KEY (tenant_id)   REFERENCES tenants (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
