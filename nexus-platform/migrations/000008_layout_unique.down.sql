-- Revert the unique constraint added in 000008_layout_unique.up.sql.
ALTER TABLE sys_layouts
    DROP INDEX uq_sys_layouts_tenant_entity_view;
