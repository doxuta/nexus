-- Reverse of 000004: drop the tenant_id columns (and their indexes) added to sys_fields and
-- sys_relationships. Explicit reversal — drop the index before the column it covers. Reverse order
-- of the up (sys_relationships first, then sys_fields) for symmetry; order is not functionally
-- significant here since the two tables are independent.

ALTER TABLE sys_relationships
    DROP INDEX idx_sys_rel_tenant,
    DROP COLUMN tenant_id;

ALTER TABLE sys_fields
    DROP INDEX idx_sys_fields_tenant,
    DROP COLUMN tenant_id;
