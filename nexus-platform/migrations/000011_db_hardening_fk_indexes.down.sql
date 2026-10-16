-- Down for 000011_db_hardening_fk_indexes — reverses RM-23, RM-22, RM-21 in reverse order.
-- Brings the schema back to v10 (FK + all 4 indexes gone). MySQL 8 / InnoDB / utf8mb4.

-- RM-23 (last applied) — drop team_sets.primary_team_id index first.
ALTER TABLE team_sets
    DROP INDEX idx_ts_primary_team;

-- RM-22 — drop the two refresh_tokens composite indexes.
ALTER TABLE refresh_tokens
    DROP INDEX idx_rt_user_active,
    DROP INDEX idx_rt_hash_tenant;

-- RM-21 — drop the sys_fields -> sys_entities foreign key.
ALTER TABLE sys_fields
    DROP FOREIGN KEY fk_sys_fields_entity;
