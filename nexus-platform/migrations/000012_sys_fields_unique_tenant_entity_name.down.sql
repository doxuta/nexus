-- Down for 000012_sys_fields_unique_tenant_entity_name — drops the unique key, restoring the
-- v11 schema (sys_fields has only the non-unique idx_sys_fields_entity / _tenant / _deleted
-- indexes). MySQL 8 / InnoDB / utf8mb4. up→down→up is clean: the down removes the constraint
-- without touching data, so a subsequent up re-runs the orphan guard + ADD UNIQUE on the same
-- rows. IF EXISTS is not supported for DROP INDEX in MySQL 8, so this assumes the up ran.
ALTER TABLE sys_fields
    DROP INDEX uq_sys_fields_tenant_entity_name;
