-- RM-19 Audit Log — general mutation audit trail (spec: docs/audit-log-spec.md §2).
-- Append-only: one row per committed mutation (record CRUD + admin entity/field/role
-- meta changes), written asynchronously from the service layer AFTER the business
-- mutation commits (fail-open — an audit-write failure never blocks the flow).
--
-- Multi-tenant invariant: every query against audit_log MUST filter tenant_id (rules:
-- đa tổ chức). entity_name carries the dynamic entity name for record mutations
-- ('accounts', ...) or a reserved meta key for admin mutations ('__entity__' /
-- '__field__' / '__role__' / '__team__'). record_id is the affected record id, NULL for
-- admin-meta mutations that have no obj_* record. diff stores {before,after} of only the
-- fields that actually changed (create = after-only, delete = before-only). No FK on
-- tenant_id / actor_user_id / record_id by design — the trail must survive (and stay
-- auditable) even after the referenced account or record is later deleted. No soft-delete
-- / update path: this is an immutable trail (audit integrity). MySQL 8 / InnoDB / utf8mb4.
CREATE TABLE IF NOT EXISTS audit_log (
    id            CHAR(36)     NOT NULL,
    tenant_id     CHAR(36)     NOT NULL,
    actor_user_id CHAR(36)     NOT NULL,                          -- who performed it (from the verified JWT subject)
    entity_name   VARCHAR(100) NOT NULL,                          -- 'accounts' | '__entity__' | '__field__' | '__role__' | '__team__'
    record_id     CHAR(36)     DEFAULT NULL,                      -- affected record id; NULL for admin-meta mutations
    action        VARCHAR(20)  NOT NULL,                          -- create | update | delete | restore
    diff          JSON         DEFAULT NULL,                      -- {before:{...}, after:{...}} — only changed fields
    at            TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_al_tenant_at (tenant_id, at),                         -- tenant-wide timeline
    KEY idx_al_record    (tenant_id, entity_name, record_id, at), -- history of one record
    KEY idx_al_actor     (tenant_id, actor_user_id, at)           -- "what did user X do"
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
