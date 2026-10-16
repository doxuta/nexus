-- Reversal of 000028_relationship_inverse_registry.up.sql
ALTER TABLE sys_relationships
    DROP INDEX idx_sys_rel_inverse,
    DROP COLUMN inverse_field,
    DROP COLUMN child_fk_column;
