-- Add a unique constraint on (tenant_id, entity_name, view_type) so each tenant can have
-- at most one custom layout per (entity, view). This makes the upsert path race-safe:
-- INSERT ... ON DUPLICATE KEY UPDATE resolves at DB level rather than relying on a
-- check-then-insert in application code. Hard-delete (no soft-delete) avoids the
-- partial-unique-index problem that MySQL 8 does not support for nullable deleted_at.
-- Note: MySQL 8 has no portable `ADD UNIQUE KEY IF NOT EXISTS`, so this DDL is not strictly
-- idempotent (re-applying on an already-migrated DB errors on the duplicate key) — acceptable
-- per database.md given golang-migrate's version table prevents re-application.
ALTER TABLE sys_layouts
    ADD UNIQUE KEY uq_sys_layouts_tenant_entity_view (tenant_id, entity_name, view_type);
