-- Reverse of 000010: revert sys_fields.tenant_id and sys_relationships.tenant_id
-- back to CHAR(36) NULL. This restores the schema to the 000004 state.
-- The down does NOT re-NULL any values; rows that were backfilled retain their
-- tenant_id, so an up→down→up cycle is safe (the up backfill is a no-op the second
-- time because all rows already have tenant_id set).

ALTER TABLE sys_relationships
    MODIFY COLUMN tenant_id CHAR(36) NULL;

ALTER TABLE sys_fields
    MODIFY COLUMN tenant_id CHAR(36) NULL;
