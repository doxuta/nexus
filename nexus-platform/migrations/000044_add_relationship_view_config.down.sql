-- Reversal of 000044_add_relationship_view_config.up.sql
ALTER TABLE sys_relationships
    DROP COLUMN view_config;
