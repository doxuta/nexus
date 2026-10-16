-- A2 debt (spec: docs/auth-security.md §A2, schema-patch lines 72-75; card t_366427ea folded into A2).
-- 000001 created sys_fields/sys_relationships WITHOUT a tenant_id column (sys_entities/sys_layouts
-- already have one). This hardens tenant isolation at the metadata layer. New migration — 000001 is
-- already applied, so we ALTER forward instead of editing it. MySQL 8 / InnoDB / utf8mb4.
--
-- Columns are NULL (not NOT NULL) on purpose: both tables already hold data, and we cannot add a
-- NOT NULL column without a default. sys_fields is backfilled from its owning entity; sys_relationships
-- is currently unused (relations live in sys_fields.options.relatedEntity) so it stays NULL.

ALTER TABLE sys_fields
    ADD COLUMN tenant_id CHAR(36) NULL,
    ADD INDEX idx_sys_fields_tenant (tenant_id);

ALTER TABLE sys_relationships
    ADD COLUMN tenant_id CHAR(36) NULL,
    ADD INDEX idx_sys_rel_tenant (tenant_id);

-- Backfill sys_fields.tenant_id from the entity that owns each field. sys_relationships is left NULL
-- (0 rows / 0 internal references today).
UPDATE sys_fields f
    JOIN sys_entities e ON f.entity_id = e.id
    SET f.tenant_id = e.tenant_id;
