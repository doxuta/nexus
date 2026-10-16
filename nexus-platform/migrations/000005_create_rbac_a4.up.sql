-- A4 RBAC schema (spec: docs/auth-security.md §A4, lines 79-94).
-- Module/action-level role-based access control: each role owns a set of
-- (entity_name, action) grants whose access_level drives row-level scope (A5).
-- The user<->role mapping is users.role_id (added nullable in 000003 line 28 as a
-- forward reference); this migration closes that loop by adding the deferred FK.
-- There is intentionally NO separate acl_user_roles table and NO field-level
-- table here: field-level security is A6 (deferred). MySQL 8 / InnoDB / utf8mb4.

-- acl_roles: a named role per tenant. UNIQUE(tenant_id, name) so role names are
-- unique within a tenant but may repeat across tenants. FK to tenants enforces
-- tenant existence; idx covers the FK and tenant-scoped lookups.
CREATE TABLE IF NOT EXISTS acl_roles (
    id         CHAR(36)     NOT NULL PRIMARY KEY,
    tenant_id  CHAR(36)     NOT NULL,
    name       VARCHAR(100) NOT NULL,
    label      VARCHAR(150) NOT NULL,
    created_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uq_acl_roles_tenant_name (tenant_id, name),
    INDEX idx_acl_roles_tenant (tenant_id),
    CONSTRAINT fk_acl_roles_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- acl_role_actions: the grant rows for a role. entity_name '*' = every entity.
-- action is one of view/create/edit/delete. access_level is the row-level scope
-- (none/owner/team/all, DEFAULT 'all') consumed by the A5 team-security filter.
-- UNIQUE(role_id, entity_name, action) makes each (role, entity, action) grant
-- single-valued; the FK to acl_roles cascades so deleting a role drops its grants.
CREATE TABLE IF NOT EXISTS acl_role_actions (
    id           CHAR(36)     NOT NULL PRIMARY KEY,
    role_id      CHAR(36)     NOT NULL,
    entity_name  VARCHAR(100) NOT NULL,                    -- '*' = every entity
    action       VARCHAR(20)  NOT NULL,                    -- view/create/edit/delete
    access_level VARCHAR(20)  NOT NULL DEFAULT 'all',      -- none/owner/team/all (row-level)
    created_at   TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uq_ara_role_entity_action (role_id, entity_name, action),
    INDEX idx_ara_role (role_id),
    CONSTRAINT fk_ara_role FOREIGN KEY (role_id) REFERENCES acl_roles (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Close the forward reference from 000003: users.role_id now FKs acl_roles.
-- Index the FK column first (FKs must be indexed), then add the constraint.
-- ON DELETE SET NULL: deleting a role detaches its users rather than deleting them
-- (the A1 is_admin super-gate still works while role_id is NULL).
ALTER TABLE users
    ADD INDEX idx_users_role (role_id),
    ADD CONSTRAINT fk_users_role FOREIGN KEY (role_id) REFERENCES acl_roles (id) ON DELETE SET NULL;
