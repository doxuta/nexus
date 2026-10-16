-- RM-42 / A6 Field-Level Security — per-field view/edit grants per role (spec:
-- docs/auth-security.md §A6 deferred note in 000005, docs/_planner/backend-audit-roadmap.md RM-42).
--
-- A4 (000005) authorizes (entity, action) at the MODULE level and A5 narrows ROWS; this table
-- adds the FIELD dimension: for a given (tenant, role, entity, field) it records whether the
-- role may VIEW (can_view) and/or EDIT (can_edit) that field. Enforcement (services layer):
--   - READ  (get/list/aggregate/export): a field with can_view=0 is STRIPPED from the data the
--     role sees — it never reaches the client.
--   - WRITE (create/update): a field with can_edit=0 present in the payload is rejected
--     403 FIELD_FORBIDDEN.
-- is_admin BYPASSES this entirely (super-user), exactly like the A4 module ACL.
--
-- DEFAULT-ALLOW semantics: a field with NO row here is fully visible+editable for the role.
-- Field ACL is therefore a RESTRICTIVE OVERLAY — admins only insert rows to HIDE/LOCK specific
-- sensitive fields (e.g. 'revenue', 'salary') for specific roles, and every pre-existing field
-- on every entity keeps working unchanged with no backfill. A row with can_view=0 implies the
-- field is also un-editable (you cannot edit what you cannot see); the write guard treats
-- can_view=0 as can_edit=0 regardless of the stored can_edit value (defense in depth).
--
-- Multi-tenant invariant (rules: đa tổ chức): every query MUST filter tenant_id. role_id FKs
-- acl_roles (same tenant) and CASCADES on role delete so a removed role drops its field grants.
-- entity_name + field_name are the dynamic entity/field NAMES (validated identifiers, server-side
-- metadata — never raw client input on the DDL path). MySQL 8 / InnoDB / utf8mb4.
CREATE TABLE IF NOT EXISTS sys_field_acl (
    id          CHAR(36)     NOT NULL PRIMARY KEY,
    tenant_id   CHAR(36)     NOT NULL,
    role_id     CHAR(36)     NOT NULL,
    entity_name VARCHAR(100) NOT NULL,                  -- dynamic entity name (e.g. 'accounts')
    field_name  VARCHAR(100) NOT NULL,                  -- dynamic field name (e.g. 'revenue')
    can_view    BOOLEAN      NOT NULL DEFAULT TRUE,      -- 0 → field stripped from read responses
    can_edit    BOOLEAN      NOT NULL DEFAULT TRUE,      -- 0 → field in a write payload → 403 FIELD_FORBIDDEN
    created_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    -- One grant row per (tenant, role, entity, field): a field's visibility/editability for a
    -- role is single-valued. tenant_id is part of the key so the same (role,entity,field) can
    -- never be smuggled across tenants (defense in depth on top of the role_id FK).
    UNIQUE KEY uq_sfa_tenant_role_entity_field (tenant_id, role_id, entity_name, field_name),
    -- Hot-path lookup: load all field grants for (tenant, role, entity) in one indexed read on
    -- every non-admin record request — covered by the unique key's leftmost prefix, but an
    -- explicit covering index keeps the planner honest and documents the access pattern.
    INDEX idx_sfa_lookup (tenant_id, role_id, entity_name),
    -- FK to acl_roles (the role must exist in the SAME tenant — the role row itself is
    -- tenant-scoped). ON DELETE CASCADE drops a role's field grants when the role is deleted,
    -- mirroring acl_role_actions (000005). Index the FK column first.
    INDEX idx_sfa_role (role_id),
    CONSTRAINT fk_sfa_role FOREIGN KEY (role_id) REFERENCES acl_roles (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
