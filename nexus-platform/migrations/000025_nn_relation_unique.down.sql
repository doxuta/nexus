-- Reversal of 000008_nn_relation_unique.up.sql
ALTER TABLE sys_relationships
    DROP INDEX uq_sysrel_tenant_join_table,
    DROP INDEX uq_sysrel_tenant_name;
